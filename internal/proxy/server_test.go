package proxy

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestAnthropicMessagesTranslatesImageBlock(t *testing.T) {
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
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{
		Store:      db,
		Codex:      codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		RequireKey: true,
	})

	body := `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},` +
		`{"type":"text","text":"describe"}` +
		`]}]}`
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
	msg, ok := input[0].(map[string]any)
	if !ok {
		t.Fatalf("expected input[0] object, got %v", input[0])
	}
	content, ok := msg["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("expected content len=2, got %v", msg["content"])
	}
	imgBlock, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("expected content[0] object, got %v", content[0])
	}
	if imgBlock["type"] != "input_image" {
		t.Fatalf("expected type=input_image, got %v", imgBlock["type"])
	}
	if imgBlock["image_url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("expected data url, got %v", imgBlock["image_url"])
	}
	textBlock, ok := content[1].(map[string]any)
	if !ok {
		t.Fatalf("expected content[1] object, got %v", content[1])
	}
	if textBlock["type"] != "input_text" || textBlock["text"] != "describe" {
		t.Fatalf("expected input_text describe, got %v", textBlock)
	}
}

func TestAnthropicMessagesTranslatesImageURL(t *testing.T) {
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
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{
		Store:      db,
		Codex:      codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		RequireKey: true,
	})

	body := `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}` +
		`]}]}`
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
	input, _ := upstreamBody["input"].([]any)
	if len(input) == 0 {
		t.Fatalf("expected input, got %v", upstreamBody["input"])
	}
	msg, _ := input[0].(map[string]any)
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected content len=1, got %v", content)
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "input_image" || block["image_url"] != "https://example.com/x.png" {
		t.Fatalf("expected image url block, got %v", block)
	}
}

func TestAnthropicMessagesHoistsImageFromToolResult(t *testing.T) {
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
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{
		Store:      db,
		Codex:      codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		RequireKey: true,
	})

	body := `{"model":"claude-3-5-sonnet","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"read run.png"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/run.png"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
		`{"type":"text","text":"image dimensions: 100x100"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}` +
		`]}]}` +
		`]}`
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
		t.Fatalf("parse upstream: %v", err)
	}
	input, _ := upstreamBody["input"].([]any)
	if len(input) < 4 {
		t.Fatalf("expected >=4 input items (user, fn_call, fn_call_output, hoisted_user_image), got %d: %v", len(input), input)
	}

	// Item 2 = function_call_output with text-only output
	fnOut, _ := input[2].(map[string]any)
	if fnOut["type"] != "function_call_output" {
		t.Fatalf("expected input[2] function_call_output, got %v", fnOut["type"])
	}
	output, _ := fnOut["output"].(string)
	if !strings.Contains(output, "image dimensions") {
		t.Fatalf("expected text content preserved in output, got %q", output)
	}
	if strings.Contains(output, "AAAA") || strings.Contains(output, "base64") {
		t.Fatalf("expected image data stripped from output, got %q", output)
	}

	// Item 3 = synthetic user message containing input_image
	hoisted, _ := input[3].(map[string]any)
	if hoisted["type"] != "message" || hoisted["role"] != "user" {
		t.Fatalf("expected input[3] user message, got %v", hoisted)
	}
	content, _ := hoisted["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected hoisted content len=1, got %v", content)
	}
	imgBlock, _ := content[0].(map[string]any)
	if imgBlock["type"] != "input_image" {
		t.Fatalf("expected type=input_image, got %v", imgBlock["type"])
	}
	if imgBlock["image_url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("expected data url, got %v", imgBlock["image_url"])
	}
}

func TestAnthropicMessagesToolResultWithoutImagePassesThrough(t *testing.T) {
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
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{
		Store:      db,
		Codex:      codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		RequireKey: true,
	})

	body := `{"model":"claude-3-5-sonnet","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"read x"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/x.txt"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"hello world"}]}` +
		`]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key.Secret)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var upstreamBody map[string]any
	_ = json.Unmarshal(captured, &upstreamBody)
	input, _ := upstreamBody["input"].([]any)
	// No hoisted message should be added when tool_result has no image
	for i, item := range input {
		m, _ := item.(map[string]any)
		if m["type"] == "message" && m["role"] == "user" {
			content, _ := m["content"].([]any)
			for _, c := range content {
				cm, _ := c.(map[string]any)
				if cm["type"] == "input_image" {
					t.Fatalf("unexpected hoisted input_image at input[%d]: %v", i, m)
				}
			}
		}
	}
	// function_call_output should carry "hello world"
	for _, item := range input {
		m, _ := item.(map[string]any)
		if m["type"] == "function_call_output" {
			if m["output"] != "hello world" {
				t.Fatalf("expected output='hello world', got %v", m["output"])
			}
			return
		}
	}
	t.Fatalf("function_call_output not found in input: %v", input)
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

