// Package netproxy 为所有出站 HTTP 客户端提供统一的代理解析。
//
// 背景：Go 的 http.DefaultTransport 只认 HTTP_PROXY/HTTPS_PROXY 环境变量，
// **不读 Windows 系统代理设置**。GUI 应用从资源管理器/托盘/开机启动拉起时
// 不继承终端会话里的代理变量，直连 api.anthropic.com 会被地区封锁（403
// "Request not allowed"）。这里的解析顺序：
//
//  1. 环境变量 HTTP_PROXY / HTTPS_PROXY / NO_PROXY（与 go run / 终端行为一致）
//  2. 系统代理设置（Windows IE/WinHTTP，含 PAC；macOS/Linux 回退到环境变量）
package netproxy

import (
	"net/http"
	"net/url"
	"time"

	"github.com/mattn/go-ieproxy"
)

// ProxyFunc 返回 Transport.Proxy 使用的解析函数：环境变量优先，系统代理兜底。
func ProxyFunc() func(*http.Request) (*url.URL, error) {
	sysFunc := ieproxy.GetProxyFunc()
	return func(req *http.Request) (*url.URL, error) {
		if u, err := http.ProxyFromEnvironment(req); err != nil || u != nil {
			return u, err
		}
		return sysFunc(req)
	}
}

// NewTransport 返回带统一代理解析的 Transport，其余参数沿用 DefaultTransport。
func NewTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = ProxyFunc()
	return tr
}

// NewClient 构造使用统一代理解析的 http.Client。timeout 为 0 表示不限时（流式响应）。
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: NewTransport(),
		Timeout:   timeout,
	}
}
