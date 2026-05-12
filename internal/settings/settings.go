// Package settings 负责持久化用户偏好。
//
// 配置文件位置（按平台）：
//
//	Windows:  %APPDATA%\localClaudeCodeProxy\config.json
//	macOS:    ~/Library/Application Support/localClaudeCodeProxy/config.json
//	Linux:    ~/.config/localClaudeCodeProxy/config.json
//
// 设计原则：
//   - 永远返回有效结构。文件不存在 / JSON 损坏时回退到默认值，不让 UI 崩。
//   - Save 时先写临时文件再 rename，避免半截写入污染原文件。
//   - 字段加新值时设计成"零值即默认"或在 Load 后用 applyDefaults 兜底，向后兼容老配置。
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// appDirName 是放在 user config dir 下的应用目录名。
const appDirName = "localClaudeCodeProxy"

// fileName 是配置文件名。
const fileName = "config.json"

// Settings 是持久化到磁盘的用户偏好。
//
// 注意：所有字段必须可被 zero-value 表示一个有意义的"默认"，否则 Load 老配置时会出现意外行为。
type Settings struct {
	// LastProvider 是上次选择的代理类型。当前支持：
	//   - chatgpt: ChatGPT Plus/Pro -> OpenAI/Codex API
	//   - claude:  Claude Code -> Anthropic API
	LastProvider string `json:"lastProvider"`

	// AutoStartProxy 为 true 时，应用启动后会自动用上次的 LastHost/LastPort 启动代理。
	AutoStartProxy bool `json:"autoStartProxy"`

	// HideOnClose 为 true 时，点窗口关闭按钮 = 隐藏到托盘；为 false 时 = 真退出。
	// 默认 true（沿袭"应用常驻托盘"语义）。
	HideOnClose bool `json:"hideOnClose"`

	// LaunchOnBoot 为 true 时，已通过 autostart 包注册了开机启动。
	// 这只是 UI 状态镜像，实际启动项以系统注册表/LaunchAgents 为准；
	// 切换该字段时 ProxyService 会同步调 autostart.Enable / Disable。
	LaunchOnBoot bool `json:"launchOnBoot"`

	// LastHost / LastPort 是上次成功 Start 时使用的监听参数，
	// 用于 AutoStartProxy 重启时复用。
	LastHost string `json:"lastHost"`
	LastPort int    `json:"lastPort"`
}

// Default 返回开箱即用的默认值。
func Default() Settings {
	return Settings{
		LastProvider:   "chatgpt",
		AutoStartProxy: false,
		HideOnClose:    true,
		LaunchOnBoot:   false,
		LastHost:       "127.0.0.1",
		LastPort:       8765,
	}
}

// applyDefaults 给从磁盘读出的配置补齐缺省值。
// 例：旧版本配置没有 LastPort 字段，反序列化后 LastPort=0，需要补成 8765。
func applyDefaults(s *Settings) {
	if s.LastProvider == "" {
		s.LastProvider = "chatgpt"
	}
	if s.LastHost == "" {
		s.LastHost = "127.0.0.1"
	}
	if s.LastPort <= 0 || s.LastPort > 65535 {
		s.LastPort = 8765
	}
}

// Store 是 Settings 的线程安全管理器，负责加载/保存到磁盘。
type Store struct {
	mu   sync.RWMutex
	path string
	data Settings
}

// NewStore 创建管理器并立即从磁盘加载。
//
// 即使加载失败也会返回一个可用的 Store（带默认值）和 error，
// 调用方可以选择忽略错误（首次启动很正常）或日志记录。
func NewStore() (*Store, error) {
	path, err := configPath()
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	s := &Store{
		path: path,
		data: Default(),
	}
	loadErr := s.load()
	return s, loadErr
}

// Path 返回配置文件的绝对路径，用于 UI 展示或诊断。
func (s *Store) Path() string {
	return s.path
}

// Get 返回当前配置的副本（值类型，外部修改不影响内部）。
func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

// Update 用新值覆盖整个配置并立即持久化。
func (s *Store) Update(next Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	applyDefaults(&next)
	s.data = next
	return s.saveLocked()
}

// Patch 在持有锁的情况下让调用方做局部修改并落盘。
//
// 用法:
//
//	store.Patch(func(s *Settings) {
//	    s.LastPort = 9000
//	})
func (s *Store) Patch(fn func(*Settings)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.data)
	applyDefaults(&s.data)
	return s.saveLocked()
}

// load 读取磁盘配置；文件不存在视为正常情况返回 nil。
func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 首次启动，使用默认值，不视为错误。
			return nil
		}
		return fmt.Errorf("read settings: %w", err)
	}
	var loaded Settings
	if err := json.Unmarshal(b, &loaded); err != nil {
		// 文件损坏，回退到默认值；返回错误让调用方决定是否日志记录。
		return fmt.Errorf("parse settings (using defaults): %w", err)
	}
	applyDefaults(&loaded)
	s.data = loaded
	return nil
}

// saveLocked 必须在持有 s.mu 的情况下调用。
//
// 写法：先写到 .tmp 再原子 rename，避免半截写入。
func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("mkdir settings: %w", err)
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write tmp settings: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		// rename 失败时清理 tmp，避免残留
		_ = os.Remove(tmp)
		return fmt.Errorf("rename settings: %w", err)
	}
	return nil
}

// configPath 计算配置文件的绝对路径。
func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("UserConfigDir: %w", err)
	}
	return filepath.Join(dir, appDirName, fileName), nil
}
