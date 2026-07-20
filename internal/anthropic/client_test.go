package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
)

func testAccount(t *testing.T, db *store.DB) store.Account {
	t.Helper()
	account := store.Account{
		ID: "claude-1", Provider: store.ProviderAnthropicClaude, Name: "main", Priority: 1, Enabled: true,
		AccessToken: "provider-token", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(8 * time.Hour),
	}
	if err := db.UpsertAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	return account
}

func TestMessagesPassesBodyAndAllowedClaudeCodeHeaders(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	account := testAccount(t, db)
	wantBody := []byte(`{"model":"claude-opus-4-6","system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"thinking":{"type":"enabled","budget_tokens":2048},"messages":[],"stream":false}`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != string(wantBody) {
			t.Errorf("body changed\ngot  %s\nwant %s", body, wantBody)
		}
		if r.URL.Query().Get("beta") != "true" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer provider-token" {
			t.Errorf("authorization=%q", got)
		}
		if got := r.Header.Get("x-api-key"); got != "" {
			t.Errorf("local x-api-key leaked: %q", got)
		}
		if got := r.Header.Get("Anthropic-Version"); got != AnthropicVersion {
			t.Errorf("version=%q", got)
		}
		beta := r.Header.Get("Anthropic-Beta")
		for _, value := range []string{"client-feature", OAuthBeta, ClaudeCodeBeta} {
			if !strings.Contains(beta, value) {
				t.Errorf("beta %q missing %q", beta, value)
			}
		}
		if got := r.Header.Get("X-Stainless-Lang"); got != "js" {
			t.Errorf("stainless=%q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "claude-code/2.1.214" {
			t.Errorf("user-agent=%q", got)
		}
		if got := r.Header.Get("X-App"); got != "cli" {
			t.Errorf("x-app=%q", got)
		}
		if got := r.Header.Get("X-Claude-Code-Version"); got != "2.1.214" {
			t.Errorf("claude-code-version=%q", got)
		}
		if got := r.Header.Get("Anthropic-Future-Capability"); got != "preserved" {
			t.Errorf("future anthropic header=%q", got)
		}
		if got := r.Header.Get("X-Not-Allowed"); got != "" {
			t.Errorf("unexpected header forwarded: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_123")
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer server.Close()

	manager := codex.NewProviderTokenManager(db, nil, nil)
	client := NewClient(server.URL+"/v1/messages", server.URL+"/usage", manager, nil)
	client.SetHTTPClient(server.Client())
	header := http.Header{
		"Authorization":               []string{"Bearer local-router-key"},
		"X-Api-Key":                   []string{"local-router-key"},
		"Anthropic-Beta":              []string{"client-feature"},
		"Anthropic-Future-Capability": []string{"preserved"},
		"User-Agent":                  []string{"claude-code/2.1.214"},
		"X-App":                       []string{"cli"},
		"X-Stainless-Lang":            []string{"js"},
		"X-Claude-Code-Version":       []string{"2.1.214"},
		"X-Not-Allowed":               []string{"secret"},
	}
	result, err := client.ExecuteMessages(ctx, ExecuteParams{Account: account, Body: wantBody, Header: header})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != http.StatusOK || string(result.Body) != `{"id":"msg_1"}` || result.Header.Get("Request-Id") != "req_123" {
		t.Fatalf("result=%+v", result)
	}
}

func TestMessagesRefreshesOnceAfterUnauthorized(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	account := testAccount(t, db)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "" {
			t.Errorf("synthetic user-agent=%q", got)
		}
		call := calls.Add(1)
		if call == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"authentication_error"}}`))
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer refreshed-access" {
			t.Errorf("authorization=%q", got)
		}
		_, _ = w.Write([]byte(`{"id":"msg_ok"}`))
	}))
	defer server.Close()
	manager := codex.NewProviderTokenManager(db, map[string]codex.Refresher{
		store.ProviderAnthropicClaude: codex.RefreshFunc(func(context.Context, string) (codex.TokenSet, error) {
			return codex.TokenSet{AccessToken: "refreshed-access", RefreshToken: "rotated-refresh", ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
		}),
	}, nil)
	client := NewClient(server.URL+"/v1/messages", server.URL+"/usage", manager, nil)
	client.SetHTTPClient(server.Client())
	result, err := client.ExecuteMessages(ctx, ExecuteParams{Account: account, Body: []byte(`{"model":"claude-opus-4-6"}`)})
	if err != nil || result.Status != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls.Load(), err)
	}
	stored, _ := db.GetAccount(ctx, account.ID)
	if stored.RefreshToken != "rotated-refresh" {
		t.Fatalf("refresh token=%q", stored.RefreshToken)
	}
}

func TestRetryAfterSetsInferenceCooldown(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	account := testAccount(t, db)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error"}`))
	}))
	defer server.Close()
	client := NewClient(server.URL+"/v1/messages", server.URL+"/usage", codex.NewProviderTokenManager(db, nil, nil), nil)
	client.SetHTTPClient(server.Client())
	result, err := client.ExecuteMessages(ctx, ExecuteParams{Account: account, Body: []byte(`{"model":"claude-opus-4-6"}`)})
	if err != nil || !result.Retryable {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.CooldownUntil.IsZero() || time.Until(result.CooldownUntil) < 110*time.Second {
		t.Fatalf("cooldown hint=%v", result.CooldownUntil)
	}
	stored, _ := db.GetAccount(ctx, account.ID)
	if stored.CooldownUntil.Valid {
		t.Fatalf("client must leave retry persistence to the router: %v", stored.CooldownUntil)
	}
}

