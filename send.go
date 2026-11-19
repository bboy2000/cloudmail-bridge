package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ========== 登录换 JWT（发信用）==========

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// setLoginCred 设置发信账号的邮箱密码（仅内存）
func (c *cloudMailClient) setLoginCred(email, password string) {
	c.jwtMu.Lock()
	defer c.jwtMu.Unlock()
	c.loginMail, c.loginPass = email, password
}

func (c *cloudMailClient) getJWT() string {
	c.jwtMu.RLock()
	defer c.jwtMu.RUnlock()
	return c.jwt
}

// login 用邮箱密码换用户 JWT。POST /api/login -> data.token（网络错误重试3次）
func (c *cloudMailClient) login() (string, error) {
	c.jwtMu.RLock()
	email, password := c.loginMail, c.loginPass
	c.jwtMu.RUnlock()
	if strings.TrimSpace(email) == "" || password == "" {
		return "", fmt.Errorf("login email/password not configured")
	}

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		b, _ := json.Marshal(loginRequest{Email: email, Password: password})
		req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/login", bytes.NewReader(b))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 600 * time.Millisecond)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		var ar apiResponse
		if err := json.Unmarshal(body, &ar); err != nil {
			return "", fmt.Errorf("login bad JSON: %w (body=%s)", err, string(body))
		}
		if ar.Code != 200 {
			return "", fmt.Errorf("login failed code=%d msg=%s", ar.Code, ar.Message)
		}
		var d struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(ar.Data, &d); err != nil || d.Token == "" {
			return "", fmt.Errorf("login token decode: %w (data=%s)", err, string(ar.Data))
		}
		c.jwtMu.Lock()
		c.jwt = d.Token
		c.jwtMu.Unlock()
		return d.Token, nil
	}
	return "", fmt.Errorf("POST /api/login failed after 3 attempts: %w", lastErr)
}

// ensureJWT 确保有 JWT
func (c *cloudMailClient) ensureJWT() error {
	if c.getJWT() != "" {
		return nil
	}
	_, err := c.login()
	return err
}

// authedRequest 构造带 JWT 的请求
func (c *cloudMailClient) authedRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.getJWT())
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// ========== 账号列表 ==========

