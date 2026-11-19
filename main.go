// cloudmail-bridge 阶段2：IMAP 服务端 + cloud-mail API 真实数据接入
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// ---------- Session 实现 ----------

type session struct {
	store    *mailStore
	auth     *authManager
	user     string
	client   *cloudMailClient // 该会话登录用户的专属 client（带 JWT）
	mu       sync.Mutex
	selected string
	closed   bool
	active   bool // 该会话是否已计入 auth 的在线计数
	restored bool // 本连接是否已处理过"下载全部邮件"请求，避免重复触发
}

func (s *session) Close() error {
	s.mu.Lock()
	if s.active && s.user != "" {
		s.auth.decActive(s.user)
		s.active = false
	}
	s.closed = true
	s.mu.Unlock()
	return nil
}

func (s *session) Login(username, password string) error {
	if strings.TrimSpace(username) == "" || password == "" {
		return imapserver.ErrAuthFailed
	}
	// 密码交给 cloud-mail 网站验证；验证失败直接拒绝登录
	c, err := s.auth.authenticate(username, password)
	if err != nil {
		log.Printf("LOGIN FAIL %s: %v", username, err)
		return imapserver.ErrAuthFailed
	}
	// 同一连接重复 LOGIN：先释放旧账号的在线计数，避免泄漏
	if s.active && s.user != "" {
		s.auth.decActive(s.user)
		s.active = false
	}
	s.user = normEmail(username)
	s.client = c
	s.store.ensureUser(s.user)
	// 计入在线会话：只要这个连接还开着，后台就会为该用户同步
	s.auth.incActive(s.user)
	s.active = true
	// 登录成功后按需同步该用户（含其名下所有小号）的收件
	go s.auth.ensureSynced(s.user, c)
	log.Printf("LOGIN ok: %s (active sessions tracked)", s.user)
	return nil
}

func (s *session) Select(mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	msgs := s.store.messages(s.user, mailbox)
	if msgs == nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Mailbox not found"}
	}
	s.selected = mailbox
	var firstUnseen uint32
	for i, m := range msgs {
		if !m.seen {
			firstUnseen = uint32(i + 1)
			break
		}
	}
	log.Printf("SELECT %s user=%s: %d messages", mailbox, s.user, len(msgs))
	// 只声明真正支持的标志。\Answered / \Flagged / \Draft 在 cloud-mail 侧没有
	// 对应的写接口（星标是独立的 star 表，要额外查询会多耗 D1 额度），
	// 若在此声明为可写，客户端会发 STORE 却存不住，点星标就会没反应。
	// 不多承诺，客户端就不会显示这些操作入口。
	return &imap.SelectData{
		Flags:             []imap.Flag{imap.FlagSeen, imap.FlagDeleted},
		PermanentFlags:    []imap.Flag{imap.FlagSeen, imap.FlagDeleted},
		NumMessages:       uint32(len(msgs)),
		FirstUnseenSeqNum: firstUnseen,
		UIDNext:           nextUID(msgs),
		UIDValidity:       1,
	}, nil
}

func (s *session) Create(mailbox string, options *imap.CreateOptions) error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "CREATE not implemented"}
}
func (s *session) Delete(mailbox string) error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "DELETE not implemented"}
}
func (s *session) Rename(mailbox, newName string, options *imap.RenameOptions) error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "RENAME not implemented"}
}
func (s *session) Subscribe(mailbox string) error   { return nil }
func (s *session) Unsubscribe(mailbox string) error { return nil }

