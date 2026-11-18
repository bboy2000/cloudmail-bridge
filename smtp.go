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

	log.Printf("[smtp] sending: from=%s to=%v subject=%q textLen=%d htmlLen=%d",
		s.from, recipients, msg.Subject, len(msg.Text), len(msg.HTML))

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
	Subject  string
	FromName string
	From     []string
	To       []string
	Text     string
	HTML     string
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

	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			pct := part.Header.Get("Content-Type")
			pb, _ := io.ReadAll(part)
			text := decodeBody(pb, part.Header.Get("Content-Transfer-Encoding"))
			if strings.HasPrefix(pct, "text/html") && out.HTML == "" {
				out.HTML = text
			} else if strings.HasPrefix(pct, "text/plain") && out.Text == "" {
				out.Text = text
			}
		}
	} else {
		text := decodeBody(body, m.Header.Get("Content-Transfer-Encoding"))
		if mediaType == "text/html" {
			out.HTML = text
		} else {
			out.Text = text
		}
	}
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
