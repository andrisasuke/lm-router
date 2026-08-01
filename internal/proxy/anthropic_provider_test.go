package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/anthropic"
	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func anthropicProxyFixture(t *testing.T, upstream *httptest.Server, accounts ...store.Account) (http.Handler, *store.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, account := range accounts {
		if account.Provider == "" {
			account.Provider = store.ProviderAnthropicClaude
		}
		if account.ExpiresAt.IsZero() {
			account.ExpiresAt = time.Now().Add(8 * time.Hour)
		}
		account.Enabled = true
		if err := db.UpsertAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	tokens := codex.NewProviderTokenManager(db, nil, nil)
	client := anthropic.NewClient(upstream.URL+"/v1/messages", upstream.URL+"/api/oauth/usage", tokens, nil)
	client.SetHTTPClient(upstream.Client())
	return New(ServerConfig{Store: db, Anthropic: client, RequireKey: false}), db
}

func TestClaudeMessagesAreProxiedNativeWithResponseHeaders(t *testing.T) {
	wantBody := []byte(`{"model":"  ClAuDe-opus-4-6  ","system":[{"type":"text","text":"keep","cache_control":{"type":"ephemeral"}}],"thinking":{"type":"enabled","budget_tokens":1024},"tools":[{"name":"run","input_schema":{"type":"object"}}],"messages":[],"stream":false}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, wantBody) {
			t.Errorf("native body changed\ngot  %s\nwant %s", body, wantBody)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_native")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "7")
		_, _ = w.Write([]byte(`{"id":"msg_native","type":"message"}`))
	}))
	defer upstream.Close()
	handler, _ := anthropicProxyFixture(t, upstream, store.Account{ID: "claude", Name: "main", Priority: 1, AccessToken: "oauth"})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(wantBody))
	req.Header.Set("Anthropic-Beta", "client-beta")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Body.String() != `{"id":"msg_native","type":"message"}` {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if resp.Header().Get("Request-Id") != "req_native" || resp.Header().Get("Anthropic-Ratelimit-Requests-Remaining") != "7" {
		t.Fatalf("headers=%v", resp.Header())
	}
}

func TestClaudeCountTokensUsesNativeAnthropicEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"input_tokens":123}`))
	}))
	defer upstream.Close()
	handler, _ := anthropicProxyFixture(t, upstream, store.Account{ID: "claude", Name: "main", Priority: 1, AccessToken: "oauth"})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"claude-opus-4-6","messages":[]}`))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Body.String() != `{"input_tokens":123}` {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestModelPrefixRouteMatrixRejectsUnsupportedCombinations(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream should not be called")
	}))
	defer upstream.Close()
	handler, _ := anthropicProxyFixture(t, upstream)
	tests := []struct {
		path string
		body string
	}{
		{"/v1/responses", `{"model":" CLAUDE-opus-4-6 ","input":"hi"}`},
		{"/v1/chat/completions", `{"model":"claude-opus-4-6","messages":[]}`},
		{"/v1/messages", `{"model":"gemini-pro","messages":[]}`},
		{"/v1/messages/count_tokens", `{"model":"sonnet","messages":[]}`},
	}
	for _, test := range tests {
		req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d body=%s", test.path, resp.Code, resp.Body.String())
		}
	}
}

func TestClaudeFailoverMovesFrom429ToNextProviderAccount(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer first-token" {
			firstCalls.Add(1)
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`))
			return
		}
		secondCalls.Add(1)
		_, _ = w.Write([]byte(`{"id":"msg_second"}`))
	}))
	defer upstream.Close()
	handler, db := anthropicProxyFixture(t, upstream,
		store.Account{ID: "first", Name: "first", Priority: 1, AccessToken: "first-token"},
		store.Account{ID: "second", Name: "second", Priority: 2, AccessToken: "second-token"},
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6","messages":[]}`))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Body.String() != `{"id":"msg_second"}` || firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("status=%d body=%s first=%d second=%d", resp.Code, resp.Body.String(), firstCalls.Load(), secondCalls.Load())
	}
	first, _ := db.GetAccount(context.Background(), "first")
	second, _ := db.GetAccount(context.Background(), "second")
	if !first.CooldownUntil.Valid || first.ConsecutiveFailures != 1 {
		t.Fatalf("expected persisted first failure: %+v", first)
	}
	if first.Priority != 2 || second.Priority != 1 {
		t.Fatalf("fallback was not promoted: first=%d second=%d", first.Priority, second.Priority)
	}
	// Make the failed account available again. The promoted account must still
	// be selected first on the next request.
	if err := db.ResetFailureState(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6","messages":[]}`)))
	if resp.Code != http.StatusOK || firstCalls.Load() != 1 || secondCalls.Load() != 2 {
		t.Fatalf("next route status=%d first=%d second=%d", resp.Code, firstCalls.Load(), secondCalls.Load())
	}
}

