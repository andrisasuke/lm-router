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

	"github.com/andrisasuke/lm-router/internal/anthropic"
	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/customprovider"
	"github.com/andrisasuke/lm-router/internal/oauth"
	"github.com/andrisasuke/lm-router/internal/proxy"
	"github.com/andrisasuke/lm-router/internal/store"
)

const (
	DefaultCodexBaseURL = "https://chatgpt.com/backend-api/codex/responses"
	DefaultClaudeModel  = "claude-opus-4-8"
)

type ProviderService struct {
	DB                   *store.DB
	BaseURL              string
	AnthropicMessagesURL string
	AnthropicUsageURL    string
	Logger               Logger
}

type AuthSession struct {
	Provider    string
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
	return s.NewAuthSessionForProvider(store.ProviderOpenAICodex, redirectURI)
}

func (s ProviderService) NewAuthSessionForProvider(provider, redirectURI string) AuthSession {
	provider, err := store.CanonicalProvider(provider)
	if err != nil {
		provider = store.ProviderOpenAICodex
	}
	if redirectURI == "" && provider == store.ProviderAnthropicClaude {
		redirectURI = "https://console.anthropic.com/oauth/code/callback"
	} else if redirectURI == "" {
		redirectURI = "http://localhost:1455/auth/callback"
	}
	state := randomBase64URLString(32)
	verifier := randomBase64URLString(32)
	cfg := oauth.Config{
		RedirectURI:   redirectURI,
		State:         state,
		CodeChallenge: pkceChallenge(verifier),
	}
	authURL := oauth.NewCodexFlow(cfg).AuthURL()
	if provider == store.ProviderAnthropicClaude {
		authURL = oauth.NewAnthropicFlow(cfg).AuthURL()
	}
	return AuthSession{Provider: provider, State: state, Verifier: verifier, RedirectURI: redirectURI, AuthURL: authURL}
}

