// transforms.go 实现 OAuth 代理对请求 body 的强制改写以及对流式响应的工具名还原。
//
// 关键改写规则（参考 opencode-claude-auth/src/transforms.ts）：
//
//  1. system[0] 文本块**必须**以 "You are Claude Code, Anthropic's official CLI for Claude." 开头，
//     否则上游会把请求识别为非 Claude Code 客户端而拒绝。
//  2. 所有 tools[].name 与 messages[].content[].name (tool_use) **必须**以 "mcp_" + PascalCase
//     形式出现，否则同样会被识别为非 CC 客户端。响应里要把 mcp_X 还原回 x。
//  3. 修复 orphan 的 tool_use / tool_result 配对，避免 400 错误。
//
// 我们刻意**不**实现 billing header / split identity prefix / relocate non-core system 这些，
// 它们是 OpenCode 特有的兼容代码，对 Claude Code CLI 这种"原生"客户端不需要。
package ccproxy

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

// SystemIdentity 是上游用于识别 Claude Code 客户端的固定前缀。
const SystemIdentity = "You are Claude Code, Anthropic's official CLI for Claude."

const toolPrefix = "mcp_"

// prefixToolName 将工具名转为 mcp_ + PascalCase，如 "bash" → "mcp_Bash"。
//
// Claude Code 用 PascalCase tool 名；小写名（mcp_bash）会被上游识别为"多个工具时
// 名称大小写不规范"而拒绝（参考 transforms.ts 注释）。
func prefixToolName(name string) string {
	if name == "" {
		return name
	}
	if strings.HasPrefix(name, toolPrefix) {
		// 已有前缀但首字母可能是小写：仍要规范化
		rest := name[len(toolPrefix):]
		return toolPrefix + capitalizeFirst(rest)
	}
	return toolPrefix + capitalizeFirst(name)
}

// unprefixToolName 还原 prefixToolName，例如 "mcp_Bash" → "bash"。
func unprefixToolName(name string) string {
	rest := strings.TrimPrefix(name, toolPrefix)
	return lowercaseFirst(rest)
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func lowercaseFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

// repairToolPairs 移除孤立的 tool_use / tool_result 块，避免上游 400。
//
// Claude API 要求每个 tool_use 在后续消息里必须有同 id 的 tool_result，
// 反过来每个 tool_result 必须对应一个已存在的 tool_use。中断 / 重连场景容易留下孤儿。
func repairToolPairs(messages []map[string]any) []map[string]any {
	toolUseIDs := map[string]struct{}{}
	toolResultIDs := map[string]struct{}{}

	for _, msg := range messages {
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			t, _ := block["type"].(string)
			switch t {
			case "tool_use":
				if id, ok := block["id"].(string); ok {
					toolUseIDs[id] = struct{}{}
				}
			case "tool_result":
				if id, ok := block["tool_use_id"].(string); ok {
					toolResultIDs[id] = struct{}{}
				}
			}
		}
	}

	orphanUses := map[string]struct{}{}
	for id := range toolUseIDs {
		if _, ok := toolResultIDs[id]; !ok {
			orphanUses[id] = struct{}{}
		}
	}
	orphanResults := map[string]struct{}{}
	for id := range toolResultIDs {
		if _, ok := toolUseIDs[id]; !ok {
			orphanResults[id] = struct{}{}
		}
	}
	if len(orphanUses) == 0 && len(orphanResults) == 0 {
		return messages
	}

	out := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		blocks, ok := msg["content"].([]any)
		if !ok {
			out = append(out, msg)
			continue
		}
		filtered := make([]any, 0, len(blocks))
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				filtered = append(filtered, b)
				continue
			}
			t, _ := block["type"].(string)
			switch t {
			case "tool_use":
				if id, ok := block["id"].(string); ok {
					if _, orphan := orphanUses[id]; orphan {
						continue
					}
				}
			case "tool_result":
				if id, ok := block["tool_use_id"].(string); ok {
					if _, orphan := orphanResults[id]; orphan {
						continue
					}
				}
			}
			filtered = append(filtered, b)
		}
		// 过滤后 content 为空的消息一并删除
		if len(filtered) == 0 {
			continue
		}
		msg["content"] = filtered
		out = append(out, msg)
	}
	return out
}

