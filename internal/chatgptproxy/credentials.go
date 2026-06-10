// Package chatgptproxy 提供 ChatGPT Plus/Pro 订阅转 OpenAI 兼容 API 的核心能力。
//
// 凭证来源按优先级排列：
//   - 本应用自己的 OAuth 文件：用户通过 UI 登录后写入
//   - Codex CLI 的 %USERPROFILE%\.codex\auth.json
//   - OpenCode 的 auth.json（兼容 opencode 的 openai OAuth 结构）
//
// OAuth 协议细节来自本机 opencode 的 packages/opencode/src/plugin/codex.ts。
package chatgptproxy

import (
	"encoding/base64"
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

const (
	issuerURL           = "https://auth.openai.com"
	oauthTokenURL       = issuerURL + "/oauth/token"
	oauthClientID       = "app_EMoamEEZ73f0CkXaXp7hrann"
	credCacheTTL        = 30 * time.Second
	earlyExpireGap      = 60 * time.Second
	defaultExpiresInSec = 3600
)

const (
	AuthKindOAuth = "oauth"
	AuthKindAPI   = "api_key"
)

type credentialSource string

const (
	sourceLocal    credentialSource = "local"
	sourceCodexCLI credentialSource = "codex_cli"
	sourceOpenCode credentialSource = "opencode"
)

// Credentials 是内存中的去结构化凭证。敏感字段不直接暴露给前端。
type Credentials struct {
	Kind         string
	AccessToken  string
	RefreshToken string
	IDToken      string
	AccountID    string
	Email        string
	ExpiresAt    int64 // 毫秒时间戳；API Key 模式为 0
	Source       credentialSource
	SourcePath   string
}

// AuthInfo 是代理发请求时需要的最小认证信息。
type AuthInfo struct {
	Kind        string
	AccessToken string
	AccountID   string
	Source      string
	SourcePath  string
	ExpiresAt   int64
}

// AccountInfo 是给 UI 展示的去敏账户状态。
type AccountInfo struct {
	HasCredentials bool      `json:"hasCredentials"`
	AuthType       string    `json:"authType,omitempty"`
	Source         string    `json:"source,omitempty"`
	SourceLabel    string    `json:"sourceLabel,omitempty"`
	AccountID      string    `json:"accountId,omitempty"`
	Email          string    `json:"email,omitempty"`
	ExpiresAt      time.Time `json:"expiresAt,omitzero"`
	Path           string    `json:"path,omitempty"`
}

// CredentialManager 管理 ChatGPT/Codex OAuth 凭证读取、刷新和写回。
type CredentialManager struct {
	httpClient *http.Client

	mu       sync.Mutex
	cached   *Credentials
	cachedAt time.Time
}

// NewCredentialManager 构造默认凭证管理器。
func NewCredentialManager() *CredentialManager {
	return &CredentialManager{
		httpClient: netproxy.NewClient(20 * time.Second),
	}
}

// LocalAuthPath 返回本应用保存 ChatGPT OAuth 凭证的位置。
func LocalAuthPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("APPDATA"), "localClaudeCodeProxy")
	} else {
		dir = filepath.Join(dir, "localClaudeCodeProxy")
	}
	return filepath.Join(dir, "chatgpt_auth.json")
}

// CodexAuthPath 返回 Codex CLI 的默认 auth.json 路径。
func CodexAuthPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("USERPROFILE")
	}
	return filepath.Join(home, ".codex", "auth.json")
}

func openCodeAuthPaths() []string {
	home, _ := os.UserHomeDir()
	paths := []string{}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		paths = append(paths, filepath.Join(local, "opencode", "auth.json"))
	}
	if roaming := os.Getenv("APPDATA"); roaming != "" {
		paths = append(paths, filepath.Join(roaming, "opencode", "auth.json"))
	}
	if home != "" {
		paths = append(paths, filepath.Join(home, ".local", "share", "opencode", "auth.json"))
	}
	return paths
}

