// betas.go 维护 anthropic-beta 头部的基础列表与"长上下文降级"策略。
//
// 当上游报"long context required"类错误时，我们按 LongContextBetas 顺序逐个剔除并重试，
// 这是 Claude API OAuth 模式下绕开"超额计费"错误的关键技巧（参考 opencode-claude-auth/src/betas.ts）。
package ccproxy

import (
	"strings"
	"sync"
)

// BaseBetas 是发送给 Anthropic 的固定 beta flag 列表，匹配 Claude Code v2.1.x 的实际行为。
var BaseBetas = []string{
	"claude-code-20250219",
	"oauth-2025-04-20",
	"interleaved-thinking-2025-05-14",
	"prompt-caching-scope-2026-01-05",
	"context-management-2025-06-27",
	"advisor-tool-2026-03-01",
}

// LongContextBetas 是遇到长上下文 / 配额错误时按顺序剔除的 flag。
//
// 顺序很重要：先剔除 interleaved-thinking 这种"非结构性"，再剔除 context-1m 这种"功能性"。
var LongContextBetas = []string{
	"context-1m-2025-08-07",
	"interleaved-thinking-2025-05-14",
}

// betaExclusionStore 按 modelID 跟踪本进程中已剔除的 beta，进程重启后重置。
type betaExclusionStore struct {
	mu       sync.Mutex
	excluded map[string]map[string]struct{}
}

// NewBetaExclusionStore 构造一个新的剔除记录器。
func NewBetaExclusionStore() *betaExclusionStore {
	return &betaExclusionStore{excluded: map[string]map[string]struct{}{}}
}

// Get 返回 modelID 当前已剔除的 beta 集合（拷贝）。
func (s *betaExclusionStore) Get(modelID string) map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.excluded[modelID]
	if !ok {
		return nil
	}
	out := make(map[string]struct{}, len(src))
	for k := range src {
		out[k] = struct{}{}
	}
	return out
}

// Add 把 beta 加入 modelID 的剔除集合。
func (s *betaExclusionStore) Add(modelID, beta string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.excluded[modelID]
	if !ok {
		set = map[string]struct{}{}
		s.excluded[modelID] = set
	}
	set[beta] = struct{}{}
}

// NextToExclude 返回 modelID 下一个应剔除的 beta；全部剔除完则返回空串。
func (s *betaExclusionStore) NextToExclude(modelID string) string {
	excluded := s.Get(modelID)
	for _, b := range LongContextBetas {
		if _, ok := excluded[b]; !ok {
			return b
		}
	}
	return ""
}

// EffectiveBetas 计算实际要发送的 beta 列表，已剔除的会被过滤。
//
// incomingBeta 为客户端原本携带的 anthropic-beta 头（逗号分隔），将与 BaseBetas 合并去重。
func (s *betaExclusionStore) EffectiveBetas(modelID, incomingBeta string) []string {
	excluded := s.Get(modelID)

	merged := make([]string, 0, len(BaseBetas)+4)
	seen := map[string]struct{}{}

	add := func(beta string) {
		if beta == "" {
			return
		}
		if _, ok := excluded[beta]; ok {
			return
		}
		if _, ok := seen[beta]; ok {
			return
		}
		seen[beta] = struct{}{}
		merged = append(merged, beta)
	}

	for _, b := range BaseBetas {
		add(b)
	}
	for b := range strings.SplitSeq(incomingBeta, ",") {
		add(strings.TrimSpace(b))
	}
	return merged
}

// IsLongContextError 判断响应正文是否表征"长上下文不可用"错误，需要降级 betas。
func IsLongContextError(body string) bool {
	switch {
	case strings.Contains(body, "Extra usage is required for long context requests"):
		return true
	case strings.Contains(body, "long context beta is not yet available"):
		return true
	case strings.Contains(body, "You're out of extra usage"):
		return true
	}
	return false
}
