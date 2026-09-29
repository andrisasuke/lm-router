package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/customprovider"
	"github.com/andrisasuke/lm-router/internal/store"
)

func newTestDBWithKey(t *testing.T) (*store.DB, string) {
	t.Helper()
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	key, err := db.CreateAPIKey(context.Background(), "test")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return db, key.Secret
}

func customTestAccount(id, prefix, baseURL, compatType, apiType string) store.Account {
	return store.Account{
		ID: id, Provider: store.ProviderCustom, Name: id, Enabled: true,
		AccessToken: "sk-custom-test", Prefix: prefix, BaseURL: baseURL,
		CompatType: compatType, APIType: apiType,
	}
}

func TestCustomProviderReportsAccountActivity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			db, apiKey := newTestDBWithKey(t)
			var active atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if !active.Load() {
					t.Error("account should be active while custom upstream is handling the request")
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
					return
				}
				_, _ = w.Write([]byte(`{"id":"custom_active"}`))
			}))
			defer upstream.Close()

			if err := db.UpsertAccount(context.Background(), customTestAccount("custom", "myapi", upstream.URL, store.CompatOpenAIStyle, store.CustomAPITypeChat)); err != nil {
				t.Fatal(err)
			}
			events := &activityEvents{}
			handler := New(ServerConfig{
				Store: db, Codex: codex.NewClient("http://invalid", codex.NewTokenManager(db, nil)), RequireKey: true,
				OnAccountActivity: func(accountID string, requesting bool) {
					active.Store(requesting)
					events.callback(accountID, requesting)
				},
			})
			body := `{"model":"myapi/gpt-4o","messages":[],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+apiKey)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if active.Load() {
				t.Fatal("account should be idle after custom request finishes")
			}
			if got, want := events.snapshot(), []string{"custom:active", "custom:idle"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("events=%v, want %v", got, want)
			}
		})
	}
}

func TestCustomProviderChatCompletionsPassesThroughAndStripsPrefix(t *testing.T) {
	db, apiKey := newTestDBWithKey(t)
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		gotModel, _ = payload["model"].(string)
		w.Write([]byte(`{"id":"upstream-1","choices":[]}`))
	}))
	defer upstream.Close()

	if err := db.UpsertAccount(context.Background(), customTestAccount("c1", "myapi", upstream.URL, store.CompatOpenAIStyle, store.CustomAPITypeChat)); err != nil {
		t.Fatal(err)
	}
	srv := New(ServerConfig{Store: db, Codex: codex.NewClient("http://invalid", codex.NewTokenManager(db, nil)), RequireKey: true})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"myapi/gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream-1") {
		t.Fatalf("expected upstream body forwarded verbatim, got %s", rec.Body.String())
	}
	if gotModel != "gpt-4o-mini" {
		t.Fatalf("upstream saw model=%q, want prefix stripped", gotModel)
	}
}

func TestCustomProviderAnthropicMessagesPassthroughUsesAPIKeyHeader(t *testing.T) {
	db, apiKey := newTestDBWithKey(t)
	var gotAPIKey, gotAuth, gotBeta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("Anthropic-Beta")
		w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer upstream.Close()

	if err := db.UpsertAccount(context.Background(), customTestAccount("c1", "myclaude", upstream.URL, store.CompatAnthropicStyle, "")); err != nil {
		t.Fatal(err)
	}
	srv := New(ServerConfig{Store: db, Codex: codex.NewClient("http://invalid", codex.NewTokenManager(db, nil)), RequireKey: true})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"myclaude/claude-3","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotAPIKey != "sk-custom-test" {
		t.Fatalf("x-api-key=%q", gotAPIKey)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization should be unset, got %q", gotAuth)
	}
	if gotBeta != "" {
		t.Fatalf("no forced Anthropic-Beta header expected, got %q", gotBeta)
	}
}

func TestCustomProviderWrongEndpointReturns400(t *testing.T) {
	db, apiKey := newTestDBWithKey(t)
	if err := db.UpsertAccount(context.Background(), customTestAccount("c1", "myapi", "http://unused", store.CompatOpenAIStyle, store.CustomAPITypeChat)); err != nil {
		t.Fatal(err)
	}
	srv := New(ServerConfig{Store: db, Codex: codex.NewClient("http://invalid", codex.NewTokenManager(db, nil)), RequireKey: true})

	// This connection is chat-only; hitting /v1/responses must 400, not dispatch.
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"myapi/gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCustomProviderUnknownPrefixReturns400(t *testing.T) {
	db, apiKey := newTestDBWithKey(t)
	srv := New(ServerConfig{Store: db, Codex: codex.NewClient("http://invalid", codex.NewTokenManager(db, nil)), RequireKey: true})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"nope/foo","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown custom provider prefix") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCustomProviderDisabledConnectionReturns400(t *testing.T) {
	db, apiKey := newTestDBWithKey(t)
	account := customTestAccount("c1", "myapi", "http://unused", store.CompatOpenAIStyle, store.CustomAPITypeChat)
	account.Enabled = false
	if err := db.UpsertAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	srv := New(ServerConfig{Store: db, Codex: codex.NewClient("http://invalid", codex.NewTokenManager(db, nil)), RequireKey: true})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"myapi/gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "disabled") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCustomProviderPropagatesUpstreamErrorVerbatim(t *testing.T) {
	db, apiKey := newTestDBWithKey(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer upstream.Close()

	if err := db.UpsertAccount(context.Background(), customTestAccount("c1", "myapi", upstream.URL, store.CompatOpenAIStyle, store.CustomAPITypeChat)); err != nil {
		t.Fatal(err)
	}
	srv := New(ServerConfig{Store: db, Codex: codex.NewClient("http://invalid", codex.NewTokenManager(db, nil)), RequireKey: true})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"myapi/gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "slow down") {
		t.Fatalf("expected verbatim upstream body, got %s", rec.Body.String())
	}

	// No cooldown bookkeeping: single connection per prefix, nothing to fail over to.
	account, err := db.GetAccount(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if account.CooldownUntil.Valid {
		t.Fatalf("expected no cooldown recorded for a custom connection, got %+v", account.CooldownUntil)
	}
}

func TestCustomProviderCountTokensUsesLocalEstimateForAnthropicCompat(t *testing.T) {
	db, apiKey := newTestDBWithKey(t)
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	if err := db.UpsertAccount(context.Background(), customTestAccount("c1", "myclaude", upstream.URL, store.CompatAnthropicStyle, "")); err != nil {
		t.Fatal(err)
	}
	srv := New(ServerConfig{Store: db, Codex: codex.NewClient("http://invalid", codex.NewTokenManager(db, nil)), RequireKey: true})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"myclaude/claude-3","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Fatal("expected local estimate, upstream should not be called")
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["input_tokens"]; !ok {
		t.Fatalf("body=%v", body)
	}
}

func TestCustomProviderCountTokensRejectsOpenAICompat(t *testing.T) {
	db, apiKey := newTestDBWithKey(t)
	if err := db.UpsertAccount(context.Background(), customTestAccount("c1", "myapi", "http://unused", store.CompatOpenAIStyle, store.CustomAPITypeChat)); err != nil {
		t.Fatal(err)
	}
	srv := New(ServerConfig{Store: db, Codex: codex.NewClient("http://invalid", codex.NewTokenManager(db, nil)), RequireKey: true})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"myapi/gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestGptAndClaudeModelsStillRouteWhenNoSlashPresent is a regression test:
// resolveModelRoute must not change existing behavior for plain (no "/")
// gpt*/claude* model ids.
func TestGptAndClaudeModelsStillRouteWhenNoSlashPresent(t *testing.T) {
	db, apiKey := newTestDBWithKey(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	if err := db.UpsertAccount(context.Background(), store.Account{
		ID: "codex-1", Provider: store.ProviderOpenAICodex, Name: "one", Priority: 1, Enabled: true,
		AccessToken: "token-1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	srv := New(ServerConfig{Store: db, Codex: codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)), RequireKey: true, Custom: customprovider.NewClient(nil)})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.3-codex","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}
