//go:build windows

package autostart

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// runKeyPath 是 HKCU 下的 Run 子键，用户级开机启动项住在这里，不需要管理员权限。
const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// quoteIfNeeded 给 exe 路径加引号；如果路径里有空格，注册表里不加引号 Windows 会把空格之后当成参数。
func quoteIfNeeded(p string) string {
	if strings.HasPrefix(p, `"`) && strings.HasSuffix(p, `"`) {
		return p
	}
	if !strings.ContainsAny(p, " \t") {
		return p
	}
	return `"` + p + `"`
}

func enable(name, exePath string, args []string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: open run key: %w", err)
	}
	defer k.Close()

	cmd := quoteIfNeeded(exePath)
	if len(args) > 0 {
		// 简单地把每个 arg 也用引号包起来；不处理引号内的转义（args 里不应该有引号）。
		for _, a := range args {
			if a == "" {
				continue
			}
			if strings.ContainsAny(a, " \t") && !strings.HasPrefix(a, `"`) {
				cmd += " " + `"` + a + `"`
			} else {
				cmd += " " + a
			}
		}
	}

	if err := k.SetStringValue(name, cmd); err != nil {
		return fmt.Errorf("autostart: set value: %w", err)
	}
	return nil
}

func disable(name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil // Run 子键不存在，等价于"已 disabled"
		}
		return fmt.Errorf("autostart: open run key: %w", err)
	}
	defer k.Close()

	if err := k.DeleteValue(name); err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil // 值本来就没有，幂等
		}
		return fmt.Errorf("autostart: delete value: %w", err)
	}
	return nil
}

func isEnabled(name string) (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("autostart: open run key: %w", err)
	}
	defer k.Close()

	_, _, err = k.GetStringValue(name)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("autostart: read value: %w", err)
	}
	return true, nil
}

func supported() bool { return true }
