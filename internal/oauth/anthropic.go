package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	AnthropicClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	AnthropicAuthorizeURL = "https://claude.ai/oauth/authorize"
	AnthropicTokenURL     = "https://api.anthropic.com/v1/oauth/token"
	AnthropicScope        = "org:create_api_key user:profile user:inference"
)

type AnthropicFlow struct {
	cfg Config
}

func NewAnthropicFlow(cfg Config) AnthropicFlow {
	return AnthropicFlow{cfg: cfg}
}

func (f AnthropicFlow) AuthURL() string {
	values := url.Values{}
	values.Set("code", "true")
	values.Set("client_id", AnthropicClientID)
	values.Set("response_type", "code")
	values.Set("redirect_uri", f.cfg.RedirectURI)
	values.Set("scope", AnthropicScope)
	values.Set("code_challenge", f.cfg.CodeChallenge)
	values.Set("code_challenge_method", "S256")
	values.Set("state", f.cfg.State)
	return AnthropicAuthorizeURL + "?" + strings.ReplaceAll(values.Encode(), "+", "%20")
}

// ParseAnthropicCallback accepts either a browser callback URL or the
// copy/paste format emitted by Claude's OAuth page: code#state.
func ParseAnthropicCallback(raw, expectedState string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("missing oauth code")
	}
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "/") {
		return ParseCallbackURL(raw, expectedState)
	}
	parts := strings.SplitN(raw, "#", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
		return "", errors.New("missing oauth state; expected code#state")
	}
	if strings.TrimSpace(parts[1]) != expectedState {
		return "", errors.New("oauth state mismatch")
	}
	code := strings.TrimSpace(parts[0])
	if code == "" {
		return "", errors.New("missing oauth code")
	}
	return code, nil
}

func ExchangeAnthropicCode(ctx context.Context, code, state, verifier, redirectURI string) (TokenResponse, error) {
	return DoAnthropicTokenRequest(ctx, http.DefaultClient, AnthropicTokenURL, AnthropicExchangePayload(code, state, verifier, redirectURI))
}

func AnthropicExchangePayload(code, state, verifier, redirectURI string) map[string]any {
	return map[string]any{
		"code":          code,
		"state":         state,
		"grant_type":    "authorization_code",
		"client_id":     AnthropicClientID,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	}
}

func RefreshAnthropicToken(ctx context.Context, refreshToken string) (TokenResponse, error) {
	return DoAnthropicTokenRequest(ctx, http.DefaultClient, AnthropicTokenURL, AnthropicRefreshPayload(refreshToken))
}

func AnthropicRefreshPayload(refreshToken string) map[string]any {
	return map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     AnthropicClientID,
	}
}

// DoAnthropicTokenRequest is exported so protocol tests can use a mock HTTP
// server without replacing package globals.
func DoAnthropicTokenRequest(ctx context.Context, client *http.Client, endpoint string, payload map[string]any) (TokenResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return TokenResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return TokenResponse{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return TokenResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TokenResponse{}, errors.New(string(respBody))
	}
	var tokens TokenResponse
	if err := json.Unmarshal(respBody, &tokens); err != nil {
		return TokenResponse{}, err
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return TokenResponse{}, errors.New("token response missing access_token")
	}
	return tokens, nil
}