// ensureSystemIdentity 保证 system[] 第一条文本块以 SystemIdentity 开头。
//
// 三种情况：
//  1. 不存在 system → 新建 [{ type:"text", text: SystemIdentity }]
//  2. system 存在但首条不以 SystemIdentity 开头 → 在最前面 unshift 一条
//  3. 首条已经满足 → 不动
//
// 这一步不能省：Claude Code 上游只接受身份前缀严格匹配的请求。
func ensureSystemIdentity(parsed map[string]any) {
	systemRaw, ok := parsed["system"]
	if !ok || systemRaw == nil {
		parsed["system"] = []any{
			map[string]any{"type": "text", "text": SystemIdentity},
		}
		return
	}

	// system 字段在 Anthropic API 里允许是字符串或数组，统一成数组处理。
	var entries []any
	switch v := systemRaw.(type) {
	case string:
		entries = []any{map[string]any{"type": "text", "text": v}}
	case []any:
		entries = v
	default:
		// 未知格式直接覆盖
		parsed["system"] = []any{
			map[string]any{"type": "text", "text": SystemIdentity},
		}
		return
	}

	hasIdentity := false
	if len(entries) > 0 {
		if first, ok := entries[0].(map[string]any); ok {
			if text, ok := first["text"].(string); ok && strings.HasPrefix(text, SystemIdentity) {
				hasIdentity = true
			}
		}
	}
	if !hasIdentity {
		entries = append([]any{map[string]any{"type": "text", "text": SystemIdentity}}, entries...)
	}
	parsed["system"] = entries
}

// stripEffortForHaiku 对 haiku 模型移除 output_config.effort / thinking.effort 字段。
//
// haiku 不支持 effort 参数，OpenCode/某些客户端会无脑下发，触发 400。
func stripEffortForHaiku(parsed map[string]any) {
	model, _ := parsed["model"].(string)
	if !strings.Contains(strings.ToLower(model), "haiku") {
		return
	}
	if oc, ok := parsed["output_config"].(map[string]any); ok {
		delete(oc, "effort")
		if len(oc) == 0 {
			delete(parsed, "output_config")
		}
	}
	if th, ok := parsed["thinking"].(map[string]any); ok {
		delete(th, "effort")
		if len(th) == 0 {
			delete(parsed, "thinking")
		}
	}
}

// applyToolNamePrefix 改写 tools[].name 以及 messages[].content[]里 tool_use.name。
func applyToolNamePrefix(parsed map[string]any) {
	if tools, ok := parsed["tools"].([]any); ok {
		for i, t := range tools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if name, ok := tool["name"].(string); ok && name != "" {
				tool["name"] = prefixToolName(name)
				tools[i] = tool
			}
		}
	}

	messages, ok := parsed["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for i, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t != "tool_use" {
				continue
			}
			if name, ok := block["name"].(string); ok && name != "" {
				block["name"] = prefixToolName(name)
				blocks[i] = block
			}
		}
		msg["content"] = blocks
	}
}

// TransformBody 对入站请求 body 做完整改写，输出可直接转发给 Anthropic 的 body。
//
// 不是 JSON / 解析失败时直接原样返回，避免阻断"非 messages"端点（如未来扩展）。
func TransformBody(body []byte) []byte {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}

	ensureSystemIdentity(parsed)
	stripEffortForHaiku(parsed)
	applyToolNamePrefix(parsed)

	if msgs, ok := parsed["messages"].([]any); ok {
		typed := make([]map[string]any, 0, len(msgs))
		for _, m := range msgs {
			if msg, ok := m.(map[string]any); ok {
				typed = append(typed, msg)
			}
		}
		repaired := repairToolPairs(typed)
		anyMsgs := make([]any, len(repaired))
		for i, m := range repaired {
			anyMsgs[i] = m
		}
		parsed["messages"] = anyMsgs
	}

	out, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return out
}

// 用于响应里把 mcp_Foo 还原回 foo。
//
// 上游响应（无论是流式 SSE 还是错误 JSON）都会包含我们注入的 mcp_ 前缀工具名，
// 必须在转发回客户端前剥离，否则客户端一脸懵。
var stripToolPrefixRe = regexp.MustCompile(`"name"\s*:\s*"mcp_([^"]+)"`)

// StripToolPrefix 从响应文本中移除 mcp_ 前缀（仅对 "name": "mcp_X" 字段生效）。
func StripToolPrefix(text []byte) []byte {
	return stripToolPrefixRe.ReplaceAllFunc(text, func(match []byte) []byte {
		// match 形如 `"name": "mcp_Bash"`，提取 mcp_ 后内容
		sub := stripToolPrefixRe.FindSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		restored := unprefixToolName("mcp_" + string(sub[1]))
		return []byte(`"name": "` + restored + `"`)
	})
}
