package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/oauth"
	"github.com/andrisasuke/lm-router/internal/store"
)

func TestClaudeAuthSessionUsesProviderDefaultCallback(t *testing.T) {
	session := (ProviderService{}).NewAuthSessionForProvider("claude", "")
	if session.Provider != store.ProviderAnthropicClaude || session.RedirectURI != "https://console.anthropic.com/oauth/code/callback" {
		t.Fatalf("session=%+v", session)
	}
	parsed, err := url.Parse(session.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme+"://"+parsed.Host+parsed.Path != oauth.AnthropicAuthorizeURL {
		t.Fatalf("auth URL=%s", session.AuthURL)
	}
	if parsed.Query().Get("redirect_uri") != session.RedirectURI {
		t.Fatalf("redirect_uri=%q", parsed.Query().Get("redirect_uri"))
	}
}

func TestQuotaUsesConfigurableCodexUsageURL(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account := store.Account{
		ID: "codex", Provider: store.ProviderOpenAICodex, Name: "main", Enabled: true,
		AccessToken: "token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":5},"secondary_window":{"used_percent":6}}}`))
	}))
	defer server.Close()

	info, err := (ProviderService{
		DB:            db,
		BaseURL:       "https://inference.invalid/responses",
		CodexUsageURL: server.URL + "/backend-api/wham/usage",
	}).Quota(ctx, account)
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if got := <-requests; got != "GET /backend-api/wham/usage" {
		t.Fatalf("request=%q", got)
	}
	if info.Primary == nil || info.Secondary == nil || info.Primary.UsedPercent != 5 || info.Secondary.UsedPercent != 6 {
		t.Fatalf("info=%+v", info)
	}
}
