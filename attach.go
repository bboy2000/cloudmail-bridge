package main

// 附件下载：按官方源码 cvtR2Url(att.key) 的规则拼 URL，
// 把 cloud-mail 的附件字节填进 emailItem.AttList[].Data，
// 供 buildRawEmail 生成带 attachment 的 RFC822。

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// 单个附件上限，避免异常大的对象把网关内存吃满。
const maxAttachmentBytes = 50 << 20 // 50 MiB

// attachmentURL 复刻网页版 cvtR2Url：
//   - key 本身是 http(s):// 绝对地址 -> 直接用
//   - 配置了 attachment_base_url -> 作为对象存储域名前缀
//   - 否则 -> 用站点 base_url（对应官方源码无 r2Domain 时的本站 /attachments 代理）
func attachmentURL(cfg *Config, key string) string {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(strings.ToLower(key), "http://") || strings.HasPrefix(strings.ToLower(key), "https://") {
		return key
	}
	base := strings.TrimRight(cfg.AttachmentBaseURL, "/")
	if base == "" {
		base = strings.TrimRight(cfg.BaseURL, "/")
	}
	return base + "/" + strings.TrimLeft(key, "/")
}

// fetchAttachmentData 下载单个附件。失败只记日志、不阻断收信：
// 客户端仍能看到正文，只是这一封没有附件。
// 站点经 Cloudflare，偶发 connection reset（实测约 1/3 概率），必须重试，
// 否则一次抖动就导致这封附件永久缺失（旧记录有 hasMessageWithAttachments 保护，
// 但当时那轮没下到就补不上了）。
func fetchAttachmentData(hc *http.Client, cfg *Config, att *mailAttachment) error {
	if att == nil || strings.TrimSpace(att.Key) == "" {
		return fmt.Errorf("attachment has no key")
	}
	url := attachmentURL(cfg, att.Key)

	const attempts = 4
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// 指数退避：150ms / 300ms / 600ms，抖动一般由出口瞬断引起，稍等即可恢复
			time.Sleep(time.Duration(150<<uint(attempt-1)) * time.Millisecond)
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "cloudmail-bridge/0.3")
		req.Header.Set("Accept", "*/*")
		resp, err := hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			resp.Body.Close()
			// 4xx 是永久错误（如 NoSuchKey），重试无意义
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				return lastErr
			}
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentBytes+1))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if len(data) > maxAttachmentBytes {
			return fmt.Errorf("attachment too large (>%d bytes)", maxAttachmentBytes)
		}
		att.Data = data
		return nil
	}
	return fmt.Errorf("after %d attempts: %w", attempts, lastErr)
}

// enrichAttachments 为一批邮件补齐附件字节。
// user 用于跳过已入库邮件，避免每次同步重复下载同一附件（省对象存储流量）。
func (a *authManager) enrichAttachments(c *cloudMailClient, user string, items []emailItem) {
	for i := range items {
		it := &items[i]
		if len(it.AttList) == 0 {
			continue
		}
		// 已入库且已有附件的邮件不再重复下载；只有"旧记录缺附件"时才补。
		if a.store.hasMessageWithAttachments(user, it.EmailID) {
			continue
		}
		for j := range it.AttList {
			att := &it.AttList[j]
			if len(att.Data) > 0 || strings.TrimSpace(att.Key) == "" {
				continue
			}
			if err := fetchAttachmentData(c.http, a.cfg, att); err != nil {
				log.Printf("[attach] user=%s emailId=%d file=%q 下载失败: %v", user, it.EmailID, att.Filename, err)
				continue
			}
			log.Printf("[attach] user=%s emailId=%d file=%q size=%d OK", user, it.EmailID, att.Filename, len(att.Data))
		}
	}
}
