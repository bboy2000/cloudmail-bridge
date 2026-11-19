package main

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

func base64DecodeString(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	return string(b), err
}

func quotedPrintableDecode(b []byte) (string, error) {
	r := quotedprintable.NewReader(strings.NewReader(string(b)))
	out, err := io.ReadAll(r)
	return string(out), err
}

// ========== SMTP Backend / Session ==========

type smtpBackend struct {
	client *cloudMailClient
	auth   *authManager
	store  *mailStore
	cfg    *Config
}

func (be *smtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	tlsState, _ := c.TLSConnectionState()
	log.Printf("[smtp] SESSION new: helo=%q tlsHandshake=%v", c.Hostname(), tlsState.HandshakeComplete)
	return &smtpSession{backend: be}, nil
}

type smtpSession struct {
	backend *smtpBackend
	auth    bool
	user    string
	client  *cloudMailClient // 该会话登录用户的专属 client（带 JWT）
	from    string
	to      []string
}

func (s *smtpSession) AuthMechanisms() []string {
	return []string{sasl.Plain, sasl.Login}
}

// loginServer 实现 SMTP AUTH LOGIN 的服务端（网易/QQ/Outlook 等国内客户端常用）。
// LOGIN 是 challenge-response：发 "Username:" → 收用户名 → 发 "Password:" → 收密码。
type loginServer struct {
	step int // 0=待发Username挑战, 1=待收用户名/发Password挑战, 2=待收密码
	user string
	auth func(username, password string) error
}

func (l *loginServer) Next(response []byte) (challenge []byte, done bool, err error) {
	switch l.step {
	case 0:
		// 客户端可能在 AUTH LOGIN 后直接带初始响应（用户名），也可能是空
		if response != nil {
			l.user = string(response)
			l.step = 2
			return []byte("Password:"), false, nil
		}
		l.step = 1
		return []byte("Username:"), false, nil
	case 1:
		l.user = string(response)
		l.step = 2
		return []byte("Password:"), false, nil
	case 2:
		if err := l.auth(l.user, string(response)); err != nil {
			return nil, true, err
		}
		return nil, true, nil
	}
	return nil, true, errors.New("unexpected login step")
}

// verify 是 PLAIN 和 LOGIN 共用的凭据校验：密码交给 cloud-mail 网站验证。
func (s *smtpSession) verify(username, password string) error {
	c, err := s.backend.auth.authenticate(username, password)
	if err != nil {
		log.Printf("[smtp] AUTH FAIL %s: %v", username, err)
		return errors.New("authentication failed")
	}
	s.auth = true
	s.user = normEmail(username)
	s.client = c
	log.Printf("[smtp] AUTH ok: %s (mech done)", s.user)
	return nil
}

func (s *smtpSession) Auth(mech string) (sasl.Server, error) {
	log.Printf("[smtp] AUTH mech=%s", mech)
	switch strings.ToUpper(mech) {
	case sasl.Login:
		return &loginServer{auth: s.verify}, nil
	default:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			return s.verify(username, password)
		}), nil
	}
}

func (s *smtpSession) Mail(from string, opts *smtp.MailOptions) error {
	if !s.auth {
		return smtp.ErrAuthRequired
	}
	s.from = from
	s.to = nil
	return nil
}

func (s *smtpSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	if !s.auth {
		return smtp.ErrAuthRequired
	}
	s.to = append(s.to, to)
	return nil
}

