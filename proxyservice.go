// proxyservice.go 是暴露给前端的 Wails service。
//
// 前端通过自动生成的 bindings 调用 ProxyService.Start / Stop / Status / Account 等方法，
// service 内部把请求转发到 internal/ccproxy 的实际实现。
//
// 设计取舍：
//   - 不让前端直接持有 *ccproxy.Server / *ccproxy.CredentialManager 引用；
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
	"localclaudecodeproxy/internal/settings"
)

// autostartName 是开机启动项注册时使用的标识符（Windows 注册表值名 / macOS plist label 后缀）。
const autostartName = "localClaudeCodeProxy"

// ProxyStatus 是 Status() 的返回结构，对应前端可读字段。
type ProxyStatus struct {
	Running bool   `json:"running"`
	Addr    string `json:"addr"`
	Port    int    `json:"port"`
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
	LastHost       string `json:"lastHost"`
	LastPort       int    `json:"lastPort"`
}

// maxLogBuffer 是后端日志环形缓冲区的最大条目数。
// 前端可通过 GetLogs() 随时获取全量缓存，不依赖事件时序。
const maxLogBuffer = 500

// ProxyService 是 Wails 注册的服务对象，全局单例。
type ProxyService struct {
	mu       sync.Mutex
	app      *application.App
	creds    *ccproxy.CredentialManager
	server   *ccproxy.Server
	settings *settings.Store
	port     int // 上次启动的端口；0 表示尚未启动

	logMu  sync.RWMutex
	logBuf []LogEntry // 环形缓冲区，上限 maxLogBuffer 条
}

// NewProxyService 构造服务实例。app 引用用于发送前端事件。
//
// store 是已经 Load 过的配置存储；service 会在 Start 成功后把 LastHost/Port 写回去。
func NewProxyService(app *application.App, store *settings.Store) *ProxyService {
	creds := ccproxy.NewCredentialManager()
	svc := &ProxyService{
		app:      app,
		creds:    creds,
		settings: store,
	}
	svc.server = ccproxy.NewServer(creds, svc.emitLog)
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

// LastEndpoint 返回上次启动时使用的 host / port，供 AutoStartProxy 复用。
func (s *ProxyService) LastEndpoint() (string, int) {
	cur := s.settings.Get()
	return cur.LastHost, cur.LastPort
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
func (s *ProxyService) Start(host string, port int) (ProxyStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.server.IsRunning() {
		return s.statusLocked(), nil
	}

	if port == 0 {
		port = 8765 // 默认端口
	}

	if err := s.server.Start(ccproxy.Config{
		Host: host,
		Port: port,
	}); err != nil {
		return ProxyStatus{}, fmt.Errorf("start proxy: %w", err)
	}
	s.port = port

	// 持久化"上次启动参数"，下次 AutoStartProxy 时复用。
	// 写盘失败不影响 Start 成功语义，仅打日志。
	if err := s.settings.Patch(func(st *settings.Settings) {
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

	if !s.server.IsRunning() {
		return s.statusLocked(), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.server.Stop(ctx); err != nil {
		return ProxyStatus{}, fmt.Errorf("stop proxy: %w", err)
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
	return ProxyStatus{
		Running: s.server.IsRunning(),
		Addr:    s.server.ListenAddr(),
		Port:    s.port,
	}
}

// Account 返回去敏后的账户信息（订阅类型 / 过期时间 / 文件路径）。
//
// 前端用这个判断"凭证文件是否存在"以及展示"还有多少时间过期"。
func (s *ProxyService) Account() ccproxy.AccountInfo {
	return s.creds.AccountInfo()
}

// RefreshAccount 强制清掉凭证缓存并触发一次刷新（用于 UI 上的 "Refresh" 按钮）。
//
// 返回的 AccountInfo 是刷新后的最新状态。
func (s *ProxyService) RefreshAccount() (ccproxy.AccountInfo, error) {
	s.creds.Invalidate()
	if _, err := s.creds.GetAccessToken(); err != nil {
		return s.creds.AccountInfo(), err
	}
	return s.creds.AccountInfo(), nil
}

// shutdown 进程退出前调用，确保监听 socket 关闭。
func (s *ProxyService) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.server.Stop(ctx)
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
