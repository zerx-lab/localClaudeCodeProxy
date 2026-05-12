package chatgptproxy

import (
	"encoding/json"
	"strings"
)

const defaultInstructions = "You are a helpful assistant."

// NormalizeRequestBody 把常见 OpenAI API 请求体整理成 Codex backend 接受的 Responses 形态。
//
// Codex backend 要求顶层 instructions，且订阅转发必须禁用 OpenAI Responses 存储。
// OpenCode 会在调用 OpenAI OAuth 时主动设置 options.instructions、store=false，
// 并清空 maxOutputTokens 以避免生成 max_output_tokens；
// 本代理需要兼容直接打 API 的客户端，因此在这里补齐。
func NormalizeRequestBody(path string, body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	if strings.Contains(path, "/chat/completions") {
		if out, ok := normalizeChatCompletions(body); ok {
			return out
		}
		return body
	}
	if strings.HasSuffix(path, "/responses") {
		if out, ok := ensureResponsesInstructions(body); ok {
			return out
		}
		return body
	}
	return body
}

func ensureResponsesInstructions(body []byte) ([]byte, bool) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false
	}
	if strings.TrimSpace(asString(parsed["instructions"])) == "" {
		if instructions := extractInstructionsFromInput(parsed["input"]); instructions != "" {
			parsed["instructions"] = instructions
			parsed["input"] = removeInstructionItems(parsed["input"])
		} else {
			parsed["instructions"] = defaultInstructions
		}
	}
	enforceNoStore(parsed)
	stripCodexUnsupportedParams(parsed)
	out, err := json.Marshal(parsed)
	if err != nil {
		return nil, false
	}
	return out, true
}

func normalizeChatCompletions(body []byte) ([]byte, bool) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false
	}

	next := make(map[string]any, len(parsed)+2)
	for k, v := range parsed {
		switch k {
		case "messages", "max_tokens", "max_completion_tokens", "n", "logit_bias":
			continue
		default:
			next[k] = v
		}
	}
	messages, _ := parsed["messages"].([]any)
	input := make([]any, 0, len(messages))
	var instructions []string
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := asString(msg["role"])
		content := msg["content"]
		switch role {
		case "system", "developer":
			text := contentText(content)
			if text != "" {
				instructions = append(instructions, text)
			}
		case "assistant", "user":
			input = append(input, map[string]any{
				"role":    role,
				"content": content,
			})
		case "tool":
			// Responses API 的 tool result 结构比 Chat Completions 更严格。
			// 这里保守地把 tool 消息作为 user 上下文传入，避免普通客户端直接 400。
			input = append(input, map[string]any{
				"role":    "user",
				"content": content,
			})
		}
	}
	if len(input) == 0 {
		input = []any{map[string]any{"role": "user", "content": ""}}
	}
	if len(instructions) > 0 {
		next["instructions"] = strings.Join(instructions, "\n")
	} else if strings.TrimSpace(asString(next["instructions"])) == "" {
		next["instructions"] = defaultInstructions
	}
	next["input"] = input
	enforceNoStore(next)
	stripCodexUnsupportedParams(next)

	out, err := json.Marshal(next)
	if err != nil {
		return nil, false
	}
	return out, true
}

func enforceNoStore(parsed map[string]any) {
	parsed["store"] = false
	stripInputItemIDs(parsed["input"])
}

func stripCodexUnsupportedParams(parsed map[string]any) {
	delete(parsed, "max_output_tokens")
	delete(parsed, "max_tokens")
	delete(parsed, "max_completion_tokens")
}

func stripInputItemIDs(input any) {
	items, ok := input.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		delete(msg, "id")
	}
}

func extractInstructionsFromInput(input any) string {
	items, ok := input.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := asString(msg["role"])
		if role != "system" && role != "developer" {
			continue
		}
		if text := contentText(msg["content"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func removeInstructionItems(input any) any {
	items, ok := input.([]any)
	if !ok {
		return input
	}
	filtered := make([]any, 0, len(items))
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		role := asString(msg["role"])
		if role == "system" || role == "developer" {
			continue
		}
		filtered = append(filtered, item)
	}
	if len(filtered) == 0 {
		return []any{map[string]any{"role": "user", "content": ""}}
	}
	return filtered
}

func contentText(content any) string {
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		var parts []string
		for _, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			t := asString(part["type"])
			if t != "" && t != "text" && t != "input_text" && t != "output_text" {
				continue
			}
			if text := strings.TrimSpace(asString(part["text"])); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func asString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}
