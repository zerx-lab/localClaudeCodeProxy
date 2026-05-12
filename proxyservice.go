// proxyservice.go 是暴露给前端的 Wails service。
//
// 前端通过自动生成的 bindings 调用 ProxyService.Start / Stop / Status / Account 等方法，
// service 内部按 provider 把请求转发到 internal/chatgptproxy 或 internal/ccproxy 的实际实现。
//
// 设计取舍：
//   - 不让前端直接持有具体 proxy Server / CredentialManager 引用；
//     所有跨进程方法都返回 plain struct（值类型），避免 binding 生成器对接口/指针报警。
//   - Start 把日志通过 Wails 事件 "proxy:log" 发到前端，让 UI 能展示实时活动。
package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"localclaudecodeproxy/internal/autostart"
	"localclaudecodeproxy/internal/ccproxy"
	"localclaudecodeproxy/internal/chatgptproxy"
	"localclaudecodeproxy/internal/settings"
)

// autostartName 是开机启动项注册时使用的标识符（Windows 注册表值名 / macOS plist label 后缀）。
const autostartName = "localClaudeCodeProxy"

const (
	ProviderChatGPT = "chatgpt"
	ProviderClaude  = "claude"
)

// ProxyStatus 是 Status() 的返回结构，对应前端可读字段。
type ProxyStatus struct {
	Running      bool   `json:"running"`
	Addr         string `json:"addr"`
	Port         int    `json:"port"`
	Provider     string `json:"provider"`
	ProviderName string `json:"providerName"`
}

