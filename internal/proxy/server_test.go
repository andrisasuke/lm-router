package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
)

func TestResponsesFallsBackToSecondAccount(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	for _, acct := range []store.Account{
		{ID: "acct_1", Provider: "openai-codex", Name: "one", Priority: 1, Enabled: true, AccessToken: "token-1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)},
		{ID: "acct_2", Provider: "openai-codex", Name: "two", Priority: 2, Enabled: true, AccessToken: "token-2", RefreshToken: "r2", ExpiresAt: time.Now().Add(time.Hour)},
	} {
		if err := db.UpsertAccount(ctx, acct); err != nil {
			t.Fatalf("upsert account: %v", err)
		}
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer token-1" {
			http.Error(w, `{"error":{"type":"usage_limit_reached","resets_in_seconds":60}}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{
		Store:      db,
		Codex:      codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		RequireKey: true,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.3-codex","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+key.Secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("expected second account response, got %s", rec.Body.String())
	}
}

func TestV1RequiresLocalAPIKey(t *testing.T) {
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient("http://invalid", codex.NewTokenManager(db, nil)), RequireKey: true})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json error: %v", err)
	}
	if body["error"] == nil {
		t.Fatalf("expected openai-shaped error, got %v", body)
	}
}

func TestResponsesNonStreamingReturnsJSON(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	acct := store.Account{ID: "acct_1", Provider: "openai-codex", Name: "one", Priority: 1, Enabled: true, AccessToken: "token-1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.UpsertAccount(ctx, acct); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\" world\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.3-codex","input":"hi","stream":false}`))
	req.Header.Set("Authorization", "Bearer "+key.Secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type=%s", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json error: %v", err)
	}
	if body["object"] != "response" {
		t.Fatalf("unexpected body: %v", body)
	}
	output, ok := body["output"].([]any)
	if !ok || len(output) == 0 {
		t.Fatalf("missing output: %v", body)
	}
}

