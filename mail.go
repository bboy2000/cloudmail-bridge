package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"
)

// buildRawEmail 把 cloud-mail 的邮件记录转成标准 RFC822 邮件（multipart/alternative：纯文本 + HTML）
func buildRawEmail(item emailItem, domain string) []byte {
	var buf bytes.Buffer

	from := mail.Address{Name: item.SendName, Address: item.SendEmail}
	buf.WriteString("From: " + from.String() + "\r\n")

	to := mail.Address{Name: item.ToName, Address: item.ToEmail}
	buf.WriteString("To: " + to.String() + "\r\n")

	if item.Subject != "" {
		buf.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", item.Subject) + "\r\n")
	} else {
		buf.WriteString("Subject: (no subject)\r\n")
	}

	// cloud-mail 的 createTime 是 UTC（无时区后缀），必须按 UTC 解析；
	// 用 ParseInLocation(..., time.Local) 会把 12:xx UTC 当成 12:xx 北京时间，整体早 8 小时。
	t, err := time.Parse("2006-01-02 15:04:05", item.CreateTime)
	if err != nil {
		// 兼容带时区或 T 分隔的格式
		t, err = time.Parse(time.RFC3339, item.CreateTime)
		if err != nil {
			t = time.Now()
		}
	}
	buf.WriteString("Date: " + t.Local().Format(time.RFC1123Z) + "\r\n")
	buf.WriteString(fmt.Sprintf("Message-ID: <cloudmail-%d@%s>\r\n", item.EmailID, domain))
	buf.WriteString(fmt.Sprintf("X-CloudMail-ID: %d\r\n", item.EmailID))
	buf.WriteString("X-CloudMail-Bridge: cloudmail-bridge/0.2\r\n")

	boundary := fmt.Sprintf("cloudmail-boundary-%d", item.EmailID)
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n")
	buf.WriteString("\r\n")

	textBody := item.Text
	if strings.TrimSpace(textBody) == "" {
		textBody = htmlToPlainText(item.Content)
	}

	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	buf.WriteString("Content-Transfer-Encoding: base64\r\n")
	buf.WriteString("\r\n")
	buf.WriteString(base64Wrap(textBody))
	buf.WriteString("\r\n")

	if strings.TrimSpace(item.Content) != "" {
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n")
		buf.WriteString("Content-Transfer-Encoding: base64\r\n")
		buf.WriteString("\r\n")
		buf.WriteString(base64Wrap(item.Content))
		buf.WriteString("\r\n")
	}

	buf.WriteString("--" + boundary + "--\r\n")

	return buf.Bytes()
}

// base64Wrap 按 RFC2045 每 76 字符折行
func base64Wrap(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var b strings.Builder
	for i := 0; i < len(enc); i += 76 {
		end := i + 76
		if end > len(enc) {
			end = len(enc)
		}
		b.WriteString(enc[i:end])
		b.WriteString("\r\n")
	}
	return b.String()
}

// htmlToPlainText 极简 HTML 转纯文本：去标签、解实体、压缩空行。够 IMAP 客户端预览用。
func htmlToPlainText(html string) string {
	s := strings.ReplaceAll(html, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = strings.ReplaceAll(s, "</p>", "\n")
	s = strings.ReplaceAll(s, "</div>", "\n")

	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	out := b.String()
	out = strings.ReplaceAll(out, "&nbsp;", " ")
	out = strings.ReplaceAll(out, "&lt;", "<")
	out = strings.ReplaceAll(out, "&gt;", ">")
	out = strings.ReplaceAll(out, "&amp;", "&")
	out = strings.ReplaceAll(out, "&quot;", "\"")
	out = strings.ReplaceAll(out, "&#39;", "'")

	lines := strings.Split(out, "\n")
	var cleaned []string
	blank := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if !blank {
				cleaned = append(cleaned, "")
			}
			blank = true
		} else {
			cleaned = append(cleaned, line)
			blank = false
		}
	}
	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}