func (s ProviderService) AddFromCallback(ctx context.Context, session AuthSession, name, callbackURL string) (store.Account, error) {
	provider, err := store.CanonicalProvider(session.Provider)
	if err != nil {
		return store.Account{}, err
	}
	if strings.TrimSpace(name) == "" {
		name = provider
	}
	tokens, metadata, err := exchangeProviderCallback(ctx, provider, session, callbackURL)
	if err != nil {
		return store.Account{}, err
	}
	priority, err := s.DB.NextPriorityForProvider(ctx, provider)
	if err != nil {
		return store.Account{}, err
	}
	account := store.Account{
		ID:           "acct_" + randomHexString(8),
		Provider:     provider,
		Name:         name,
		Priority:     priority,
		Enabled:      true,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAt:    oauth.ExpiryTime(tokens.ExpiresIn),
		MetadataJSON: mustJSON(metadata),
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
	tokens, metadata, err := exchangeProviderCallback(ctx, existing.Provider, session, callbackURL)
	if err != nil {
		return store.Account{}, err
	}
	updated := existing
	updated.NeedsReauth = false
	updated.AccessToken = tokens.AccessToken
	updated.RefreshToken = tokens.RefreshToken
	updated.ExpiresAt = oauth.ExpiryTime(tokens.ExpiresIn)
	if metaJSON := mustJSON(metadata); metaJSON != "{}" && metaJSON != "" {
		updated.MetadataJSON = metaJSON
	}
	if err := s.DB.UpsertAccount(ctx, updated); err != nil {
		return store.Account{}, err
	}
	return updated, nil
}

type AddCustomProviderParams struct {
	Name, Prefix, BaseURL, APIKey, CompatType, APIType string
}

// AddCustomProvider saves a static-API-key connection to a user-configured
// OpenAI-compatible or Anthropic-compatible endpoint. Unlike AddFromCallback,
// there is no OAuth exchange: the caller already has everything needed.
func (s ProviderService) AddCustomProvider(ctx context.Context, params AddCustomProviderParams) (store.Account, error) {
	name := strings.TrimSpace(params.Name)
	if name == "" {
		name = params.Prefix
	}
	priority, err := s.DB.NextPriorityForProvider(ctx, store.ProviderCustom)
	if err != nil {
		return store.Account{}, err
	}
	account := store.Account{
		ID:          "acct_" + randomHexString(8),
		Provider:    store.ProviderCustom,
		Name:        name,
		Priority:    priority,
		Enabled:     true,
		AccessToken: params.APIKey,
		Prefix:      params.Prefix,
		BaseURL:     params.BaseURL,
		CompatType:  params.CompatType,
		APIType:     params.APIType,
	}
	if err := s.DB.UpsertAccount(ctx, account); err != nil {
		return store.Account{}, err
	}
	return account, nil
}

// UpdateCustomProviderParams uses pointer fields so an omitted flag/field
// leaves the stored value untouched. A nil or empty APIKey always means
// "keep the current key" — there is no way to clear it back to empty.
type UpdateCustomProviderParams struct {
	Name, Prefix, BaseURL, APIKey, APIType *string
}

func (s ProviderService) UpdateCustomProvider(ctx context.Context, id string, params UpdateCustomProviderParams) (store.Account, error) {
	account, err := s.DB.MustGetAccount(ctx, id)
	if err != nil {
		return store.Account{}, err
	}
	if account.Provider != store.ProviderCustom {
		return store.Account{}, fmt.Errorf("account %s is not a custom provider connection", id)
	}
	if params.Name != nil {
		account.Name = *params.Name
	}
	if params.Prefix != nil {
		account.Prefix = *params.Prefix
	}
	if params.BaseURL != nil {
		account.BaseURL = *params.BaseURL
	}
	if params.APIKey != nil && *params.APIKey != "" {
		account.AccessToken = *params.APIKey
	}
	if params.APIType != nil {
		account.APIType = *params.APIType
	}
	if err := s.DB.UpsertAccount(ctx, account); err != nil {
		return store.Account{}, err
	}
	return account, nil
}

func (s ProviderService) List(ctx context.Context) ([]store.Account, error) {
	return s.DB.ListAccounts(ctx)
}

func (s ProviderService) ListProvider(ctx context.Context, provider string) ([]store.Account, error) {
	return s.DB.ListAccountsByProvider(ctx, provider)
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
	return NewProviderTokenManager(s.DB).RefreshNow(ctx, id)
}

func (s ProviderService) Test(ctx context.Context, account store.Account, model string) (ProviderTestResult, error) {
	if account.Provider == store.ProviderCustom {
		return s.testCustomProvider(ctx, account, model)
	}
	if account.Provider == store.ProviderAnthropicClaude {
		client := anthropic.NewClient(s.AnthropicMessagesURL, s.AnthropicUsageURL, NewProviderTokenManager(s.DB), s.Logger)
		info, err := client.FetchUsage(ctx, account)
		if err != nil {
			return ProviderTestResult{}, err
		}
		output := "OAuth usage available"
		if !info.Available {
			output = "connected; quota temporarily unavailable"
		}
		return ProviderTestResult{AccountID: account.ID, Name: account.Name, Status: info.Status, OK: info.Connected, Output: output}, nil
	}
	if model == "" {
		model = "gpt-5.3-codex"
	}
	baseURL := s.BaseURL
	if baseURL == "" {
		baseURL = DefaultCodexBaseURL
	}
	client := codex.NewClientWithLogger(baseURL, NewProviderTokenManager(s.DB), s.Logger, 64*1024)
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

// testCustomProvider probes a custom connection using its own base_url and
// api_key. anthropic-compatible: a minimal /messages ping — a 4xx/5xx other
// than 401/403 still means "we reached a server," so only 2xx counts as
// fully connected, and 401/403 is called out explicitly as an auth failure
// rather than folded into a generic "not connected." openai-compatible: a
// GET /models probe, which some third-party servers may not implement — a
// documented v1 limitation, not a bug.
func (s ProviderService) testCustomProvider(ctx context.Context, account store.Account, model string) (ProviderTestResult, error) {
	client := customprovider.NewClient(s.Logger)
	var result customprovider.ExecuteResult
	var err error
	if account.CompatType == store.CompatAnthropicStyle {
		if model == "" {
			model = "test"
		}
		body := []byte(mustJSON(map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]any{{"role": "user", "content": "ping"}},
		}))
		result, err = client.Execute(ctx, customprovider.ExecuteParams{Account: account, Path: "/messages", Body: body})
	} else {
		result, err = client.Execute(ctx, customprovider.ExecuteParams{Account: account, Path: "/models", Method: http.MethodGet})
	}
	if err != nil {
		return ProviderTestResult{}, err
	}
	output := string(result.Body)
	switch {
	case result.Status >= 200 && result.Status < 300:
		return ProviderTestResult{AccountID: account.ID, Name: account.Name, Status: result.Status, OK: true, Output: output}, nil
	case result.Status == http.StatusUnauthorized || result.Status == http.StatusForbidden:
		return ProviderTestResult{AccountID: account.ID, Name: account.Name, Status: result.Status, OK: false, Output: "authentication failed: " + output}, nil
	default:
		return ProviderTestResult{AccountID: account.ID, Name: account.Name, Status: result.Status, OK: false, Output: output}, nil
	}
}

