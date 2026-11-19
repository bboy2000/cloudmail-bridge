package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// cloud-mail 的 emailList 请求/响应结构（严格对照源码 entity/email.js 与 public-service.js）
type emailListRequest struct {
	ToEmail   string `json:"toEmail,omitempty"`
	SendName  string `json:"sendName,omitempty"`
	SendEmail string `json:"sendEmail,omitempty"`
	Subject   string `json:"subject,omitempty"`
	Content   string `json:"content,omitempty"`
	Type      *int   `json:"type,omitempty"`    // 0=收信 RECEIVE, 1=发信 SEND
	IsDel     *int   `json:"isDel,omitempty"`   // 0=正常
	Num       int    `json:"num"`               // 页码，从 1 开始
	Size      int    `json:"size"`              // 每页条数
	TimeSort  string `json:"timeSort,omitempty"` // asc / desc（按 emailId）
}

type genTokenRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// mailAttachment 对应 API full=1 的 attList 元素。
// 官方 API 文档只列出 attId/filename/mimeType/size，实际对象还带 key
// （网页版用 cvtR2Url(att.key) 拼对象存储直链下载）。
// Data 是网关下载后的字节，不参与 JSON 解码。
type mailAttachment struct {
	AttID    int64  `json:"attId"`
	Filename string `json:"filename"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
	Key      string `json:"key"`
	Data     []byte `json:"-"`
}

type emailItem struct {
	EmailID    int64            `json:"emailId"`
	SendEmail  string           `json:"sendEmail"`
	Name       string           `json:"name"`     // 文档 #3 标准字段：发件人名字
	SendName   string           `json:"sendName"` // 兼容旧 public 接口的同义字段
	Subject    string           `json:"subject"`
	ToEmail    string           `json:"toEmail"`
	ToName     string           `json:"toName"`
	Type       int              `json:"type"`
	CreateTime string           `json:"createTime"`
	Content    string           `json:"content"` // HTML 正文
	Text       string           `json:"text"`    // 纯文本正文
	IsDel      int              `json:"isDel"`
	Unread     int              `json:"unread"`  // 0=未读 1=已读
	AttList    []mailAttachment `json:"attList"` // full=1 时返回
}

// senderName 发件人显示名：标准接口给 name，旧 public 接口给 sendName，优先用标准字段。
func (it emailItem) senderName() string {
	if strings.TrimSpace(it.Name) != "" {
		return it.Name
	}
	return it.SendName
}

type apiResponse struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type cloudMailClient struct {
	baseURL string
	http    *http.Client

	mu        sync.RWMutex
	token     string // public token（收信用）
	adminMail string
	adminPass string

	jwtMu     sync.RWMutex
	jwt       string // 用户登录态 token（发信用）
	loginMail string
	loginPass string

	accountsMu sync.RWMutex
	accounts   []mailAccount // 发件账号列表缓存（很少变）
}

func newCloudMailClient(baseURL string) *cloudMailClient {
	// 禁用 HTTP/2：Cloudflare 端对 Go 复用连接的 HTTP/2 偶发 EOF，
	// 强制 HTTP/1.1 + 关闭长连接复用可稳定性更好（轮询间隔 60s，短连接开销可忽略）。
	tr := &http.Transport{
		TLSNextProto:     make(map[string]func(string, *tls.Conn) http.RoundTripper), // 禁用 HTTP/2 自动升级
		DisableKeepAlives: true,
		MaxIdleConns:      0,
	}
	return &cloudMailClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second, Transport: tr},
	}
}

// setAdmin 设置管理员凭据（仅存内存，用于 token 过期时自动续期）
func (c *cloudMailClient) setAdmin(email, password string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.adminMail, c.adminPass = email, password
}

// SetToken 直接注入已知 token（用户自行生成时用）
func (c *cloudMailClient) SetToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = strings.TrimSpace(token)
}

func (c *cloudMailClient) getToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

// genToken 用管理员账号换取 public token。源码：genToken -> verifyUser（校验 email===c.env.admin）
func (c *cloudMailClient) genToken() (string, error) {
	c.mu.RLock()
	email, password := c.adminMail, c.adminPass
	c.mu.RUnlock()
	if strings.TrimSpace(email) == "" || password == "" {
		return "", fmt.Errorf("admin email/password not configured")
	}

	b, err := json.Marshal(genTokenRequest{Email: email, Password: password})
	if err != nil {
		return "", err
	}
	url := c.baseURL + "/api/public/genToken"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var ar apiResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return "", fmt.Errorf("bad JSON: %w (body=%s)", err, string(body))
	}
	if ar.Code != 200 {
		return "", fmt.Errorf("genToken failed code=%d msg=%s", ar.Code, ar.Message)
	}
	var d struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(ar.Data, &d); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	if d.Token == "" {
		return "", fmt.Errorf("empty token returned")
	}
	c.mu.Lock()
	c.token = d.Token
	c.mu.Unlock()
	return d.Token, nil
}

// ensureToken 确保有可用 token；没有则用管理员凭据生成。
func (c *cloudMailClient) ensureToken() error {
	if c.getToken() != "" {
		return nil
	}
	_, err := c.genToken()
	return err
}

// genTokenIfNeeded 返回当前 token（已有则直接用，否则生成）。用于启动自检。
func (c *cloudMailClient) genTokenIfNeeded() (string, error) {
	if tok := c.getToken(); tok != "" {
		return tok, nil
	}
	return c.genToken()
}

// maskToken 只显示 token 前 8 位，避免完整 token 打进日志
func maskToken(t string) string {
	if len(t) <= 8 {
		return "****"
	}
	return t[:8] + "****"
}

// fetchEmailList 拉取一页邮件（自动带 Authorization）。
// - 401：token 失效 -> 重新生成后重试
// - 网络类错误（EOF/超时等）：指数退避重试最多 3 次
func (c *cloudMailClient) fetchEmailList(toEmail string, mailType int, num, size int) ([]emailItem, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		list, err, unauthorized := c.doFetchEmailList(toEmail, mailType, num, size)
		if unauthorized {
			// token 失效：重新生成后重试
			if _, gerr := c.genToken(); gerr != nil {
				return nil, fmt.Errorf("token expired and regenerate failed: %w", gerr)
			}
			continue
		}
		if err == nil {
			return list, nil
		}
		lastErr = err
		// 只对网络类错误重试；API 业务错误（code!=200）不重试
		if !isNetworkErr(err.Error()) {
			return nil, err
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return nil, lastErr
}

func isNetworkErr(msg string) bool {
	low := strings.ToLower(msg)
	for _, kw := range []string{"eof", "timeout", "connection reset", "broken pipe", "i/o timeout", "forcibly closed", "wsarecv", "connection refused", "dial ", "no such host"} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

func (c *cloudMailClient) doFetchEmailList(toEmail string, mailType int, num, size int) ([]emailItem, error, bool) {
	if err := c.ensureToken(); err != nil {
		return nil, err, false
	}

	reqBody := emailListRequest{
		ToEmail:   toEmail,
		Type:      &mailType,
		IsDel:     intPtr(0),
		Num:       num,
		Size:      size,
		TimeSort:  "desc",
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err, false
	}

	url := c.baseURL + "/api/public/emailList"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", c.getToken()) // 源码 constant.TOKEN_HEADER='Authorization'
	req.Header.Set("User-Agent", "cloudmail-bridge/0.2")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err), false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err, false
	}

	var ar apiResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("bad JSON: %w (body=%s)", err, string(body)), false
	}
	if ar.Code == 401 {
		return nil, fmt.Errorf("API 401: %s", ar.Message), true
	}
	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("HTTP 401: %s", string(body)), true
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, url, string(body)), false
	}
	if ar.Code != 200 {
		return nil, fmt.Errorf("API error code=%d msg=%s", ar.Code, ar.Message), false
	}
	var list []emailItem
	if len(ar.Data) > 0 && string(ar.Data) != "null" {
		if err := json.Unmarshal(ar.Data, &list); err != nil {
			return nil, fmt.Errorf("decode data: %w", err), false
		}
	}
	return list, nil, false
}

func intPtr(v int) *int { return &v }
