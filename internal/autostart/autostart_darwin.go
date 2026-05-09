//go:build darwin

package autostart

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// labelPrefix 是 LaunchAgent 的 reverse-DNS Label 前缀；最终 Label 为 prefix + "." + name。
const labelPrefix = "io.localclaudecodeproxy"

// plistDoc 是 LaunchAgent plist 的 Go 表示。
//
// LaunchAgent 完整 spec 见 launchd.plist(5)，这里只用最基础字段：
//
//	Label:            唯一标识
//	ProgramArguments: 启动命令 + 参数
//	RunAtLoad:        true 表示登录时启动
type plistDoc struct {
	XMLName xml.Name `xml:"plist"`
	Version string   `xml:"version,attr"`
	Dict    plistDict
}

type plistDict struct {
	XMLName xml.Name `xml:"dict"`
	Entries []any    `xml:",any"`
}

// 简洁起见，下面三个 helper 直接拼出 plist 文本，绕过反射式编码。
// (encoding/xml 对 plist <key>/<value> 平铺结构支持不好。)

func renderPlist(label, exePath string, args []string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	b.WriteString("  <key>Label</key>\n")
	b.WriteString("  <string>" + escapeXML(label) + "</string>\n")
	b.WriteString("  <key>ProgramArguments</key>\n")
	b.WriteString("  <array>\n")
	b.WriteString("    <string>" + escapeXML(exePath) + "</string>\n")
	for _, a := range args {
		if a == "" {
			continue
		}
		b.WriteString("    <string>" + escapeXML(a) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key>\n")
	b.WriteString("  <true/>\n")
	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return []byte(b.String())
}

func escapeXML(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func plistPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("autostart: home dir: %w", err)
	}
	label := labelPrefix + "." + sanitize(name)
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func enable(name, exePath string, args []string) error {
	p, err := plistPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("autostart: mkdir LaunchAgents: %w", err)
	}
	label := strings.TrimSuffix(filepath.Base(p), ".plist")
	body := renderPlist(label, exePath, args)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("autostart: write plist tmp: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("autostart: rename plist: %w", err)
	}
	return nil
}

func disable(name string) error {
	p, err := plistPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("autostart: remove plist: %w", err)
	}
	return nil
}

func isEnabled(name string) (bool, error) {
	p, err := plistPath(name)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("autostart: stat plist: %w", err)
	}
	return true, nil
}

func supported() bool { return true }