func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	s.store.ensureUser(s.user)
	for _, name := range knownMailboxes {
		msgs := s.store.data[s.user][name]
		attrs := []imap.MailboxAttr{imap.MailboxAttrHasNoChildren}
		switch name {
		case "Sent":
			attrs = append(attrs, imap.MailboxAttrSent)
		case "Drafts":
			attrs = append(attrs, imap.MailboxAttrDrafts)
		case "Trash":
			attrs = append(attrs, imap.MailboxAttrTrash)
		case "Junk":
			attrs = append(attrs, imap.MailboxAttrJunk)
		}
		data := &imap.ListData{Attrs: attrs, Delim: '/', Mailbox: name}
		if options != nil && options.ReturnStatus != nil {
			n := uint32(len(msgs))
			data.Status = &imap.StatusData{Mailbox: name, NumMessages: &n, UIDNext: nextUID(msgs), UIDValidity: 1}
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	log.Printf("LIST done for user %s", s.user)
	return nil
}

func (s *session) Status(mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	msgs := s.store.messages(s.user, mailbox)
	if msgs == nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Mailbox not found"}
	}
	n := uint32(len(msgs))
	unseen := uint32(s.store.unseenCount(s.user, mailbox))
	deleted := uint32(s.store.deletedCountIn(s.user, mailbox))
	// 未读数要返回，否则客户端未读角标不准（之前恒为空，部分客户端显示全部未读）。
	return &imap.StatusData{
		Mailbox:     mailbox,
		NumMessages: &n,
		NumUnseen:   &unseen,
		NumDeleted:  &deleted,
		UIDNext:     nextUID(msgs),
		UIDValidity: 1,
	}, nil
}

func (s *session) Append(mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "APPEND not implemented"}
}
func (s *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error { return nil }

// Idle 实现 IMAP IDLE：订阅 store 的新邮件通知，有新邮件就向客户端推 EXISTS。
// 没有真正推送通道时，靠轮询（poll_interval）触发 notifyIdle，属于准实时。
func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if s.selected == "" {
		<-stop
		return nil
	}
	ch := s.store.subscribeIdle(s.user)
	defer s.store.unsubscribeIdle(s.user, ch)
	log.Printf("[idle] user=%s mailbox=%s started", s.user, s.selected)
	defer log.Printf("[idle] user=%s mailbox=%s stopped", s.user, s.selected)
	for {
		select {
		case <-stop:
			return nil
		case <-ch:
			msgs := s.store.messages(s.user, s.selected)
			if err := w.WriteNumMessages(uint32(len(msgs))); err != nil {
				return err
			}
			log.Printf("[idle] user=%s mailbox=%s -> EXISTS %d", s.user, s.selected, len(msgs))
		}
	}
}
func (s *session) Unselect() error { s.selected = ""; return nil }

// Expunge 移除已标记 \Deleted 的邮件（同时回写云端删除）。
// 之前这里是空实现，导致客户端删掉的邮件在下次同步时又从云端拉回来。
// uids 非 nil 时只处理这些 UID（UID EXPUNGE 扩展）。
func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	if s.selected == "" {
		return nil
	}
	seqNums, _ := s.store.expungeDeleted(s.user, s.selected)
	for _, seq := range seqNums {
		if err := w.WriteExpunge(seq); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	msgs := s.store.messages(s.user, s.selected)
	var seqs []uint32
	var uids []imap.UID
	for i, m := range msgs {
		if !matchCriteria(m, uint32(i+1), criteria) {
			continue
		}
		seqs = append(seqs, uint32(i+1))
		uids = append(uids, m.uid)
	}
	if kind == imapserver.NumKindUID {
		return &imap.SearchData{All: imap.UIDSetNum(uids...)}, nil
	}
	return &imap.SearchData{All: imap.SeqSetNum(seqs...)}, nil
}

// hasFlag 判断邮件是否带有某个 IMAP 标志。
// 网关只跟踪 \Seen 与 \Deleted；其余（\Answered、\Flagged、\Draft、\Recent）
// 一律视为未设置——不跟踪的标志如果谎报为已设置，客户端会显示一个存不住的状态。
func (m *storedMessage) hasFlag(f imap.Flag) bool {
	switch f {
	case imap.FlagSeen:
		return m.seen
	case imap.FlagDeleted:
		return m.deleted
	default:
		return false
	}
}

