package proxy

import "testing"

// messagesToInput must convert a Chat Completions tool round-trip into the
// Responses API item shapes the backend accepts:
//   - assistant tool_calls  -> function_call items
//   - role "tool" results   -> function_call_output items (never role "tool")
func TestMessagesToInput_ToolRoundTrip(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "weather in Jakarta?"},
		map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{
				map[string]any{
					"id":       "call_abc",
					"type":     "function",
					"function": map[string]any{"name": "get_weather", "arguments": `{"city":"Jakarta"}`},
				},
			},
		},
		map[string]any{"role": "tool", "tool_call_id": "call_abc", "content": "sunny, 31C"},
	}

	items := messagesToInput(messages)

	var sawCall, sawOutput bool
	for _, it := range items {
		switch it["type"] {
		case "function_call":
			sawCall = true
			if it["call_id"] != "call_abc" || it["name"] != "get_weather" {
				t.Errorf("function_call fields wrong: %v", it)
			}
		case "function_call_output":
			sawOutput = true
			if it["call_id"] != "call_abc" || it["output"] != "sunny, 31C" {
				t.Errorf("function_call_output fields wrong: %v", it)
			}
		case "message":
			if it["role"] == "tool" {
				t.Error("role 'tool' must not survive into Responses input")
			}
		}
	}
	if !sawCall {
		t.Error("missing function_call item from assistant tool_calls")
	}
	if !sawOutput {
		t.Error("missing function_call_output item from tool result")
	}
}

func TestChatToolsToResponsesTools_Flattens(t *testing.T) {
	in := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "Get weather",
				"parameters":  map[string]any{"type": "object"},
			},
		},
	}
	out := chatToolsToResponsesTools(in)
	if len(out) != 1 {
		t.Fatalf("want 1 tool, got %d", len(out))
	}
	tool := out[0]
	if tool["type"] != "function" || tool["name"] != "get_weather" {
		t.Errorf("flatten failed: %v", tool)
	}
	if _, nested := tool["function"]; nested {
		t.Error("nested 'function' key should be removed after flatten")
	}
	if chatToolsToResponsesTools(nil) != nil {
		t.Error("nil tools should return nil")
	}
}

func TestChatUsageFromResponses_Maps(t *testing.T) {
	final := map[string]any{
		"usage": map[string]any{
			"input_tokens":  float64(93),
			"output_tokens": float64(19),
			"total_tokens":  float64(112),
		},
	}
	u := chatUsageFromResponses(final)
	if u == nil {
		t.Fatal("expected usage")
	}
	if u["prompt_tokens"].(float64) != 93 || u["completion_tokens"].(float64) != 19 || u["total_tokens"].(float64) != 112 {
		t.Errorf("usage mapping wrong: %v", u)
	}
	if chatUsageFromResponses(nil) != nil {
		t.Error("nil final should return nil usage")
	}
}

func TestContentToString(t *testing.T) {
	if got := contentToString("plain"); got != "plain" {
		t.Errorf("string: got %q", got)
	}
	blocks := []any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "text", "text": "b"},
	}
	if got := contentToString(blocks); got != "ab" {
		t.Errorf("blocks: got %q", got)
	}
}
