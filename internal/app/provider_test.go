package app

import (
	"net/url"
	"testing"

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
