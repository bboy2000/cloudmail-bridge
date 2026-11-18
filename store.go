package main

import (
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
)

// storedMessage 是一条已转成 RFC822 的邮件。uid 直接沿用 cloud-mail 的 emailId（稳定唯一）。
type storedMessage struct {
	uid   imap.UID
	emailID int64
	raw   []byte
	flags []imap.Flag
	date  time.Time
	seen  bool
}

// mailStore 全局内存存储：按 用户 -> 邮箱文件夹 -> 邮件列表 组织。
type mailStore struct {
	mu            sync.RWMutex
	data          map[string]map[string][]*storedMessage
	knownIDs      map[string]map[int64]bool // user -> emailId -> 是否已入库（去重）
	onNewMail     func(user string, m *storedMessage)
	onSeenChange  func(user string, emailID int64, seen bool) // 已读标记变更回调（回写 cloud-mail）
	sentSeq       imap.UID // 本地发出邮件的 UID 序列（高位段，避免和 emailId 冲突）

	// IDLE 订阅：user -> 一组订阅者（每个 IDLE 的 session 一个 channel）
	idleMu   sync.Mutex
	idleSubs map[string]map[chan struct{}]struct{}
}

// sentUIDBase 本地 Sent 邮件的 UID 起点（高位段）。小于此值的 uid 是 cloud-mail 真实 emailId。
const sentUIDBase = imap.UID(100000000)

var knownMailboxes = []string{"INBOX", "Sent", "Drafts", "Trash", "Junk"}

func newMailStore() *mailStore {
	s := &mailStore{
		data:     make(map[string]map[string][]*storedMessage),
		knownIDs: make(map[string]map[int64]bool),
		idleSubs: make(map[string]map[chan struct{}]struct{}),
	}
	for _, u := range []string{""} {
		s.ensureUser(u)
	}
	return s
}

// subscribeIdle 注册一个 IDLE 订阅者，返回通知 channel（buffered=1，避免阻塞广播）。
func (s *mailStore) subscribeIdle(user string) chan struct{} {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	if s.idleSubs[user] == nil {
		s.idleSubs[user] = make(map[chan struct{}]struct{})
	}
	ch := make(chan struct{}, 1)
	s.idleSubs[user][ch] = struct{}{}
	return ch
}

func (s *mailStore) unsubscribeIdle(user string, ch chan struct{}) {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	if subs := s.idleSubs[user]; subs != nil {
		delete(subs, ch)
	}
}

// notifyIdle 非阻塞地通知某用户所有 IDLE 订阅者"有新邮件"。
func (s *mailStore) notifyIdle(user string) {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	for ch := range s.idleSubs[user] {
		select {
		case ch <- struct{}{}:
		default: // 已有待处理通知，跳过
		}
	}
}

func (s *mailStore) ensureUser(user string) {
	if _, ok := s.data[user]; !ok {
		s.data[user] = make(map[string][]*storedMessage)
		for _, mb := range knownMailboxes {
			s.data[user][mb] = []*storedMessage{}
		}
	}
	if _, ok := s.knownIDs[user]; !ok {
		s.knownIDs[user] = make(map[int64]bool)
	}
}

// messages 返回某用户某文件夹的邮件（只读快照指针，调用方需在外层持锁或只读）。
func (s *mailStore) messages(user, mailbox string) []*storedMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.ensureUser(user)
	return s.data[user][mailbox]
}

// addReceived 把 cloud-mail 拉回来的收信写入 INBOX；返回新增条数。
// unreadMap 可选（emailId -> 是否已读，来自 JWT 接口）；nil 时一律按未读。
func (s *mailStore) addReceived(user string, items []emailItem, domain string, unreadMap map[int64]bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureUser(user)

	added := 0
	for _, item := range items {
		if s.knownIDs[user][item.EmailID] {
			continue
		}
		// createTime 是 UTC，按 UTC 解析（ParseInLocation+Local 会早 8 小时），再转本地展示
		t, err := time.Parse("2006-01-02 15:04:05", item.CreateTime)
		if err != nil {
			t, err = time.Parse(time.RFC3339, item.CreateTime)
			if err != nil {
				t = time.Now()
			}
		}
		t = t.Local()
		seen := unreadMap[item.EmailID] // true=已读（unread=1）
		flags := []imap.Flag{}
		if seen {
			flags = append(flags, imap.FlagSeen)
		}
		m := &storedMessage{
			uid:     imap.UID(item.EmailID),
			emailID: item.EmailID,
			raw:     buildRawEmail(item, domain),
			flags:   flags,
			date:    t,
			seen:    seen,
		}
		s.data[user]["INBOX"] = append(s.data[user]["INBOX"], m)
		s.knownIDs[user][item.EmailID] = true
		added++
		if s.onNewMail != nil {
			go s.onNewMail(user, m)
		}
	}
	// 按 emailId 升序排列，保证 IMAP seq 稳定
	sort.SliceStable(s.data[user]["INBOX"], func(i, j int) bool {
		return s.data[user]["INBOX"][i].emailID < s.data[user]["INBOX"][j].emailID
	})
	if added > 0 {
		log.Printf("[store] user=%s INBOX +%d (total %d)", user, added, len(s.data[user]["INBOX"]))
		// 释放锁后再广播，避免与 IDLE session 读锁互相等待
		go s.notifyIdle(user)
	}
	return added
}

