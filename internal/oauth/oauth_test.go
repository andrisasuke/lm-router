package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBuildAuthURLUsesCodexPKCEParams(t *testing.T) {
	flow := NewCodexFlow(Config{
		RedirectURI:   "http://localhost:1455/auth/callback",
		State:         "state123",
		CodeChallenge: "challenge456",
	})

	raw := flow.AuthURL()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := u.Query()

	assertEqual(t, u.Scheme+"://"+u.Host+u.Path, "https://auth.openai.com/oauth/authorize")
	assertEqual(t, q.Get("response_type"), "code")
	assertEqual(t, q.Get("client_id"), CodexClientID)
	assertEqual(t, q.Get("redirect_uri"), "http://localhost:1455/auth/callback")
	assertEqual(t, q.Get("scope"), "openid profile email offline_access")
	assertEqual(t, q.Get("code_challenge"), "challenge456")
	assertEqual(t, q.Get("code_challenge_method"), "S256")
	assertEqual(t, q.Get("state"), "state123")
	assertEqual(t, q.Get("id_token_add_organizations"), "true")
	assertEqual(t, q.Get("codex_cli_simplified_flow"), "true")
	assertEqual(t, q.Get("originator"), "codex_cli_rs")

	if strings.Contains(raw, "+") {
		t.Fatalf("auth url should encode spaces as %%20, got %s", raw)
	}
}

func TestExchangeCodeBodyMatchesCodexFlow(t *testing.T) {
	values := ExchangeCodeValues("code123", "verifier456", "http://localhost:1455/auth/callback")
	assertEqual(t, values.Get("grant_type"), "authorization_code")
	assertEqual(t, values.Get("code"), "code123")
	assertEqual(t, values.Get("client_id"), CodexClientID)
	assertEqual(t, values.Get("redirect_uri"), "http://localhost:1455/auth/callback")
	assertEqual(t, values.Get("code_verifier"), "verifier456")
	if values.Get("scope") != "" {
		t.Fatalf("authorization_code exchange must not include scope, got %q", values.Get("scope"))
	}
}

func TestParseCallbackValidatesState(t *testing.T) {
	code, err := ParseCallbackURL("http://localhost:1455/auth/callback?code=abc&state=state123", "state123")
	if err != nil {
		t.Fatalf("parse callback: %v", err)
	}
	assertEqual(t, code, "abc")

	if _, err := ParseCallbackURL("http://localhost:1455/auth/callback?code=abc&state=wrong", "state123"); err == nil {
		t.Fatal("expected state mismatch error")
	}
}

func TestIsUnrecoverableRefreshError(t *testing.T) {
	if !IsUnrecoverableRefreshError(errors.New(`{"error":"invalid_grant"}`)) {
		t.Fatal("expected invalid_grant to be unrecoverable")
	}
	if IsUnrecoverableRefreshError(errors.New(`{"error":"server_error"}`)) {
		t.Fatal("did not expect server_error to be unrecoverable")
	}
}

func TestAnthropicAuthURLAndCallbackFormats(t *testing.T) {
	flow := NewAnthropicFlow(Config{RedirectURI: "https://console.anthropic.com/oauth/code/callback", State: "state123", CodeChallenge: "challenge456"})
	u, err := url.Parse(flow.AuthURL())
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	assertEqual(t, u.Scheme+"://"+u.Host+u.Path, AnthropicAuthorizeURL)
	assertEqual(t, q.Get("client_id"), AnthropicClientID)
	assertEqual(t, q.Get("scope"), AnthropicScope)
	assertEqual(t, q.Get("code"), "true")
	assertEqual(t, q.Get("code_challenge_method"), "S256")
	assertEqual(t, q.Get("state"), "state123")

	for _, raw := range []string{
		"auth-code#state123",
		"https://console.anthropic.com/oauth/code/callback?code=auth-code&state=state123",
	} {
		code, err := ParseAnthropicCallback(raw, "state123")
		if err != nil || code != "auth-code" {
			t.Fatalf("parse %q got=%q err=%v", raw, code, err)
		}
	}
	if _, err := ParseAnthropicCallback("auth-code#wrong", "state123"); err == nil {
		t.Fatal("expected state mismatch")
	}
}

func TestAnthropicTokenRequestUsesJSONAndReadsRotatedRefreshToken(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type=%q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-access", "refresh_token": "rotated-refresh", "expires_in": 7200, "scope": AnthropicScope,
		})
	}))
	defer server.Close()

	tokens, err := DoAnthropicTokenRequest(context.Background(), server.Client(), server.URL, map[string]any{
		"grant_type": "refresh_token", "refresh_token": "old-refresh", "client_id": AnthropicClientID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured["grant_type"] != "refresh_token" || captured["client_id"] != AnthropicClientID {
		t.Fatalf("payload=%v", captured)
	}
	if tokens.RefreshToken != "rotated-refresh" || tokens.ExpiresIn != 7200 || tokens.Scope != AnthropicScope {
		t.Fatalf("tokens=%+v", tokens)
	}
}

func TestAnthropicExchangeAndRefreshPayloads(t *testing.T) {
	exchange := AnthropicExchangePayload("code", "state", "verifier", "https://callback")
	for key, want := range map[string]string{
		"code": "code", "state": "state", "code_verifier": "verifier", "redirect_uri": "https://callback",
		"grant_type": "authorization_code", "client_id": AnthropicClientID,
	} {
		if got := exchange[key]; got != want {
			t.Fatalf("exchange[%s]=%v want %s", key, got, want)
		}
	}
	refresh := AnthropicRefreshPayload("refresh")
	if refresh["grant_type"] != "refresh_token" || refresh["refresh_token"] != "refresh" || refresh["client_id"] != AnthropicClientID {
		t.Fatalf("refresh=%v", refresh)
	}
}

func assertEqual(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