func (s *smtpSession) Data(r io.Reader) error {
	if !s.auth {
		return smtp.ErrAuthRequired
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	msg, err := parseIncomingMail(raw)
	if err != nil {
		log.Printf("[smtp] parse mail failed: %v", err)
		return err
	}
	// 收件人兜底：优先用 SMTP RCPT，其次邮件头 To
	recipients := s.to
	if len(recipients) == 0 {
		recipients = msg.To
	}
	if len(recipients) == 0 {
		return errors.New("no recipients")
	}

	log.Printf("[smtp] sending: from=%s to=%v subject=%q textLen=%d htmlLen=%d atts=%d",
		s.from, recipients, msg.Subject, len(msg.Text), len(msg.HTML), len(msg.Attachments))

	// 用该会话登录用户的专属 client 发现其名下账号（JWT 隔离）
	cl := s.client
	if cl == nil {
		cl = s.backend.client
	}
	accounts, err := cl.listAccounts()
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	accountID := int64(0)
	senderEmail := s.user
	for _, a := range accounts {
		if strings.EqualFold(a.Email, s.user) || expectAccount(s.user, a.Email, accounts) {
			accountID = a.AccountID
			senderEmail = a.Email
			break
		}
	}
	if accountID == 0 && len(accounts) > 0 {
		// 兜底用第一个账号
		accountID = accounts[0].AccountID
		senderEmail = accounts[0].Email
	}
	if accountID == 0 {
		return errors.New("no sender account available")
	}

	recips := make([]string, 0, len(recipients))
	for _, addr := range recipients {
		clean := extractAddr(addr)
		if clean != "" {
			recips = append(recips, clean)
		}
	}
	if len(recips) == 0 {
		return errors.New("no valid recipients")
	}

	sendReq := sendEmailRequest{
		AccountID:    accountID,
		Name:         msg.FromName,
		ReceiveEmail: recips,
		Subject:      msg.Subject,
		Text:         msg.Text,
		Content:      msg.HTML,
		Attachments:  msg.Attachments,
	}
	emailID, err := cl.sendEmail(sendReq)
	if err != nil {
		log.Printf("[smtp] send FAILED: %v", err)
		return fmt.Errorf("send via cloud-mail: %w", err)
	}
	log.Printf("[smtp] send OK via cloud-mail (emailId=%d accountId=%d sender=%s)", emailID, accountID, senderEmail)

	// 写入本地 Sent 存储，让 IMAP 客户端在「已发送」能看到。
	// 用 cloud-mail 返回的真实 emailId 做 UID —— 与远端同步的 Sent 天然去重。
	if emailID > 0 {
		s.backend.store.addSent(s.user, raw, msg.Subject, imap.UID(emailID))
	}

	return nil
}

func (s *smtpSession) Reset()        { s.from = ""; s.to = nil }
func (s *smtpSession) Logout() error { return nil }

// expectAccount：登录用户名可能是不带域名前缀，这里做宽松匹配
func expectAccount(login, accountEmail string, accounts []mailAccount) bool {
	return strings.EqualFold(extractAddr(login), extractAddr(accountEmail))
}

func extractAddr(s string) string {
	if i := strings.LastIndex(s, "<"); i >= 0 && strings.HasSuffix(s, ">") {
		return strings.TrimSpace(s[i+1 : len(s)-1])
	}
	return strings.TrimSpace(strings.Trim(s, "\" "))
}

// ========== RFC822 解析 ==========

type incomingMail struct {
	Subject     string
	FromName    string
	From        []string
	To          []string
	Text        string
	HTML        string
	Attachments []sendAttachment // 文档 #4 发信附件（Base64）
}

// collectParts 递归遍历 MIME 树：正文写进 out，附件（Content-Disposition: attachment
// 或非 text 的 part）解码后追加到 out.Attachments。发信接口要 Base64，这里直接编码。
func collectParts(out *incomingMail, mediaType string, params map[string]string, headers mail.Header, body []byte) {
	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		mr := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF || err != nil {
				break
			}
			pb, _ := io.ReadAll(part)
			pct, pparams, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
			if err != nil {
				pct = "text/plain"
				pparams = nil
			}
			collectParts(out, pct, pparams, mail.Header(part.Header), pb)
		}
	case mediaType == "text/html":
		if out.HTML == "" {
			out.HTML = decodeBody(body, headers.Get("Content-Transfer-Encoding"))
		}
	case mediaType == "text/plain":
		if out.Text == "" {
			out.Text = decodeBody(body, headers.Get("Content-Transfer-Encoding"))
		}
	default:
		// 非 text/* 视为附件（image/audio/application 等）
		addAttachment(out, headers, mediaType, body)
	}
}

