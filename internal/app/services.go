package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/oauth"
	"github.com/andrisasuke/lm-router/internal/proxy"
	"github.com/andrisasuke/lm-router/internal/store"
)

const DefaultCodexBaseURL = "https://chatgpt.com/backend-api/codex/responses"

type ProviderService struct {
	DB      *store.DB
	BaseURL string
	Logger  Logger
}

type AuthSession struct {
	State       string
	Verifier    string
	RedirectURI string
	AuthURL     string
}

type ProviderTestResult struct {
	AccountID string
	Name      string
	Status    int
	OK        bool
	Output    string
}

func (s ProviderService) NewAuthSession(redirectURI string) AuthSession {
	if redirectURI == "" {
		redirectURI = "http://localhost:1455/auth/callback"
	}
	state := randomBase64URLString(32)
	verifier := randomBase64URLString(32)
	flow := oauth.NewCodexFlow(oauth.Config{
		RedirectURI:   redirectURI,
		State:         state,
		CodeChallenge: pkceChallenge(verifier),
	})
	return AuthSession{State: state, Verifier: verifier, RedirectURI: redirectURI, AuthURL: flow.AuthURL()}
}

func (s ProviderService) AddFromCallback(ctx context.Context, session AuthSession, name, callbackURL string) (store.Account, error) {
	if strings.TrimSpace(name) == "" {
		name = "openai-codex"
	}
	code, err := oauth.ParseCallbackURL(callbackURL, session.State)
	if err != nil {
		return store.Account{}, err
	}
	tokens, err := oauth.ExchangeCode(ctx, code, session.Verifier, session.RedirectURI)
	if err != nil {
		return store.Account{}, err
	}
	meta, err := oauth.DecodeIDToken(tokens.IDToken)
	if err != nil && tokens.IDToken != "" {
		return store.Account{}, err
	}
	priority, err := s.DB.NextPriority(ctx)
	if err != nil {
		return store.Account{}, err
	}
	account := store.Account{
		ID:           "acct_" + randomHexString(8),
		Provider:     "openai-codex",
		Name:         name,
		Priority:     priority,
		Enabled:      true,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAt:    oauth.ExpiryTime(tokens.ExpiresIn),
		MetadataJSON: mustJSON(meta),
	}
	if err := s.DB.UpsertAccount(ctx, account); err != nil {
		return store.Account{}, err
	}
	return account, nil
}

// ReAuthFromCallback exchanges a new OAuth callback for fresh tokens but
// preserves the existing account's identity (id, alias, priority, enabled).
// Clears the NeedsReauth flag. Used by the TUI Re-authenticate menu item.
func (s ProviderService) ReAuthFromCallback(ctx context.Context, session AuthSession, accountID, callbackURL string) (store.Account, error) {
	existing, err := s.DB.GetAccount(ctx, accountID)
	if err != nil {
		return store.Account{}, err
	}
	code, err := oauth.ParseCallbackURL(callbackURL, session.State)
	if err != nil {
		return store.Account{}, err
	}
	tokens, err := oauth.ExchangeCode(ctx, code, session.Verifier, session.RedirectURI)
	if err != nil {
		return store.Account{}, err
	}
	meta, err := oauth.DecodeIDToken(tokens.IDToken)
	if err != nil && tokens.IDToken != "" {
		return store.Account{}, err
	}
	updated := existing
	updated.NeedsReauth = false
	updated.AccessToken = tokens.AccessToken
	updated.RefreshToken = tokens.RefreshToken
	updated.ExpiresAt = oauth.ExpiryTime(tokens.ExpiresIn)
	if metaJSON := mustJSON(meta); metaJSON != "{}" && metaJSON != "" {
		updated.MetadataJSON = metaJSON
	}
	if err := s.DB.UpsertAccount(ctx, updated); err != nil {
		return store.Account{}, err
	}
	return updated, nil
}

func (s ProviderService) List(ctx context.Context) ([]store.Account, error) {
	return s.DB.ListAccounts(ctx)
}

func (s ProviderService) Delete(ctx context.Context, id string) error {
	return s.DB.DeleteAccount(ctx, id)
}

func (s ProviderService) Rename(ctx context.Context, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("connection alias is required")
	}
	return s.DB.RenameAccount(ctx, id, name)
}

func (s ProviderService) SetEnabled(ctx context.Context, id string, enabled bool) error {
	return s.DB.SetAccountEnabled(ctx, id, enabled)
}

func (s ProviderService) Refresh(ctx context.Context, id string) (store.Account, error) {
	return codex.NewTokenManager(s.DB, codex.OAuthRefresher{}).RefreshNow(ctx, id)
}