// setSeen 修改已读标记（STORE \Seen 时调用）。
// 若改的是 cloud-mail 真实收信（uid < sentUIDBase，即 emailId），异步回写到远端。
// 注意：cloud-mail 只有"标记已读"接口，没有"标记未读"接口；取消已读只改本地。
func (s *mailStore) setSeen(user, mailbox string, uid imap.UID, seen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.data[user][mailbox]
	for _, m := range msgs {
		if m.uid == uid {
			if m.seen == seen {
				return // 状态没变，不回写
			}
			m.seen = seen
			m.flags = []imap.Flag{}
			if seen {
				m.flags = append(m.flags, imap.FlagSeen)
			}
			// 只对真实收信（INBOX + 真实 emailId）回写；本地 Sent 不动
			if mailbox == "INBOX" && uid < sentUIDBase && m.emailID > 0 && s.onSeenChange != nil {
				go s.onSeenChange(user, m.emailID, seen)
			}
			return
		}
	}
}

// applyUnread 用远端的 unread 状态校正已入库 INBOX 邮件的 seen 标记。
// 只校正状态不同的，且不触发回写（这是远端 -> 本地的单向校正）。
func (s *mailStore) applyUnread(user string, unreadMap map[int64]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := 0
	for _, m := range s.data[user]["INBOX"] {
		want, ok := unreadMap[m.emailID]
		if !ok {
			continue
		}
		if m.seen != want {
			m.seen = want
			m.flags = []imap.Flag{}
			if want {
				m.flags = append(m.flags, imap.FlagSeen)
			}
			changed++
		}
	}
	if changed > 0 {
		log.Printf("[store] user=%s unread 校正 %d 封", user, changed)
	}
}

// addSent 把 SMTP 发出的邮件写入本地 Sent 文件夹（供 IMAP 客户端「已发送」可见）。
// uid 用 cloud-mail 返回的真实 emailId —— 与从远端同步的 Sent 天然去重。
func (s *mailStore) addSent(user string, raw []byte, subject string, uid imap.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureUser(user)
	if s.knownIDs[user][int64(uid)] {
		return // 已在 Sent（远端同步过），避免重复
	}
	m := &storedMessage{
		uid:     uid,
		emailID: int64(uid),
		raw:     raw,
		flags:   []imap.Flag{imap.FlagSeen},
		date:    time.Now(),
		seen:    true,
	}
	s.data[user]["Sent"] = append(s.data[user]["Sent"], m)
	s.knownIDs[user][int64(uid)] = true
	s.sortSent(user)
	log.Printf("[store] user=%s Sent +1 (total %d, uid=%d)", user, len(s.data[user]["Sent"]), uid)
}

// addSentFromCloud 把从 cloud-mail 拉回的真实发件记录写入 Sent。
// 用 emailItem 直接构造 RFC822（发信人=自己，收件人=toEmail）。
func (s *mailStore) addSentFromCloud(user string, items []emailItem, domain string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureUser(user)
	added := 0
	for _, item := range items {
		if s.knownIDs[user][item.EmailID] {
			continue
		}
		// createTime 是 UTC，按 UTC 解析（ParseInLocation+Local 会早 8 小时），再转本地展示
		t, err := time.Parse("2006-01-02 15:04:05", item.CreateTime)
		if err != nil {
			t, err = time.Parse(time.RFC3339, item.CreateTime)
			if err != nil {
				t = time.Now()
			}
		}
		t = t.Local()
		m := &storedMessage{
			uid:     imap.UID(item.EmailID),
			emailID: item.EmailID,
			raw:     buildRawEmail(item, domain),
			flags:   []imap.Flag{imap.FlagSeen}, // 发件默认已读
			date:    t,
			seen:    true,
		}
		s.data[user]["Sent"] = append(s.data[user]["Sent"], m)
		s.knownIDs[user][item.EmailID] = true
		added++
	}
	if added > 0 {
		s.sortSent(user)
		log.Printf("[store] user=%s Sent(from cloud) +%d (total %d)", user, added, len(s.data[user]["Sent"]))
		go s.notifyIdle(user)
	}
	return added
}

