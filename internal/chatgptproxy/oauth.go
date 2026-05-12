package chatgptproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pkg/browser"
)

const (
	oauthPort          = 1455
	oauthCallbackPath  = "/auth/callback"
	oauthCallbackURL   = "http://localhost:1455/auth/callback"
	oauthLoginTimeout  = 5 * time.Minute
	oauthDeviceBaseURL = issuerURL + "/codex/device"
)

type pkceCodes struct {
	Verifier  string
	Challenge string
}

// LoginResult 是 UI 触发浏览器 OAuth 登录后的返回值。
type LoginResult struct {
	Success   bool        `json:"success"`
	Account   AccountInfo `json:"account"`
	LoginURL  string      `json:"loginUrl,omitempty"`
	Message   string      `json:"message,omitempty"`
	ExpiresAt time.Time   `json:"expiresAt,omitzero"`
}

func generatePKCE() (pkceCodes, error) {
	verifier, err := randomOAuthString(43)
	if err != nil {
		return pkceCodes{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	return pkceCodes{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

func randomOAuthString(length int) (string, error) {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = chars[int(b)%len(chars)]
	}
	return string(buf), nil
}

func buildAuthorizeURL(pkce pkceCodes, state string) string {
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", oauthClientID)
	params.Set("redirect_uri", oauthCallbackURL)
	params.Set("scope", "openid profile email offline_access")
	params.Set("code_challenge", pkce.Challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")
	params.Set("state", state)
	params.Set("originator", "opencode")
	return issuerURL + "/oauth/authorize?" + params.Encode()
}

func (m *CredentialManager) exchangeCodeForTokens(ctx context.Context, code string, pkce pkceCodes) (TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", oauthCallbackURL)
	form.Set("client_id", oauthClientID)
	form.Set("code_verifier", pkce.Verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return TokenResponse{}, fmt.Errorf("exchange chatgpt oauth code: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return TokenResponse{}, fmt.Errorf("exchange chatgpt oauth code: status %d", resp.StatusCode)
	}
	var tokens TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return TokenResponse{}, fmt.Errorf("decode chatgpt oauth response: %w", err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return TokenResponse{}, fmt.Errorf("chatgpt oauth response missing access_token/refresh_token")
	}
	return tokens, nil
}

// LoginWithBrowser 启动本地 OAuth 回调服务，打开浏览器并等待登录完成。
func (m *CredentialManager) LoginWithBrowser(ctx context.Context) (LoginResult, error) {
	ctx, cancel := context.WithTimeout(ctx, oauthLoginTimeout)
	defer cancel()

	pkce, err := generatePKCE()
	if err != nil {
		return LoginResult{}, fmt.Errorf("generate pkce: %w", err)
	}
	state, err := randomOAuthString(43)
	if err != nil {
		return LoginResult{}, fmt.Errorf("generate oauth state: %w", err)
	}
	authURL := buildAuthorizeURL(pkce, state)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", oauthPort))
	if err != nil {
		return LoginResult{}, fmt.Errorf("listen oauth callback port %d: %w", oauthPort, err)
	}

	resultCh := make(chan TokenResponse, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	server := &http.Server{Handler: mux}

	mux.HandleFunc(oauthCallbackPath, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if oauthErr := query.Get("error"); oauthErr != "" {
			msg := query.Get("error_description")
			if msg == "" {
				msg = oauthErr
			}
			errCh <- fmt.Errorf("%s", msg)
			writeOAuthHTML(w, false, msg)
			return
		}
		code := query.Get("code")
		if code == "" {
			errCh <- fmt.Errorf("OAuth 回调缺少 code")
			writeOAuthHTML(w, false, "OAuth 回调缺少 code")
			return
		}
		if query.Get("state") != state {
			errCh <- fmt.Errorf("OAuth state 不匹配")
			writeOAuthHTML(w, false, "OAuth state 不匹配")
			return
		}
		tokens, err := m.exchangeCodeForTokens(r.Context(), code, pkce)
		if err != nil {
			errCh <- err
			writeOAuthHTML(w, false, err.Error())
			return
		}
		resultCh <- tokens
		writeOAuthHTML(w, true, "")
	})

	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	defer server.Shutdown(context.Background())

	if err := browser.OpenURL(authURL); err != nil {
		return LoginResult{LoginURL: authURL}, fmt.Errorf("open browser: %w", err)
	}

	select {
	case tokens := <-resultCh:
		if err := SaveLocalAuth(tokens); err != nil {
			return LoginResult{}, err
		}
		m.Invalidate()
		account := m.AccountInfo()
		return LoginResult{
			Success:   true,
			Account:   account,
			LoginURL:  authURL,
			Message:   "登录成功",
			ExpiresAt: account.ExpiresAt,
		}, nil
	case err := <-errCh:
		return LoginResult{LoginURL: authURL}, err
	case <-ctx.Done():
		return LoginResult{LoginURL: authURL}, fmt.Errorf("等待 ChatGPT OAuth 登录超时")
	}
}

func writeOAuthHTML(w http.ResponseWriter, ok bool, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	title := "登录成功"
	body := "你可以关闭此窗口并返回 localClaudeCodeProxy。"
	color := "#31d07d"
	if !ok {
		title = "登录失败"
		body = html.EscapeString(message)
		color = "#ff6b5c"
	}
	fmt.Fprintf(w, `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <title>%s</title>
  <style>
    body{margin:0;height:100vh;display:grid;place-items:center;background:#10131a;color:#f4f6fb;font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
    main{max-width:520px;padding:32px;text-align:center}
    h1{margin:0 0 12px;color:%s;font-size:28px}
    p{margin:0;color:#aeb7c8;line-height:1.7}
  </style>
</head>
<body>
  <main>
    <h1>%s</h1>
    <p>%s</p>
  </main>
  <script>setTimeout(function(){ window.close() }, 1800)</script>
</body>
</html>`, title, color, title, body)
}

// DeviceAuthorizeURL 暴露设备码登录地址，当前 UI 未使用，保留给后续无浏览器环境。
func DeviceAuthorizeURL() string {
	return oauthDeviceBaseURL
}
