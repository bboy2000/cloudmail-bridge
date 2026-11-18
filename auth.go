package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// authManager 负责"把邮箱密码交给网站验证"，并为每个已登录用户提供独立 client。
//
// 设计要点（对应用户需求）：
//   - 不限制邮箱数量：任意邮箱 + 正确密码即可登录，配置里不写死邮箱名单。
//   - 密码交给网站验证：调 cloud-mail /api/login；密码错直接拒绝。
//   - 验证一次缓存 JWT：同一邮箱在 TTL 内重复登录可复用，减少 CF 请求；
//     会话级语义——TTL 到期或密码变更导致 401 时会自动重新验证。
//   - 每用户独立 client：JWT / 账号列表 / 发信身份互相隔离，不串号。
type authManager struct {
	baseURL string
	admin   *cloudMailClient // 管理员 client：提供 public token（按 toEmail 拉收件）
	cfg     *Config
	store   *mailStore

	mu      sync.Mutex
	entries map[string]*authEntry // 归一化邮箱 -> 缓存项

	ttl time.Duration
}

type authEntry struct {
	client    *cloudMailClient // 该用户的专属 client（带用户 JWT）
	expiresAt time.Time
	syncedAt  time.Time
	active    int // 当前在线的客户端会话数（IMAP+SMTP），0 表示无人在线
}

// incActive / decActive 维护用户在线会话计数。
// 后台轮询只对 active>0 的用户同步——客户端全关掉后不再读 cloud-mail，
// 从根上避免 D1 免费额度被空闲轮询读穿。
func (a *authManager) incActive(email string) {
	a.mu.Lock()
	e := a.ensureEntry(normEmail(email))
	e.active++
	a.mu.Unlock()
}

func (a *authManager) decActive(email string) {
	a.mu.Lock()
	if e, ok := a.entries[normEmail(email)]; ok && e.active > 0 {
		e.active--
	}
	a.mu.Unlock()
}

// ensureEntry 返回邮箱对应条目，不存在则创建。调用方须持有 a.mu。
func (a *authManager) ensureEntry(email string) *authEntry {
	e, ok := a.entries[email]
	if !ok {
		e = &authEntry{expiresAt: time.Now().Add(a.ttl)}
		a.entries[email] = e
	}
	return e
}

func newAuthManager(baseURL string, admin *cloudMailClient, cfg *Config, store *mailStore) *authManager {
	ttl := time.Duration(cfg.JWTCacheTTL) * time.Hour
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &authManager{
		baseURL: baseURL,
		admin:   admin,
		cfg:     cfg,
		store:   store,
		entries: make(map[string]*authEntry),
		ttl:     ttl,
	}
}

func normEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// authenticate 验证邮箱+密码并返回一个带该用户 JWT 的专属 client。
//
// 按用户明确选择的"方案①（会话级）"：每个新连接都调 /api/login 重新验证，
// 不跨连接复用 JWT —— 这样邮箱密码一旦修改，新连接立刻用新密码验证，
// 旧密码会验证失败、无法收发（符合用户 B 的预期）。
// 同一连接内由 session 持有返回的 client，无需每次命令重复验证。
func (a *authManager) authenticate(email, password string) (*cloudMailClient, error) {
	email = normEmail(email)
	if email == "" || password == "" {
		return nil, fmt.Errorf("empty email or password")
	}
	if !strings.Contains(email, "@") {
		return nil, fmt.Errorf("invalid email")
	}

	// 建该用户专属 client，设置管理员凭据（用于 public token 拉收件 + token 过期自动续）
	c := newCloudMailClient(a.baseURL)
	c.setAdmin(a.cfg.AdminEmail, a.cfg.AdminPassword)
	if a.cfg.PublicToken != "" {
		c.SetToken(a.cfg.PublicToken)
	}
	c.setLoginCred(email, password)

	// 让用户 JWT 立刻生成。login() 内部直接打 /api/login，密码错会返回错误。
	tok, err := c.login()
	if err != nil {
		return nil, fmt.Errorf("网站验证失败: %w", err)
	}
	log.Printf("[auth] 网站验证通过: %s (jwt=%s)", email, maskToken(tok))

	// 记录最近一次登录的 client，供已读回写（clientFor）与同步状态使用。
	// 注意：这里保留的是"最近会话"的引用，不作为认证缓存 —— 认证每次都走网站。
	a.mu.Lock()
	e := a.ensureEntry(email)
	e.client = c
	e.expiresAt = time.Now().Add(a.ttl)
	a.mu.Unlock()
	return c, nil
}

// ensureSynced 登录成功后按该用户 JWT 动态发现其名下所有账号并同步收件/Sent。
// 每个邮箱只在首次登录或超过 syncInterval 时同步一次，避免重复拉取。
// finder 用该用户 JWT 的 account/list 发现账号；收件用 public token 按 toEmail 拉。
func (a *authManager) ensureSynced(email string, c *cloudMailClient) error {
	return a.syncUser(email, c, false)
}