func TestAnthropicMessagesRejectsMissingAuth(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	srv := New(ServerConfig{
		Store:      db,
		Codex:      codex.NewClient("http://invalid", codex.NewTokenManager(db, nil)),
		RequireKey: true,
	})

	body := `{"model":"gpt-5.5","max_tokens":16,"messages":[{"role":"user","content":"Say pong"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json error: %v", err)
	}
	if resp["type"] != "error" {
		t.Fatalf("expected top-level type=error, got %v", resp)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %v", resp)
	}
	if errObj["type"] != "authentication_error" {
		t.Fatalf("expected authentication_error, got %v", errObj["type"])
	}
}

func TestAnthropicMessagesAcceptsXAPIKey(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	acct := store.Account{ID: "acct_1", Provider: "openai-codex", Name: "one", Priority: 1, Enabled: true, AccessToken: "token-1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.UpsertAccount(ctx, acct); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{
		Store:      db,
		Codex:      codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		RequireKey: true,
	})

	body := `{"model":"gpt-5.5","max_tokens":16,"messages":[{"role":"user","content":"Say pong"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key.Secret)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("x-api-key auth rejected: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusNotFound {
		t.Fatalf("route not registered: status=%d", rec.Code)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAnthropicMessagesAliasV1V1(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	acct := store.Account{ID: "acct_1", Provider: "openai-codex", Name: "one", Priority: 1, Enabled: true, AccessToken: "token-1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.UpsertAccount(ctx, acct); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{
		Store:      db,
		Codex:      codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		RequireKey: true,
	})

	body := `{"model":"gpt-5.5","max_tokens":16,"messages":[{"role":"user","content":"Say pong"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key.Secret)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("x-api-key auth rejected: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusNotFound {
		t.Fatalf("alias /v1/v1/messages returned 404, route must be registered")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAnthropicMessagesTranslatesSystemAndStringContent(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	acct := store.Account{ID: "acct_1", Provider: "openai-codex", Name: "one", Priority: 1, Enabled: true, AccessToken: "token-1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.UpsertAccount(ctx, acct); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{
		Store:      db,
		Codex:      codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		RequireKey: true,
	})

	body := `{"model":"claude-3-5-sonnet","system":"You are helpful","messages":[{"role":"user","content":"ping"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key.Secret)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var upstreamBody map[string]any
	if err := json.Unmarshal(captured, &upstreamBody); err != nil {
		t.Fatalf("failed to parse upstream body: %v", err)
	}
	if upstreamBody["instructions"] != "You are helpful" {
		t.Fatalf("expected instructions='You are helpful', got %v", upstreamBody["instructions"])
	}
	input, ok := upstreamBody["input"].([]any)
	if !ok || len(input) == 0 {
		t.Fatalf("expected input array, got %v", upstreamBody["input"])
	}
	firstMsg, ok := input[0].(map[string]any)
	if !ok {
		t.Fatalf("expected input[0] to be object, got %v", input[0])
	}
	if firstMsg["role"] != "user" {
		t.Fatalf("expected role=user, got %v", firstMsg["role"])
	}
}

func TestAnthropicMessagesTranslatesTextBlocks(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	acct := store.Account{ID: "acct_1", Provider: "openai-codex", Name: "one", Priority: 1, Enabled: true, AccessToken: "token-1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.UpsertAccount(ctx, acct); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{
		Store:      db,
		Codex:      codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		RequireKey: true,
	})

	body := `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":[{"type":"text","text":"hello world"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key.Secret)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var upstreamBody map[string]any
	if err := json.Unmarshal(captured, &upstreamBody); err != nil {
		t.Fatalf("failed to parse upstream body: %v", err)
	}
	input, ok := upstreamBody["input"].([]any)
	if !ok || len(input) == 0 {
		t.Fatalf("expected input array, got %v", upstreamBody["input"])
	}
	firstMsg, ok := input[0].(map[string]any)
	if !ok {
		t.Fatalf("expected input[0] to be object, got %v", input[0])
	}
	content, ok := firstMsg["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("expected content array, got %v", firstMsg["content"])
	}
	block, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("expected content[0] to be object, got %v", content[0])
	}
	if block["text"] != "hello world" {
		t.Fatalf("expected text='hello world', got %v", block["text"])
	}
}

func TestAnthropicMessagesReturnsAnthropicMessageJSON(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	acct := store.Account{ID: "acct_1", Provider: "openai-codex", Name: "one", Priority: 1, Enabled: true, AccessToken: "token-1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.UpsertAccount(ctx, acct); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{
		Store:      db,
		Codex:      codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		RequireKey: true,
	})

	body := `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"Say pong"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key.Secret)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json error: %v", err)
	}
	if resp["type"] != "message" {
		t.Fatalf("expected type=message, got %v", resp["type"])
	}
	if resp["role"] != "assistant" {
		t.Fatalf("expected role=assistant, got %v", resp["role"])
	}
	content, ok := resp["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("expected content array, got %v", resp["content"])
	}
	block, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("expected content[0] to be object, got %v", content[0])
	}
	if block["type"] != "text" {
		t.Fatalf("expected content[0].type=text, got %v", block["type"])
	}
	if block["text"] != "pong" {
		t.Fatalf("expected content[0].text=pong, got %v", block["text"])
	}
}

func TestAnthropicMessagesStreamingEvents(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil { t.Fatalf("open store: %v", err) }
	defer db.Close()

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil { t.Fatalf("create key: %v", err) }
	acct := store.Account{ID:"acct_1", Provider:"openai-codex", Name:"one", Priority:1, Enabled:true, AccessToken:"token-1", RefreshToken:"r1", ExpiresAt:time.Now().Add(time.Hour)}
	if err := db.UpsertAccount(ctx, acct); err != nil { t.Fatalf("upsert: %v", err) }

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store:db, Codex:codex.NewClient(upstream.URL, codex.NewTokenManager(db,nil)), RequireKey:true})

	body := `{"model":"gpt-5.5","max_tokens":16,"messages":[{"role":"user","content":"Say pong"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", key.Secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type=%s", ct)
	}
	respBody := rec.Body.String()
	// Assert all required event types present in correct order
	events := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	lastIdx := -1
	for _, event := range events {
		idx := strings.Index(respBody, event)
		if idx < 0 {
			t.Fatalf("missing event %q in response:\n%s", event, respBody)
		}
		if idx <= lastIdx {
			t.Fatalf("event %q out of order in response:\n%s", event, respBody)
		}
		lastIdx = idx
	}
	// Assert delta contains "pong"
	if !strings.Contains(respBody, "pong") {
		t.Fatalf("expected delta pong in response:\n%s", respBody)
	}
}

func TestChatCompletionsStreamReturnsDone(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	acct := store.Account{ID: "acct_1", Provider: "openai-codex", Name: "one", Priority: 1, Enabled: true, AccessToken: "token-1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.UpsertAccount(ctx, acct); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.3-codex","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+key.Secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("expected DONE marker, got %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("expected final stop chunk, got %s", rec.Body.String())
	}
	if strings.Index(rec.Body.String(), `"finish_reason":"stop"`) > strings.Index(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("finish chunk must be emitted before DONE, got %s", rec.Body.String())
	}
}
