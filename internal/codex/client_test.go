package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/store"
)

func TestOpenResponsesStreamForwardsSafeCodexIdentityHeaders(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	account := store.Account{
		ID:           "acct_1",
		Provider:     store.ProviderOpenAICodex,
		Name:         "main",
		Enabled:      true,
		AccessToken:  "upstream-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
		MetadataJSON: `{"chatgpt_account_id":"workspace-account"}`,
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	requestHeaders := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHeaders <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	inbound := make(http.Header)
	inbound.Set("Authorization", "Bearer local-router-key")
	inbound.Set("Cookie", "session=local")
	inbound.Set("ChatGPT-Account-Id", "client-supplied-account")
	inbound.Set("User-Agent", "codex_exec/0.149.1")
	inbound.Set("Originator", "codex_exec")
	inbound.Set("Session-Id", "session-1")
	inbound.Set("Thread-Id", "thread-1")
	inbound.Set("X-Client-Request-Id", "request-1")
	inbound.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	inbound.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	inbound.Set("X-Unrelated", "do-not-forward")

	client := NewClient(upstream.URL, NewTokenManager(db, nil))
	stream, err := client.OpenResponsesStream(ctx, ExecuteParams{
		Account: account,
		Body:    []byte(`{"model":"gpt-5.3-codex","input":"ping"}`),
		Headers: inbound,
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_ = stream.Body.Close()

	got := <-requestHeaders
	for key, want := range map[string]string{
		"Authorization":                          "Bearer upstream-token",
		"ChatGPT-Account-Id":                     "workspace-account",
		"User-Agent":                             "codex_exec/0.149.1",
		"Originator":                             "codex_exec",
		"Session-Id":                             "session-1",
		"Thread-Id":                              "thread-1",
		"X-Client-Request-Id":                    "request-1",
		"X-Codex-Beta-Features":                  "remote_compaction_v2",
		"X-OpenAI-Internal-Codex-Responses-Lite": "true",
	} {
		if value := got.Get(key); value != want {
			t.Errorf("%s=%q want %q", key, value, want)
		}
	}
	if got.Get("Cookie") != "" || got.Get("X-Unrelated") != "" {
		t.Fatalf("unsafe headers forwarded: %v", got)
	}
	formatted := formatHeaders(got)
	if strings.Contains(formatted, "upstream-token") || strings.Contains(formatted, "workspace-account") {
		t.Fatalf("sensitive identity leaked in formatted headers: %s", formatted)
	}
}

func TestPrepareRequestEnablesNativeWebSearchForCodexCustomProvider(t *testing.T) {
	headers := make(http.Header)
	headers.Set(CodexResponsesLiteHeader, "true")
	headers.Set(RouterWebSearchHeader, "live")

	body, forwardedHeaders, err := prepareRequest([]byte(`{
		"model":"gpt-5.6-sol",
		"input":[{"role":"user","content":[{"type":"input_text","text":"latest news"}]}],
		"include":["reasoning.encrypted_content"]
	}`), headers)
	if err != nil {
		t.Fatalf("prepare request: %v", err)
	}
	if got := forwardedHeaders.Get(RouterWebSearchHeader); got != "" {
		t.Fatalf("private router header was forwarded=%q", got)
	}
	if got := forwardedHeaders.Get(CodexResponsesLiteHeader); got != "" {
		t.Fatalf("responses-lite header was retained after adding hosted tools=%q", got)
	}
	if headers.Get(CodexResponsesLiteHeader) != "true" || headers.Get(RouterWebSearchHeader) != "live" {
		t.Fatal("prepare request mutated inbound headers")
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%#v", payload["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "web_search" || tool["external_web_access"] != true {
		t.Fatalf("web search tool=%#v", tool)
	}
	include, _ := payload["include"].([]any)
	if len(include) != 2 || include[0] != "reasoning.encrypted_content" || include[1] != "web_search_call.action.sources" {
		t.Fatalf("include=%#v", payload["include"])
	}
}

func TestPrepareRequestLeavesGenericAndDisabledSearchRequestsUnchanged(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)

	generic, genericHeaders, err := prepareRequest(body, make(http.Header))
	if err != nil {
		t.Fatalf("prepare generic request: %v", err)
	}
	var genericPayload map[string]any
	if err := json.Unmarshal(generic, &genericPayload); err != nil {
		t.Fatalf("decode generic request: %v", err)
	}
	if _, ok := genericPayload["tools"]; ok {
		t.Fatalf("generic request unexpectedly gained tools=%#v", genericPayload["tools"])
	}
	if genericHeaders.Get(RouterWebSearchHeader) != "" {
		t.Fatalf("generic headers=%v", genericHeaders)
	}

	disabledHeaders := make(http.Header)
	disabledHeaders.Set(CodexResponsesLiteHeader, "true")
	disabledHeaders.Set(RouterWebSearchHeader, "disabled")
	disabled, forwardedHeaders, err := prepareRequest(body, disabledHeaders)
	if err != nil {
		t.Fatalf("prepare disabled request: %v", err)
	}
	var disabledPayload map[string]any
	if err := json.Unmarshal(disabled, &disabledPayload); err != nil {
		t.Fatalf("decode disabled request: %v", err)
	}
	if _, ok := disabledPayload["tools"]; ok {
		t.Fatalf("disabled request unexpectedly gained tools=%#v", disabledPayload["tools"])
	}
	if forwardedHeaders.Get(CodexResponsesLiteHeader) != "true" || forwardedHeaders.Get(RouterWebSearchHeader) != "" {
		t.Fatalf("disabled forwarded headers=%v", forwardedHeaders)
	}
}

func TestOpenResponsesStreamKeepsAccountIdentityAfterTokenRefresh(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	account := store.Account{
		ID:           "acct_1",
		Provider:     store.ProviderOpenAICodex,
		Name:         "main",
		Enabled:      true,
		AccessToken:  "expired-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
		MetadataJSON: `{"chatgpt_account_id":"workspace-account"}`,
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	attempts := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		attempts <- auth + "|" + r.Header.Get("ChatGPT-Account-Id")
		if auth == "Bearer expired-token" {
			http.Error(w, `{"error":{"message":"expired"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	refresher := RefreshFunc(func(context.Context, string) (TokenSet, error) {
		return TokenSet{AccessToken: "fresh-token", RefreshToken: "fresh-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	client := NewClient(upstream.URL, NewTokenManager(db, refresher))
	stream, err := client.OpenResponsesStream(ctx, ExecuteParams{Account: account, Body: []byte(`{"model":"gpt-5.3-codex","input":"ping"}`)})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_ = stream.Body.Close()

	for _, want := range []string{
		"Bearer expired-token|workspace-account",
		"Bearer fresh-token|workspace-account",
	} {
		if got := <-attempts; got != want {
			t.Fatalf("upstream identity=%q want %q", got, want)
		}
	}
}

func TestApplyCodexProtocolHeadersUsesSafeFallbacksForMissingMetadata(t *testing.T) {
	dst := make(http.Header)
	inbound := make(http.Header)
	inbound.Set("User-Agent", "openai-python/2.0")
	inbound.Set("X-Client-Request-Id", "generic-request")
	applyCodexProtocolHeaders(dst, inbound, store.Account{ID: "acct_fallback", MetadataJSON: "{"})

	if got := dst.Get("Session-Id"); got != "acct_fallback" {
		t.Fatalf("session id=%q", got)
	}
	if got := dst.Get("User-Agent"); got != "codex-cli/1.0.18 (macOS; arm64)" {
		t.Fatalf("generic client changed upstream user agent=%q", got)
	}
	if got := dst.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("generic request header was forwarded=%q", got)
	}
	if got := dst.Get("ChatGPT-Account-Id"); got != "" {
		t.Fatalf("unexpected account id=%q", got)
	}
}

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