// readFromDisk 按优先级读取可用凭证。
func readFromDisk() (*Credentials, error) {
	if creds, err := readLocalAuth(LocalAuthPath()); err == nil {
		return creds, nil
	}
	if creds, err := readCodexAuth(CodexAuthPath()); err == nil {
		return creds, nil
	}
	for _, path := range openCodeAuthPaths() {
		if creds, err := readOpenCodeAuth(path); err == nil {
			return creds, nil
		}
	}
	return nil, fmt.Errorf("未找到 ChatGPT/Codex OAuth 凭证；已检查 %s、%s 和 OpenCode auth.json", LocalAuthPath(), CodexAuthPath())
}

func readLocalAuth(path string) (*Credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Type         string `json:"type"`
		AccessToken  string `json:"access"`
		RefreshToken string `json:"refresh"`
		IDToken      string `json:"idToken"`
		Expires      int64  `json:"expires"`
		AccountID    string `json:"accountId"`
		Email        string `json:"email"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Type != AuthKindOAuth || doc.AccessToken == "" || doc.RefreshToken == "" {
		return nil, errors.New("local chatgpt auth missing oauth tokens")
	}
	accountID, email, expires := enrichFromTokens(doc.IDToken, doc.AccessToken, doc.AccountID, doc.Email, doc.Expires)
	return &Credentials{
		Kind:         AuthKindOAuth,
		AccessToken:  doc.AccessToken,
		RefreshToken: doc.RefreshToken,
		IDToken:      doc.IDToken,
		AccountID:    accountID,
		Email:        email,
		ExpiresAt:    expires,
		Source:       sourceLocal,
		SourcePath:   path,
	}, nil
}

func readCodexAuth(path string) (*Credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		AuthMode     string `json:"auth_mode"`
		OpenAIAPIKey string `json:"OPENAI_API_KEY"`
		LastRefresh  string `json:"last_refresh"`
		Tokens       struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Tokens.AccessToken != "" && doc.Tokens.RefreshToken != "" {
		expiresHint := int64(0)
		if t, err := time.Parse(time.RFC3339Nano, doc.LastRefresh); err == nil {
			expiresHint = t.Add(defaultExpiresInSec * time.Second).UnixMilli()
		}
		accountID, email, expires := enrichFromTokens(doc.Tokens.IDToken, doc.Tokens.AccessToken, doc.Tokens.AccountID, "", expiresHint)
		return &Credentials{
			Kind:         AuthKindOAuth,
			AccessToken:  doc.Tokens.AccessToken,
			RefreshToken: doc.Tokens.RefreshToken,
			IDToken:      doc.Tokens.IDToken,
			AccountID:    accountID,
			Email:        email,
			ExpiresAt:    expires,
			Source:       sourceCodexCLI,
			SourcePath:   path,
		}, nil
	}
	if strings.TrimSpace(doc.OpenAIAPIKey) != "" {
		return &Credentials{
			Kind:        AuthKindAPI,
			AccessToken: strings.TrimSpace(doc.OpenAIAPIKey),
			Source:      sourceCodexCLI,
			SourcePath:  path,
		}, nil
	}
	return nil, errors.New("codex auth missing tokens or api key")
}

func readOpenCodeAuth(path string) (*Credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	rawOpenAI, ok := doc["openai"]
	if !ok {
		return nil, errors.New("opencode auth missing openai")
	}
	var openai struct {
		Type      string `json:"type"`
		Access    string `json:"access"`
		Refresh   string `json:"refresh"`
		Expires   int64  `json:"expires"`
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(rawOpenAI, &openai); err != nil {
		return nil, err
	}
	if openai.Type != AuthKindOAuth || openai.Access == "" || openai.Refresh == "" {
		return nil, errors.New("opencode openai auth is not oauth")
	}
	accountID, email, expires := enrichFromTokens("", openai.Access, openai.AccountID, "", openai.Expires)
	return &Credentials{
		Kind:         AuthKindOAuth,
		AccessToken:  openai.Access,
		RefreshToken: openai.Refresh,
		AccountID:    accountID,
		Email:        email,
		ExpiresAt:    expires,
		Source:       sourceOpenCode,
		SourcePath:   path,
	}, nil
}

// SaveLocalAuth 保存 UI 登录得到的 OAuth token。
func SaveLocalAuth(tokens TokenResponse) error {
	accountID, email, expires := tokenResponseAccount(tokens)
	doc := map[string]any{
		"type":      AuthKindOAuth,
		"access":    tokens.AccessToken,
		"refresh":   tokens.RefreshToken,
		"idToken":   tokens.IDToken,
		"expires":   expires,
		"accountId": accountID,
		"email":     email,
	}
	path := LocalAuthPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir chatgpt auth dir: %w", err)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal chatgpt auth: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write chatgpt auth: %w", err)
	}
	return nil
}

func writeToSource(creds *Credentials) error {
	switch creds.Source {
	case sourceLocal:
		return writeLocal(creds)
	case sourceCodexCLI:
		return writeCodex(creds)
	case sourceOpenCode:
		return writeOpenCode(creds)
	default:
		return nil
	}
}

func writeLocal(creds *Credentials) error {
	doc := map[string]any{
		"type":      AuthKindOAuth,
		"access":    creds.AccessToken,
		"refresh":   creds.RefreshToken,
		"idToken":   creds.IDToken,
		"expires":   creds.ExpiresAt,
		"accountId": creds.AccountID,
		"email":     creds.Email,
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(creds.SourcePath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(creds.SourcePath, out, 0o600)
}

func writeCodex(creds *Credentials) error {
	var doc map[string]any
	if raw, err := os.ReadFile(creds.SourcePath); err == nil {
		_ = json.Unmarshal(raw, &doc)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	tokens, _ := doc["tokens"].(map[string]any)
	if tokens == nil {
		tokens = map[string]any{}
	}
	tokens["access_token"] = creds.AccessToken
	tokens["refresh_token"] = creds.RefreshToken
	if creds.IDToken != "" {
		tokens["id_token"] = creds.IDToken
	}
	if creds.AccountID != "" {
		tokens["account_id"] = creds.AccountID
	}
	doc["auth_mode"] = "chatgpt"
	doc["tokens"] = tokens
	doc["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(creds.SourcePath, out, 0o600)
}

func writeOpenCode(creds *Credentials) error {
	var doc map[string]any
	if raw, err := os.ReadFile(creds.SourcePath); err == nil {
		_ = json.Unmarshal(raw, &doc)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	openai, _ := doc["openai"].(map[string]any)
	if openai == nil {
		openai = map[string]any{}
	}
	openai["type"] = AuthKindOAuth
	openai["access"] = creds.AccessToken
	openai["refresh"] = creds.RefreshToken
	openai["expires"] = creds.ExpiresAt
	if creds.AccountID != "" {
		openai["accountId"] = creds.AccountID
	}
	doc["openai"] = openai
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(creds.SourcePath, out, 0o600)
}

func (c *Credentials) isExpired() bool {
	if c.Kind == AuthKindAPI {
		return false
	}
	if c.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(earlyExpireGap).After(time.UnixMilli(c.ExpiresAt))
}

// TokenResponse 是 OpenAI OAuth token 端点的响应结构。
type TokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (m *CredentialManager) refresh(refreshToken string) (*Credentials, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", oauthClientID)

	req, err := http.NewRequest(http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh chatgpt oauth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh chatgpt oauth: status %d", resp.StatusCode)
	}

	var tokens TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return nil, fmt.Errorf("decode chatgpt refresh response: %w", err)
	}
	if tokens.AccessToken == "" {
		return nil, errors.New("refresh response missing access_token")
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = refreshToken
	}
	accountID, email, expires := tokenResponseAccount(tokens)
	return &Credentials{
		Kind:         AuthKindOAuth,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		IDToken:      tokens.IDToken,
		AccountID:    accountID,
		Email:        email,
		ExpiresAt:    expires,
	}, nil
}

func tokenResponseAccount(tokens TokenResponse) (string, string, int64) {
	expiresIn := tokens.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = defaultExpiresInSec
	}
	expires := time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
	return enrichFromTokens(tokens.IDToken, tokens.AccessToken, "", "", expires)
}

func enrichFromTokens(idToken, accessToken, accountHint, emailHint string, expiresHint int64) (string, string, int64) {
	accountID := accountHint
	email := emailHint
	expires := expiresHint
	for _, token := range []string{idToken, accessToken} {
		claims := parseJWTClaims(token)
		if claims == nil {
			continue
		}
		if accountID == "" {
			accountID = extractAccountID(claims)
		}
		if email == "" {
			if v, ok := claims["email"].(string); ok {
				email = v
			}
		}
		if exp, ok := numericClaim(claims["exp"]); ok {
			expMs := int64(exp) * 1000
			if expires == 0 || expMs < expires {
				expires = expMs
			}
		}
	}
	return accountID, email, expires
}

func parseJWTClaims(token string) map[string]any {
	if token == "" {
		return nil
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims
}

func extractAccountID(claims map[string]any) string {
	if v, ok := claims["chatgpt_account_id"].(string); ok && v != "" {
		return v
	}
	if nested, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if v, ok := nested["chatgpt_account_id"].(string); ok && v != "" {
			return v
		}
	}
	if orgs, ok := claims["organizations"].([]any); ok && len(orgs) > 0 {
		if first, ok := orgs[0].(map[string]any); ok {
			if v, ok := first["id"].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

func numericClaim(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func sourceLabel(source credentialSource) string {
	switch source {
	case sourceLocal:
		return "本应用 OAuth"
	case sourceCodexCLI:
		return "Codex CLI"
	case sourceOpenCode:
		return "OpenCode"
	default:
		return string(source)
	}
}

// GetAuth 返回当前可用的认证信息，必要时刷新 OAuth token。
func (m *CredentialManager) GetAuth() (AuthInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	creds, err := m.getCachedOrLoad()
	if err != nil {
		return AuthInfo{}, err
	}
	if creds.Kind == AuthKindOAuth && creds.isExpired() {
		refreshed, err := m.refresh(creds.RefreshToken)
		if err != nil {
			return AuthInfo{}, fmt.Errorf("chatgpt token expired and refresh failed: %w", err)
		}
		refreshed.Source = creds.Source
		refreshed.SourcePath = creds.SourcePath
		if refreshed.AccountID == "" {
			refreshed.AccountID = creds.AccountID
		}
		if refreshed.Email == "" {
			refreshed.Email = creds.Email
		}
		if refreshed.IDToken == "" {
			refreshed.IDToken = creds.IDToken
		}
		if err := writeToSource(refreshed); err != nil {
			fmt.Fprintf(os.Stderr, "chatgptproxy: warn: write refreshed credentials failed: %v\n", err)
		}
		creds = refreshed
		m.cached = refreshed
		m.cachedAt = time.Now()
	}
	return AuthInfo{
		Kind:        creds.Kind,
		AccessToken: creds.AccessToken,
		AccountID:   creds.AccountID,
		Source:      string(creds.Source),
		SourcePath:  creds.SourcePath,
		ExpiresAt:   creds.ExpiresAt,
	}, nil
}

func (m *CredentialManager) getCachedOrLoad() (*Credentials, error) {
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

// AccountInfo 返回去敏账户信息供前端展示。
func (m *CredentialManager) AccountInfo() AccountInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	info := AccountInfo{Path: LocalAuthPath()}
	creds, err := m.getCachedOrLoad()
	if err != nil {
		return info
	}
	info.HasCredentials = true
	info.AuthType = creds.Kind
	info.Source = string(creds.Source)
	info.SourceLabel = sourceLabel(creds.Source)
	info.AccountID = creds.AccountID
	info.Email = creds.Email
	info.Path = creds.SourcePath
	if creds.ExpiresAt > 0 {
		info.ExpiresAt = time.UnixMilli(creds.ExpiresAt)
	}
	return info
}

// Invalidate 清除内存缓存。
func (m *CredentialManager) Invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cached = nil
	m.cachedAt = time.Time{}
}