func TestAnthropicCountTokens(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil { t.Fatalf("open store: %v", err) }
	defer db.Close()

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil { t.Fatalf("create key: %v", err) }

	srv := New(ServerConfig{Store:db, Codex:codex.NewClient("http://invalid", codex.NewTokenManager(db,nil)), RequireKey:true})

	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"Hello world this is a test"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("x-api-key", key.Secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v", err)
	}
	tokens, ok := resp["input_tokens"].(float64)
	if !ok || tokens < 5 {
		t.Fatalf("expected input_tokens >= 5, got %v", resp)
	}
}

func TestAnthropicMessagesUsesExistingAccountFallback(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil { t.Fatalf("open store: %v", err) }
	defer db.Close()

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil { t.Fatalf("create key: %v", err) }

	for _, acct := range []store.Account{
		{ID:"acct_1", Provider:"openai-codex", Name:"one", Priority:1, Enabled:true, AccessToken:"token-1", RefreshToken:"r1", ExpiresAt:time.Now().Add(time.Hour)},
		{ID:"acct_2", Provider:"openai-codex", Name:"two", Priority:2, Enabled:true, AccessToken:"token-2", RefreshToken:"r2", ExpiresAt:time.Now().Add(time.Hour)},
	} {
		if err := db.UpsertAccount(ctx, acct); err != nil { t.Fatalf("upsert: %v", err) }
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer token-1" {
			http.Error(w, `{"error":{"type":"usage_limit_reached","resets_in_seconds":60}}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store:db, Codex:codex.NewClient(upstream.URL, codex.NewTokenManager(db,nil)), RequireKey:true})

	body := `{"model":"gpt-5.5","max_tokens":16,"messages":[{"role":"user","content":"Say pong"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", key.Secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v", err)
	}
	content, _ := resp["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content in response: %v", resp)
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	if text != "pong" {
		t.Fatalf("expected content[0].text=pong, got %q", text)
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

func TestAnthropicMessagesForwardsThinking(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil { t.Fatalf("open store: %v", err) }
	defer db.Close()

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil { t.Fatalf("create key: %v", err) }
	acct := store.Account{ID:"acct_1", Provider:"openai-codex", Name:"one", Priority:1, Enabled:true, AccessToken:"token-1", RefreshToken:"r1", ExpiresAt:time.Now().Add(time.Hour)}
	if err := db.UpsertAccount(ctx, acct); err != nil { t.Fatalf("upsert: %v", err) }

	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store:db, Codex:codex.NewClient(upstream.URL, codex.NewTokenManager(db,nil)), RequireKey:true})

	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":16000}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", key.Secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var upstreamBody map[string]any
	if err := json.Unmarshal(captured, &upstreamBody); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	reasoning, ok := upstreamBody["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("expected reasoning field in upstream body, got %v", upstreamBody)
	}
	if reasoning["effort"] != "high" {
		t.Fatalf("expected reasoning.effort=high, got %v", reasoning["effort"])
	}
	if reasoning["summary"] != "auto" {
		t.Fatalf("expected reasoning.summary=auto, got %v", reasoning["summary"])
	}
}

func TestAnthropicMessagesForwardsAdaptiveThinking(t *testing.T) {
	db, _, apiKey := setupAnthropicTest(t)

	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive","budget_tokens":0}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var upstreamBody map[string]any
	if err := json.Unmarshal(captured, &upstreamBody); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	reasoning, ok := upstreamBody["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("expected reasoning field for adaptive thinking, got %v", upstreamBody)
	}
	if reasoning["summary"] != "auto" {
		t.Fatalf("expected reasoning.summary=auto, got %v", reasoning["summary"])
	}
	if reasoning["effort"] == nil {
		t.Fatalf("expected reasoning.effort to be set for adaptive, got nil")
	}
}

func TestAnthropicMessagesEmitsThinkingThenText(t *testing.T) {
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
		_, _ = w.Write([]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking step 1\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\" step 2\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store:db, Codex:codex.NewClient(upstream.URL, codex.NewTokenManager(db,nil)), RequireKey:true})

	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", key.Secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	respBody := rec.Body.String()
	// Payload content — checked with Contains because map key order in JSON is non-deterministic.
	for _, want := range []string{"thinking_delta", "thinking step 1", "text_delta", "answer", `"output_tokens":5`, `"input_tokens":10`} {
		if !strings.Contains(respBody, want) {
			t.Fatalf("expected %q in response:\n%s", want, respBody)
		}
	}
	// Event sequence — use "event: X" header lines which are deterministically ordered.
	checkEventOrder := func(markers []string) {
		lastIdx := -1
		for _, m := range markers {
			idx := strings.Index(respBody[lastIdx+1:], m)
			if idx < 0 {
				t.Fatalf("missing event %q in response:\n%s", m, respBody)
			}
			lastIdx = lastIdx + 1 + idx
		}
	}
	checkEventOrder([]string{
		"event: message_start",
		"event: content_block_start",  // thinking
		"event: content_block_stop",   // thinking
		"event: content_block_start",  // text
		"event: content_block_delta",  // text
		"event: content_block_stop",   // text
		"event: message_delta",
		"event: message_stop",
	})
	// Thinking block must appear before text block.
	if strings.Index(respBody, "thinking_delta") > strings.Index(respBody, "text_delta") {
		t.Fatalf("expected thinking before text:\n%s", respBody)
	}
}

func TestAnthropicMessagesExtractsUsage(t *testing.T) {
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
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":42,\"output_tokens\":7}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store:db, Codex:codex.NewClient(upstream.URL, codex.NewTokenManager(db,nil)), RequireKey:true})

	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", key.Secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	respBody := rec.Body.String()
	if !strings.Contains(respBody, `"output_tokens":7`) {
		t.Fatalf("expected output_tokens:7 in response:\n%s", respBody)
	}
	if !strings.Contains(respBody, `"input_tokens":42`) {
		t.Fatalf("expected input_tokens:42 in response:\n%s", respBody)
	}
	if strings.Contains(respBody, `"thinking"`) {
		t.Fatalf("expected no thinking block in response:\n%s", respBody)
	}
	if !strings.Contains(respBody, "message_delta") {
		t.Fatalf("expected message_delta in response:\n%s", respBody)
	}
	if !strings.Contains(respBody, "message_stop") {
		t.Fatalf("expected message_stop in response:\n%s", respBody)
	}
}

func setupAnthropicTest(t *testing.T) (*store.DB, store.Account, string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	acct := store.Account{ID: "acct_1", Provider: "openai-codex", Name: "one", Priority: 1, Enabled: true, AccessToken: "token-1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.UpsertAccount(ctx, acct); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	return db, acct, key.Secret
}

func TestAnthropicMessagesForwardsTools(t *testing.T) {
	db, _, apiKey := setupAnthropicTest(t)

	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

	body := `{
		"model": "gpt-5.5",
		"messages": [{"role": "user", "content": "search something"}],
		"tools": [{"name": "web_search_exa", "description": "search", "input_schema": {"type": "object", "properties": {"query": {"type": "string"}}, "required": ["query"]}}],
		"tool_choice": {"type": "auto"},
		"stream": true
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var upstreamBody map[string]any
	if err := json.Unmarshal(captured, &upstreamBody); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	tools, ok := upstreamBody["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("expected tools array in upstream body, got %v", upstreamBody["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("expected tool to be object, got %v", tools[0])
	}
	if tool["type"] != "function" {
		t.Fatalf("expected tool.type=function, got %v", tool["type"])
	}
	if tool["name"] != "web_search_exa" {
		t.Fatalf("expected tool.name=web_search_exa, got %v", tool["name"])
	}
	params, ok := tool["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("expected tool.parameters to be object, got %v", tool["parameters"])
	}
	if params["type"] != "object" {
		t.Fatalf("expected parameters.type=object, got %v", params["type"])
	}
	if upstreamBody["tool_choice"] != "auto" {
		t.Fatalf("expected tool_choice=auto (string), got %v", upstreamBody["tool_choice"])
	}
}

func TestAnthropicMessagesAugmentsReadToolDescription(t *testing.T) {
	db, _, apiKey := setupAnthropicTest(t)

	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

	body := `{
		"model": "gpt-5.5",
		"messages": [{"role": "user", "content": "read x"}],
		"tools": [
			{"name": "Read", "description": "Read a file.", "input_schema": {"type": "object", "properties": {"file_path": {"type": "string"}, "pages": {"type": "string"}}, "required": ["file_path"]}},
			{"name": "Bash", "description": "Run shell.", "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}
		],
		"stream": true
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var upstreamBody map[string]any
	if err := json.Unmarshal(captured, &upstreamBody); err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	tools, _ := upstreamBody["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}

	// Read tool — description must contain the CODEX_GUARDRAIL for pages.
	readTool, _ := tools[0].(map[string]any)
	if readTool["name"] != "Read" {
		t.Fatalf("expected first tool=Read, got %v", readTool["name"])
	}
	readDesc, _ := readTool["description"].(string)
	if !strings.Contains(readDesc, "CODEX_GUARDRAIL") {
		t.Fatalf("expected Read description to contain CODEX_GUARDRAIL, got %q", readDesc)
	}
	if !strings.Contains(readDesc, "pages") || !strings.Contains(readDesc, ".pdf") {
		t.Fatalf("expected Read description to mention pages+.pdf, got %q", readDesc)
	}
	// Schema must have pages STRIPPED — Codex auto-injects empty values, breaking Read.
	readParams, _ := readTool["parameters"].(map[string]any)
	readProps, _ := readParams["properties"].(map[string]any)
	if _, ok := readProps["pages"]; ok {
		t.Fatalf("expected pages stripped from Read schema, got %v", readProps)
	}
	if _, ok := readProps["file_path"]; !ok {
		t.Fatalf("expected file_path preserved in Read schema, got %v", readProps)
	}

	// Bash tool — description must be untouched (no CODEX_GUARDRAIL).
	bashTool, _ := tools[1].(map[string]any)
	if bashTool["name"] != "Bash" {
		t.Fatalf("expected second tool=Bash, got %v", bashTool["name"])
	}
	bashDesc, _ := bashTool["description"].(string)
	if strings.Contains(bashDesc, "CODEX_GUARDRAIL") {
		t.Fatalf("expected Bash description untouched, got %q", bashDesc)
	}
	if bashDesc != "Run shell." {
		t.Fatalf("expected Bash description=\"Run shell.\", got %q", bashDesc)
	}
}

func TestAnthropicMessagesTranslatesToolChoiceAny(t *testing.T) {
	db, _, apiKey := setupAnthropicTest(t)

	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

	body := `{
		"model": "gpt-5.5",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"name": "web_search_exa", "description": "search"}],
		"tool_choice": {"type": "any"},
		"stream": true
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var upstreamBody map[string]any
	if err := json.Unmarshal(captured, &upstreamBody); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	if upstreamBody["tool_choice"] != "required" {
		t.Fatalf("expected tool_choice=required, got %v", upstreamBody["tool_choice"])
	}
}

func TestAnthropicMessagesTranslatesToolChoiceSpecific(t *testing.T) {
	db, _, apiKey := setupAnthropicTest(t)

	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

	body := `{
		"model": "gpt-5.5",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"name": "web_search_exa", "description": "search"}],
		"tool_choice": {"type": "tool", "name": "web_search_exa"},
		"stream": true
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var upstreamBody map[string]any
	if err := json.Unmarshal(captured, &upstreamBody); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	tc, ok := upstreamBody["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("expected tool_choice to be object, got %v", upstreamBody["tool_choice"])
	}
	if tc["type"] != "function" {
		t.Fatalf("expected tool_choice.type=function, got %v", tc["type"])
	}
	if tc["name"] != "web_search_exa" {
		t.Fatalf("expected tool_choice.name=web_search_exa, got %v", tc["name"])
	}
}

func TestAnthropicMessagesPreservesMultiTurnToolHistory(t *testing.T) {
	db, _, apiKey := setupAnthropicTest(t)

	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

	body := `{
		"model": "gpt-5.5",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "search AI news"}]},
			{"role": "assistant", "content": [
				{"type": "text", "text": "Let me search."},
				{"type": "tool_use", "id": "toolu_01abc", "name": "web_search_exa", "input": {"query": "AI news"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_01abc", "content": "[{\"title\":\"AI News\",\"url\":\"https://ai.com\"}]"}
			]}
		],
		"stream": true
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var upstreamBody map[string]any
	if err := json.Unmarshal(captured, &upstreamBody); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	input, ok := upstreamBody["input"].([]any)
	if !ok || len(input) < 4 {
		t.Fatalf("expected at least 4 input items, got %v", upstreamBody["input"])
	}

	item0, _ := input[0].(map[string]any)
	if item0["type"] != "message" || item0["role"] != "user" {
		t.Fatalf("input[0] expected user message, got %v", item0)
	}

	item1, _ := input[1].(map[string]any)
	if item1["type"] != "message" || item1["role"] != "assistant" {
		t.Fatalf("input[1] expected assistant message, got %v", item1)
	}

	item2, _ := input[2].(map[string]any)
	if item2["type"] != "function_call" {
		t.Fatalf("input[2] expected function_call, got %v", item2)
	}
	if item2["call_id"] != "toolu_01abc" {
		t.Fatalf("input[2].call_id expected toolu_01abc, got %v", item2["call_id"])
	}
	if item2["name"] != "web_search_exa" {
		t.Fatalf("input[2].name expected web_search_exa, got %v", item2["name"])
	}

	item3, _ := input[3].(map[string]any)
	if item3["type"] != "function_call_output" {
		t.Fatalf("input[3] expected function_call_output, got %v", item3)
	}
	if item3["call_id"] != "toolu_01abc" {
		t.Fatalf("input[3].call_id expected toolu_01abc, got %v", item3["call_id"])
	}
}

func TestAnthropicMessagesTranslatesToolChoiceNone(t *testing.T) {
	db, _, apiKey := setupAnthropicTest(t)

	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

	body := `{
		"model": "gpt-5.5",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"name": "test", "description": "t", "input_schema": {"type": "object", "properties": {}}}],
		"tool_choice": {"type": "none"},
		"stream": true
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var upstreamBody map[string]any
	if err := json.Unmarshal(captured, &upstreamBody); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	if upstreamBody["tool_choice"] != "none" {
		t.Fatalf("expected tool_choice=none (string), got %v", upstreamBody["tool_choice"])
	}
}

func TestAnthropicMessagesNonStreamingWithTools(t *testing.T) {
	db, _, apiKey := setupAnthropicTest(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"web_search_exa","arguments":""}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"query\":\"AI news\"}"}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":3}}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"search AI news"}],"tools":[{"name":"web_search_exa","description":"search","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body2 := rec.Body.String()
	if !strings.Contains(body2, `"type":"tool_use"`) {
		t.Fatalf("expected tool_use block: %s", body2)
	}
	if !strings.Contains(body2, `"name":"web_search_exa"`) {
		t.Fatalf("expected tool name: %s", body2)
	}
	if !strings.Contains(body2, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected stop_reason tool_use: %s", body2)
	}
}

func TestAnthropicMessagesEmitsToolUseStream(t *testing.T) {
	db, _, apiKey := setupAnthropicTest(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"web_search_exa\",\"arguments\":\"\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{\\\"que\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"ry\\\":\\\"x\\\"}\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"web_search_exa\",\"arguments\":\"{\\\"query\\\":\\\"x\\\"}\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":3}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"search"}],"tools":[{"name":"web_search_exa","description":"search"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	respBody := rec.Body.String()
	// The delta string `{"que` is JSON-encoded in the SSE stream as `{\"que`
	for _, want := range []string{"tool_use", "web_search_exa", "call_1", "input_json_delta", `{\"que`} {
		if !strings.Contains(respBody, want) {
			t.Fatalf("expected %q in response:\n%s", want, respBody)
		}
	}
	if !strings.Contains(respBody, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected stop_reason=tool_use in response:\n%s", respBody)
	}

	checkEventOrder := func(markers []string) {
		lastIdx := -1
		for _, m := range markers {
			idx := strings.Index(respBody[lastIdx+1:], m)
			if idx < 0 {
				t.Fatalf("missing event %q in response:\n%s", m, respBody)
			}
			lastIdx = lastIdx + 1 + idx
		}
	}
	checkEventOrder([]string{
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	})
}

func TestAnthropicMessagesEmitsThinkingSignature(t *testing.T) {
	db, _, apiKey := setupAnthropicTest(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"sky is blue\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"Because of Rayleigh scattering.\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"Why is sky blue?"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	respBody := rec.Body.String()
	if !strings.Contains(respBody, "signature_delta") {
		t.Fatalf("expected signature_delta event in response:\n%s", respBody)
	}
	if strings.Contains(respBody, `"signature":""`) {
		t.Fatalf("expected non-empty signature value, got empty string in:\n%s", respBody)
	}
	if !strings.Contains(respBody, `"signature":`) {
		t.Fatalf("expected signature key in response:\n%s", respBody)
	}
	// signature_delta must appear before content_block_stop for the thinking block
	sigIdx := strings.Index(respBody, "signature_delta")
	// Find first content_block_stop after a thinking_delta
	thinkIdx := strings.Index(respBody, "thinking_delta")
	stopIdx := strings.Index(respBody[thinkIdx:], "content_block_stop")
	if stopIdx < 0 {
		t.Fatalf("expected content_block_stop after thinking_delta:\n%s", respBody)
	}
	stopIdx += thinkIdx
	if sigIdx > stopIdx {
		t.Fatalf("signature_delta must appear before content_block_stop for thinking block:\n%s", respBody)
	}
}

func TestAnthropicMessagesNonStreamingIncludesThinking(t *testing.T) {
	db, _, apiKey := setupAnthropicTest(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"sky is blue\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"Because of Rayleigh scattering.\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"Why is sky blue?"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json error: %v", err)
	}
	content, ok := resp["content"].([]any)
	if !ok || len(content) < 2 {
		t.Fatalf("expected at least 2 content blocks, got %v", resp["content"])
	}
	thinkingBlock, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("expected content[0] to be object, got %v", content[0])
	}
	if thinkingBlock["type"] != "thinking" {
		t.Fatalf("expected content[0].type=thinking, got %v", thinkingBlock["type"])
	}
	thinking, _ := thinkingBlock["thinking"].(string)
	if thinking == "" {
		t.Fatalf("expected non-empty thinking content, got empty string")
	}
	sig, _ := thinkingBlock["signature"].(string)
	if sig == "" {
		t.Fatalf("expected non-empty signature in thinking block, got empty string")
	}
	textBlock, ok := content[1].(map[string]any)
	if !ok {
		t.Fatalf("expected content[1] to be object, got %v", content[1])
	}
	if textBlock["type"] != "text" {
		t.Fatalf("expected content[1].type=text, got %v", textBlock["type"])
	}
}

func TestAnthropicMessagesAcceptsThinkingInHistory(t *testing.T) {
	db, _, apiKey := setupAnthropicTest(t)

	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"Sunsets are red.\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":5}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

	body := `{
		"model": "gpt-5.5",
		"messages": [
			{"role":"user","content":"Why is sky blue?"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"Some reasoning","signature":"fakesig123"},
				{"type":"text","text":"Because Rayleigh scattering."}
			]},
			{"role":"user","content":"What about sunsets?"}
		],
		"stream": false
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var upstreamBody map[string]any
	if err := json.Unmarshal(captured, &upstreamBody); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	input, ok := upstreamBody["input"].([]any)
	if !ok || len(input) == 0 {
		t.Fatalf("expected input array, got %v", upstreamBody["input"])
	}
	// Upstream input must not contain any thinking-typed items
	for i, item := range input {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "thinking" || m["type"] == "redacted_thinking" {
			t.Fatalf("input[%d] unexpectedly contains thinking block: %v", i, m)
		}
		// Also check inside message content arrays
		if content, ok := m["content"].([]any); ok {
			for j, c := range content {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if cm["type"] == "thinking" || cm["type"] == "redacted_thinking" {
					t.Fatalf("input[%d].content[%d] unexpectedly contains thinking block: %v", i, j, cm)
				}
			}
		}
	}
	// The assistant text block must be present in the upstream input
	foundAssistantText := false
	for _, item := range input {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] != "message" || m["role"] != "assistant" {
			continue
		}
		if content, ok := m["content"].([]any); ok {
			for _, c := range content {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if cm["type"] == "output_text" {
					foundAssistantText = true
				}
			}
		}
	}
	if !foundAssistantText {
		t.Fatalf("expected assistant output_text block in upstream input, got %v", input)
	}
}

func TestAnthropicMessagesEmitsThinkingFromAlternateReasoningEvents(t *testing.T) {
	cases := []string{
		"response.reasoning_text.delta",
		"response.reasoning.delta",
		"response.reasoning_summary_part.delta",
		"response.reasoning_part.delta",
	}
	for _, evType := range cases {
		t.Run(evType, func(t *testing.T) {
			db, _, apiKey := setupAnthropicTest(t)

			eventLine := fmt.Sprintf("data: {\"type\":\"%s\",\"delta\":\"alt thinking\"}\n\n", evType)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(eventLine))
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer.\"}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}))
			defer upstream.Close()

			srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

			body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":true}`
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			respBody := rec.Body.String()
			for _, want := range []string{"thinking_delta", "alt thinking", "signature_delta"} {
				if !strings.Contains(respBody, want) {
					t.Fatalf("expected %q in response for %s:\n%s", want, evType, respBody)
				}
			}
		})
	}
}

func TestAnthropicMessagesNonStreamingThinkingFromAlternateReasoningEvents(t *testing.T) {
	cases := []string{
		"response.reasoning_text.delta",
		"response.reasoning.delta",
		"response.reasoning_summary_part.delta",
		"response.reasoning_part.delta",
	}
	for _, evType := range cases {
		t.Run(evType, func(t *testing.T) {
			db, _, apiKey := setupAnthropicTest(t)

			eventLine := fmt.Sprintf("data: {\"type\":\"%s\",\"delta\":\"alt thinking\"}\n\n", evType)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(eventLine))
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer.\"}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}))
			defer upstream.Close()

			srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

			body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":false}`
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("json: %v", err)
			}
			content, ok := resp["content"].([]any)
			if !ok || len(content) < 2 {
				t.Fatalf("expected at least 2 content blocks for %s, got %v", evType, resp["content"])
			}
			thinkingBlock, _ := content[0].(map[string]any)
			if thinkingBlock["type"] != "thinking" {
				t.Fatalf("expected content[0].type=thinking for %s, got %v", evType, thinkingBlock["type"])
			}
			if thinkingBlock["thinking"] != "alt thinking" {
				t.Fatalf("expected thinking text 'alt thinking' for %s, got %v", evType, thinkingBlock["thinking"])
			}
			if sig, _ := thinkingBlock["signature"].(string); sig == "" {
				t.Fatalf("expected non-empty signature for %s", evType)
			}
		})
	}
}

