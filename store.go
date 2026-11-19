package main

import (
	"encoding/json"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
)

// storedMessage 是一条已转成 RFC822 的邮件。uid 直接沿用 cloud-mail 的 emailId（稳定唯一）。
type storedMessage struct {
	uid       imap.UID
	emailID   int64
	raw       []byte
	flags     []imap.Flag
	date      time.Time
	seen      bool
	deleted   bool // 客户端已标记 \Deleted，等待 EXPUNGE
	hasAttach bool // raw 里已包含附件（避免同步时重复下载）
}

// attachmentsReady 判断 item 的所有附件都已下载好，可以生成带附件的 raw。
func attachmentsReady(it emailItem) bool {
	if len(it.AttList) == 0 {
		return false
	}
	for _, att := range it.AttList {
		if len(att.Data) == 0 {
			return false
		}
	}
	return true
}

// hasMessageWithAttachments 供附件下载前判断：该邮件已入库且 raw 已带附件则跳过，
// 避免每次同步重复拉取同一附件（省对象存储流量）。
func (s *mailStore) hasMessageWithAttachments(user string, emailID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, mb := range []string{"INBOX", "Sent"} {
		for _, m := range s.data[user][mb] {
			if m.emailID == emailID {
				return m.hasAttach
			}
		}
	}
	return false
}

// findMessageLocked 在指定文件夹按 emailID 找邮件，调用方须持 s.mu。
func (s *mailStore) findMessageLocked(user, mailbox string, emailID int64) *storedMessage {
	for _, m := range s.data[user][mailbox] {
		if m.emailID == emailID {
			return m
		}
	}
	return nil
}

// mailStore 全局内存存储：按 用户 -> 邮箱文件夹 -> 邮件列表 组织。
type mailStore struct {
	mu           sync.RWMutex
	data         map[string]map[string][]*storedMessage
	knownIDs     map[string]map[int64]bool // user -> emailId -> 是否已入库（去重）
	onNewMail    func(user string, m *storedMessage)
	onSeenChange func(user string, emailID int64, seen bool) // 已读标记变更回调（回写 cloud-mail）
	onDelete     func(user string, emailIDs []int64)         // 删除回调（可选扩展，默认不启用）
	onRestore    func(user string)                           // 恢复回调：清空已删记录后触发全量重新同步
	sentSeq      imap.UID                                    // 本地发出邮件的 UID 序列（高位段，避免和 emailId 冲突）

	// 已删除集合（持久化）：user -> emailId -> true。
	// 作用：云端同步若仍返回该邮件（站点侧"同步删除"未开启、或删除接口只是标记），
	// 网关据此跳过，避免客户端删掉的邮件刷新后又回来。重启后从磁盘恢复。
	deletedIDs  map[string]map[int64]bool
	deletedMu   sync.RWMutex
	deletedPath string // 已删集合落盘路径，空=不持久化

	// IDLE 订阅：user -> 一组订阅者（每个 IDLE 的 session 一个 channel）
	idleMu   sync.Mutex
	idleSubs map[string]map[chan struct{}]struct{}
}

// sentUIDBase 本地 Sent 邮件的 UID 起点（高位段）。小于此值的 uid 是 cloud-mail 真实 emailId。
const sentUIDBase = imap.UID(100000000)

var knownMailboxes = []string{"INBOX", "Sent", "Drafts", "Trash", "Junk"}

