package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	CodexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	authURL       = "https://auth.openai.com/oauth/authorize"
	tokenURL      = "https://auth.openai.com/oauth/token"
	scope         = "openid profile email offline_access"
)

type Config struct {
	RedirectURI   string
	State         string
	CodeChallenge string
}

type Flow struct {
	cfg Config
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

type AccountMetadata struct {
	Email            string `json:"email"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	ChatGPTPlanType  string `json:"chatgpt_plan_type"`
}

func NewCodexFlow(cfg Config) Flow {
	return Flow{cfg: cfg}
}

func (f Flow) AuthURL() string {
	values := url.Values{}
	values.Set("response_type", "code")
	values.Set("client_id", CodexClientID)
	values.Set("redirect_uri", f.cfg.RedirectURI)
	values.Set("scope", scope)
	values.Set("code_challenge", f.cfg.CodeChallenge)
	values.Set("code_challenge_method", "S256")
	values.Set("state", f.cfg.State)
	values.Set("id_token_add_organizations", "true")
	values.Set("codex_cli_simplified_flow", "true")
	values.Set("originator", "codex_cli_rs")
	return authURL + "?" + strings.ReplaceAll(values.Encode(), "+", "%20")
}

func ParseCallbackURL(rawURL, expectedState string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if got := q.Get("state"); got != expectedState {
		return "", errors.New("oauth state mismatch")
	}
	code := q.Get("code")
	if code == "" {
		return "", errors.New("missing oauth code")
	}
	return code, nil
}

func ExchangeCode(ctx context.Context, code, verifier, redirectURI string) (TokenResponse, error) {
	return doTokenRequest(ctx, ExchangeCodeValues(code, verifier, redirectURI))
}

func ExchangeCodeValues(code, verifier, redirectURI string) url.Values {
	values := url.Values{}
	values.Set("grant_type", "authorization_code")
	values.Set("code", code)
	values.Set("client_id", CodexClientID)
	values.Set("redirect_uri", redirectURI)
	values.Set("code_verifier", verifier)
	return values
}

func RefreshToken(ctx context.Context, refreshToken string) (TokenResponse, error) {
	values := url.Values{}
	values.Set("grant_type", "refresh_token")
	values.Set("refresh_token", refreshToken)
	values.Set("client_id", CodexClientID)
	values.Set("scope", scope)
	return doTokenRequest(ctx, values)
}

func IsUnrecoverableRefreshError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, code := range []string{
		"refresh_token_reused",
		"invalid_grant",
		"token_expired",
		"invalid_token",
	} {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

func DecodeIDToken(idToken string) (AccountMetadata, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return AccountMetadata{}, errors.New("invalid id_token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return AccountMetadata{}, err
	}
	var claims struct {
		Email string `json:"email"`
		Auth  struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			ChatGPTPlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return AccountMetadata{}, err
	}
	return AccountMetadata{
		Email:            claims.Email,
		ChatGPTAccountID: claims.Auth.ChatGPTAccountID,
		ChatGPTPlanType:  claims.Auth.ChatGPTPlanType,
	}, nil
}

func ExpiryTime(expiresIn int64) time.Time {
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return time.Now().UTC().Add(time.Duration(expiresIn) * time.Second)
}

func doTokenRequest(ctx context.Context, values url.Values) (TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return TokenResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return TokenResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TokenResponse{}, errors.New(string(body))
	}
	var tokens TokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return TokenResponse{}, err
	}
	return tokens, nil
}
