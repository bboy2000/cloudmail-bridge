package main

import (
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func TestMatchCriteriaFlags(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	unread := &storedMessage{uid: 10, seen: false, raw: make([]byte, 500), date: now}
	read := &storedMessage{uid: 11, seen: true, raw: make([]byte, 500), date: now}
	del := &storedMessage{uid: 12, seen: false, deleted: true, raw: make([]byte, 500), date: now}

	cases := []struct {
		name   string
		m      *storedMessage
		crit   *imap.SearchCriteria
		expect bool
	}{
		{"ALL 空条件", unread, &imap.SearchCriteria{}, true},
		{"UNSEEN 命中未读", unread, &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, true},
		{"UNSEEN 排除已读", read, &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, false},
		{"SEEN 命中已读", read, &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}}, true},
		{"SEEN 排除未读", unread, &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}}, false},
		{"DELETED 命中已删", del, &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagDeleted}}, true},
		{"DELETED 不再误判为 SEEN", read, &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagDeleted}}, false},
		{"UNDELETED 命中", read, &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagDeleted}}, true},
		{"UNDELETED 排除已删", del, &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagDeleted}}, false},
		{"FLAGGED 不支持则命中不到", read, &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged}}, false},
		{"UNFLAGGED 都命中", read, &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagFlagged}}, true},
	}

	for _, c := range cases {
		got := matchCriteria(c.m, 1, c.crit)
		if got != c.expect {
			t.Errorf("%s: 期望 %v，实际 %v", c.name, c.expect, got)
		}
	}
}

func TestMatchCriteriaDateSizeUID(t *testing.T) {
	d := func(y, m, day int) time.Time { return time.Date(y, time.Month(m), day, 12, 0, 0, 0, time.UTC) }
	old := &storedMessage{uid: 5, raw: make([]byte, 100), date: d(2026, 11, 1)}
	newer := &storedMessage{uid: 20, raw: make([]byte, 900), date: d(2026, 11, 18)}
	since := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

	if matchCriteria(old, 1, &imap.SearchCriteria{Since: since}) {
		t.Error("SINCE 应排除 11-01 的旧邮件")
	}
	if !matchCriteria(newer, 1, &imap.SearchCriteria{Since: since}) {
		t.Error("SINCE 应命中 11-18 的邮件")
	}
	if !matchCriteria(old, 1, &imap.SearchCriteria{Before: before}) {
		t.Error("BEFORE 应命中 11-01 的旧邮件")
	}
	if matchCriteria(newer, 1, &imap.SearchCriteria{Before: before}) {
		t.Error("BEFORE 应排除 11-18 的邮件")
	}
	if !matchCriteria(newer, 1, &imap.SearchCriteria{Larger: 500}) {
		t.Error("LARGER 500 应命中 900 字节")
	}
	if matchCriteria(old, 1, &imap.SearchCriteria{Larger: 500}) {
		t.Error("LARGER 500 应排除 100 字节")
	}
	if !matchCriteria(old, 1, &imap.SearchCriteria{Smaller: 500}) {
		t.Error("SMALLER 500 应命中 100 字节")
	}
	if !matchCriteria(newer, 1, &imap.SearchCriteria{UID: []imap.UIDSet{imap.UIDSetNum(20)}}) {
		t.Error("UID 20 应命中")
	}
	if matchCriteria(newer, 1, &imap.SearchCriteria{UID: []imap.UIDSet{imap.UIDSetNum(5)}}) {
		t.Error("UID 5 不应命中 uid=20 的邮件")
	}
}

func TestMatchCriteriaHeader(t *testing.T) {
	m := &storedMessage{uid: 1, raw: []byte("From: a@b.com\r\nSubject: Hello World\r\n\r\nbody")}
	if !matchCriteria(m, 1, &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "hello"}}}) {
		t.Error("头部搜索应忽略大小写")
	}
	if matchCriteria(m, 1, &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "nothere"}}}) {
		t.Error("头部搜索不匹配时不应命中")
	}
}

// 中文主题是 RFC2047 编码的，搜索中文必须能命中；搜编码原串也应命中。
func TestMatchCriteriaHeaderEncoded(t *testing.T) {
	m := &storedMessage{uid: 1, raw: []byte("Subject: =?utf-8?q?=E6=B5=8B=E8=AF=95=E9=82=AE=E4=BB=B6?=\r\n\r\nbody")}
	if !matchCriteria(m, 1, &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "测试"}}}) {
		t.Error("搜中文应能命中编码后的主题")
	}
	if !matchCriteria(m, 1, &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "邮件"}}}) {
		t.Error("搜部分中文应能命中")
	}
	if matchCriteria(m, 1, &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "不存在的词"}}}) {
		t.Error("不匹配时不应命中")
	}
}