func (s ProviderService) Test(ctx context.Context, account store.Account, model string) (ProviderTestResult, error) {
	if model == "" {
		model = "gpt-5.3-codex"
	}
	baseURL := s.BaseURL
	if baseURL == "" {
		baseURL = DefaultCodexBaseURL
	}
	client := codex.NewClientWithLogger(baseURL, codex.NewTokenManager(s.DB, codex.OAuthRefresher{}), s.Logger, 64*1024)
	body := []byte(mustJSON(map[string]any{"model": model, "input": "ping", "stream": false}))
	result, err := client.ExecuteResponses(ctx, codex.ExecuteParams{Account: account, Body: body})
	if err != nil {
		return ProviderTestResult{}, err
	}
	output := string(result.Body)
	if strings.Contains(result.Header.Get("Content-Type"), "text/event-stream") {
		output = codex.ConvertResponsesSSEToOutput(result.Body)
	}
	return ProviderTestResult{
		AccountID: account.ID,
		Name:      account.Name,
		Status:    result.Status,
		OK:        result.Status >= 200 && result.Status < 300,
		Output:    output,
	}, nil
}

func (s ProviderService) Quota(ctx context.Context, account store.Account) (codex.QuotaInfo, error) {
	baseURL := s.BaseURL
	if baseURL == "" {
		baseURL = DefaultCodexBaseURL
	}
	client := codex.NewClientWithLogger(baseURL, codex.NewTokenManager(s.DB, codex.OAuthRefresher{}), s.Logger, 64*1024)
	return client.FetchQuota(ctx, account)
}

type KeyService struct {
	DB *store.DB
}

func (s KeyService) Create(ctx context.Context, name string) (store.APIKey, error) {
	if strings.TrimSpace(name) == "" {
		name = "default"
	}
	return s.DB.CreateAPIKey(ctx, name)
}

func (s KeyService) List(ctx context.Context) ([]store.APIKey, error) {
	return s.DB.ListAPIKeys(ctx)
}

func (s KeyService) Delete(ctx context.Context, id string) error {
	return s.DB.DeleteAPIKey(ctx, id)
}

func NewProxyHandler(db *store.DB, settings store.Settings, logger Logger) http.Handler {
	bodyLimit := settings.LogBodyLimit
	if bodyLimit <= 0 {
		bodyLimit = 64 * 1024
	}
	codexLogger := logger
	if !settings.LogUpstream {
		codexLogger = discardLogger{}
	}
	client := codex.NewClientWithLogger(DefaultCodexBaseURL, codex.NewTokenManager(db, codex.OAuthRefresher{}), codexLogger, bodyLimit)
	client.SetUpstreamTimeout(settings.UpstreamTimeoutSeconds)
	return proxy.New(proxy.ServerConfig{
		Store:       db,
		Codex:       client,
		RequireKey:  true,
		Logger:      logger,
		LogRequests: settings.LogRequests,
	})
}

func CodexConfigText(port int, apiKey, model string) string {
	if port <= 0 {
		port = 19090
	}
	if apiKey == "" {
		apiKey = "sk-lm-router-REPLACE_ME"
	}
	if model == "" {
		model = "gpt-5.3-codex"
	}
	return fmt.Sprintf("model = %q\nmodel_provider = %q\n\n[model_providers.lm-router]\nname = %q\nbase_url = %q\nwire_api = %q\n\n[agents.subagent]\nmodel = %q\n\n{\n  \"auth_mode\": \"apikey\",\n  \"OPENAI_API_KEY\": %q\n}\n",
		model, "lm-router", "LM Router", fmt.Sprintf("http://127.0.0.1:%d/v1", port), "responses", model, apiKey)
}

func HumanError(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "unknown error"
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err == nil {
		if msg := stringField(payload, "message"); msg != "" {
			return msg
		}
		if detail := stringField(payload, "detail"); detail != "" {
			return detail
		}
		if errObj, ok := payload["error"].(map[string]any); ok {
			if msg := stringField(errObj, "message"); msg != "" {
				return msg
			}
			if code := stringField(errObj, "code"); code != "" {
				return code
			}
			if typ := stringField(errObj, "type"); typ != "" {
				return typ
			}
		}
	}
	return raw
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

func stringField(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func randomHexString(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)[:n]
}

func randomBase64URLString(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func FormatProviderStatus(account store.Account) string {
	status := "Active"
	if !account.Enabled {
		status = "Disabled"
	}
	if account.NeedsReauth {
		status = "Needs re-auth"
	}
	if account.CooldownUntil.Valid && account.CooldownUntil.Time.After(time.Now()) {
		status = "Cooldown"
	}
	return status
}