func newMailStore() *mailStore {
	s := &mailStore{
		data:       make(map[string]map[string][]*storedMessage),
		knownIDs:   make(map[string]map[int64]bool),
		deletedIDs: make(map[string]map[int64]bool),
		idleSubs:   make(map[string]map[chan struct{}]struct{}),
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
		// Restore 已移除：不再创建虚拟文件夹。
	}
	if _, ok := s.knownIDs[user]; !ok {
		s.knownIDs[user] = make(map[int64]bool)
	}
	s.deletedMu.Lock()
	if _, ok := s.deletedIDs[user]; !ok {
		s.deletedIDs[user] = make(map[int64]bool)
	}
	s.deletedMu.Unlock()
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
		// 云端标记已删（isDel=1）或用户在本网关删过：一律不入库。
		// 这是"客户端删掉后刷新又回来"的关键防线之一。
		if item.IsDel == 1 || s.isDeletedID(user, item.EmailID) {
			continue
		}
		ready := attachmentsReady(item)
		if s.knownIDs[user][item.EmailID] {
			// 已入库但上次没拿到附件：这次补齐后重建 raw，让客户端可见附件。
			if ready {
				if old := s.findMessageLocked(user, "INBOX", item.EmailID); old != nil && !old.hasAttach {
					old.raw = buildRawEmail(item, domain)
					old.hasAttach = true
					log.Printf("[store] user=%s emailId=%d 补齐附件 raw（INBOX）", user, item.EmailID)
				}
			}
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
			uid:       imap.UID(item.EmailID),
			emailID:   item.EmailID,
			raw:       buildRawEmail(item, domain),
			flags:     flags,
			date:      t,
			seen:      seen,
			hasAttach: ready,
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
//
// 重要：已读状态只允许单向「未读 -> 已读」，绝不把本地已读改回未读。
// 原因：cloud-mail 只有「标记已读」接口，没有「标记未读」接口，
// 所以云端的 unread 一旦变已读就不会回退。若云端返回未读而本地已读，
// 只可能是回写失败或尚未完成；此时必须信任本地，否则用户点开过的邮件
// 会在下一个同步周期被改回未读（客户端刷新后看到"又变未读了"）。
func (s *mailStore) applyUnread(user string, unreadMap map[int64]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed, keptRead := 0, 0
	for _, m := range s.data[user]["INBOX"] {
		want, ok := unreadMap[m.emailID]
		if !ok {
			continue
		}
		if m.seen == want {
			continue
		}
		if !want {
			// 本地已读、云端未读：回写未完成或失败，保留本地已读，不回退。
			keptRead++
			continue
		}
		m.seen = true
		m.flags = []imap.Flag{}
		m.flags = append(m.flags, imap.FlagSeen)
		if m.deleted {
			m.flags = append(m.flags, imap.FlagDeleted)
		}
		changed++
	}
	if changed > 0 || keptRead > 0 {
		log.Printf("[store] user=%s unread 校正：置已读 %d 封，保留本地已读 %d 封（云端仍报未读）", user, changed, keptRead)
	}
}

// ============ 删除支持（IMAP \Deleted + EXPUNGE）============

// reflagLocked 按 seen/deleted 重算 flags。调用方须持 s.mu。
func (m *storedMessage) reflagLocked() {
	m.flags = []imap.Flag{}
	if m.seen {
		m.flags = append(m.flags, imap.FlagSeen)
	}
	if m.deleted {
		m.flags = append(m.flags, imap.FlagDeleted)
	}
}

// setDeleted 处理 IMAP STORE \Deleted / -\Deleted。
// 标记删除时立刻把 emailId 记入已删集合（持久化），这样即使客户端不发 EXPUNGE，
// 下次同步也不会把邮件再拉回来。
func (s *mailStore) setDeleted(user, mailbox string, uid imap.UID, deleted bool) {
	s.mu.Lock()
	var target *storedMessage
	for _, m := range s.data[user][mailbox] {
		if m.uid == uid {
			target = m
			break
		}
	}
	if target == nil {
		s.mu.Unlock()
		return
	}
	if target.deleted == deleted {
		s.mu.Unlock()
		return
	}
	target.deleted = deleted
	target.reflagLocked()
	emailID := target.emailID
	isReal := mailbox == "INBOX" && uid < sentUIDBase && emailID > 0
	s.mu.Unlock()

	if !isReal {
		return
	}
	if deleted {
		s.addDeletedID(user, emailID)
		// 可选扩展点：若将来要「客户端删 = 服务器也删」，在这里接 onDelete。
		// 当前默认不设置 onDelete —— 只在客户端/网关侧删除，不动 cloud-mail 服务器。
		if s.onDelete != nil {
			go s.onDelete(user, []int64{emailID})
		}
		log.Printf("[store] user=%s emailId=%d 标记 \\Deleted（已记入已删集合，同步时跳过；服务器邮件不变）", user, emailID)
	} else {
		s.removeDeletedID(user, emailID)
		log.Printf("[store] user=%s emailId=%d 取消 \\Deleted（已从已删集合移除）", user, emailID)
	}
}

// expungeDeleted 从本地移除已标记 \Deleted 的邮件。
// 返回被移除邮件的序号（降序，供 IMAP EXPUNGE 通知用）和需要云端删除的 emailId。
// 降序很重要：按从大到小通知，前面邮件的序号才不会被删除操作影响。
func (s *mailStore) expungeDeleted(user, mailbox string) (seqNums []uint32, realIDs []int64) {
	s.mu.Lock()
	msgs := s.data[user][mailbox]
	kept := make([]*storedMessage, 0, len(msgs))
	// 从头到尾遍历收集，最后反转成降序
	for i, m := range msgs {
		if !m.deleted {
			kept = append(kept, m)
			continue
		}
		seqNums = append(seqNums, uint32(i+1))
		if mailbox == "INBOX" && m.uid < sentUIDBase && m.emailID > 0 {
			realIDs = append(realIDs, m.emailID)
			delete(s.knownIDs[user], m.emailID)
		}
	}
	removed := len(seqNums)
	s.data[user][mailbox] = kept
	s.mu.Unlock()

	// 反转成降序
	for i, j := 0, len(seqNums)-1; i < j; i, j = i+1, j-1 {
		seqNums[i], seqNums[j] = seqNums[j], seqNums[i]
	}

	for _, id := range realIDs {
		s.addDeletedID(user, id)
	}
	if removed > 0 {
		log.Printf("[store] user=%s EXPUNGE %s 移除 %d 封", user, mailbox, removed)
		if len(realIDs) > 0 && s.onDelete != nil {
			go s.onDelete(user, realIDs)
		}
		go s.notifyIdle(user)
	}
	return seqNums, realIDs
}

// addDeletedID 把 emailId 记入已删集合并落盘。
func (s *mailStore) addDeletedID(user string, emailID int64) {
	s.deletedMu.Lock()
	if s.deletedIDs[user] == nil {
		s.deletedIDs[user] = make(map[int64]bool)
	}
	already := s.deletedIDs[user][emailID]
	s.deletedIDs[user][emailID] = true
	s.deletedMu.Unlock()
	if !already {
		s.saveDeleted()
	}
}

// removeDeletedID 从已删集合移除（客户端取消删除标记时）。
func (s *mailStore) removeDeletedID(user string, emailID int64) {
	s.deletedMu.Lock()
	if m := s.deletedIDs[user]; m != nil && m[emailID] {
		delete(m, emailID)
		s.deletedMu.Unlock()
		s.saveDeleted()
		return
	}
	s.deletedMu.Unlock()
}

// isDeletedID 判断某 emailId 是否已被用户删除。
func (s *mailStore) isDeletedID(user string, emailID int64) bool {
	s.deletedMu.RLock()
	defer s.deletedMu.RUnlock()
	return s.deletedIDs[user][emailID]
}

// deletedCount 返回该用户被客户端删掉的邮件数。
func (s *mailStore) deletedCount(user string) int {
	s.deletedMu.RLock()
	defer s.deletedMu.RUnlock()
	return len(s.deletedIDs[user])
}

// restoreAll 供「客户端主动请求下载全部邮件」时调用。
// IMAP 没有"下载全部"这个命令，客户端（如网易邮箱大师的「收取历史邮件」）
// 实际发出的是 UID FETCH 1:* 且请求整封正文（BODY[]）——覆盖全部邮件。
// 网关收到这种请求即视为用户要"全部邮件"，此时：
//  1. 清空该用户的已删记录（被删的邮件重新可见）
//  2. 触发一次全量同步，把之前被跳过/删掉的邮件从服务器重新拉回来
//
// 注意：不清空现有缓存，避免当前这轮 FETCH 返回空列表让客户端看到"邮件全没了"。
// 只影响网关本地状态，服务器上的邮件自始至终没有被动过。
func (s *mailStore) restoreAll(user string) {
	// 先取出要恢复的 id，再清空集合。
	s.deletedMu.Lock()
	ids := make([]int64, 0, len(s.deletedIDs[user]))
	for id := range s.deletedIDs[user] {
		ids = append(ids, id)
	}
	s.deletedIDs[user] = make(map[int64]bool)
	s.deletedMu.Unlock()
	s.saveDeleted()

	// 从去重表里移除这些 id，让下次同步重新拉取它们。
	s.mu.Lock()
	for _, id := range ids {
		delete(s.knownIDs[user], id)
	}
	s.mu.Unlock()

	log.Printf("[restore] user=%s 客户端请求下载全部邮件：已清空已删记录(%d 条)，开始重新拉取", user, len(ids))

	if s.onRestore != nil {
		s.onRestore(user)
	}
	go s.notifyIdle(user)
}

// saveDeleted 把已删集合原子落盘（先写临时文件再 rename）。
// 不落盘的话，网关重启后已删集合清空，客户端删掉的邮件会从云端重新拉回来。
func (s *mailStore) saveDeleted() {
	if s.deletedPath == "" {
		return
	}
	s.deletedMu.RLock()
	snapshot := make(map[string][]int64, len(s.deletedIDs))
	for u, set := range s.deletedIDs {
		if len(set) == 0 {
			continue
		}
		ids := make([]int64, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		snapshot[u] = ids
	}
	s.deletedMu.RUnlock()

	b, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		log.Printf("[store] 序列化已删集合失败: %v", err)
		return
	}
	tmp := s.deletedPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("[store] 写已删集合失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.deletedPath); err != nil {
		log.Printf("[store] 替换已删集合失败: %v", err)
	}
}

// loadDeleted 从磁盘恢复已删集合。文件不存在视为空，正常启动。
func (s *mailStore) loadDeleted() {
	if s.deletedPath == "" {
		return
	}
	b, err := os.ReadFile(s.deletedPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[store] 读已删集合失败: %v", err)
		}
		return
	}
	var raw map[string][]int64
	if err := json.Unmarshal(b, &raw); err != nil {
		log.Printf("[store] 解析已删集合失败（按空处理）: %v", err)
		return
	}
	s.deletedMu.Lock()
	total := 0
	for u, ids := range raw {
		if s.deletedIDs[u] == nil {
			s.deletedIDs[u] = make(map[int64]bool)
		}
		for _, id := range ids {
			s.deletedIDs[u][id] = true
			total++
		}
	}
	s.deletedMu.Unlock()
	log.Printf("[store] 已删集合已恢复：%d 个用户 / 共 %d 条", len(raw), total)
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
		if item.IsDel == 1 || s.isDeletedID(user, item.EmailID) {
			continue
		}
		ready := attachmentsReady(item)
		if s.knownIDs[user][item.EmailID] {
			if ready {
				if old := s.findMessageLocked(user, "Sent", item.EmailID); old != nil && !old.hasAttach {
					old.raw = buildRawEmail(item, domain)
					old.hasAttach = true
					log.Printf("[store] user=%s emailId=%d 补齐附件 raw（Sent）", user, item.EmailID)
				}
			}
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
			uid:       imap.UID(item.EmailID),
			emailID:   item.EmailID,
			raw:       buildRawEmail(item, domain),
			flags:     []imap.Flag{imap.FlagSeen}, // 发件默认已读
			date:      t,
			seen:      true,
			hasAttach: ready,
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

// syncFromCloud 为旧配置 users 做启动同步，返回 用户->收件新增条数。
// 全部走文档 #3 标准接口 GET /api/email/list（需 JWT）：收件 type=0、发件 type=1、full=1。
// 没有登录凭据（JWT）时标准接口不可用，直接跳过——不再回退到 public 隐藏接口。
func syncFromCloud(client *cloudMailClient, cfg *Config, store *mailStore) map[string]int {
	result := make(map[string]int)
	users := cfg.Users
	if len(users) == 0 {
		log.Printf("[sync] 未配置 users，跳过启动同步（新架构由客户端登录时按需同步）")
		return result
	}
	if cfg.LoginPassword == "" {
		log.Printf("[sync] 未配置 login_password，标准接口 /api/email/list 需要 JWT，跳过启动同步")
		return result
	}

	accounts, err := client.listAccounts()
	if err != nil {
		log.Printf("[sync] listAccounts ERROR: %v（跳过启动同步）", err)
		return result
	}

	for _, user := range users {
		total := 0
		for _, a := range accounts {
			// 只同步该 user 对应的账号；找不到精确匹配时跳过，避免串号。
			if !strings.EqualFold(a.Email, user) || a.AccountID <= 0 {
				continue
			}
			recv, err := client.fetchAllEmailList(a.AccountID, 0)
			if err != nil {
				log.Printf("[sync] user=%s 收件 accountId=%d ERROR: %v", user, a.AccountID, err)
			} else {
				um := make(map[int64]bool, len(recv))
				for _, it := range recv {
					um[it.EmailID] = it.Unread == 1
				}
				store.applyUnread(user, um)
				total += store.addReceived(user, recv, cfg.Domain, um)
			}
			sent, err := client.fetchAllEmailList(a.AccountID, 1)
			if err != nil {
				log.Printf("[sync] user=%s 发件 accountId=%d ERROR: %v", user, a.AccountID, err)
			} else {
				store.addSentFromCloud(user, sent, cfg.Domain)
			}
		}
		result[user] = total
	}
	return result
}
