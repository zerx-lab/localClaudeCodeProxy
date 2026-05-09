// server.go 实现基于 gin 的本地 HTTP 代理服务，把 Anthropic API 请求重写后转发到上游，
// 同时把 mcp_ 工具前缀从响应中剥离。
//
// 强制 headers 一节非常关键：缺任何一项，上游都会把请求识别为非 Claude Code 客户端而拒绝/限流。
package ccproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	upstreamBase = "https://api.anthropic.com"

	// 重试 cap：retry-after 超过这个值视为"配额已耗尽"，立刻把错误暴露给客户端。
	defaultMaxRetryDelay = 30 * time.Second

	// CC v2.1.112 是当前 anthropic 接受的"客户端版本"。改动这个值前先去 Claude Code 实际抓包确认。
	ccVersion         = "2.1.112"
	stainlessPkgVer   = "0.81.0"
	stainlessRuntime  = "node"
	stainlessLang     = "js"
	stainlessTimeout  = "600"
	stainlessRetryCnt = "0"
)

// LogFunc 是外部注入的日志回调（前端订阅这个事件即可看到代理活动）。
type LogFunc func(level, msg string, kv map[string]any)

// Config 控制代理行为。
type Config struct {
	// Host 默认 "127.0.0.1"，仅本机可访问。设成 "0.0.0.0" 暴露到局域网，请确保使用者知道风险。
	Host string
	// Port 监听端口；0 表示由系统分配空闲端口。
	Port int
	// MaxRetryDelay 最大可接受的 retry-after 时长，超过则放弃重试。
	MaxRetryDelay time.Duration
}

// Server 是本地代理。**实例可重复 Start/Stop**。
type Server struct {
	creds *CredentialManager
	betas *betaExclusionStore

	// sessionID 在进程生命周期内固定，匹配 Claude Code 的 X-Claude-Code-Session-Id 行为。
	sessionID string

	httpc *http.Client
	onLog LogFunc

	mu            sync.Mutex
	running       bool
	httpServer    *http.Server
	actualAddr    string
	maxRetryDelay time.Duration
}

// NewServer 构造代理服务实例。onLog 可为 nil。
func NewServer(creds *CredentialManager, onLog LogFunc) *Server {
	if onLog == nil {
		onLog = func(string, string, map[string]any) {}
	}
	return &Server{
		creds:         creds,
		betas:         NewBetaExclusionStore(),
		sessionID:     uuid.NewString(),
		httpc:         &http.Client{Timeout: 0}, // 不设超时：流式响应可能持续很久
		onLog:         onLog,
		maxRetryDelay: defaultMaxRetryDelay,
	}
}

// IsRunning 报告代理当前是否在监听。
func (s *Server) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// ListenAddr 返回当前监听地址（如 "127.0.0.1:8765"），未运行时返回空串。
func (s *Server) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return ""
	}
	return s.actualAddr
}

// Start 启动监听，重复调用返回错误。
func (s *Server) Start(cfg Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return errors.New("server already running")
	}

	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	if cfg.MaxRetryDelay > 0 {
		s.maxRetryDelay = cfg.MaxRetryDelay
	}

	addr := host + ":" + strconv.Itoa(cfg.Port)

	// 提前 Listen 是为了在端口分配后就能拿到 actualAddr（cfg.Port=0 时尤其重要）。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(s.requestLogger())

	// 全部 /v1/* 请求都走代理转发，未来扩 /v1/models 等接口也走同一个 handler。
	r.Any("/v1/*path", s.handleProxy)

	// 健康检查（前端可用来验证服务确实在跑）。
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"ok":        true,
			"sessionId": s.sessionID,
		})
	})

	server := &http.Server{Handler: r}
	s.httpServer = server
	s.actualAddr = ln.Addr().String()
	s.running = true

	// 实际服务在后台 goroutine 运行；ln 由 Serve 接管，关闭由 Shutdown 处理。
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.onLog("error", "http server stopped with error", map[string]any{"error": err.Error()})
		}
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	s.onLog("info", "proxy started", map[string]any{"addr": s.actualAddr})
	return nil
}

// Stop 优雅停止；超过 ctx 期限会强制关闭。
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	server := s.httpServer
	wasRunning := s.running
	s.mu.Unlock()

	if !wasRunning || server == nil {
		return nil
	}
	err := server.Shutdown(ctx)

	s.mu.Lock()
	s.running = false
	s.httpServer = nil
	s.actualAddr = ""
	s.mu.Unlock()

	s.onLog("info", "proxy stopped", nil)
	return err
}

