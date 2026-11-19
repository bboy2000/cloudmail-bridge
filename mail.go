package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"net/mail"
	neturl "net/url"
	"regexp"
	"strings"
	"time"
)

// proxyURLPattern 匹配完整的图片代理相对路径（含 exp/sig 等后续参数）：
//   /proxy?url=https%3A%2F%2F...&exp=...&sig=...
// 必须整段吃掉：url= 值本身是 %编码的（不含裸 &），后面跟 exp/sig；
// 若只替换到第一个 &，残留的 &exp=... 会被拼进图片文件名导致 COS 返回 404。
// 属性分隔符 " ' 空白 > 之外都算路径内容，兼容 HTML 的 &amp; 转义。
var proxyURLPattern = regexp.MustCompile(`/proxy\?url=([^"'\s>]+)`)

// restoreProxyImageURLs 把 HTML 里的 /proxy?url=<编码地址>&... 整段还原成真实 http(s) 直链。
// 只处理能解码出 http(s) 地址的项，其余原样保留；解码失败也原样保留。
func restoreProxyImageURLs(html string) string {
	return proxyURLPattern.ReplaceAllStringFunc(html, func(match string) string {
		m := proxyURLPattern.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		// url= 参数值到第一个 & 为止（%编码里不会有裸 &），后面都是 exp/sig，丢弃。
		encoded := m[1]
		if i := strings.IndexAny(encoded, "&"); i >= 0 {
			encoded = encoded[:i]
		}
		real, err := neturl.QueryUnescape(encoded)
		if err != nil {
			return match
		}
		real = strings.TrimSpace(real)
		if !strings.HasPrefix(real, "http://") && !strings.HasPrefix(real, "https://") {
			return match
		}
		return real
	})
}

// buildRawEmail 把 cloud-mail 的邮件记录转成标准 RFC822。
// 有附件时：multipart/mixed = [multipart/alternative 正文] + 各附件 part。
// 无附件时：保持原来的 multipart/alternative（纯文本 + HTML）。
func buildRawEmail(item emailItem, domain string) []byte {
	var buf bytes.Buffer

	from := mail.Address{Name: item.senderName(), Address: item.SendEmail}
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
	buf.WriteString("X-CloudMail-Bridge: cloudmail-bridge/0.3\r\n")

	buf.WriteString("MIME-Version: 1.0\r\n")

	atts := make([]mailAttachment, 0, len(item.AttList))
	for _, att := range item.AttList {
		if len(att.Data) > 0 {
			atts = append(atts, att)
		}
	}

	if len(atts) == 0 {
		boundary := fmt.Sprintf("cloudmail-boundary-%d", item.EmailID)
		buf.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n")
		buf.WriteString("\r\n")
		buf.Write(alternativePart(item, boundary))
		return buf.Bytes()
	}

	mixedBoundary := fmt.Sprintf("cloudmail-mixed-%d", item.EmailID)
	altBoundary := fmt.Sprintf("cloudmail-alt-%d", item.EmailID)
	buf.WriteString("Content-Type: multipart/mixed; boundary=\"" + mixedBoundary + "\"\r\n")
	buf.WriteString("\r\n")

	// 正文作为 mixed 的第一个子 part：完整的 multipart/alternative。
	buf.WriteString("--" + mixedBoundary + "\r\n")
	buf.WriteString("Content-Type: multipart/alternative; boundary=\"" + altBoundary + "\"\r\n")
	buf.WriteString("\r\n")
	buf.Write(alternativePart(item, altBoundary))

	// 后续每个附件一个 part，base64 编码（二进制安全）。
	for _, att := range atts {
		filename := safeFilename(att.Filename)
		encName := mime.QEncoding.Encode("utf-8", filename)
		ctype := strings.TrimSpace(att.MimeType)
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		buf.WriteString("--" + mixedBoundary + "\r\n")
		buf.WriteString("Content-Type: " + ctype + "; name=\"" + encName + "\"\r\n")
		buf.WriteString("Content-Transfer-Encoding: base64\r\n")
		buf.WriteString("Content-Disposition: attachment; filename=\"" + encName + "\"\r\n")
		buf.WriteString("\r\n")
		writeBase64Lines(&buf, att.Data)
	}
	buf.WriteString("--" + mixedBoundary + "--\r\n")

	return buf.Bytes()
}

// alternativePart 生成 multipart/alternative 的正文部分（不含顶层 MIME-Version）。
func alternativePart(item emailItem, boundary string) []byte {
	var buf bytes.Buffer

	textBody := item.Text
	if strings.TrimSpace(textBody) == "" {
		textBody = htmlToPlainText(item.Content)
	}

	// 正文是纯 UTF-8 文本，直接用 8bit 传输，不做 base64 编码。
	// 早期版本用 base64，部分客户端（如网易邮箱大师）渲染 multipart/alternative
	// 时会把编码串原文露出来显示成乱码。8bit 没有解码步骤，从根上避免该问题。
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	buf.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	buf.WriteString("\r\n")
	buf.WriteString(crlf(textBody))
	buf.WriteString("\r\n")

	if strings.TrimSpace(item.Content) != "" {
		// 还原发件方写死的 /proxy?url=<编码地址> 相对路径为真实直链，
		// 否则 IMAP 客户端无法解析相对 URL，图片全部显示成碎图。
		htmlBody := restoreProxyImageURLs(item.Content)
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n")
		buf.WriteString("Content-Transfer-Encoding: 8bit\r\n")
		buf.WriteString("\r\n")
		buf.WriteString(crlf(htmlBody))
		buf.WriteString("\r\n")
	}

	buf.WriteString("--" + boundary + "--\r\n")
	return buf.Bytes()
}

// writeBase64Lines 按 RFC2045 每行 76 字符输出 base64。
func writeBase64Lines(buf *bytes.Buffer, data []byte) {
	s := base64.StdEncoding.EncodeToString(data)
	const width = 76
	for len(s) > width {
		buf.WriteString(s[:width])
		buf.WriteString("\r\n")
		s = s[width:]
	}
	if s != "" {
		buf.WriteString(s)
		buf.WriteString("\r\n")
	}
}

// safeFilename 去掉文件名里会破坏 MIME 头的字符。
func safeFilename(name string) string {
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\"", "'")
	if strings.TrimSpace(name) == "" {
		return "attachment"
	}
	return name
}

// crlf 把正文里的裸 \n 规范成 \r\n（已是 \r\n 的不重复转换），
// 符合 RFC822 行结束与 IMAP literal 要求。
func crlf(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// htmlToPlainText 极简 HTML 转纯文本：去标签、解实体、压缩空行。够 IMAP 客户端预览用。
func htmlToPlainText(html string) string {
	s := strings.ReplaceAll(html, "<br>", "\n")
	s = strings.ReplaceAll(html, "<br/>", "\n")
	s = strings.ReplaceAll(html, "<br />", "\n")
	s = strings.ReplaceAll(html, "</p>", "\n")
	s = strings.ReplaceAll(html, "</div>", "\n")

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
