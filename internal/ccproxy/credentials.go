// Package ccproxy 提供 Claude Code 订阅本地代理的核心逻辑。
//
// credentials.go 负责：
//   - 从 Windows 下 %USERPROFILE%\.claude\.credentials.json 读取 OAuth 凭证
//   - 通过 https://claude.ai/v1/oauth/token 刷新 access token
//   - 30s 内存缓存避免反复读盘
//   - 提前 60s 判定过期，避免边界情况下用着用着就 401
package ccproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"localclaudecodeproxy/internal/netproxy"
)

// Anthropic 用于 Claude Code 客户端的 OAuth 端点 / client_id（参考 opencode-claude-auth）。
const (
	oauthTokenURL  = "https://claude.ai/v1/oauth/token"
	oauthClientID  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	credCacheTTL   = 30 * time.Second
	earlyExpireGap = 60 * time.Second
)

// ClaudeCredentials 是凭证文件中 claudeAiOauth 字段的反序列化结果。
type ClaudeCredentials struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresAt        int64  `json:"expiresAt"` // 毫秒时间戳
	SubscriptionType string `json:"subscriptionType,omitempty"`
}

// AccountInfo 是用于前端展示的账户信息（去除敏感 token）。
type AccountInfo struct {
	HasCredentials   bool      `json:"hasCredentials"`
	SubscriptionType string    `json:"subscriptionType,omitempty"`
	ExpiresAt        time.Time `json:"expiresAt,omitzero"`
	Path             string    `json:"path,omitempty"`
}

// CredentialManager 包装凭证读取与刷新逻辑，并对外提供线程安全的 GetAccessToken。
type CredentialManager struct {
	httpClient *http.Client

	mu       sync.Mutex
	cached   *ClaudeCredentials
	cachedAt time.Time
}

// NewCredentialManager 构造默认的凭证管理器。
func NewCredentialManager() *CredentialManager {
	return &CredentialManager{
		httpClient: netproxy.NewClient(15 * time.Second),
	}
}

// CredentialsPath 返回 Windows 下默认的凭证文件路径。
//
// 我们刻意只支持 Windows 路径：项目的目标平台是 Wails3 桌面应用，
// macOS keychain 路径在 Windows 不可用，反之亦然——保持单一来源更可控。
func CredentialsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// 退化到环境变量；UserHomeDir 在 Windows 上几乎不会失败
		home = os.Getenv("USERPROFILE")
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

// readFromDisk 从凭证文件读取并解析 OAuth 凭证。
func readFromDisk() (*ClaudeCredentials, error) {
	path := CredentialsPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credentials %s: %w", path, err)
	}

	// 文件结构形如：{ "claudeAiOauth": { "accessToken": ..., ... } }
	var wrapper struct {
		ClaudeAiOauth *ClaudeCredentials `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, fmt.Errorf("parse credentials json: %w", err)
	}
	if wrapper.ClaudeAiOauth == nil ||
		wrapper.ClaudeAiOauth.AccessToken == "" ||
		wrapper.ClaudeAiOauth.RefreshToken == "" {
		return nil, errors.New("credentials file missing claudeAiOauth.accessToken/refreshToken")
	}
	return wrapper.ClaudeAiOauth, nil
}

// writeToDisk 将刷新后的凭证回写到文件，保留原 JSON 中的其他字段。
//
// 凭证刷新后必须回写：否则下次读取还是旧 token，刷新逻辑会被反复触发。
func writeToDisk(creds *ClaudeCredentials) error {
	path := CredentialsPath()

	// 读取整个原 JSON，仅替换 claudeAiOauth 字段，保留 scopes / rateLimitTier 等。
	var doc map[string]any
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &doc)
	}
	if doc == nil {
		doc = map[string]any{}
	}

	existing, _ := doc["claudeAiOauth"].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	existing["accessToken"] = creds.AccessToken
	existing["refreshToken"] = creds.RefreshToken
	existing["expiresAt"] = creds.ExpiresAt
	if creds.SubscriptionType != "" {
		existing["subscriptionType"] = creds.SubscriptionType
	}
	doc["claudeAiOauth"] = existing

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir credentials dir: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	return nil
}

// isExpired 判定凭证是否在 earlyExpireGap 内过期。
func (c *ClaudeCredentials) isExpired() bool {
	if c.ExpiresAt <= 0 {
		return true
	}
	expireAt := time.UnixMilli(c.ExpiresAt)
	return time.Now().Add(earlyExpireGap).After(expireAt)
}

// refresh 用 refresh_token 调用 OAuth 端点换取新 access_token。
func (m *CredentialManager) refresh(refreshToken string) (*ClaudeCredentials, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", oauthClientID)
	form.Set("refresh_token", refreshToken)

	req, err := http.NewRequest(http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh oauth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh oauth: status %d", resp.StatusCode)
	}

	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"` // 秒
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}
	if data.AccessToken == "" {
		return nil, errors.New("refresh response missing access_token")
	}

	expiresIn := data.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 36000 // 默认 10h，匹配观察值
	}

	newRefresh := data.RefreshToken
	if newRefresh == "" {
		newRefresh = refreshToken // 上游不返回则沿用旧值
	}

	return &ClaudeCredentials{
		AccessToken:  data.AccessToken,
		RefreshToken: newRefresh,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn)*time.Second).UnixMilli(),
	}, nil
}

// getCachedOrLoad 命中缓存返回，否则读盘。**不持锁**调用者需先持有 m.mu。
func (m *CredentialManager) getCachedOrLoad() (*ClaudeCredentials, error) {
	if m.cached != nil && time.Since(m.cachedAt) < credCacheTTL {
		return m.cached, nil
	}
	creds, err := readFromDisk()
	if err != nil {
		return nil, err
	}
	m.cached = creds
	m.cachedAt = time.Now()
	return creds, nil
}

// GetAccessToken 返回当前可用的 access token，必要时刷新并回写文件。
func (m *CredentialManager) GetAccessToken() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	creds, err := m.getCachedOrLoad()
	if err != nil {
		return "", err
	}
	if !creds.isExpired() {
		return creds.AccessToken, nil
	}

	// 过期或临近过期：刷新
	refreshed, err := m.refresh(creds.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("token expired and refresh failed: %w", err)
	}
	// 保留 subscriptionType 字段（OAuth 响应不带这个字段）
	refreshed.SubscriptionType = creds.SubscriptionType

	if err := writeToDisk(refreshed); err != nil {
		// 写盘失败不致命：本次内存里能用，只是下次进程启动还要再刷
		// 但我们记录到 stderr 让调用方知道
		fmt.Fprintf(os.Stderr, "ccproxy: warn: write refreshed credentials failed: %v\n", err)
	}
	m.cached = refreshed
	m.cachedAt = time.Now()
	return refreshed.AccessToken, nil
}

// AccountInfo 返回去敏后的账户信息供前端展示。
func (m *CredentialManager) AccountInfo() AccountInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	info := AccountInfo{Path: CredentialsPath()}
	creds, err := m.getCachedOrLoad()
	if err != nil {
		return info
	}
	info.HasCredentials = true
	info.SubscriptionType = creds.SubscriptionType
	info.ExpiresAt = time.UnixMilli(creds.ExpiresAt)
	return info
}

// Invalidate 清除内存缓存，下次取 token 时强制读盘 + 刷新。
//
// 用于：UI 端按下 "Refresh" 按钮、刷新失败后重试。
func (m *CredentialManager) Invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cached = nil
	m.cachedAt = time.Time{}
}
