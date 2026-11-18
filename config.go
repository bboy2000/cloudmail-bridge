package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// Config 网关配置。可用 config.json 或环境变量提供（环境变量优先）。
type Config struct {
	BaseURL        string   `json:"base_url"`          // cloud-mail 根地址，例如 https://example.com
	ListenAddr     string   `json:"listen_addr"`       // IMAP 监听地址，默认 0.0.0.0:993
	SMTPListenAddr string   `json:"smtp_listen_addr"`  // SMTP 监听地址，默认 0.0.0.0:465
	Domain         string   `json:"domain"`            // 邮箱域名，用于生成 Message-ID
	PollInterval   int      `json:"poll_interval"`     // 轮询间隔（秒）
	PageSize       int      `json:"page_size"`        // 每页拉取条数
	MaxPages       int      `json:"max_pages"`        // 最多拉几页，0=全量
	InsecureAuth   bool     `json:"insecure_auth"`     // 允许明文认证
	TLSMode        string   `json:"tls_mode"`         // implicit / starttls / plain
	CertFile       string   `json:"cert_file"`        // 证书链 fullchain.pem
	KeyFile        string   `json:"key_file"`         // 私钥 key.pem
	ImapUser       string   `json:"imap_user"`        // 可选固定用户名，留空=任意
	ImapPassword   string   `json:"imap_password"`    // 可选固定密码，留空=任意
	Users          []string `json:"users"`            // 兼容旧配置，新架构忽略

	// 认证二选一：AdminEmail+AdminPassword（自动续 token）或 PublicToken
	AdminEmail    string `json:"admin_email"`
	AdminPassword string `json:"admin_password"`
	PublicToken   string `json:"public_token"`

	// 兼容旧配置，运行时忽略
	LoginPassword string `json:"login_password"`

	JWTCacheTTL  int `json:"jwt_cache_ttl"` // JWT 缓存小时数
	SyncInterval int `json:"sync_interval"` // 同一邮箱两次同步最小间隔（分钟）
}

func defaultConfig() *Config {
	return &Config{
		BaseURL:        "https://example.com",
		ListenAddr:     "0.0.0.0:993",
		SMTPListenAddr: "0.0.0.0:465",
		Domain:         "example.com",
		PollInterval:   60,
		PageSize:       100,
		MaxPages:       0,
		InsecureAuth:   true,
		TLSMode:        "plain",
		JWTCacheTTL:    12,
		SyncInterval:   2,
	}
}

// applyEnv 用环境变量覆盖配置（环境变量优先于 config.json）。
func (c *Config) applyEnv() {
	setStr := func(dst *string, keys ...string) {
		for _, k := range keys {
			if v := os.Getenv(k); v != "" {
				*dst = v
				return
			}
		}
	}
	setInt := func(dst *int, key string) {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	setBool := func(dst *bool, key string) {
		if v := os.Getenv(key); v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				*dst = b
			}
		}
	}

	setStr(&c.BaseURL, "BASE_URL")
	setStr(&c.Domain, "DOMAIN")
	setStr(&c.ListenAddr, "IMAP_ADDR", "LISTEN_ADDR")
	setStr(&c.SMTPListenAddr, "SMTP_ADDR", "SMTP_LISTEN_ADDR")
	setStr(&c.PublicToken, "PUBLIC_TOKEN")
	setStr(&c.AdminEmail, "ADMIN_EMAIL")
	setStr(&c.AdminPassword, "ADMIN_PASSWORD")
	setStr(&c.TLSMode, "TLS_MODE")
	setStr(&c.CertFile, "CERT_FILE", "TLS_CERT")
	setStr(&c.KeyFile, "KEY_FILE", "TLS_KEY")
	setInt(&c.PollInterval, "POLL_INTERVAL")
	setInt(&c.SyncInterval, "SYNC_INTERVAL")
	setInt(&c.PageSize, "PAGE_SIZE")
	setInt(&c.MaxPages, "MAX_PAGES")
	setInt(&c.JWTCacheTTL, "JWT_CACHE_TTL")
	setBool(&c.InsecureAuth, "INSECURE_AUTH")
}

func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()
	if path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			if err := json.Unmarshal(b, cfg); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
		} else if path != "config.json" {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		// path=="config.json" 且不存在时，用默认值+环境变量继续
	}
	cfg.applyEnv()
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://example.com"
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 100
	}
	return cfg, nil
}