func TestClaudePromotionSwapsFirstFailureWithThirdAccountOnly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer first-token", "Bearer middle-token":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"busy"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"msg_third"}`))
		}
	}))
	defer upstream.Close()
	handler, db := anthropicProxyFixture(t, upstream,
		store.Account{ID: "first", Name: "first", Priority: 1, AccessToken: "first-token"},
		store.Account{ID: "middle", Name: "middle", Priority: 2, AccessToken: "middle-token"},
		store.Account{ID: "third", Name: "third", Priority: 3, AccessToken: "third-token"},
	)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6","messages":[]}`)))
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	first, _ := db.GetAccount(context.Background(), "first")
	middle, _ := db.GetAccount(context.Background(), "middle")
	third, _ := db.GetAccount(context.Background(), "third")
	if first.Priority != 3 || middle.Priority != 2 || third.Priority != 1 {
		t.Fatalf("priorities first=%d middle=%d third=%d", first.Priority, middle.Priority, third.Priority)
	}
}

func TestClaudeCountTokensFallbackDoesNotPromote(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer first-token" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"limit"}`))
			return
		}
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer upstream.Close()
	handler, db := anthropicProxyFixture(t, upstream,
		store.Account{ID: "first", Name: "first", Priority: 1, AccessToken: "first-token"},
		store.Account{ID: "second", Name: "second", Priority: 2, AccessToken: "second-token"},
	)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"claude-opus-4-6","messages":[]}`)))
	first, _ := db.GetAccount(context.Background(), "first")
	second, _ := db.GetAccount(context.Background(), "second")
	if resp.Code != http.StatusOK || first.Priority != 1 || second.Priority != 2 {
		t.Fatalf("status=%d priorities first=%d second=%d", resp.Code, first.Priority, second.Priority)
	}
}

func TestClaudeNonRetryableErrorStopsWithoutCooldownOrPromotion(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid request"}`))
	}))
	defer upstream.Close()
	handler, db := anthropicProxyFixture(t, upstream,
		store.Account{ID: "first", Name: "first", Priority: 1, AccessToken: "first-token"},
		store.Account{ID: "second", Name: "second", Priority: 2, AccessToken: "second-token"},
	)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6","messages":[]}`)))
	first, _ := db.GetAccount(context.Background(), "first")
	if resp.Code != http.StatusBadRequest || calls.Load() != 1 || first.ConsecutiveFailures != 0 || first.CooldownUntil.Valid {
		t.Fatalf("status=%d calls=%d account=%+v", resp.Code, calls.Load(), first)
	}
}