// dateOnly 把时间截断到"当天零点"，用于 SINCE/BEFORE 的比较（IMAP 按天比较）。
func dateOnly(t time.Time) time.Time {
	y, mo, d := t.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, t.Location())
}

// matchCriteria 判断某封邮件是否命中 SEARCH 条件。
//
// 已支持：标志（SEEN/UNSEEN、DELETED/UNDELETED 等）、SINCE/BEFORE、
// LARGER/SMALLER、UID/序号集合、头部字段搜索（SUBJECT/FROM/TO 等）。
// 未支持：BODY、TEXT、KEYWORD、NOT、OR —— 这些条件会被忽略（宁可多返回，
// 不会漏掉邮件），且不影响上面已支持的条件生效。
func matchCriteria(m *storedMessage, seq uint32, criteria *imap.SearchCriteria) bool {
	if criteria == nil {
		return true
	}

	// 标志：要求同时满足 criteria.Flag 里的每一个，且都不满足 NotFlag 里的。
	// 注意 UNSEEN 在库中被解析为 NotFlag=[\Seen]，不是 Flag。
	for _, f := range criteria.Flag {
		if !m.hasFlag(f) {
			return false
		}
	}
	for _, f := range criteria.NotFlag {
		if m.hasFlag(f) {
			return false
		}
	}

	// 日期：按天比较，忽略时分秒
	if !criteria.Since.IsZero() && m.date.Before(dateOnly(criteria.Since)) {
		return false
	}
	if !criteria.Before.IsZero() && !m.date.Before(dateOnly(criteria.Before)) {
		return false
	}

	// 大小（整封 raw 的字节数）
	if criteria.Larger > 0 && int64(len(m.raw)) <= criteria.Larger {
		return false
	}
	if criteria.Smaller > 0 && int64(len(m.raw)) >= criteria.Smaller {
		return false
	}

	// UID / 序号集合
	for _, us := range criteria.UID {
		if !us.Contains(m.uid) {
			return false
		}
	}
	for _, ss := range criteria.SeqNum {
		if !ss.Contains(seq) {
			return false
		}
	}

	// 头部字段搜索（SEARCH SUBJECT xxx 会被库转成 Header{Key:"Subject"}）
	// 主题/发件人常是 RFC2047 编码（如 =?utf-8?q?...?=），必须先解码再比较，
	// 否则搜中文永远匹配不到。也保留一次对原始串的比对，兼容客户端传编码后的值。
	for _, hf := range criteria.Header {
		raw := parseHeader(m.raw, hf.Key+":")
		val := decodeHeaderValue(raw)
		needle := strings.ToLower(hf.Value)
		if strings.Contains(strings.ToLower(val), needle) ||
			strings.Contains(strings.ToLower(raw), needle) {
			continue
		}
		return false
	}

	return true
}

// decodeHeaderValue 解码 RFC2047 编码的头部值；失败时原样返回。
func decodeHeaderValue(s string) string {
	if s == "" {
		return s
	}
	if out, err := new(mime.WordDecoder).DecodeHeader(s); err == nil {
		return out
	}
	return s
}