func (s ProviderService) Quota(ctx context.Context, account store.Account) (codex.QuotaInfo, error) {
	baseURL := s.BaseURL
	if baseURL == "" {
		baseURL = DefaultCodexBaseURL
	}
	client := codex.NewClientWithLogger(baseURL, NewProviderTokenManager(s.DB), s.Logger, 64*1024)
	return client.FetchQuota(ctx, account)
}

func (s ProviderService) ClaudeQuota(ctx context.Context, account store.Account) (anthropic.UsageInfo, error) {
	client := anthropic.NewClient(s.AnthropicMessagesURL, s.AnthropicUsageURL, NewProviderTokenManager(s.DB), s.Logger)
	return client.FetchUsage(ctx, account)
}

func exchangeProviderCallback(ctx context.Context, provider string, session AuthSession, callback string) (oauth.TokenResponse, any, error) {
	provider, err := store.CanonicalProvider(provider)
	if err != nil {
		return oauth.TokenResponse{}, nil, err
	}
	if session.Provider != "" && session.Provider != provider {
		return oauth.TokenResponse{}, nil, fmt.Errorf("oauth session is for %s, not %s", session.Provider, provider)
	}
	if provider == store.ProviderAnthropicClaude {
		code, err := oauth.ParseAnthropicCallback(callback, session.State)
		if err != nil {
			return oauth.TokenResponse{}, nil, err
		}
		tokens, err := oauth.ExchangeAnthropicCode(ctx, code, session.State, session.Verifier, session.RedirectURI)
		if err != nil {
			return oauth.TokenResponse{}, nil, err
		}
		return tokens, map[string]any{"scope": tokens.Scope}, nil
	}
	code, err := oauth.ParseCallbackURL(callback, session.State)
	if err != nil {
		return oauth.TokenResponse{}, nil, err
	}
	tokens, err := oauth.ExchangeCode(ctx, code, session.Verifier, session.RedirectURI)
	if err != nil {
		return oauth.TokenResponse{}, nil, err
	}
	meta, err := oauth.DecodeIDToken(tokens.IDToken)
	if err != nil && tokens.IDToken != "" {
		return oauth.TokenResponse{}, nil, err
	}
	return tokens, meta, nil
}

func NewProviderTokenManager(db *store.DB) *codex.TokenManager {
	return codex.NewProviderTokenManager(db, map[string]codex.Refresher{
		store.ProviderOpenAICodex:     codex.OAuthRefresher{},
		store.ProviderAnthropicClaude: anthropic.OAuthRefresher{},
	}, map[string]time.Duration{
		store.ProviderOpenAICodex:     5 * time.Minute,
		store.ProviderAnthropicClaude: anthropic.ClaudeRefreshLead,
	})
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
	tokens := NewProviderTokenManager(db)
	client := codex.NewClientWithLogger(DefaultCodexBaseURL, tokens, codexLogger, bodyLimit)
	client.SetUpstreamTimeout(settings.UpstreamTimeoutSeconds)
	claudeClient := anthropic.NewClient(anthropic.DefaultMessagesURL, anthropic.DefaultUsageURL, tokens, codexLogger)
	claudeClient.SetUpstreamTimeout(settings.UpstreamTimeoutSeconds)
	customClient := customprovider.NewClient(codexLogger)
	customClient.SetUpstreamTimeout(settings.UpstreamTimeoutSeconds)
	return proxy.New(proxy.ServerConfig{
		Store:       db,
		Codex:       client,
		Anthropic:   claudeClient,
		Custom:      customClient,
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

func ClaudeConfigText(port int, apiKey, model string) string {
	if port <= 0 {
		port = 19090
	}
	if apiKey == "" {
		apiKey = "sk-lm-router-REPLACE_ME"
	}
	lines := []string{
		fmt.Sprintf("ANTHROPIC_BASE_URL=http://127.0.0.1:%d", port),
		"ANTHROPIC_AUTH_TOKEN=" + apiKey,
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude") {
		lines = append(lines, "ANTHROPIC_MODEL="+strings.TrimSpace(model))
	}
	return strings.Join(lines, "\n") + "\n"
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
