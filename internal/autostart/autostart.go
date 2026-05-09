// Package autostart 提供跨平台"开机启动"注册能力。
//
// 支持平台：
//   - Windows: 写 HKCU\Software\Microsoft\Windows\CurrentVersion\Run
//   - macOS:   写 ~/Library/LaunchAgents/<bundle>.plist + RunAtLoad
//   - 其它:    返回 ErrUnsupported
//
// 注意：所有写操作都是 per-user 的（不需要管理员权限）。
package autostart

import "errors"

// ErrUnsupported 表示当前平台不支持开机启动注册。
var ErrUnsupported = errors.New("autostart: platform not supported")

// Enable 注册开机启动。
//
// name     是显示在系统启动项里的名字（Windows 注册表值名 / macOS plist Label 后缀）
// exePath  是要启动的可执行文件绝对路径
// args     是传给 exe 的参数（可空），目前 Windows 实现忽略，macOS 会写进 ProgramArguments
func Enable(name, exePath string, args ...string) error {
	return enable(name, exePath, args)
}

// Disable 取消开机启动注册。如果本来没开启则视为成功（幂等）。
func Disable(name string) error {
	return disable(name)
}

// IsEnabled 返回当前是否已注册开机启动。
//
// 注意：平台 API 异常时返回 (false, err)，调用方应区分"未启用"和"读不出来"。
func IsEnabled(name string) (bool, error) {
	return isEnabled(name)
}

// Supported 报告当前操作系统是否支持开机启动注册。
//
// 调用方可以根据返回值决定是否在 UI 上显示对应开关。
func Supported() bool {
	return supported()
}