// fetchAllRequested 判断这是否是"客户端请求下载全部邮件"。
// 判据：请求整封正文（BODY[] / BODY.PEEK[]，无 part 前缀）且范围覆盖全部邮件。
// 只看 FLAGS/UID/头部的常规同步不会被误判，所以不会把删掉的邮件又冒出来。
func fetchAllRequested(numSet imap.NumSet, options *imap.FetchOptions, msgs []*storedMessage) bool {
	wholeBody := false
	for _, bs := range options.BodySection {
		if len(bs.Part) == 0 && bs.Specifier == imap.PartSpecifierNone {
			wholeBody = true
			break
		}
	}
	if !wholeBody {
		return false
	}

	// IMAP 里 Stop==0 表示 "*"。`1:*` 这种从 1 到底的开放区间即"全部邮件"，
	// 即使本地当前一封都没有（全被删了）也能识别。
	switch ns := numSet.(type) {
	case imap.UIDSet:
		for _, r := range ns {
			if r.Start <= 1 && r.Stop == 0 {
				return true
			}
		}
	case imap.SeqSet:
		for _, r := range ns {
			if r.Start <= 1 && r.Stop == 0 {
				return true
			}
		}
	}

	// 退一步：范围确实覆盖了本地所有邮件也算。
	if len(msgs) == 0 {
		return false
	}
	for i, m := range msgs {
		seq := uint32(i + 1)
		switch ns := numSet.(type) {
		case imap.SeqSet:
			if !ns.Contains(seq) {
				return false
			}
		case imap.UIDSet:
			if !ns.Contains(m.uid) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (s *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	msgs := s.store.messages(s.user, s.selected)

	// 客户端点了"收取历史邮件 / 下载全部邮件"时，会发覆盖全部邮件的正文拉取请求。
	// 这时把已删记录清空并重新拉取，让被删掉的邮件重新出现。
	// 每个连接只处理一次，避免客户端反复遍历时重复触发同步。
	allRequest := fetchAllRequested(numSet, options, msgs)

	if !s.restored && s.selected == "INBOX" && allRequest {
		if n := s.store.deletedCount(s.user); n > 0 {
			s.restored = true
			log.Printf("[fetch] user=%s 检测到「下载全部邮件」请求（已删 %d 封）-> 恢复", s.user, n)
			s.store.restoreAll(s.user)
		}
	}

	// IMAP 规范（RFC3501 6.4.5）：用 BODY[...]（非 PEEK）拉取正文时，
	// 服务器必须隐式设置 \Seen —— 客户端"点开邮件"就是靠这个把邮件变已读的。
	// 之前完全没处理，导致客户端界面看着是已读、服务器和云端其实还是未读，
	// 一刷新就从服务器重读 FLAGS 变回未读。
	//
	// 例外：一次性拉全部邮件（如客户端"收取历史邮件"）不算阅读，不能标已读，
	// 否则整箱邮件会在用户没看的情况下全变成已读。
	needSeen := false
	if !allRequest {
		for _, bs := range options.BodySection {
			if !bs.Peek {
				needSeen = true
				break
			}
		}
	}

	for i, m := range msgs {
		seq := uint32(i + 1)
		var matched bool
		if ss, ok := numSet.(imap.SeqSet); ok {
			matched = ss.Contains(seq)
		}
		if us, ok := numSet.(imap.UIDSet); ok {
			matched = us.Contains(m.uid)
		}
		if !matched {
			continue
		}

		// 隐式已读必须在 WriteFlags 之前设置，这样返回给客户端的 FLAGS 才是最新的。
		// setSeen 会顺带触发异步回写云端（onSeenChange）。
		if needSeen && !m.seen {
			s.store.setSeen(s.user, s.selected, m.uid, true)
			log.Printf("[fetch] user=%s uid=%d 隐式标记已读（BODY[] 非 PEEK 拉取）", s.user, m.uid)
		}

		resp := w.CreateMessage(seq)
		if options.UID {
			resp.WriteUID(m.uid)
		}
		if options.Flags {
			resp.WriteFlags(m.flags)
		}
		if options.RFC822Size {
			resp.WriteRFC822Size(int64(len(m.raw)))
		}
		if options.InternalDate {
			resp.WriteInternalDate(m.date)
		}
		if options.Envelope {
			env := &imap.Envelope{
				Subject: parseHeader(m.raw, "Subject:"),
			}
			if from := parseAddressHeader(m.raw, "From:"); from != nil {
				env.From = []imap.Address{*from}
				env.Sender = []imap.Address{*from}
			}
			if to := parseAddressHeader(m.raw, "To:"); to != nil {
				env.To = []imap.Address{*to}
			}
			resp.WriteEnvelope(env)
		}
		if options.BodyStructure != nil {
			resp.WriteBodyStructure(bodyStructureOf(m.raw))
		}
		for _, bs := range options.BodySection {
			// BODY.PEEK[HEADER] / [HEADER.FIELDS] / [TEXT] / [n] 必须按 section 切片，
			// 不能一律返回整封 raw —— 否则严格客户端（网易邮箱大师）会丢弃邮件。
			data := sectionBytes(m.raw, bs)
			wc := resp.WriteBodySection(bs, int64(len(data)))
			_, _ = wc.Write(data)
			_ = wc.Close()
		}
		if err := resp.Close(); err != nil {
			return err
		}
	}
	log.Printf("FETCH done in %s user=%s (%d total)", s.selected, s.user, len(msgs))
	return nil
}

func (s *session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	msgs := s.store.messages(s.user, s.selected)
	for i, m := range msgs {
		seq := uint32(i + 1)
		var matched bool
		if ss, ok := numSet.(imap.SeqSet); ok {
			matched = ss.Contains(seq)
		}
		if us, ok := numSet.(imap.UIDSet); ok {
			matched = us.Contains(m.uid)
		}
		if !matched {
			continue
		}
		// 覆盖式 STORE n FLAGS (...) 语义是"把标志集合设为这些"，
		// 未列出的标志要被清掉，所以不能复用 +FLAGS/-FLAGS 的逐项处理。
		if flags.Op == imap.StoreFlagsSet {
			seen, deleted := false, false
			for _, f := range flags.Flags {
				switch f {
				case imap.FlagSeen:
					seen = true
				case imap.FlagDeleted:
					deleted = true
				}
			}
			s.store.setFlags(s.user, s.selected, m.uid, seen, deleted)
		} else {
			for _, f := range flags.Flags {
				if f == imap.FlagSeen {
					switch flags.Op {
					case imap.StoreFlagsAdd:
						s.store.setSeen(s.user, s.selected, m.uid, true)
					case imap.StoreFlagsDel:
						s.store.setSeen(s.user, s.selected, m.uid, false)
					}
				}
				if f == imap.FlagDeleted {
					switch flags.Op {
					case imap.StoreFlagsAdd:
						s.store.setDeleted(s.user, s.selected, m.uid, true)
					case imap.StoreFlagsDel:
						s.store.setDeleted(s.user, s.selected, m.uid, false)
					}
				}
			}
		}
		resp := w.CreateMessage(seq)
		resp.WriteUID(m.uid)
		resp.WriteFlags(s.store.messages(s.user, s.selected)[i].flags)
		if err := resp.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "COPY not implemented"}
}

func parseHeader(raw []byte, prefix string) string {
	for _, line := range strings.Split(string(raw), "\r\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

// parseAddressHeader 从 RFC822 头里解析出第一个地址，格式 "Name <addr>" 或 "<addr>" 或 "addr"
func parseAddressHeader(raw []byte, prefix string) *imap.Address {
	h := parseHeader(raw, prefix)
	if h == "" {
		return nil
	}
	addr := &imap.Address{}
	if i := strings.LastIndex(h, "<"); i >= 0 && strings.HasSuffix(h, ">") {
		name := strings.TrimSpace(strings.Trim(h[:i], "\" "))
		addr.Mailbox, addr.Host, _ = strings.Cut(strings.Trim(h[i+1:len(h)-1], " "), "@")
		addr.Name = name
	} else {
		addr.Mailbox, addr.Host, _ = strings.Cut(strings.TrimSpace(h), "@")
	}
	if addr.Mailbox == "" {
		return nil
	}
	return addr
}

// ---------- main ----------

func main() {
	configPath := flag.String("config", "config.json", "config file path")
	addrFlag := flag.String("addr", "", "override IMAP listen address")
	once := flag.Bool("once", false, "sync once then exit (for testing)")
	testAPI := flag.String("testapi", "", "test cloud-mail API connectivity, value=optional toEmail; exits after")
	testLogin := flag.Bool("testlogin", false, "test login + account list (needs login_password); exits after")
	testSent := flag.Bool("testsent", false, "query cloud-mail sent emails (type=1); exits after")
	testRead := flag.Bool("testread", false, "query cloud-mail inbox unread status (type=0); exits after")
	injectAfter := flag.Int("inject-after", 0, "debug: inject a fake mail into INBOX after N seconds (for IDLE test); 0=disabled")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		if *configPath == "config.json" {
			log.Printf("no config file, using defaults: %v", err)
			cfg = defaultConfig()
		} else {
			log.Fatalf("load config: %v", err)
		}
	}
	if *addrFlag != "" {
		cfg.ListenAddr = *addrFlag
	}

	store := newMailStore()
	client := newCloudMailClient(cfg.BaseURL)
	client.setAdmin(cfg.AdminEmail, cfg.AdminPassword)
	if cfg.PublicToken != "" {
		client.SetToken(cfg.PublicToken)
	}
	if len(cfg.Users) > 0 {
		client.setLoginCred(cfg.Users[0], cfg.LoginPassword)
	}

	// 动态多账号认证管理器：密码交给网站验证，每用户独立 JWT
	auth := newAuthManager(cfg.BaseURL, client, cfg, store)

	// 已读回写：IMAP STORE \Seen 时由 store 异步触发，用该用户自己的 JWT 回写 cloud-mail。
	// cloud-mail 只有"标记已读"接口，取消已读（seen=false）只保留在本地，远端无法回写。
	store.onSeenChange = func(user string, emailID int64, seen bool) {
		if !seen {
			log.Printf("[readback] user=%s emailId=%d 取消已读仅本地生效（cloud-mail 无标记未读接口）", user, emailID)
			return
		}
		cl, err := auth.clientFor(user)
		if err != nil {
			log.Printf("[readback] user=%s emailId=%d 跳过回写：%v", user, emailID, err)
			return
		}
		log.Printf("[readback] user=%s emailId=%d -> 标记已读", user, emailID)
		if err := cl.markRead([]int64{emailID}); err != nil {
			log.Printf("[readback] user=%s emailId=%d ERROR: %v", user, emailID, err)
		} else {
			log.Printf("[readback] user=%s emailId=%d OK", user, emailID)
		}
	}

	// 删除：只在本网关/客户端侧生效，**不删除 cloud-mail 服务器上的邮件**。
	// 做法：把 emailId 记入已删集合（落盘持久化），同步时跳过，客户端刷新不再重新下载。
	// 网页版和服务器上的邮件保持原样，不受影响。
	// 注意：这里故意不设置 store.onDelete —— 不调用 DELETE /api/email/delete。
	store.onDelete = nil

	// 恢复：客户端删掉 Restore 文件夹里那封说明邮件后，立刻强制全量重新同步。
	store.onRestore = func(user string) {
		cl, err := auth.clientFor(user)
		if err != nil {
			log.Printf("[restore] user=%s 无法恢复：%v", user, err)
			return
		}
		if err := auth.syncUser(user, cl, true); err != nil {
			log.Printf("[restore] user=%s 全量同步失败：%v", user, err)
			return
		}
		log.Printf("[restore] user=%s 全量重新下载完成", user)
	}

	// 已删集合落盘：/app/data 由 compose 挂载到宿主机，容器重建后不再把删掉的邮件拉回来。
	store.deletedPath = "data/deleted-ids.json"
	if err := os.MkdirAll("data", 0o755); err != nil {
		log.Printf("[store] 创建 data 目录失败: %v（已删集合将不持久化）", err)
		store.deletedPath = ""
	}
	store.loadDeleted()

	// 查询 cloud-mail 收件 unread 状态（验证已读回写）
	if *testRead {
		accounts, err := client.listAccounts()
		if err != nil {
			log.Fatalf("listAccounts ERROR: %v", err)
		}
		if len(accounts) == 0 {
			log.Fatalf("no accounts")
		}
		items, err := client.listInboxEmails(accounts[0].AccountID, 0, 10)
		if err != nil {
			log.Fatalf("listInboxEmails ERROR: %v", err)
		}
		fmt.Printf("cloud-mail inbox (%d, accountId=%d) unread: 0=未读 1=已读:\n", len(items), accounts[0].AccountID)
		for _, e := range items {
			fmt.Printf("  id=%d unread=%d subject=%q\n", e.EmailID, e.Unread, e.Subject)
		}
		return
	}

	// 查询 cloud-mail 已发送记录（验证网页版可见）
	if *testSent {
		accounts, err := client.listAccounts()
		if err != nil {
			log.Fatalf("listAccounts ERROR: %v", err)
		}
		if len(accounts) == 0 {
			log.Fatalf("no accounts")
		}
		sent, err := client.listSentEmails(accounts[0].AccountID, 5)
		if err != nil {
			log.Fatalf("listSentEmails ERROR: %v", err)
		}
		fmt.Printf("cloud-mail sent (%d, accountId=%d):\n", len(sent), accounts[0].AccountID)
		for _, e := range sent {
			fmt.Printf("  id=%d to=%s subject=%q time=%s status=%d\n", e.EmailID, e.ToEmail, e.Subject, e.CreateTime, e.IsDel)
		}
		return
	}

	// 登录 + 账号列表测试模式
	if *testLogin {
		if cfg.LoginPassword == "" {
			log.Fatalf("need login_password for test mode")
		}
		log.Printf("=== test login: %s ===", cfg.Users[0])
		tok, err := client.login()
		if err != nil {
			log.Fatalf("login ERROR: %v", err)
		}
		log.Printf("login OK: jwt=%s...", maskToken(tok))
		accounts, err := client.listAccounts()
		if err != nil {
			log.Fatalf("listAccounts ERROR: %v", err)
		}
		fmt.Printf("accounts (%d):\n", len(accounts))
		for _, a := range accounts {
			fmt.Printf("  accountId=%d email=%s name=%s\n", a.AccountID, a.Email, a.Name)
		}
		return
	}

	// API 连通性测试模式
	if *testAPI != "" {
		log.Printf("=== test API: base=%s toEmail=%q ===", cfg.BaseURL, *testAPI)
		if cfg.PublicToken == "" && (cfg.AdminEmail == "" || cfg.AdminPassword == "") {
			log.Fatalf("need ADMIN_EMAIL+ADMIN_PASSWORD or PUBLIC_TOKEN")
		}
		tok, err := client.genTokenIfNeeded()
		if err != nil {
			log.Fatalf("token ERROR: %v", err)
		}
		log.Printf("token acquired: %s...", maskToken(tok))
		items, err := client.fetchEmailList(*testAPI, 0, 1, 3)
		if err != nil {
			log.Fatalf("API ERROR: %v", err)
		}
		fmt.Printf("OK: got %d emails\n", len(items))
		for i, it := range items {
			fmt.Printf("--- [%d] id=%d from=%s <%s> to=%s subject=%s time=%s textLen=%d htmlLen=%d\n",
				i+1, it.EmailID, it.SendName, it.SendEmail, it.ToEmail, it.Subject, it.CreateTime, len(it.Text), len(it.Content))
		}
		return
	}

	log.Printf("=== cloudmail-bridge stage3 (dynamic multi-account) ===")
	log.Printf("config: baseURL=%s domain=%s users=%v poll=%ds imap=%s smtp=%s ttl=%dh", cfg.BaseURL, cfg.Domain, cfg.Users, cfg.PollInterval, cfg.ListenAddr, cfg.SMTPListenAddr, cfg.JWTCacheTTL)

	// 首次同步（兼容旧配置 users；新架构下用户密码由客户端登录时提供）
	res := syncFromCloud(client, cfg, store)
	for u, n := range res {
		log.Printf("initial sync: user=%s +%d", u, n)
	}
	if *once {
		// 测试模式：打印 INBOX 内容后退出
		for _, u := range cfg.Users {
			msgs := store.messages(u, "INBOX")
			log.Printf("[once] user=%s INBOX %d msgs", u, len(msgs))
			for i, m := range msgs {
				fmt.Printf("--- [%d] uid=%d date=%s size=%d ---\n%s\n", i+1, m.uid, m.date.Format(time.RFC3339), len(m.raw), string(m.raw))
			}
		}
		return
	}

	// 定时轮询
	if cfg.PollInterval > 0 {
		go func() {
			ticker := time.NewTicker(time.Duration(cfg.PollInterval) * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				// 动态账号模式：不再依赖 config.json 里的 users/login_password，
				// 而是对实际登录过的邮箱逐个同步，新邮件才能进 IDLE 推送。
				auth.syncAll()
			}
		}()
	}

	// 调试：N 秒后注入假邮件，验证 IDLE 推送
	if *injectAfter > 0 && len(cfg.Users) > 0 {
		go func() {
			time.Sleep(time.Duration(*injectAfter) * time.Second)
			store.injectFake(cfg.Users[0], "[idle-test] push at "+time.Now().Format("15:04:05"))
		}()
	}

	// 证书路径：配置文件优先，环境变量兼容回退
	certFile := cfg.CertFile
	keyFile := cfg.KeyFile
	if certFile == "" {
		certFile = os.Getenv("TLS_CERT")
	}
	if keyFile == "" {
		keyFile = os.Getenv("TLS_KEY")
	}
	var tlsConfig *tls.Config
	if cfg.TLSMode == "implicit" || cfg.TLSMode == "starttls" {
		if certFile == "" || keyFile == "" {
			log.Fatalf("tls_mode=%s 需要 cert_file/key_file（或在配置里填证书路径）", cfg.TLSMode)
		}
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			log.Fatalf("load cert: %v", err)
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		log.Printf("TLS loaded (cert=%s key=%s)", certFile, keyFile)
	}

	caps := imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapIdle:      {},
	}

	// 隐式 TLS 下连接本身已加密，允许 PLAIN 认证（等价于 insecure_auth，但通道安全）
	imapInsecure := cfg.InsecureAuth
	if cfg.TLSMode == "implicit" {
		imapInsecure = true
	}

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &session{store: store, auth: auth}, &imapserver.GreetingData{}, nil
		},
		Caps:         caps,
		InsecureAuth: imapInsecure,
		TLSConfig:    tlsConfig,
	})

	switch cfg.TLSMode {
	case "implicit":
		log.Printf("IMAP server listening on %s (implicit TLS)", cfg.ListenAddr)
		go func() {
			if err := srv.ListenAndServeTLS(cfg.ListenAddr); err != nil && err != net.ErrClosed {
				log.Printf("serve: %v", err)
			}
		}()
	default: // plain / starttls 都从明文端口起；starttls 的 TLSConfig 已在 Options 里
		ln, err := net.Listen("tcp", cfg.ListenAddr)
		if err != nil {
			log.Fatalf("listen: %v", err)
		}
		log.Printf("IMAP server listening on %s (tls_mode=%s insecure=%v)", cfg.ListenAddr, cfg.TLSMode, cfg.InsecureAuth)
		go func() {
			if err := srv.Serve(ln); err != nil && err != net.ErrClosed {
				log.Printf("serve: %v", err)
			}
		}()
	}

	// SMTP 服务（发信走 cloud-mail /api/email/send，用登录用户自己的 JWT）
	// 新架构下密码由客户端登录时提供，不再依赖 config 的 login_password。
	{
		smtpSrv, err := startSMTPServer(cfg, client, auth, store, tlsConfig)
		if err != nil {
			log.Printf("SMTP start failed: %v (IMAP still available)", err)
		} else {
			defer smtpSrv.Close()
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nshutting down...")
	_ = srv.Close()
	_ = io.Discard
}