func TestAnthropicMessagesStreamUsageAlwaysIncludesCacheFields(t *testing.T) {
	db, _, apiKey := setupAnthropicTest(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true})

	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	respBody := rec.Body.String()
	for _, want := range []string{
		`"cache_read_input_tokens":0`,
		`"cache_creation_input_tokens":0`,
		`"input_tokens":3`,
		`"output_tokens":1`,
	} {
		if !strings.Contains(respBody, want) {
			t.Fatalf("expected %q in response:\n%s", want, respBody)
		}
	}
}

func TestChatContentToCodex_textString(t *testing.T) {
	out := chatContentToCodex("user", "hello world")
	if len(out) != 1 {
		t.Fatalf("expected 1 part, got %d", len(out))
	}
	if out[0]["type"] != "input_text" || out[0]["text"] != "hello world" {
		t.Errorf("unexpected part: %+v", out[0])
	}
}

func TestChatContentToCodex_assistantString(t *testing.T) {
	out := chatContentToCodex("assistant", "ok")
	if out[0]["type"] != "output_text" {
		t.Errorf("assistant text should be output_text, got %v", out[0]["type"])
	}
}

func TestChatContentToCodex_imageURLObject(t *testing.T) {
	raw := []any{
		map[string]any{"type": "text", "text": "what is this?"},
		map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:image/jpeg;base64,AAAA"},
		},
	}
	out := chatContentToCodex("user", raw)
	if len(out) != 2 {
		t.Fatalf("expected 2 parts, got %d: %+v", len(out), out)
	}
	if out[0]["type"] != "input_text" {
		t.Errorf("part 0 type: %v", out[0]["type"])
	}
	if out[1]["type"] != "input_image" {
		t.Errorf("part 1 type: %v", out[1]["type"])
	}
	if out[1]["image_url"] != "data:image/jpeg;base64,AAAA" {
		t.Errorf("part 1 image_url: %v", out[1]["image_url"])
	}
}