// addAttachment 把一个 part 作为发信附件追加：原始字节转 Base64。
// 内嵌图片（Content-ID + related）也一并作为附件发送，至少不丢资源。
func addAttachment(out *incomingMail, headers mail.Header, mediaType string, body []byte) {
	disp, dparams, _ := mime.ParseMediaType(headers.Get("Content-Disposition"))
	filename := dparams["filename"]
	if filename == "" {
		filename = headers.Get("Content-Type")
		if f, p, err := mime.ParseMediaType(filename); err == nil {
			filename = p["name"]
			mediaType = f
		}
	}
	if filename == "" {
		// 有 Content-ID 的内嵌图片：用 cid 作文件名
		if cid := strings.Trim(headers.Get("Content-ID"), "<> "); cid != "" {
			filename = cid
		}
	}
	if filename == "" {
		filename = fmt.Sprintf("attachment-%d", len(out.Attachments)+1)
	}
	// 客户端（网易/QQ 等）常把附件名做成 RFC2047 编码：
	//   =?utf-8?Q?xxx=5Fyyy.jpg?=  或  =?utf-8?B?...?=
	// 不解码就原样发给 cloud-mail，站点会把 ? 当特殊字符替换，对方收到就是乱码
	// （如 =-utf-8-Q-...-=）。这里按 RFC2047 解回真实文件名；普通文件名不受影响。
	if decoded, derr := (&mime.WordDecoder{}).DecodeHeader(filename); derr == nil && decoded != "" {
		filename = decoded
	}
	filename = safeFilename(filename)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	_ = disp // disposition 仅用于命名，附件与否由 mediaType 判定
	// body 是按 Content-Transfer-Encoding 编码的（常见 base64）：
	// 必须先解码回原始字节，再编码给发信接口，否则会双重 base64 发出乱码。
	raw := []byte(decodeBody(body, headers.Get("Content-Transfer-Encoding")))
	out.Attachments = append(out.Attachments, sendAttachment{
		Filename:    filename,
		Content:     base64.StdEncoding.EncodeToString(raw),
		ContentType: mediaType,
		Size:        len(raw),
	})
}

func parseIncomingMail(raw []byte) (*incomingMail, error) {
	m, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	out := &incomingMail{}
	wd := &mime.WordDecoder{}
	if subj, err := wd.DecodeHeader(m.Header.Get("Subject")); err == nil {
		out.Subject = subj
	} else {
		out.Subject = m.Header.Get("Subject")
	}
	if froms, err := mail.ParseAddressList(m.Header.Get("From")); err == nil && len(froms) > 0 {
		out.FromName = froms[0].Name
		for _, a := range froms {
			out.From = append(out.From, a.Address)
		}
	}
	if tos, err := mail.ParseAddressList(m.Header.Get("To")); err == nil {
		for _, a := range tos {
			out.To = append(out.To, a.Address)
		}
	}

	mediaType, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		mediaType = "text/plain"
	}
	body, _ := io.ReadAll(m.Body)
	collectParts(out, mediaType, params, m.Header, body)
	return out, nil
}

func decodeBody(b []byte, enc string) string {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		clean := strings.NewReplacer("\r", "", "\n", "", " ", "").Replace(string(b))
		dec, err := base64DecodeString(clean)
		if err == nil {
			return dec
		}
	case "quoted-printable":
		dec, err := quotedPrintableDecode(b)
		if err == nil {
			return dec
		}
	}
	return string(b)
}

// ========== SMTP 服务启动 ==========

func startSMTPServer(cfg *Config, client *cloudMailClient, auth *authManager, store *mailStore, tlsConfig *tls.Config) (*smtp.Server, error) {
	be := &smtpBackend{client: client, auth: auth, store: store, cfg: cfg}
	s := smtp.NewServer(be)
	s.Addr = cfg.SMTPListenAddr
	s.Domain = cfg.Domain
	s.EnableSMTPUTF8 = true
	s.ReadTimeout = 60 * time.Second
	s.WriteTimeout = 60 * time.Second
	s.MaxMessageBytes = 20 * 1024 * 1024
	s.MaxRecipients = 50

	// 隐式 TLS：连接本身加密，允许 PLAIN 认证；明文模式也需要 AllowInsecureAuth
	if cfg.TLSMode == "implicit" || cfg.TLSMode == "plain" {
		s.AllowInsecureAuth = true
	}
	if cfg.TLSMode == "implicit" || cfg.TLSMode == "starttls" {
		s.TLSConfig = tlsConfig
	}

	go func() {
		if cfg.TLSMode == "implicit" {
			log.Printf("SMTP server listening on %s (implicit TLS)", s.Addr)
			if err := s.ListenAndServeTLS(); err != nil && err != smtp.ErrServerClosed {
				log.Printf("SMTP serve: %v", err)
			}
		} else {
			log.Printf("SMTP server listening on %s (tls_mode=%s)", s.Addr, cfg.TLSMode)
			if err := s.ListenAndServe(); err != nil && err != smtp.ErrServerClosed {
				log.Printf("SMTP serve: %v", err)
			}
		}
	}()
	return s, nil
}
