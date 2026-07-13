package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/store"
)

func TestFetchQuotaUsesGPT55ProbeModel(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	account := store.Account{
		ID:           "acct_1",
		Provider:     "openai-codex",
		Name:         "main",
		Enabled:      true,
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	requestBody := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody <- body
		w.Header().Set("x-codex-primary-used-percent", "10")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	client := NewClient(upstream.URL, NewTokenManager(db, nil))
	if _, err := client.FetchQuota(ctx, account); err != nil {
		t.Fatalf("fetch quota: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(<-requestBody, &body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if got := body["model"]; got != "gpt-5.5" {
		t.Errorf("probe model: got %v, want gpt-5.5", got)
	}
}

// SSE sample mirroring a real backend reply that emits a function_call.
const functionCallSSE = `event: response.output_item.added
data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_abc","name":"get_weather"},"output_index":0,"sequence_number":2}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","delta":"{\"city\":\"Jakarta\"}","item_id":"fc_1","output_index":0,"sequence_number":3}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","arguments":"{\"city\":\"Jakarta\"}","item_id":"fc_1","output_index":0,"sequence_number":4}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","status":"completed","arguments":"{\"city\":\"Jakarta\"}","call_id":"call_abc","name":"get_weather"},"output_index":0,"sequence_number":5}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":93,"output_tokens":19,"total_tokens":112}}}

data: [DONE]
`

const textSSE = `event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello there"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_2","object":"response","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}

data: [DONE]
`

func TestConvertResponsesSSEToItems_FunctionCall(t *testing.T) {
	items, final := ConvertResponsesSSEToItems([]byte(functionCallSSE))
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	item := items[0]
	if item["type"] != "function_call" {
		t.Fatalf("want function_call, got %v", item["type"])
	}
	if item["name"] != "get_weather" {
		t.Errorf("name: want get_weather, got %v", item["name"])
	}
	if item["call_id"] != "call_abc" {
		t.Errorf("call_id: want call_abc, got %v", item["call_id"])
	}
	if item["arguments"] != `{"city":"Jakarta"}` {
		t.Errorf("arguments: got %v", item["arguments"])
	}
	if final == nil {
		t.Fatal("expected final response.completed object")
	}
	usage, _ := final["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 112 {
		t.Errorf("usage total_tokens: got %v", usage["total_tokens"])
	}
}

func TestOutputTextFromItems_IgnoresFunctionCall(t *testing.T) {
	items, _ := ConvertResponsesSSEToItems([]byte(functionCallSSE))
	if got := OutputTextFromItems(items); got != "" {
		t.Errorf("function_call should yield no text, got %q", got)
	}

	items, _ = ConvertResponsesSSEToItems([]byte(textSSE))
	if got := OutputTextFromItems(items); got != "Hello there" {
		t.Errorf("text: want %q, got %q", "Hello there", got)
	}
}
