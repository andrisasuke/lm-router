package proxy

import (
	"context"
	"encoding/json"
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
		t.Fatalf("expected not 401 with x-api-key, got 401 body=%s", rec.Body.String())
	}
	if rec.Code == http.StatusNotFound {
		t.Fatalf("route not found (404), route must be registered")
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

	if rec.Code == http.StatusNotFound {
		t.Fatalf("alias /v1/v1/messages returned 404, route must be registered")
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