type mailAccount struct {
	AccountID int64  `json:"accountId"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	UserID    int64  `json:"userId"`
	Sort      int    `json:"sort"`
}

// listAccounts 拿当前登录用户的发件账号列表（带缓存 + 网络重试5次 + 401重新登录）
func (c *cloudMailClient) listAccounts() ([]mailAccount, error) {
	c.accountsMu.RLock()
	cached := c.accounts
	c.accountsMu.RUnlock()
	if len(cached) > 0 {
		return cached, nil
	}
	if err := c.ensureJWT(); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("size", "30")
	path := "/api/account/list?" + q.Encode()

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		req, err := c.authedRequest(http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		var ar apiResponse
		if err := json.Unmarshal(body, &ar); err != nil {
			return nil, fmt.Errorf("account/list bad JSON: %w (body=%s)", err, string(body))
		}
		if ar.Code == 401 {
			if _, e := c.login(); e != nil {
				return nil, fmt.Errorf("JWT expired and relogin failed: %w", e)
			}
			continue
		}
		if ar.Code != 200 {
			return nil, fmt.Errorf("account/list error code=%d msg=%s", ar.Code, ar.Message)
		}
		var list []mailAccount
		if len(ar.Data) > 0 && string(ar.Data) != "null" {
			if err := json.Unmarshal(ar.Data, &list); err != nil {
				return nil, fmt.Errorf("decode accounts: %w", err)
			}
		}
		c.accountsMu.Lock()
		c.accounts = list
		c.accountsMu.Unlock()
		return list, nil
	}
	return nil, fmt.Errorf("GET account/list failed after 5 attempts: %w", lastErr)
}

// ========== 发信 ==========

// listSentEmails 查询 cloud-mail 已发送记录（type=1），用于验证网页版「已发送」可见
func (c *cloudMailClient) listSentEmails(accountID int64, size int) ([]emailItem, error) {
	if err := c.ensureJWT(); err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/api/email/list?accountId=%d&type=1&size=%d&full=0", accountID, size)
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		req, err := c.authedRequest(http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		var ar apiResponse
		if err := json.Unmarshal(body, &ar); err != nil {
			return nil, fmt.Errorf("email/list bad JSON: %w (body=%s)", err, string(body))
		}
		if ar.Code == 401 {
			if _, e := c.login(); e != nil {
				return nil, fmt.Errorf("relogin failed: %w", e)
			}
			continue
		}
		if ar.Code != 200 {
			return nil, fmt.Errorf("email/list error code=%d msg=%s", ar.Code, ar.Message)
		}
		// data 是 {total, latestEmail, list:[...]}
		var d struct {
			List []emailItem `json:"list"`
		}
		if err := json.Unmarshal(ar.Data, &d); err != nil {
			return nil, fmt.Errorf("decode email/list: %w", err)
		}
		return d.List, nil
	}
	return nil, fmt.Errorf("email/list failed after 5 attempts: %w", lastErr)
}

// ========== 标记已读 ==========

// markRead 把邮件标记为已读。cloud-mail 源码：PUT /api/email/read
// body {emailIds:[...]} -> set unread = READ(1)，按 userId 过滤。
// 注意：cloud-mail 只有"标记已读"接口，没有反向的"标记未读"接口。
func (c *cloudMailClient) markRead(emailIDs []int64) error {
	if len(emailIDs) == 0 {
		return nil
	}
	if err := c.ensureJWT(); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]interface{}{"emailIds": emailIDs})

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		req, err := c.authedRequest(http.MethodPut, "/api/email/read", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		var ar apiResponse
		if err := json.Unmarshal(respBody, &ar); err != nil {
			return fmt.Errorf("read bad JSON: %w (body=%s)", err, string(respBody))
		}
		if ar.Code == 401 {
			if _, e := c.login(); e != nil {
				return fmt.Errorf("JWT expired and relogin failed: %w", e)
			}
			continue
		}
		if ar.Code != 200 {
			return fmt.Errorf("read error code=%d msg=%s", ar.Code, ar.Message)
		}
		return nil
	}
	return fmt.Errorf("PUT /api/email/read failed after 5 attempts: %w", lastErr)
}

// ========== 删除邮件（预留，当前不启用）==========
//
// 说明：本网关的删除语义是「只在客户端/网关侧删除，不动 cloud-mail 服务器上的邮件」。
// 因此正常流程不会调用本函数；仅在此后需要「删客户端 = 删服务器」时才接上。
// 若启用，需在 main.go 设置 store.onDelete 回调。

// deleteEmails 删除邮件。严格按文档 #5：DELETE /api/email/delete?emailIds=1,2
// 文档原文："删除当前用户的邮件。默认标记删除，系统开启同步删除时为永久删除"
func (c *cloudMailClient) deleteEmails(emailIDs []int64) error {
	if len(emailIDs) == 0 {
		return nil
	}
	if err := c.ensureJWT(); err != nil {
		return err
	}
	ids := make([]string, 0, len(emailIDs))
	for _, id := range emailIDs {
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	path := "/api/email/delete?emailIds=" + url.QueryEscape(strings.Join(ids, ","))

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		req, err := c.authedRequest(http.MethodDelete, path, nil)
		if err != nil {
			return err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		var ar apiResponse
		if err := json.Unmarshal(respBody, &ar); err != nil {
			return fmt.Errorf("delete bad JSON: %w (body=%s)", err, string(respBody))
		}
		if ar.Code == 401 {
			if _, e := c.login(); e != nil {
				return fmt.Errorf("JWT expired and relogin failed: %w", e)
			}
			continue
		}
		if ar.Code != 200 {
			return fmt.Errorf("delete error code=%d msg=%s", ar.Code, ar.Message)
		}
		return nil
	}
	return fmt.Errorf("DELETE /api/email/delete failed after 5 attempts: %w", lastErr)
}

// getEmailListPage 带重试的 GET /api/email/list 单页查询（JWT 鉴权）。
func (c *cloudMailClient) getEmailListPage(path string) ([]emailItem, error) {
	if err := c.ensureJWT(); err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		req, err := c.authedRequest(http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		var ar apiResponse
		if err := json.Unmarshal(body, &ar); err != nil {
			return nil, fmt.Errorf("email/list bad JSON: %w (body=%s)", err, string(body))
		}
		if ar.Code == 401 {
			if _, e := c.login(); e != nil {
				return nil, fmt.Errorf("relogin failed: %w", e)
			}
			continue
		}
		if ar.Code != 200 {
			return nil, fmt.Errorf("email/list error code=%d msg=%s", ar.Code, ar.Message)
		}
		var d struct {
			List []emailItem `json:"list"`
		}
		if err := json.Unmarshal(ar.Data, &d); err != nil {
			return nil, fmt.Errorf("decode email/list: %w", err)
		}
		return d.List, nil
	}
	return nil, fmt.Errorf("email/list failed after 5 attempts: %w", lastErr)
}

// listInboxEmails 查一页（用于调试验证 unread）。
func (c *cloudMailClient) listInboxEmails(accountID int64, mailType, size int) ([]emailItem, error) {
	path := fmt.Sprintf("/api/email/list?accountId=%d&type=%d&size=%d&full=0", accountID, mailType, size)
	return c.getEmailListPage(path)
}

// fetchAllEmailList 用 emailId 游标分页拉全量（type: 0收件 1发件）。
// cloud-mail 文档：size 最大 50；emailId 游标 + timeSort=0 查更早。
func (c *cloudMailClient) fetchAllEmailList(accountID int64, mailType int) ([]emailItem, error) {
	var all []emailItem
	var cursor int64
	for page := 0; page < 40; page++ { // 最多 40 页 × 50 = 2000 封，防死循环
		path := fmt.Sprintf("/api/email/list?accountId=%d&type=%d&size=50&full=1", accountID, mailType)
		if cursor > 0 {
			path += fmt.Sprintf("&emailId=%d&timeSort=0", cursor)
		}
		items, err := c.getEmailListPage(path)
		if err != nil {
			return all, err
		}
		all = append(all, items...)
		if len(items) < 50 {
			break
		}
		lastID := items[len(items)-1].EmailID
		if lastID == cursor {
			break
		}
		cursor = lastID
	}
	return all, nil
}

// 文档确认：receiveEmail 是 array<string>（纯邮箱字符串数组），不是对象数组
// sendAttachment 是文档 #4 /api/email/send 的 attachments 子结构：
// content 为文件内容 Base64，contentType 为 MIME，size 可选。
type sendAttachment struct {
	Filename    string `json:"filename"`
	Content     string `json:"content"` // Base64
	ContentType string `json:"contentType,omitempty"`
	Size        int    `json:"size,omitempty"`
}

type sendEmailRequest struct {
	AccountID    int64            `json:"accountId"`
	Name         string           `json:"name,omitempty"`
	ReceiveEmail []string         `json:"receiveEmail"`
	Subject      string           `json:"subject"`
	Text         string           `json:"text,omitempty"`
	Content      string           `json:"content,omitempty"` // HTML
	SendType     string           `json:"sendType,omitempty"`
	EmailID      int64            `json:"emailId,omitempty"` // reply 时必填（文档 #4）
	Attachments  []sendAttachment `json:"attachments,omitempty"`
}

// sendEmail 通过 cloud-mail 的 /api/email/send 发信（内部走它配好的 Resend，并自动写 D1「已发送」）。
// 网络错误重试5次；401 重新登录后续试。返回 cloud-mail 分配的真实 emailId。
func (c *cloudMailClient) sendEmail(req sendEmailRequest) (int64, error) {
	if err := c.ensureJWT(); err != nil {
		return 0, err
	}
	b, _ := json.Marshal(req)

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		httpReq, err := c.authedRequest(http.MethodPost, "/api/email/send", bytes.NewReader(b))
		if err != nil {
			return 0, err
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(httpReq)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		var ar apiResponse
		if err := json.Unmarshal(body, &ar); err != nil {
			return 0, fmt.Errorf("send bad JSON: %w (body=%s)", err, string(body))
		}
		if ar.Code == 401 {
			if _, e := c.login(); e != nil {
				return 0, fmt.Errorf("JWT expired and relogin failed: %w", e)
			}
			continue
		}
		if ar.Code != 200 {
			return 0, fmt.Errorf("send error code=%d msg=%s", ar.Code, ar.Message)
		}
		// 返回 data 是数组，取首个 emailId
		var sent []struct {
			EmailID int64 `json:"emailId"`
		}
		if err := json.Unmarshal(ar.Data, &sent); err == nil && len(sent) > 0 {
			return sent[0].EmailID, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("POST /api/email/send failed after 5 attempts: %w", lastErr)
}
