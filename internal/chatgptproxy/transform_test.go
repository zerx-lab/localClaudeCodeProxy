package chatgptproxy

import (
	"encoding/json"
	"testing"
)

func TestNormalizeResponsesAddsInstructions(t *testing.T) {
	body := []byte(`{"model":"gpt-5.3-codex","input":"hello"}`)

	out := NormalizeRequestBody("/v1/responses", body)

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("normalized body is not json: %v", err)
	}
	if parsed["instructions"] != defaultInstructions {
		t.Fatalf("instructions = %q, want %q", parsed["instructions"], defaultInstructions)
	}
	if parsed["store"] != false {
		t.Fatalf("store = %v, want false", parsed["store"])
	}
	if parsed["input"] != "hello" {
		t.Fatalf("input = %v, want hello", parsed["input"])
	}
}

func TestNormalizeResponsesForcesStoreFalseAndDropsInputIDs(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.3-codex",
		"store":true,
		"max_output_tokens":123,
		"max_tokens":456,
		"max_completion_tokens":789,
		"prompt_cache_retention":"in_memory",
		"instructions":"be useful",
		"input":[{"id":"msg_123","role":"user","content":"hello"}]
	}`)

	out := NormalizeRequestBody("/v1/responses", body)

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("normalized body is not json: %v", err)
	}
	if parsed["store"] != false {
		t.Fatalf("store = %v, want false", parsed["store"])
	}
	assertTokenLimitsRemoved(t, parsed)
	input := parsed["input"].([]any)
	msg := input[0].(map[string]any)
	if _, exists := msg["id"]; exists {
		t.Fatalf("input item id should be removed: %#v", msg)
	}
}

func TestNormalizeResponsesConvertsTextContentBlocks(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.3-codex-spark",
		"instructions":"be useful",
		"input":[
			{"role":"user","content":[{"type":"text","text":"hello"}]},
			{"role":"assistant","content":[{"type":"text","text":"hi"}]}
		]
	}`)

	out := NormalizeRequestBody("/v1/responses", body)

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("normalized body is not json: %v", err)
	}
	input := parsed["input"].([]any)
	user := input[0].(map[string]any)
	userContent := user["content"].([]any)
	userText := userContent[0].(map[string]any)
	if userText["type"] != "input_text" {
		t.Fatalf("user content type = %v, want input_text", userText["type"])
	}
	assistant := input[1].(map[string]any)
	assistantContent := assistant["content"].([]any)
	assistantText := assistantContent[0].(map[string]any)
	if assistantText["type"] != "output_text" {
		t.Fatalf("assistant content type = %v, want output_text", assistantText["type"])
	}
}

func TestNormalizeResponsesPromotesChatCompletionTools(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.3-codex-spark",
		"instructions":"be useful",
		"input":"hello",
		"tools":[{
			"type":"function",
			"function":{
				"name":"search_project",
				"description":"Search the project",
				"parameters":{"type":"object","properties":{"query":{"type":"string"}}},
				"strict":true
			}
		}],
		"tool_choice":{"type":"function","function":{"name":"search_project"}}
	}`)

	out := NormalizeRequestBody("/v1/responses", body)

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("normalized body is not json: %v", err)
	}
	tools := parsed["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "search_project" {
		t.Fatalf("tool name = %v, want search_project", tool["name"])
	}
	if _, exists := tool["function"]; exists {
		t.Fatalf("nested function should be removed: %#v", tool)
	}
	if _, exists := tool["parameters"]; !exists {
		t.Fatalf("parameters should be promoted: %#v", tool)
	}
	if tool["strict"] != true {
		t.Fatalf("strict = %v, want true", tool["strict"])
	}
	choice := parsed["tool_choice"].(map[string]any)
	if choice["name"] != "search_project" {
		t.Fatalf("tool choice name = %v, want search_project", choice["name"])
	}
	if _, exists := choice["function"]; exists {
		t.Fatalf("nested tool choice function should be removed: %#v", choice)
	}
}

func TestNormalizeResponsesDropsStreamOptionsForSparkModels(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.3-codex-spark",
		"stream":true,
		"stream_options":{"include_usage":true},
		"temperature":0.7,
		"input":"hello"
	}`)

	out := NormalizeRequestBody("/v1/responses", body)

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("normalized body is not json: %v", err)
	}
	if _, exists := parsed["stream_options"]; exists {
		t.Fatalf("stream_options should be removed for spark models: %#v", parsed)
	}
	if _, exists := parsed["temperature"]; exists {
		t.Fatalf("temperature should be removed for spark models: %#v", parsed)
	}
}

func TestNormalizeResponsesKeepsStreamOptionsForNonSparkModels(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.3-codex",
		"stream":true,
		"stream_options":{"include_usage":true},
		"temperature":0.7,
		"input":"hello"
	}`)

	out := NormalizeRequestBody("/v1/responses", body)

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("normalized body is not json: %v", err)
	}
	if _, exists := parsed["stream_options"]; !exists {
		t.Fatalf("stream_options should be kept for non-spark models: %#v", parsed)
	}
	if _, exists := parsed["temperature"]; !exists {
		t.Fatalf("temperature should be kept for non-spark models: %#v", parsed)
	}
}

func TestNormalizeChatCompletionsConvertsSystemToInstructions(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.3-codex",
		"stream":true,
		"max_tokens":123,
		"messages":[
			{"role":"system","content":"be terse"},
			{"role":"user","content":"hello"}
		]
	}`)

	out := NormalizeRequestBody("/v1/chat/completions", body)

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("normalized body is not json: %v", err)
	}
	if parsed["instructions"] != "be terse" {
		t.Fatalf("instructions = %q, want be terse", parsed["instructions"])
	}
	assertTokenLimitsRemoved(t, parsed)
	if parsed["store"] != false {
		t.Fatalf("store = %v, want false", parsed["store"])
	}
	if _, exists := parsed["messages"]; exists {
		t.Fatalf("messages should be removed from normalized body")
	}
	input, ok := parsed["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input len = %d, want 1", len(input))
	}
	msg := input[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "hello" {
		t.Fatalf("input[0] = %#v", msg)
	}
}

func assertTokenLimitsRemoved(t *testing.T, parsed map[string]any) {
	t.Helper()
	for _, key := range []string{"max_output_tokens", "max_tokens", "max_completion_tokens", "prompt_cache_retention"} {
		if _, exists := parsed[key]; exists {
			t.Fatalf("%s should be removed from normalized body", key)
		}
	}
}
