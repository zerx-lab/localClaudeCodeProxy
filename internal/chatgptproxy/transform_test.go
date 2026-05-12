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
	for _, key := range []string{"max_output_tokens", "max_tokens", "max_completion_tokens"} {
		if _, exists := parsed[key]; exists {
			t.Fatalf("%s should be removed from normalized body", key)
		}
	}
}