// LogEntry 是发送到前端的日志事件 payload。
type LogEntry struct {
	Time    string         `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// SettingsView 是发给前端的设置快照。
//
// 在 Settings 基础上多带平台/路径只读字段，方便前端自适应（比如不支持开机启动的平台隐藏开关）。
type SettingsView struct {
	AutoStartProxy        bool   `json:"autoStartProxy"`
	HideOnClose           bool   `json:"hideOnClose"`
	LaunchOnBoot          bool   `json:"launchOnBoot"`
	LastProvider          string `json:"lastProvider"`
	LastHost              string `json:"lastHost"`
	LastPort              int    `json:"lastPort"`
	LaunchOnBootSupported bool   `json:"launchOnBootSupported"`
	ConfigPath            string `json:"configPath"`
}

// SettingsInput 是前端调 UpdateSettings 时传过来的可写字段（不含只读元数据）。
type SettingsInput struct {
	AutoStartProxy bool   `json:"autoStartProxy"`
	HideOnClose    bool   `json:"hideOnClose"`
	LaunchOnBoot   bool   `json:"launchOnBoot"`
	LastProvider   string `json:"lastProvider"`
	LastHost       string `json:"lastHost"`
	LastPort       int    `json:"lastPort"`
}

// ProviderInfo 是 UI provider 切换器需要的静态描述。
type ProviderInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ShortName   string `json:"shortName"`
	Description string `json:"description"`
	Protocol    string `json:"protocol"`
}

// AccountView 是统一后的账户视图，避免前端依赖具体 internal 包类型。
type AccountView struct {
	Provider       string    `json:"provider"`
	ProviderName   string    `json:"providerName"`
	HasCredentials bool      `json:"hasCredentials"`
	AuthType       string    `json:"authType,omitempty"`
	Source         string    `json:"source,omitempty"`
	SourceLabel    string    `json:"sourceLabel,omitempty"`
	AccountID      string    `json:"accountId,omitempty"`
	Email          string    `json:"email,omitempty"`
	Subscription   string    `json:"subscription,omitempty"`
	ExpiresAt      time.Time `json:"expiresAt,omitzero"`
	Path           string    `json:"path,omitempty"`
}

// maxLogBuffer 是后端日志环形缓冲区的最大条目数。
// 前端可通过 GetLogs() 随时获取全量缓存，不依赖事件时序。
const maxLogBuffer = 500

// ProxyService 是 Wails 注册的服务对象，全局单例。
type ProxyService struct {
	mu             sync.Mutex
	app            *application.App
	claudeCreds    *ccproxy.CredentialManager
	claudeServer   *ccproxy.Server
	chatgptCreds   *chatgptproxy.CredentialManager
	chatgptServer  *chatgptproxy.Server
	settings       *settings.Store
	activeProvider string
	port           int // 上次启动的端口；0 表示尚未启动

	logMu  sync.RWMutex
	logBuf []LogEntry // 环形缓冲区，上限 maxLogBuffer 条
}

// NewProxyService 构造服务实例。app 引用用于发送前端事件。
//
// store 是已经 Load 过的配置存储；service 会在 Start 成功后把 LastHost/Port 写回去。
func NewProxyService(app *application.App, store *settings.Store) *ProxyService {
	claudeCreds := ccproxy.NewCredentialManager()
	chatgptCreds := chatgptproxy.NewCredentialManager()
	svc := &ProxyService{
		app:            app,
		claudeCreds:    claudeCreds,
		chatgptCreds:   chatgptCreds,
		settings:       store,
		activeProvider: normalizeProvider(store.Get().LastProvider),
	}
	svc.claudeServer = ccproxy.NewServer(claudeCreds, svc.emitLog)
	svc.chatgptServer = chatgptproxy.NewServer(chatgptCreds, svc.emitLog)
	return svc
}

// HideOnClose 让 main.go 的 WindowClosing 钩子根据用户偏好决定是否拦截关闭。
//
// 直接读 store，避免 main 包持有 store 引用。
func (s *ProxyService) HideOnClose() bool {
	return s.settings.Get().HideOnClose
}

// AutoStartProxy 暴露当前的"启动时自动启动代理"偏好，给 main.go 在
// ApplicationStarted 事件中决定是否触发一次 Start。
func (s *ProxyService) AutoStartProxy() bool {
	return s.settings.Get().AutoStartProxy
}

// LastEndpoint 返回上次启动时使用的 provider / host / port，供 AutoStartProxy 复用。
func (s *ProxyService) LastEndpoint() (string, string, int) {
	cur := s.settings.Get()
	return normalizeProvider(cur.LastProvider), cur.LastHost, cur.LastPort
}

// Providers 返回 UI 可选的代理提供方。
func (s *ProxyService) Providers() []ProviderInfo {
	return []ProviderInfo{
		{
			ID:          ProviderChatGPT,
			Name:        "ChatGPT 订阅",
			ShortName:   "ChatGPT",
			Description: "使用 ChatGPT Plus/Pro 或 Codex CLI OAuth，转发到 Codex Responses API。",
			Protocol:    "OpenAI /v1/responses",
		},
		{
			ID:          ProviderClaude,
			Name:        "Claude Code 订阅",
			ShortName:   "Claude",
			Description: "使用 Claude Code OAuth，转发到 Anthropic Messages API。",
			Protocol:    "Anthropic /v1/messages",
		},
	}
}

// emitLog 把日志同时存入环形缓冲区并通过 Wails 事件推送到前端。
//
// 前端订阅 "proxy:log" 事件可实时收到新条目；
// 也可调用 GetLogs() 随时拉取全量缓冲（用于刷新/重新打开日志面板）。
func (s *ProxyService) emitLog(level, msg string, kv map[string]any) {
	entry := LogEntry{
		Time:    time.Now().Format(time.RFC3339),
		Level:   level,
		Message: msg,
		Fields:  kv,
	}

	// 写入环形缓冲区
	s.logMu.Lock()
	s.logBuf = append(s.logBuf, entry)
	if len(s.logBuf) > maxLogBuffer {
		s.logBuf = s.logBuf[len(s.logBuf)-maxLogBuffer:]
	}
	s.logMu.Unlock()

	if s.app != nil {
		s.app.Event.EmitEvent(&application.CustomEvent{
			Name: "proxy:log",
			Data: entry,
		})
	}
}

// GetLogs 返回后端当前保存的全部日志条目（最多 maxLogBuffer 条）。
//
// 前端在刷新、展开日志面板或重连时调用，可恢复在订阅 proxy:log 事件前已产生的历史日志。
func (s *ProxyService) GetLogs() []LogEntry {
	s.logMu.RLock()
	defer s.logMu.RUnlock()
	result := make([]LogEntry, len(s.logBuf))
	copy(result, s.logBuf)
	return result
}

// ClearLogs 清空后端日志缓冲区（对应前端的「清空」按钮）。
func (s *ProxyService) ClearLogs() {
	s.logMu.Lock()
	s.logBuf = s.logBuf[:0]
	s.logMu.Unlock()
}

// Start 启动代理监听。port 传 0 时由系统分配空闲端口（前端拿 Status().Port 显示）。
//
// host 默认 "127.0.0.1"，仅本机访问；前端如果允许暴露到局域网，可传 "0.0.0.0"。
func (s *ProxyService) Start(provider string, host string, port int) (ProxyStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	provider = normalizeProvider(provider)
	if s.anyServerRunningLocked() {
		return s.statusLocked(), nil
	}

	if port == 0 {
		port = 8765 // 默认端口
	}

	switch provider {
	case ProviderChatGPT:
		if err := s.chatgptServer.Start(chatgptproxy.Config{
			Host: host,
			Port: port,
		}); err != nil {
			return ProxyStatus{}, fmt.Errorf("start chatgpt proxy: %w", err)
		}
	case ProviderClaude:
		if err := s.claudeServer.Start(ccproxy.Config{
			Host: host,
			Port: port,
		}); err != nil {
			return ProxyStatus{}, fmt.Errorf("start claude proxy: %w", err)
		}
	default:
		return ProxyStatus{}, fmt.Errorf("未知 provider: %s", provider)
	}
	s.activeProvider = provider
	s.port = port

	// 持久化"上次启动参数"，下次 AutoStartProxy 时复用。
	// 写盘失败不影响 Start 成功语义，仅打日志。
	if err := s.settings.Patch(func(st *settings.Settings) {
		st.LastProvider = provider
		st.LastHost = host
		st.LastPort = port
	}); err != nil {
		s.emitLog("warn", "save settings failed", map[string]any{"error": err.Error()})
	}
	return s.statusLocked(), nil
}

// Stop 停止监听；未运行时是 no-op。
func (s *ProxyService) Stop() (ProxyStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.anyServerRunningLocked() {
		return s.statusLocked(), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.chatgptServer.IsRunning() {
		if err := s.chatgptServer.Stop(ctx); err != nil {
			return ProxyStatus{}, fmt.Errorf("stop chatgpt proxy: %w", err)
		}
	}
	if s.claudeServer.IsRunning() {
		if err := s.claudeServer.Stop(ctx); err != nil {
			return ProxyStatus{}, fmt.Errorf("stop claude proxy: %w", err)
		}
	}
	return s.statusLocked(), nil
}

// Status 返回当前代理运行状态。
func (s *ProxyService) Status() ProxyStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

// statusLocked 必须在持有 s.mu 时调用。
func (s *ProxyService) statusLocked() ProxyStatus {
	if s.chatgptServer.IsRunning() {
		return ProxyStatus{
			Running:      true,
			Addr:         s.chatgptServer.ListenAddr(),
			Port:         s.port,
			Provider:     ProviderChatGPT,
			ProviderName: providerLabel(ProviderChatGPT),
		}
	}
	if s.claudeServer.IsRunning() {
		return ProxyStatus{
			Running:      true,
			Addr:         s.claudeServer.ListenAddr(),
			Port:         s.port,
			Provider:     ProviderClaude,
			ProviderName: providerLabel(ProviderClaude),
		}
	}
	provider := s.activeProvider
	if provider == "" {
		provider = normalizeProvider(s.settings.Get().LastProvider)
	}
	return ProxyStatus{
		Running:      false,
		Addr:         "",
		Port:         s.port,
		Provider:     provider,
		ProviderName: providerLabel(provider),
	}
}

func (s *ProxyService) anyServerRunningLocked() bool {
	return s.chatgptServer.IsRunning() || s.claudeServer.IsRunning()
}

// Account 返回指定 provider 的去敏账户信息。
//
// 前端用这个判断"凭证文件是否存在"以及展示"还有多少时间过期"。
func (s *ProxyService) Account(provider string) AccountView {
	provider = normalizeProvider(provider)
	switch provider {
	case ProviderChatGPT:
		return chatGPTAccountToView(s.chatgptCreds.AccountInfo())
	case ProviderClaude:
		return claudeAccountToView(s.claudeCreds.AccountInfo())
	default:
		return AccountView{Provider: provider, ProviderName: providerLabel(provider)}
	}
}

// Accounts 返回全部 provider 的账户状态。
func (s *ProxyService) Accounts() []AccountView {
	return []AccountView{
		s.Account(ProviderChatGPT),
		s.Account(ProviderClaude),
	}
}

// RefreshAccount 强制清掉凭证缓存并触发一次刷新（用于 UI 上的 "Refresh" 按钮）。
//
// 返回的 AccountInfo 是刷新后的最新状态。
func (s *ProxyService) RefreshAccount(provider string) (AccountView, error) {
	provider = normalizeProvider(provider)
	switch provider {
	case ProviderChatGPT:
		s.chatgptCreds.Invalidate()
		if _, err := s.chatgptCreds.GetAuth(); err != nil {
			return s.Account(provider), err
		}
	case ProviderClaude:
		s.claudeCreds.Invalidate()
		if _, err := s.claudeCreds.GetAccessToken(); err != nil {
			return s.Account(provider), err
		}
	default:
		return s.Account(provider), fmt.Errorf("未知 provider: %s", provider)
	}
	return s.Account(provider), nil
}

// LoginChatGPT 通过浏览器完成 OpenAI OAuth 登录，并把 token 保存到本应用配置目录。
func (s *ProxyService) LoginChatGPT() (chatgptproxy.LoginResult, error) {
	return s.chatgptCreds.LoginWithBrowser(context.Background())
}

// shutdown 进程退出前调用，确保监听 socket 关闭。
func (s *ProxyService) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.chatgptServer.Stop(ctx)
	_ = s.claudeServer.Stop(ctx)
}

// GetSettings 返回当前用户偏好（含平台只读元数据）。
func (s *ProxyService) GetSettings() SettingsView {
	cur := s.settings.Get()
	// 实时校对 LaunchOnBoot：如果磁盘 settings 说 true 但实际 autostart 已被外部清除，
	// 修正为 false 让 UI 显示真实状态。读失败时保留 cur 值。
	enabled, err := autostart.IsEnabled(autostartName)
	if err == nil {
		cur.LaunchOnBoot = enabled
	}
	return SettingsView{
		AutoStartProxy:        cur.AutoStartProxy,
		HideOnClose:           cur.HideOnClose,
		LaunchOnBoot:          cur.LaunchOnBoot,
		LastProvider:          normalizeProvider(cur.LastProvider),
		LastHost:              cur.LastHost,
		LastPort:              cur.LastPort,
		LaunchOnBootSupported: autostart.Supported(),
		ConfigPath:            s.settings.Path(),
	}
}

// UpdateSettings 用前端传入的偏好覆盖磁盘配置。
//
// LaunchOnBoot 字段会触发实际的开机启动注册/取消，操作失败时返回错误且不写入新值。
func (s *ProxyService) UpdateSettings(input SettingsInput) (SettingsView, error) {
	old := s.settings.Get()

	// 1) 处理 LaunchOnBoot 副作用：先操作 OS，再写盘，避免磁盘和实际状态不一致。
	if input.LaunchOnBoot != old.LaunchOnBoot {
		if input.LaunchOnBoot {
			if !autostart.Supported() {
				return s.GetSettings(), fmt.Errorf("当前平台不支持开机启动")
			}
			exe, err := os.Executable()
			if err != nil {
				return s.GetSettings(), fmt.Errorf("read executable path: %w", err)
			}
			if err := autostart.Enable(autostartName, exe); err != nil {
				return s.GetSettings(), fmt.Errorf("enable autostart: %w", err)
			}
		} else {
			if err := autostart.Disable(autostartName); err != nil {
				return s.GetSettings(), fmt.Errorf("disable autostart: %w", err)
			}
		}
	}

	// 2) 写盘
	next := settings.Settings{
		LastProvider:   normalizeProvider(input.LastProvider),
		AutoStartProxy: input.AutoStartProxy,
		HideOnClose:    input.HideOnClose,
		LaunchOnBoot:   input.LaunchOnBoot,
		LastHost:       input.LastHost,
		LastPort:       input.LastPort,
	}
	if err := s.settings.Update(next); err != nil {
		return s.GetSettings(), fmt.Errorf("save settings: %w", err)
	}
	return s.GetSettings(), nil
}

func normalizeProvider(provider string) string {
	switch provider {
	case ProviderClaude:
		return ProviderClaude
	case ProviderChatGPT, "":
		return ProviderChatGPT
	default:
		return ProviderChatGPT
	}
}

func providerLabel(provider string) string {
	switch normalizeProvider(provider) {
	case ProviderClaude:
		return "Claude Code 订阅"
	default:
		return "ChatGPT 订阅"
	}
}

func chatGPTAccountToView(info chatgptproxy.AccountInfo) AccountView {
	return AccountView{
		Provider:       ProviderChatGPT,
		ProviderName:   providerLabel(ProviderChatGPT),
		HasCredentials: info.HasCredentials,
		AuthType:       info.AuthType,
		Source:         info.Source,
		SourceLabel:    info.SourceLabel,
		AccountID:      info.AccountID,
		Email:          info.Email,
		ExpiresAt:      info.ExpiresAt,
		Path:           info.Path,
	}
}

func claudeAccountToView(info ccproxy.AccountInfo) AccountView {
	return AccountView{
		Provider:       ProviderClaude,
		ProviderName:   providerLabel(ProviderClaude),
		HasCredentials: info.HasCredentials,
		AuthType:       "oauth",
		Source:         "claude_cli",
		SourceLabel:    "Claude Code",
		Subscription:   info.SubscriptionType,
		ExpiresAt:      info.ExpiresAt,
		Path:           info.Path,
	}
}