func TestPersistentUnauthorizedMarksClaudeAccountNeedsReauth(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	account := testAccount(t, db)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error"}}`))
	}))
	defer server.Close()
	manager := codex.NewProviderTokenManager(db, map[string]codex.Refresher{
		store.ProviderAnthropicClaude: codex.RefreshFunc(func(context.Context, string) (codex.TokenSet, error) {
			return codex.TokenSet{AccessToken: "still-invalid", RefreshToken: "rotated", ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
		}),
	}, nil)
	client := NewClient(server.URL+"/v1/messages", server.URL+"/usage", manager, nil)
	client.SetHTTPClient(server.Client())
	result, err := client.ExecuteMessages(ctx, ExecuteParams{Account: account, Body: []byte(`{"model":"claude-opus-4-6"}`)})
	if err != nil || result.Status != http.StatusUnauthorized || !result.Retryable {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	stored, _ := db.GetAccount(ctx, account.ID)
	if !stored.NeedsReauth {
		t.Fatal("persistent authorization failure did not mark account for re-authentication")
	}
}

func TestUsage429StillMeansConnectedAndDoesNotSetInferenceCooldown(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	account := testAccount(t, db)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := NewClient(server.URL+"/v1/messages", server.URL+"/usage", codex.NewProviderTokenManager(db, nil, nil), nil)
	client.SetHTTPClient(server.Client())
	info, err := client.FetchUsage(ctx, account)
	if err != nil || !info.Connected || info.Available || info.Status != http.StatusTooManyRequests {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if delay := time.Until(info.RetryAt); delay < UsageRetryCooldown-time.Second || delay > UsageRetryCooldown+time.Second {
		t.Fatalf("quota retry cooldown=%s", delay)
	}
	stored, _ := db.GetAccount(ctx, account.ID)
	if stored.CooldownUntil.Valid {
		t.Fatalf("quota probe set inference cooldown: %v", stored.CooldownUntil)
	}
}

func TestParseUsageWindowsIncludesModelSpecificWeeklyWindow(t *testing.T) {
	windows := parseUsageWindows([]byte(`{
		"five_hour":{"utilization":12.5,"resets_at":"2026-07-18T12:00:00Z"},
		"seven_day":{"utilization":40,"resets_at":"2026-07-20T12:00:00Z"},
		"seven_day_opus":{"utilization":55,"resets_at":"2026-07-20T12:00:00Z"}
	}`))
	if len(windows) != 3 {
		t.Fatalf("windows=%+v", windows)
	}
}
