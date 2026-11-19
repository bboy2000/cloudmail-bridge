package main

import (
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// newTestStore 构造一个只有内存数据、不落盘的 store。
func newTestStore(t *testing.T, user string, msgs ...*storedMessage) *mailStore {
	t.Helper()
	s := newMailStore()
	s.data[user] = map[string][]*storedMessage{"INBOX": msgs}
	s.knownIDs[user] = make(map[int64]bool)
	s.deletedIDs[user] = make(map[int64]bool)
	return s
}

func TestSetFlagsOverwrite(t *testing.T) {
	m := &storedMessage{uid: 100, emailID: 100, date: time.Now()}
	s := newTestStore(t, "u@test", m)

	// 覆盖式：设为已读 + 已删
	s.setFlags("u@test", "INBOX", 100, true, true)
	if !m.seen || !m.deleted {
		t.Fatalf("期望 seen=true deleted=true，实际 seen=%v deleted=%v", m.seen, m.deleted)
	}
	if !s.isDeletedID("u@test", 100) {
		t.Error("已删集合应记录该邮件")
	}
	// flags 必须同步重算
	found := map[imap.Flag]bool{}
	for _, f := range m.flags {
		found[f] = true
	}
	if !found[imap.FlagSeen] || !found[imap.FlagDeleted] {
		t.Errorf("flags 未同步：%v", m.flags)
	}

	// 覆盖式：清空全部标志（STORE n FLAGS ()）
	s.setFlags("u@test", "INBOX", 100, false, false)
	if m.seen || m.deleted {
		t.Fatalf("期望全部清空，实际 seen=%v deleted=%v", m.seen, m.deleted)
	}
	if s.isDeletedID("u@test", 100) {
		t.Error("取消已删后不应还在已删集合里")
	}
	if len(m.flags) != 0 {
		t.Errorf("flags 应为空，实际 %v", m.flags)
	}
}

func TestSetFlagsNoChange(t *testing.T) {
	m := &storedMessage{uid: 200, emailID: 200, seen: true, date: time.Now()}
	s := newTestStore(t, "u@test", m)
	// 状态没变时不应出错，也不应改动
	s.setFlags("u@test", "INBOX", 200, true, false)
	if !m.seen || m.deleted {
		t.Error("状态未变化时不应被改动")
	}
}

func TestStatusCounts(t *testing.T) {
	now := time.Now()
	s := newTestStore(t, "u@test",
		&storedMessage{uid: 1, emailID: 1, seen: true, date: now},
		&storedMessage{uid: 2, emailID: 2, seen: false, date: now},
		&storedMessage{uid: 3, emailID: 3, seen: false, date: now},
		&storedMessage{uid: 4, emailID: 4, seen: false, deleted: true, date: now},
	)
	if got := s.unseenCount("u@test", "INBOX"); got != 3 {
		t.Errorf("未读数期望 3，实际 %d", got)
	}
	if got := s.deletedCountIn("u@test", "INBOX"); got != 1 {
		t.Errorf("已删数期望 1，实际 %d", got)
	}
	// 不存在的邮箱应为 0，不能 panic
	if got := s.unseenCount("u@test", "NoSuchBox"); got != 0 {
		t.Errorf("不存在的邮箱未读数应为 0，实际 %d", got)
	}
}