// syncAll 供后台定时轮询调用：对所有已登录过的用户强制同步一次。
// poll_interval 本身已经做了节流，因此这里忽略 sync_interval，
// 否则动态账号模式下新邮件最长要等 sync_interval（默认 5 分钟）才进 IMAP。
func (a *authManager) syncAll() {
	type pair struct {
		email string
		c     *cloudMailClient
	}
	a.mu.Lock()
	pairs := make([]pair, 0, len(a.entries))
	for email, e := range a.entries {
		// 只同步有客户端在线的用户；无人在线时完全不读 cloud-mail（省 D1 额度）。
		if e.client != nil && e.active > 0 {
			pairs = append(pairs, pair{email: email, c: e.client})
		}
	}
	a.mu.Unlock()
	for _, p := range pairs {
		// 不再 force：遵守 sync_interval，避免高频轮询把 cloud-mail 的 D1 免费额度读穿。
		if err := a.syncUser(p.email, p.c, false); err != nil {
			log.Printf("[poll] %s 后台同步失败: %v", p.email, err)
		}
	}
}

// syncUser 实际同步逻辑。force=true 时忽略 sync_interval。
func (a *authManager) syncUser(email string, c *cloudMailClient, force bool) error {
	email = normEmail(email)
	a.mu.Lock()
	e := a.ensureEntry(email)
	needSync := force || e.client == nil || time.Since(e.syncedAt) > time.Duration(a.cfg.SyncInterval)*time.Minute
	e.client = c
	a.mu.Unlock()
	if !needSync {
		return nil
	}

	// 用该用户 JWT 发现名下所有账号
	accounts, err := c.listAccounts()
	if err != nil {
		log.Printf("[sync] %s listAccounts ERROR: %v", email, err)
		return err
	}
	if len(accounts) == 0 {
		// 兜底：至少同步登录邮箱本身
		accounts = []mailAccount{{Email: email}}
		log.Printf("[sync] %s 未发现账号，兜底用登录邮箱本身", email)
	}
	emails := make([]string, 0, len(accounts))
	for _, ac := range accounts {
		if ac.Email != "" {
			emails = append(emails, normEmail(ac.Email))
		}
	}
	log.Printf("[sync] %s 名下账号(%d): %v", email, len(emails), emails)

	// 收件：public token 按每个账号的 toEmail 拉（管理员级可见全部收件）
	for _, addr := range emails {
		total := 0
		for page := 1; ; page++ {
			items, err := c.fetchEmailList(addr, 0, page, a.cfg.PageSize)
			if err != nil {
				log.Printf("[sync] %s inbox %s page=%d ERROR: %v", email, addr, page, err)
				break
			}
			if len(items) == 0 {
				break
			}
			total += a.store.addReceived(email, items, a.cfg.Domain, nil)
			if a.cfg.MaxPages > 0 && page >= a.cfg.MaxPages {
				break
			}
			if len(items) < a.cfg.PageSize {
				break
			}
		}
		log.Printf("[sync] %s <- %s 收件 +%d（累计 INBOX %d）", email, addr, total, len(a.store.messages(email, "INBOX")))
	}

	// unread 状态 + 真实发件：用该用户 JWT，按登录邮箱对应 accountId
	acID := int64(0)
	for _, ac := range accounts {
		if normEmail(ac.Email) == email {
			acID = ac.AccountID
			break
		}
	}
	if acID == 0 && len(accounts) > 0 {
		acID = accounts[0].AccountID
	}
	if acID > 0 {
		if recv, err := c.fetchAllEmailList(acID, 0); err == nil {
			um := make(map[int64]bool, len(recv))
			for _, it := range recv {
				um[it.EmailID] = it.Unread == 1
			}
			a.store.applyUnread(email, um)
			a.store.addReceived(email, recv, a.cfg.Domain, um)
		} else {
			log.Printf("[sync] %s unread ERROR: %v", email, err)
		}
		if sent, err := c.fetchAllEmailList(acID, 1); err == nil {
			a.store.addSentFromCloud(email, sent, a.cfg.Domain)
		} else {
			log.Printf("[sync] %s sent ERROR: %v", email, err)
		}
	}

	a.mu.Lock()
	e.syncedAt = time.Now()
	a.mu.Unlock()
	log.Printf("[sync] %s 同步完成", email)
	return nil
}

// clientFor 返回某已登录用户的专属 client（用于已读回写）。未登录返回 error。
func (a *authManager) clientFor(email string) (*cloudMailClient, error) {
	a.mu.Lock()
	e, ok := a.entries[normEmail(email)]
	a.mu.Unlock()
	if !ok || e.client == nil {
		return nil, fmt.Errorf("用户 %s 未登录或缓存已过期", email)
	}
	return e.client, nil
}

// invalidate 密码变更等场景主动清掉某用户缓存（预留，当前会话级语义由 TTL + 401 触发）。
func (a *authManager) invalidate(email string) {
	a.mu.Lock()
	delete(a.entries, normEmail(email))
	a.mu.Unlock()
}