func TestClaudeAllFailuresReturnLastUpstreamResponseWithoutPromotion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer first-token" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"first"}`))
			return
		}
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Request-Id", "last-request")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"last"}`))
	}))
	defer upstream.Close()
	handler, db := anthropicProxyFixture(t, upstream,
		store.Account{ID: "first", Name: "first", Priority: 1, AccessToken: "first-token"},
		store.Account{ID: "second", Name: "second", Priority: 2, AccessToken: "second-token"},
	)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6","messages":[]}`)))
	first, _ := db.GetAccount(context.Background(), "first")
	second, _ := db.GetAccount(context.Background(), "second")
	if resp.Code != http.StatusServiceUnavailable || resp.Body.String() != `{"error":"last"}` || resp.Header().Get("Request-Id") != "last-request" {
		t.Fatalf("status=%d headers=%v body=%s", resp.Code, resp.Header(), resp.Body.String())
	}
	if first.Priority != 1 || second.Priority != 2 || first.ConsecutiveFailures != 1 || second.ConsecutiveFailures != 1 {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func TestClaudeNetworkErrorFallsBackAndPersistsBackoff(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, account := range []store.Account{
		{ID: "first", Provider: store.ProviderAnthropicClaude, Name: "first", Priority: 1, Enabled: true, AccessToken: "first-token", ExpiresAt: time.Now().Add(8 * time.Hour)},
		{ID: "second", Provider: store.ProviderAnthropicClaude, Name: "second", Priority: 2, Enabled: true, AccessToken: "second-token", ExpiresAt: time.Now().Add(8 * time.Hour)},
	} {
		if err := db.UpsertAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	client := anthropic.NewClient("https://example.test/v1/messages", "https://example.test/api/oauth/usage", codex.NewProviderTokenManager(db, nil, nil), nil)
	client.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.Header.Get("Authorization") == "Bearer first-token" {
			return nil, errors.New("connection reset")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_after_network_error"}`)),
			Request:    request,
		}, nil
	})})
	handler := New(ServerConfig{Store: db, Anthropic: client, RequireKey: false})
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6","messages":[]}`)))
	first, _ := db.GetAccount(ctx, "first")
	second, _ := db.GetAccount(ctx, "second")
	if resp.Code != http.StatusOK || calls.Load() != 2 || first.ConsecutiveFailures != 1 || !first.CooldownUntil.Valid {
		t.Fatalf("status=%d calls=%d first=%+v", resp.Code, calls.Load(), first)
	}
	if first.Priority != 2 || second.Priority != 1 {
		t.Fatalf("priorities first=%d second=%d", first.Priority, second.Priority)
	}
}

func TestConcurrentClaudeFallbackCannotDoubleSwapPriorities(t *testing.T) {
	var firstArrivals atomic.Int32
	bothFirst := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer first-token" {
			if firstArrivals.Add(1) == 2 {
				close(bothFirst)
			}
			<-bothFirst
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"limit"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_second"}`))
	}))
	defer upstream.Close()
	handler, db := anthropicProxyFixture(t, upstream,
		store.Account{ID: "first", Name: "first", Priority: 1, AccessToken: "first-token"},
		store.Account{ID: "second", Name: "second", Priority: 2, AccessToken: "second-token"},
	)
	statusCh := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6","messages":[]}`)))
			statusCh <- resp.Code
		}()
	}
	wg.Wait()
	close(statusCh)
	for status := range statusCh {
		if status != http.StatusOK {
			t.Fatalf("status=%d", status)
		}
	}
	first, _ := db.GetAccount(context.Background(), "first")
	second, _ := db.GetAccount(context.Background(), "second")
	if first.Priority != 2 || second.Priority != 1 {
		t.Fatalf("double swap reversed priorities: first=%d second=%d", first.Priority, second.Priority)
	}
}

func TestClaudeStreamDoesNotSwitchAccountAfterSuccessStarts(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
	}))
	defer upstream.Close()
	handler, _ := anthropicProxyFixture(t, upstream,
		store.Account{ID: "first", Name: "first", Priority: 1, AccessToken: "first-token"},
		store.Account{ID: "second", Name: "second", Priority: 2, AccessToken: "second-token"},
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6","messages":[],"stream":true}`))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || calls.Load() != 1 || !strings.Contains(resp.Body.String(), "message_start") {
		t.Fatalf("status=%d calls=%d body=%s", resp.Code, calls.Load(), resp.Body.String())
	}
}

func TestClaudeModelCatalogIsAdvertisedButNotUsedForValidation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"future_model_ok"}`))
	}))
	defer upstream.Close()
	handler, _ := anthropicProxyFixture(t, upstream, store.Account{ID: "claude", Name: "main", Priority: 1, AccessToken: "oauth"})

	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsResp := httptest.NewRecorder()
	handler.ServeHTTP(modelsResp, modelsReq)
	for _, model := range []string{"claude-fable-5", "claude-sonnet-5", "claude-opus-4-8", "claude-opus-4-7", "claude-haiku-4-5-20251001"} {
		if !strings.Contains(modelsResp.Body.String(), model) {
			t.Fatalf("models missing %s: %s", model, modelsResp.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-future-99","messages":[]}`))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}