// requestLogger 是简单的 gin 中间件，把每个请求的方法/路径/状态推到日志回调。
//
// 代理请求（/v1/*）会展示 model、attempt 等丰富字段（由 handleProxy 写入 gin Context）。
func (s *Server) requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		kv := map[string]any{
			"method":   c.Request.Method,
			"path":     c.Request.URL.Path,
			"status":   c.Writer.Status(),
			"duration": time.Since(start).String(),
		}

		// 如果 handleProxy 写入了模型 / 重试次数，一并带入日志字段
		if model, ok := c.Get("proxyModel"); ok {
			kv["model"] = model
		}
		if attempt, ok := c.Get("proxyAttempt"); ok {
			kv["attempt"] = attempt
		}
		if upstreamStatus, ok := c.Get("proxyUpstreamStatus"); ok {
			kv["upstreamStatus"] = upstreamStatus
		}

		// 合并调试详情（inboundHeaders / upstreamReqHeaders / reqBody / respHeaders）
		if detail, ok := c.Get("proxyDetail"); ok {
			if m, ok := detail.(map[string]any); ok {
				for k, v := range m {
					kv[k] = v
				}
			}
		}

		// healthz 用 debug，代理请求用 info（方便前端默认展示）
		level := "debug"
		if c.Request.URL.Path != "/healthz" {
			level = "info"
		}
		s.onLog(level, "request", kv)
	}
}

// handleProxy 是 /v1/* 的总入口：改写 body → 注入 headers → 重试转发 → 流式回包。
func (s *Server) handleProxy(c *gin.Context) {
	// 读完整 body 才能改写。Anthropic API 请求体不会大到放不下内存。
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "read body: " + err.Error()})
		return
	}
	_ = c.Request.Body.Close()

	// 仅对 /v1/messages 做改写；其他端点（如 /v1/models）原样转发。
	transformed := body
	if strings.HasSuffix(c.Request.URL.Path, "/messages") {
		transformed = TransformBody(body)
	}

	modelID := extractModelID(transformed)
	c.Set("proxyModel", modelID) // requestLogger 会在请求完成后读取这些字段

	// 调试详情：在 handler 全程更新同一个 map，requestLogger 返回后读取。
	detail := map[string]any{
		"inboundHeaders": headerToMap(c.Request.Header),
		"reqBody":        parseBodyForLog(transformed, 0),
		"reqBodyBytes":   len(transformed),
	}
	c.Set("proxyDetail", detail)

	for attempt := range 5 {
		token, err := s.creds.GetAccessToken()
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "get access token: " + err.Error()})
			return
		}

		upstreamURL := upstreamBase + c.Request.URL.Path
		if c.Request.URL.RawQuery != "" {
			upstreamURL += "?" + c.Request.URL.RawQuery
		}

		req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, upstreamURL, bytes.NewReader(transformed))
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "build upstream request: " + err.Error()})
			return
		}
		s.applyForwardHeaders(req, c.Request.Header, token, modelID)
		if attempt == 0 {
			detail["upstreamReqHeaders"] = redactedHeaderToMap(req.Header)
		}

		resp, err := s.httpc.Do(req)
		if err != nil {
			s.onLog("warn", "upstream call failed", map[string]any{"error": err.Error(), "attempt": attempt})
			c.JSON(http.StatusBadGateway, gin.H{"error": "upstream call: " + err.Error()})
			return
		}

		// 限流：429/529 + retry-after 处理
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			delay := parseRetryAfter(resp.Header.Get("Retry-After"), attempt)
			if delay > s.maxRetryDelay {
				// 超过 cap：把响应直接转给客户端，让用户自己决定是否重试
				s.onLog("warn", "rate limited beyond cap, surface to client", map[string]any{
					"status":     resp.StatusCode,
					"retryAfter": resp.Header.Get("Retry-After"),
					"delayMs":    delay.Milliseconds(),
				})
				detail["respHeaders"] = headerToMap(resp.Header)
				c.Set("proxyAttempt", attempt)
				c.Set("proxyUpstreamStatus", resp.StatusCode)
				s.streamResponse(c, resp, detail)
				return
			}
			s.onLog("warn", "rate limited, retrying", map[string]any{
				"status":     resp.StatusCode,
				"attempt":    attempt + 1,
				"retryAfter": resp.Header.Get("Retry-After"),
			})
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			select {
			case <-time.After(delay):
			case <-c.Request.Context().Done():
				return
			}
			continue
		}

		// 长上下文 / 超额错误：剔除一个 beta 重试
		if resp.StatusCode >= 400 {
			peek, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()

			if IsLongContextError(string(peek)) {
				next := s.betas.NextToExclude(modelID)
				if next != "" {
					s.betas.Add(modelID, next)
					s.onLog("info", "long context error: dropping beta and retrying", map[string]any{
						"model":   modelID,
						"dropped": next,
					})
					continue
				}
			}

			// 错误响应：剥前缀后透传给客户绰。同时记录 respBody 到 detail（原始 peek 结果，保留完整）。
			detail["respHeaders"] = headerToMap(resp.Header)
			detail["respBody"] = parseBodyForLog(peek, 0)
			detail["respBodyBytes"] = len(peek)
			c.Set("proxyAttempt", attempt)
			c.Set("proxyUpstreamStatus", resp.StatusCode)
			s.copyErrorResponse(c, resp, peek)
			return
		}

		// 正常响应：流式转发
		detail["respHeaders"] = headerToMap(resp.Header)
		c.Set("proxyAttempt", attempt)
		c.Set("proxyUpstreamStatus", resp.StatusCode)
		s.streamResponse(c, resp, detail)
		return
	}

	c.JSON(http.StatusBadGateway, gin.H{"error": "exceeded retry budget"})
}

