package chatgptproxy

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
	upstreamCodexResponses = "https://chatgpt.com/backend-api/codex/responses"
	upstreamOpenAIBase     = "https://api.openai.com"
)

// LogFunc 是外部注入的日志回调。
type LogFunc func(level, msg string, kv map[string]any)

// Config 控制代理监听参数。
type Config struct {
	Host string
	Port int
}

// Server 是 ChatGPT/Codex 本地代理。
type Server struct {
	creds    *CredentialManager
	session  string
	httpc    *http.Client
	onLog    LogFunc
	mu       sync.Mutex
	running  bool
	server   *http.Server
	listenAt string
}

// NewServer 构造代理实例。
func NewServer(creds *CredentialManager, onLog LogFunc) *Server {
	if onLog == nil {
		onLog = func(string, string, map[string]any) {}
	}
	return &Server{
		creds:   creds,
		session: uuid.NewString(),
		httpc:   &http.Client{Timeout: 0},
		onLog:   onLog,
	}
}

// IsRunning 报告是否正在监听。
func (s *Server) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// ListenAddr 返回监听地址。
func (s *Server) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return ""
	}
	return s.listenAt
}

// Start 启动本地 HTTP 服务。
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
	addr := host + ":" + strconv.Itoa(cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(s.requestLogger())
	r.GET("/healthz", s.handleHealth)
	r.GET("/v1/models", s.handleModels)
	r.Any("/v1/responses", s.handleProxy)
	r.Any("/v1/chat/completions", s.handleProxy)

	server := &http.Server{Handler: r}
	s.server = server
	s.listenAt = ln.Addr().String()
	s.running = true

	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.onLog("error", "chatgpt proxy stopped with error", map[string]any{"error": err.Error()})
		}
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	s.onLog("info", "chatgpt proxy started", map[string]any{"addr": s.listenAt})
	return nil
}

// Stop 优雅停止。
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	server := s.server
	wasRunning := s.running
	s.mu.Unlock()
	if !wasRunning || server == nil {
		return nil
	}
	err := server.Shutdown(ctx)
	s.mu.Lock()
	s.running = false
	s.server = nil
	s.listenAt = ""
	s.mu.Unlock()
	s.onLog("info", "chatgpt proxy stopped", nil)
	return err
}

func (s *Server) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"ok":        true,
		"provider":  "chatgpt",
		"sessionId": s.session,
	})
}

func (s *Server) handleModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   defaultModels(),
	})
}

func defaultModels() []gin.H {
	ids := []string{
		"gpt-5.5",
		"gpt-5.5-pro",
		"gpt-5.4",
		"gpt-5.4-pro",
		"gpt-5.4-mini",
		"gpt-5.4-nano",
		"gpt-5.3-codex",
		"gpt-5.3-codex-spark",
		"gpt-5.2",
		"gpt-5.2-codex",
		"gpt-5.1-codex",
		"gpt-5.1-codex-max",
		"gpt-5.1-codex-mini",
		"gpt-5-codex",
	}
	out := make([]gin.H, 0, len(ids))
	now := time.Now().Unix()
	for _, id := range ids {
		out = append(out, gin.H{
			"id":       id,
			"object":   "model",
			"created":  now,
			"owned_by": "chatgpt-subscription",
		})
	}
	return out
}

func (s *Server) requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		kv := map[string]any{
			"provider": "chatgpt",
			"method":   c.Request.Method,
			"path":     c.Request.URL.Path,
			"status":   c.Writer.Status(),
			"duration": time.Since(start).String(),
		}
		if model, ok := c.Get("proxyModel"); ok {
			kv["model"] = model
		}
		if upstreamStatus, ok := c.Get("proxyUpstreamStatus"); ok {
			kv["upstreamStatus"] = upstreamStatus
		}
		if detail, ok := c.Get("proxyDetail"); ok {
			if m, ok := detail.(map[string]any); ok {
				for k, v := range m {
					kv[k] = v
				}
			}
		}
		level := "debug"
		if c.Request.URL.Path != "/healthz" {
			level = "info"
		}
		s.onLog(level, "request", kv)
	}
}