func TestChatContentToCodex_imageURLString(t *testing.T) {
	raw := []any{
		map[string]any{"type": "image_url", "image_url": "https://example.com/x.png"},
	}
	out := chatContentToCodex("user", raw)
	if len(out) != 1 || out[0]["type"] != "input_image" {
		t.Fatalf("expected input_image, got %+v", out)
	}
	if out[0]["image_url"] != "https://example.com/x.png" {
		t.Errorf("image_url: %v", out[0]["image_url"])
	}
}

func TestChatContentToCodex_assistantDropsImage(t *testing.T) {
	raw := []any{
		map[string]any{"type": "text", "text": "here"},
		map[string]any{"type": "image_url", "image_url": "https://example.com/x.png"},
	}
	out := chatContentToCodex("assistant", raw)
	if len(out) != 1 {
		t.Fatalf("expected only text part (image dropped for assistant), got %d", len(out))
	}
	if out[0]["type"] != "output_text" {
		t.Errorf("assistant content type: %v", out[0]["type"])
	}
}

func TestMessagesToInput_textOnly(t *testing.T) {
	raw := []any{
		map[string]any{"role": "user", "content": "hello"},
	}
	out := messagesToInput(raw)
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}
	content := out[0]["content"].([]map[string]any)
	if content[0]["text"] != "hello" {
		t.Errorf("text: %v", content[0]["text"])
	}
}

func TestMessagesToInput_withImage(t *testing.T) {
	raw := []any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "what is this"},
				map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "data:image/png;base64,XYZ"},
				},
			},
		},
	}
	out := messagesToInput(raw)
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}
	content := out[0]["content"].([]map[string]any)
	if len(content) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(content))
	}
	if content[1]["type"] != "input_image" {
		t.Errorf("image part type: %v", content[1]["type"])
	}
}

func TestMessagesToInput_skipsSystem(t *testing.T) {
	raw := []any{
		map[string]any{"role": "system", "content": "you are helpful"},
		map[string]any{"role": "user", "content": "hi"},
	}
	out := messagesToInput(raw)
	if len(out) != 1 {
		t.Fatalf("expected 1 message (system skipped), got %d", len(out))
	}
	if out[0]["role"] != "user" {
		t.Errorf("expected user role, got %v", out[0]["role"])
	}
}