func (s *mailStore) sortSent(user string) {
	sort.SliceStable(s.data[user]["Sent"], func(i, j int) bool {
		return s.data[user]["Sent"][i].emailID < s.data[user]["Sent"][j].emailID
	})
}

// injectFake 往 INBOX 注入一封假邮件（仅调试 IDLE 用）。
func (s *mailStore) injectFake(user, subject string) {
	s.mu.Lock()
	s.ensureUser(user)
	uid := imap.UID(2000000000 + s.sentSeq)
	s.sentSeq++
	m := &storedMessage{
		uid:     uid,
		emailID: int64(uid),
		raw:     buildRawEmail(emailItem{EmailID: int64(uid), SendEmail: "idle-test@example.com", SendName: "IDLE Test", Subject: subject, Text: "idle push test", CreateTime: time.Now().Format("2006-01-02 15:04:05")}, "example.com"),
		flags:   []imap.Flag{},
		date:    time.Now(),
		seen:    false,
	}
	s.data[user]["INBOX"] = append(s.data[user]["INBOX"], m)
	s.mu.Unlock()
	log.Printf("[inject] user=%s INBOX +1 fake mail uid=%d subject=%q", user, uid, subject)
	go s.notifyIdle(user)
}

func nextUID(msgs []*storedMessage) imap.UID {
	var max imap.UID
	for _, m := range msgs {
		if m.uid > max {
			max = m.uid
		}
	}
	return max + 1
}

// syncFromCloud 为所有配置用户拉取收件+发件。返回 用户->收件新增条数。
// 收件仍走 public/emailList（拿正文），另用 JWT 的 /email/list 拿 unread 状态初始化；
// 发件走 JWT 的 /email/list?type=1 拉真实记录进 Sent（重启不丢）。
func syncFromCloud(client *cloudMailClient, cfg *Config, store *mailStore) map[string]int {
	result := make(map[string]int)
	users := cfg.Users
	if len(users) == 0 {
		log.Printf("[sync] 未配置 users，跳过同步（可在 config.json 里填 users）")
		return result
	}

	// 有登录凭据才查 unread + 同步发件（需要 JWT）
	canJWT := cfg.LoginPassword != ""
	var accounts []mailAccount
	if canJWT {
		var err error
		accounts, err = client.listAccounts()
		if err != nil {
			log.Printf("[sync] listAccounts ERROR (跳过 unread/Sent 同步): %v", err)
			canJWT = false
		}
	}

	for _, user := range users {
		// --- 收件：public 接口翻页拉正文 ---
		total := 0
		for page := 1; ; page++ {
			items, err := client.fetchEmailList(user, 0, page, cfg.PageSize)
			if err != nil {
				log.Printf("[sync] user=%s inbox page=%d ERROR: %v", user, page, err)
				break
			}
			if len(items) == 0 {
				break
			}
			total += store.addReceived(user, items, cfg.Domain, nil)
			if cfg.MaxPages > 0 && page >= cfg.MaxPages {
				break
			}
			if len(items) < cfg.PageSize {
				break
			}
		}
		result[user] = total

		// --- JWT 增强：unread 状态 + 真实发件 ---
		if canJWT {
			accountID := int64(0)
			for _, a := range accounts {
				if strings.EqualFold(a.Email, user) {
					accountID = a.AccountID
					break
				}
			}
			if accountID == 0 && len(accounts) > 0 {
				accountID = accounts[0].AccountID
			}
			if accountID > 0 {
				// 收件 unread：拉全量收件，建 emailId -> 已读 映射，补齐已入库邮件的 seen
				recv, err := client.fetchAllEmailList(accountID, 0)
				if err != nil {
					log.Printf("[sync] user=%s unread ERROR: %v", user, err)
				} else {
					unreadMap := make(map[int64]bool, len(recv))
					for _, it := range recv {
						unreadMap[it.EmailID] = it.Unread == 1
					}
					store.applyUnread(user, unreadMap)
					// 也用这份数据补齐正文（public 接口若漏了某些信）
					store.addReceived(user, recv, cfg.Domain, unreadMap)
				}
				// 发件：拉真实发件记录进 Sent
				sent, err := client.fetchAllEmailList(accountID, 1)
				if err != nil {
					log.Printf("[sync] user=%s sent ERROR: %v", user, err)
				} else {
					store.addSentFromCloud(user, sent, cfg.Domain)
				}
			}
		}
	}
	return result
}