// applyForwardHeaders 注入 Anthropic 上游必需的全部强制 headers。
//
// 这一步对应 opencode-claude-auth/src/index.ts 的 buildRequestHeaders。**漏一项就被识别为非 CC 客户端**。
func (s *Server) applyForwardHeaders(req *http.Request, original http.Header, accessToken, modelID string) {
	// 复制原始请求 headers，但有几条要剔除掉
	skip := map[string]bool{
		"host":              true,
		"content-length":    true,
		"connection":        true,
		"transfer-encoding": true,
		"x-api-key":         true, // 与 OAuth Bearer 冲突，必须删
		"authorization":     true, // 后面统一注入
		"anthropic-version": true,
		"anthropic-beta":    true,
		"x-app":             true,
		"user-agent":        true,
		// 禁止上游压缩响应：我们要在响应字节流里 regex 剥 mcp_ 前缀，
		// 拿到压缩流就没法操作；而剥前缀后 content-length 已变，重压缩反而麻烦。
		// 强制 identity 以裸文本形式拿响应（本地链路，带宽损失可忽略）。
		"accept-encoding": true,
	}
	for k, vs := range original {
		if skip[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	req.Header.Set("accept-encoding", "identity")
	req.Header.Set("authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-version", "2023-06-01")

	incomingBeta := original.Get("anthropic-beta")
	merged := s.betas.EffectiveBetas(modelID, incomingBeta)
	req.Header.Set("anthropic-beta", strings.Join(merged, ","))

	req.Header.Set("anthropic-dangerous-direct-browser-access", "true")
	req.Header.Set("x-app", "cli")
	req.Header.Set("user-agent", "claude-cli/"+ccVersion+" (external, sdk-cli)")
	req.Header.Set("x-client-request-id", uuid.NewString())
	req.Header.Set("X-Claude-Code-Session-Id", s.sessionID)

	for k, v := range stainlessHeaders() {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
}

// stainlessHeaders 返回 Anthropic SDK 自带的 x-stainless-* 系列伪装头。
func stainlessHeaders() map[string]string {
	osName := runtime.GOOS
	switch osName {
	case "darwin":
		osName = "MacOS"
	case "windows":
		osName = "Windows"
	case "linux":
		osName = "Linux"
	}
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	return map[string]string{
		"x-stainless-arch":            arch,
		"x-stainless-lang":            stainlessLang,
		"x-stainless-os":              osName,
		"x-stainless-package-version": stainlessPkgVer,
		"x-stainless-retry-count":     stainlessRetryCnt,
		"x-stainless-runtime":         stainlessRuntime,
		"x-stainless-runtime-version": "v" + runtime.Version()[2:], // "go1.26.1" → "v1.26.1"
		"x-stainless-timeout":         stainlessTimeout,
	}
}

// streamResponse 把上游响应（成功或限流）流式转发到客户端，逐事件剥离 mcp_ 前缀。
//
// 上游对 /v1/messages 默认返回 SSE（text/event-stream）。我们按 \n\n 切分事件，
// 每个完整事件都过一遍 StripToolPrefix 再写到客户端。
//
// detail 不为 nil 时，还会边转发边把**剥前缀后**的完整响应字节另存一份到
// detail["respBody"]/["respEvents"]，供前端调试面板展示。这里有个重要细节：
// 记录的是“到达客户端”的响应（mcp_ 前缀已剥），跟客户端看到的一致。
func (s *Server) streamResponse(c *gin.Context, resp *http.Response, detail map[string]any) {
	defer resp.Body.Close()

	// 透传 headers，但要剥离 content-length / content-encoding
	// （我们改写后的长度变了；上游可能压缩，我们要重新协商）。
	skip := map[string]bool{
		"content-length":    true,
		"content-encoding":  true,
		"transfer-encoding": true,
	}
	for k, vs := range resp.Header {
		if skip[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			c.Writer.Header().Add(k, v)
		}
	}
	c.Writer.WriteHeader(resp.StatusCode)

	flusher, _ := c.Writer.(http.Flusher)

	isSSE := isSSEResponse(resp.Header)

	// 响应全量录下来供调试。不设上限 — 环形缓冲在上层控制总量。
	var captured bytes.Buffer

	buf := make([]byte, 16*1024)
	var pending []byte
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)

			// 按 \n\n 切分完整事件并逐个剥前缀写出
			for {
				idx := bytes.Index(pending, []byte("\n\n"))
				if idx < 0 {
					break
				}
				event := pending[:idx+2]
				pending = pending[idx+2:]
				stripped := StripToolPrefix(event)
				if _, err := c.Writer.Write(stripped); err != nil {
					// 客户端推失败：收尾 detail 之后退出
					captured.Write(stripped)
					flushCaptureToDetail(detail, captured.Bytes(), isSSE)
					return
				}
				captured.Write(stripped)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		if readErr != nil {
			// 收尾：剩余不到一个事件的内容也要写出
			if len(pending) > 0 {
				stripped := StripToolPrefix(pending)
				c.Writer.Write(stripped)
				captured.Write(stripped)
				if flusher != nil {
					flusher.Flush()
				}
			}
			flushCaptureToDetail(detail, captured.Bytes(), isSSE)
			if !errors.Is(readErr, io.EOF) {
				s.onLog("warn", "upstream stream error", map[string]any{"error": readErr.Error()})
			}
			return
		}
	}
}

// flushCaptureToDetail 把已捕获的响应字节写入 detail map。
//
//   - SSE 流：同时写入 respEvents（结构化事件列表）和 respBodyRaw（完整原文）
//   - 非 SSE：写入 respBody（如能解析为 JSON 则结构化）
func flushCaptureToDetail(detail map[string]any, captured []byte, isSSE bool) {
	if detail == nil || len(captured) == 0 {
		return
	}
	detail["respBodyBytes"] = len(captured)
	if isSSE {
		detail["respEvents"] = parseSSEEvents(captured)
		detail["respBodyRaw"] = string(captured)
		return
	}
	detail["respBody"] = parseBodyForLog(captured, 0)
}

// copyErrorResponse 把已经 peek 过的错误响应转发给客户端（同样剥 mcp_ 前缀）。
func (s *Server) copyErrorResponse(c *gin.Context, resp *http.Response, peek []byte) {
	skip := map[string]bool{
		"content-length":    true,
		"content-encoding":  true,
		"transfer-encoding": true,
	}
	for k, vs := range resp.Header {
		if skip[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			c.Writer.Header().Add(k, v)
		}
	}
	c.Writer.WriteHeader(resp.StatusCode)
	c.Writer.Write(StripToolPrefix(peek))
}

// parseRetryAfter 解析 Retry-After（秒数）；空值时按指数退避。
func parseRetryAfter(value string, attempt int) time.Duration {
	if value == "" {
		return time.Duration(attempt+1) * 2 * time.Second
	}
	if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
		return time.Duration(n) * time.Second
	}
	// 不支持 HTTP-date 格式：回退指数退避
	return time.Duration(attempt+1) * 2 * time.Second
}

// extractModelID 从请求 body 中读取 model 字段；解析失败返回 "unknown"。
func extractModelID(body []byte) string {
	type modelOnly struct {
		Model string `json:"model"`
	}
	var m modelOnly
	if err := jsonUnmarshalLoose(body, &m); err != nil {
		return "unknown"
	}
	if m.Model == "" {
		return "unknown"
	}
	return m.Model
}

// headerToMap 把 http.Header 转成 map[string]string（多值用 ", " 拼接）。
func headerToMap(h http.Header) map[string]string {
	result := make(map[string]string, len(h))
	for k, vs := range h {
		result[k] = strings.Join(vs, ", ")
	}
	return result
}

// redactedHeaderToMap 同上，但对 authorization 字段做脱敏处理。
func redactedHeaderToMap(h http.Header) map[string]string {
	result := make(map[string]string, len(h))
	for k, vs := range h {
		v := strings.Join(vs, ", ")
		if strings.ToLower(k) == "authorization" && len(v) > 24 {
			v = v[:24] + "…[redacted]"
		}
		result[k] = v
	}
	return result
}

// parseBodyForLog 把原始 body 完整保留下来供前端调试。
//
// 策略（用户要求 "保证 body 数据完整保留"）：
//   - 空 body 返回 nil
//   - 能解析为 JSON → 返回解析后的对象（前端会用 JSON.stringify 美化展示）
//   - 不能解析 → 返回原始字符串
//
// 不再做大小截断。代理日志缓冲区是环形（500 条上限），
// 单条 100KB 量级的 body 完全可承受；调试时缺数据比内存压力更难受。
//
// maxBytes 参数保留是为了兼容旧调用，传 <=0 表示不限制。
func parseBodyForLog(body []byte, maxBytes int) any {
	_ = maxBytes // 已废弃，保留签名兼容；如需重新启用上限直接走该参数
	if len(body) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(body, &v); err == nil {
		return v
	}
	return string(body)
}

// sseEventSummary 是单条 SSE 事件的结构化摘要。
//
// 字段说明：
//   - Event: SSE event 类型行（message_start / content_block_delta / ...）
//   - Data:  data: 行解析后的 JSON 对象；解析失败保留原文字符串
//
// 为了保留全部信息，Data 不做任何字段截断；如果一条对话产生 2000 个 text_delta，
// 那就老老实实存 2000 条。环形缓冲限制了总条数，单条对话不会撑爆。
type sseEventSummary struct {
	Event string `json:"event"`
	Data  any    `json:"data"`
}

// parseSSEEvents 把 SSE 字节流（已按 \n\n 切分为完整事件块的拼接）解析成结构化事件列表。
//
// 输入是若干个事件块拼接，每块形如：
//
//	event: content_block_delta\ndata: {"type":"content_block_delta",...}\n\n
//
// 没有 event: 行的注释行（如 :ping）也会作为一条 "comment" 事件保留下来。
func parseSSEEvents(raw []byte) []sseEventSummary {
	if len(raw) == 0 {
		return nil
	}
	chunks := bytes.Split(raw, []byte("\n\n"))
	events := make([]sseEventSummary, 0, len(chunks))
	for _, chunk := range chunks {
		chunk = bytes.TrimRight(chunk, "\r\n")
		if len(chunk) == 0 {
			continue
		}
		var eventName string
		var dataLines [][]byte
		var comments []string
		for _, line := range bytes.Split(chunk, []byte("\n")) {
			line = bytes.TrimRight(line, "\r")
			if len(line) == 0 {
				continue
			}
			if line[0] == ':' {
				comments = append(comments, string(line[1:]))
				continue
			}
			if bytes.HasPrefix(line, []byte("event:")) {
				eventName = strings.TrimSpace(string(line[len("event:"):]))
				continue
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				dataLines = append(dataLines, bytes.TrimPrefix(bytes.TrimSpace(line[len("data:"):]), []byte(" ")))
				continue
			}
			// 其他未知行原样作为 raw 行保留
			comments = append(comments, string(line))
		}

		if len(dataLines) > 0 {
			joined := bytes.Join(dataLines, []byte("\n"))
			var parsed any
			var data any
			if err := json.Unmarshal(joined, &parsed); err == nil {
				data = parsed
			} else {
				data = string(joined)
			}
			events = append(events, sseEventSummary{Event: eventName, Data: data})
		} else if len(comments) > 0 {
			events = append(events, sseEventSummary{Event: "comment", Data: strings.Join(comments, "\n")})
		}
	}
	return events
}

// isSSEResponse 通过 content-type 判断响应是否为 SSE 流。
func isSSEResponse(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}