func (s *Server) handleProxy(c *gin.Context) {
	path := c.Request.URL.Path

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "read body: " + err.Error()})
		return
	}
	_ = c.Request.Body.Close()
	modelID := extractModelID(body)
	c.Set("proxyModel", modelID)

	detail := map[string]any{
		"inboundHeaders": headerToMap(c.Request.Header),
		"reqBody":        parseBodyForLog(body),
		"reqBodyBytes":   len(body),
	}
	c.Set("proxyDetail", detail)

	auth, err := s.creds.GetAuth()
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "get chatgpt auth: " + err.Error()})
		return
	}
	upstreamBody := body
	if auth.Kind == AuthKindOAuth {
		upstreamBody = NormalizeRequestBody(path, body)
		detail["reqBody"] = parseBodyForLog(upstreamBody)
		detail["reqBodyBytes"] = len(upstreamBody)
	}

	upstreamURL, ok := upstreamURLFor(path, c.Request.URL.RawQuery, auth.Kind)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unsupported endpoint for ChatGPT subscription proxy"})
		return
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, upstreamURL, bytes.NewReader(upstreamBody))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "build upstream request: " + err.Error()})
		return
	}
	s.applyForwardHeaders(req, c.Request.Header, auth)
	detail["upstreamReqHeaders"] = redactedHeaderToMap(req.Header)

	resp, err := s.httpc.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream call: " + err.Error()})
		return
	}
	detail["respHeaders"] = headerToMap(resp.Header)
	c.Set("proxyUpstreamStatus", resp.StatusCode)
	s.streamResponse(c, resp, detail)
}

func upstreamURLFor(path, rawQuery, authKind string) (string, bool) {
	if authKind == AuthKindAPI {
		target := upstreamOpenAIBase + path
		if rawQuery != "" {
			target += "?" + rawQuery
		}
		return target, true
	}
	if strings.HasSuffix(path, "/responses") || strings.Contains(path, "/chat/completions") {
		return upstreamCodexResponses, true
	}
	return "", false
}

func (s *Server) applyForwardHeaders(req *http.Request, original http.Header, auth AuthInfo) {
	skip := map[string]bool{
		"host":              true,
		"content-length":    true,
		"connection":        true,
		"transfer-encoding": true,
		"authorization":     true,
		"x-api-key":         true,
		"accept-encoding":   true,
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
	req.Header.Set("authorization", "Bearer "+auth.AccessToken)
	if req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}

	if auth.Kind == AuthKindOAuth {
		req.Header.Set("originator", "opencode")
		req.Header.Set("User-Agent", chatGPTUserAgent())
		req.Header.Set("session_id", s.session)
		if auth.AccountID != "" {
			req.Header.Set("ChatGPT-Account-Id", auth.AccountID)
		}
	}
}

func chatGPTUserAgent() string {
	osName := runtime.GOOS
	if osName == "windows" {
		osName = "win32"
	}
	return fmt.Sprintf("opencode/localClaudeCodeProxy (%s %s; %s)", osName, runtime.GOOS, runtime.GOARCH)
}

func (s *Server) streamResponse(c *gin.Context, resp *http.Response, detail map[string]any) {
	defer resp.Body.Close()
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
	var captured bytes.Buffer
	buf := make([]byte, 16*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, err := c.Writer.Write(chunk); err != nil {
				captured.Write(chunk)
				flushCaptureToDetail(detail, captured.Bytes(), isSSE)
				return
			}
			captured.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			flushCaptureToDetail(detail, captured.Bytes(), isSSE)
			if !errors.Is(readErr, io.EOF) {
				s.onLog("warn", "chatgpt upstream stream error", map[string]any{"error": readErr.Error()})
			}
			return
		}
	}
}

func extractModelID(body []byte) string {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "unknown"
	}
	if model, ok := parsed["model"].(string); ok && model != "" {
		return model
	}
	return "unknown"
}

func headerToMap(h http.Header) map[string]string {
	result := make(map[string]string, len(h))
	for k, vs := range h {
		result[k] = strings.Join(vs, ", ")
	}
	return result
}

func redactedHeaderToMap(h http.Header) map[string]string {
	result := make(map[string]string, len(h))
	for k, vs := range h {
		v := strings.Join(vs, ", ")
		lower := strings.ToLower(k)
		if (lower == "authorization" || lower == "x-api-key") && len(v) > 24 {
			v = v[:24] + "…[redacted]"
		}
		result[k] = v
	}
	return result
}

func parseBodyForLog(body []byte) any {
	if len(body) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(body, &v); err == nil {
		return v
	}
	return string(body)
}

type sseEventSummary struct {
	Event string `json:"event"`
	Data  any    `json:"data"`
}

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
				dataLines = append(dataLines, bytes.TrimSpace(line[len("data:"):]))
				continue
			}
			comments = append(comments, string(line))
		}
		if len(dataLines) > 0 {
			joined := bytes.Join(dataLines, []byte("\n"))
			var parsed any
			if err := json.Unmarshal(joined, &parsed); err == nil {
				events = append(events, sseEventSummary{Event: eventName, Data: parsed})
			} else {
				events = append(events, sseEventSummary{Event: eventName, Data: string(joined)})
			}
		} else if len(comments) > 0 {
			events = append(events, sseEventSummary{Event: "comment", Data: strings.Join(comments, "\n")})
		}
	}
	return events
}

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
	detail["respBody"] = parseBodyForLog(captured)
}

func isSSEResponse(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}
