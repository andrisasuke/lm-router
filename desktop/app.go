//go:build wailsapp

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	appsvc "github.com/andrisasuke/lm-router/internal/app"
	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
	iversion "github.com/andrisasuke/lm-router/internal/version"
	"github.com/wailsapp/wails/v3/pkg/application"
)

type pendingOAuth struct {
	mu        sync.Mutex
	session   appsvc.AuthSession
	accountID string
	loopback  *oauthLoopback
	completed bool
}

type App struct {
	ctx         context.Context
	db          *store.DB
	logger      *appsvc.RingLogger
	controller  *appsvc.ServerController
	profile     desktopProfile
	dataDir     string
	initErr     error
	desktopApp  *application.App
	mainWindow  application.Window
	tray        *trayController
	activity    *appsvc.AccountActivityTracker
	codexQuotas *codexQuotaTracker

	codexQuotaFetcher func(context.Context, store.Account) (codex.QuotaInfo, error)
	quotaContext      context.Context
	quotaCancel       context.CancelFunc
	quotaWorkers      sync.WaitGroup

	mu                 sync.Mutex
	pending            map[string]*pendingOAuth
	claudeQuotaRetryAt map[string]time.Time
}

func NewApp(profile desktopProfile) *App {
	return &App{
		profile:            profile,
		pending:            make(map[string]*pendingOAuth),
		claudeQuotaRetryAt: make(map[string]time.Time),
	}
}

func (a *App) attachApplication(app *application.App, mainWindow application.Window, tray *trayController) {
	a.desktopApp = app
	a.mainWindow = mainWindow
	a.tray = tray
}

func (a *App) ServiceName() string {
	return "LM Router Desktop"
}

func (a *App) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	a.ctx = ctx
	a.dataDir = a.profile.DataDir
	trayEnabled := true
	defer func() {
		a.refreshTray()
		if a.tray != nil {
			a.tray.activate(trayEnabled)
		}
	}()
	var err error
	a.db, err = store.Open(ctx, a.dataDir)
	if err != nil {
		a.initErr = fmt.Errorf("open LM Router data: %w", err)
		a.showError("LM Router could not start", a.initErr)
		return nil
	}
	settings, err := a.db.GetSettings(ctx)
	if err != nil {
		a.initErr = fmt.Errorf("read LM Router settings: %w", err)
		a.showError("LM Router could not start", a.initErr)
		return nil
	}
	trayEnabled = settings.TrayEnabled
	a.logger = appsvc.NewRingLogger(500, nil)
	log.SetOutput(a.logger)
	log.SetFlags(0)
	log.SetPrefix("")
	a.activity = appsvc.NewAccountActivityTracker(a.emitConnectionActivity)
	a.codexQuotas = newCodexQuotaTracker(a.emitConnectionQuota)
	a.codexQuotaFetcher = func(ctx context.Context, account store.Account) (codex.QuotaInfo, error) {
		return a.providerService().Quota(ctx, account)
	}
	a.quotaContext, a.quotaCancel = context.WithCancel(ctx)
	a.controller = appsvc.NewServerController(appsvc.ServerControllerConfig{
		Logger: a.logger,
		HandlerFactory: func() (http.Handler, error) {
			settings, err := a.settings()
			if err != nil {
				return nil, err
			}
			return appsvc.NewProxyHandler(a.db, settings, a.logger, appsvc.ProxyCallbacks{
				OnAccountActivity: a.activity.Update,
				OnCodexQuota:      a.observeCodexQuota,
			}), nil
		},
	})
	a.quotaWorkers.Add(2)
	go func() {
		defer a.quotaWorkers.Done()
		a.warmCodexQuotas(a.quotaContext)
	}()
	go func() {
		defer a.quotaWorkers.Done()
		a.monitorCodexQuotaResets(a.quotaContext)
	}()
	return nil
}

func (a *App) ServiceShutdown() error {
	if a.quotaCancel != nil {
		a.quotaCancel()
	}
	a.mu.Lock()
	for _, pending := range a.pending {
		pending.loopback.stop()
	}
	a.pending = make(map[string]*pendingOAuth)
	a.mu.Unlock()
	if a.controller != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = a.controller.Stop(ctx)
		cancel()
	}
	a.quotaWorkers.Wait()
	if a.db != nil {
		_ = a.db.Close()
	}
	return nil
}

func (a *App) ServerStatus() (ServerStatusVM, error) {
	if err := a.ready(); err != nil {
		return ServerStatusVM{}, err
	}
	settings, err := a.settings()
	if err != nil {
		return ServerStatusVM{}, humanError(err)
	}
	status := serverStatusVM(a.controller.Status(), settings)
	a.updateTray(status)
	return status, nil
}

func (a *App) StartServer() (ServerStatusVM, error) {
	defer a.refreshTray()
	if err := a.ready(); err != nil {
		return ServerStatusVM{}, err
	}
	settings, err := a.settings()
	if err != nil {
		return ServerStatusVM{}, humanError(err)
	}
	if a.controller.Status().State == appsvc.ServerOn {
		return serverStatusVM(a.controller.Status(), settings), nil
	}
	if settings.Port > 0 && probeHealth(settings) {
		err := fmt.Errorf("%s is already serving LM Router health checks; an existing `lm-router serve` or another desktop instance is probably running", configuredEndpoint(settings))
		a.showError("Server already running", err)
		return serverStatusVM(a.controller.Status(), settings), humanError(err)
	}
	if err := a.controller.Start(a.context(), settings.Host, settings.Port); err != nil {
		a.showError("Could not start server", err)
		return serverStatusVM(a.controller.Status(), settings), humanError(err)
	}
	return serverStatusVM(a.controller.Status(), settings), nil
}

func (a *App) StopServer() (ServerStatusVM, error) {
	defer a.refreshTray()
	if err := a.ready(); err != nil {
		return ServerStatusVM{}, err
	}
	ctx, cancel := context.WithTimeout(a.context(), 5*time.Second)
	defer cancel()
	if err := a.controller.Stop(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return ServerStatusVM{}, humanError(err)
	}
	settings, err := a.settings()
	if err != nil {
		return ServerStatusVM{}, humanError(err)
	}
	return serverStatusVM(a.controller.Status(), settings), nil
}

func (a *App) RestartServer() (ServerStatusVM, error) {
	if _, err := a.StopServer(); err != nil {
		return ServerStatusVM{}, err
	}
	return a.StartServer()
}

func (a *App) ListConnections(provider string) ([]ConnectionVM, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	service := a.providerService()
	var (
		accounts []store.Account
		err      error
	)
	if strings.TrimSpace(provider) == "" {
		accounts, err = service.List(a.context())
	} else {
		accounts, err = service.ListProvider(a.context(), provider)
	}
	if err != nil {
		return nil, humanError(err)
	}
	result := make([]ConnectionVM, 0, len(accounts))
	for _, account := range accounts {
		result = append(result, a.connectionView(account))
	}
	return result, nil
}

func (a *App) connectionView(account store.Account) ConnectionVM {
	result := connectionVM(account)
	result.Requesting = a.activity != nil && a.activity.Active(account.ID)
	if account.Provider == store.ProviderOpenAICodex {
		state := codexQuotaState{}
		if a.codexQuotas != nil {
			state = a.codexQuotas.Snapshot(account.ID)
		}
		result.Quota = connectionQuotaVM(state)
		if account.NeedsReauth && result.Quota.State == quotaStateUnknown {
			result.Quota = ConnectionQuotaVM{State: quotaStateUnavailable, Summary: "Quota unavailable"}
		}
	}
	return result
}

func (a *App) emitConnectionActivity(accountID string, requesting bool) {
	if a.desktopApp == nil {
		return
	}
	a.desktopApp.Event.Emit("connection-activity", ConnectionActivityVM{
		ID:         accountID,
		Requesting: requesting,
	})
}

func (a *App) emitConnectionQuota(accountID string, state codexQuotaState) {
	if a.desktopApp == nil {
		return
	}
	a.desktopApp.Event.Emit("connection-quota", a.connectionQuotaEvent(accountID, state))
}

func (a *App) connectionQuotaEvent(accountID string, state codexQuotaState) ConnectionQuotaEventVM {
	event := ConnectionQuotaEventVM{
		ID:    accountID,
		Quota: connectionQuotaVM(state),
	}
	if a.db == nil {
		return event
	}
	account, err := a.db.MustGetAccount(a.context(), accountID)
	if err != nil {
		return event
	}
	connection := connectionVM(account)
	event.Status = connection.Status
	event.CooldownUntil = connection.CooldownUntil
	event.ConsecutiveFailures = connection.ConsecutiveFailures
	return event
}

func (a *App) BeginOAuth(provider string) (OAuthSessionVM, error) {
	return a.beginOAuth(provider, "")
}

func (a *App) BeginReauth(id string) (OAuthSessionVM, error) {
	if err := a.ready(); err != nil {
		return OAuthSessionVM{}, err
	}
	account, err := a.db.MustGetAccount(a.context(), id)
	if err != nil {
		return OAuthSessionVM{}, humanError(err)
	}
	if account.Provider == store.ProviderCustom {
		return OAuthSessionVM{}, humanError(errors.New("custom provider connections do not use OAuth"))
	}
	return a.beginOAuth(account.Provider, account.ID)
}

func (a *App) AwaitOAuth(sessionID, name string) (ConnectionVM, error) {
	pending, err := a.getPending(sessionID)
	if err != nil {
		return ConnectionVM{}, err
	}
	if pending.loopback == nil {
		return ConnectionVM{}, humanError(errors.New("automatic OAuth callback is unavailable; paste the callback manually"))
	}
	select {
	case callback := <-pending.loopback.callback:
		if callback == "" {
			return ConnectionVM{}, humanError(errors.New("OAuth session was cancelled"))
		}
		return a.completeOAuth(sessionID, name, callback)
	case <-a.context().Done():
		return ConnectionVM{}, humanError(a.context().Err())
	}
}

func (a *App) CancelOAuth(sessionID string) {
	a.mu.Lock()
	pending := a.pending[sessionID]
	delete(a.pending, sessionID)
	a.mu.Unlock()
	if pending != nil {
		pending.loopback.cancel()
	}
}

func (a *App) SubmitCallback(sessionID, name, callbackURL string) (ConnectionVM, error) {
	if strings.TrimSpace(callbackURL) == "" {
		return ConnectionVM{}, humanError(errors.New("callback URL or code#state is required"))
	}
	return a.completeOAuth(sessionID, name, callbackURL)
}

func (a *App) AddCustomProvider(input CustomProviderInput) (ConnectionVM, error) {
	if err := a.ready(); err != nil {
		return ConnectionVM{}, err
	}
	account, err := a.providerService().AddCustomProvider(a.context(), appsvc.AddCustomProviderParams{
		Name: input.Name, Prefix: input.Prefix, BaseURL: input.BaseURL,
		APIKey: input.APIKey, CompatType: input.CompatType, APIType: input.APIType,
	})
	if err != nil {
		return ConnectionVM{}, humanError(err)
	}
	return a.connectionView(account), nil
}

func (a *App) UpdateCustomProvider(id string, input CustomProviderUpdate) (ConnectionVM, error) {
	if err := a.ready(); err != nil {
		return ConnectionVM{}, err
	}
	name, prefix, baseURL, apiKey, apiType := input.Name, input.Prefix, input.BaseURL, input.APIKey, input.APIType
	account, err := a.providerService().UpdateCustomProvider(a.context(), id, appsvc.UpdateCustomProviderParams{
		Name: &name, Prefix: &prefix, BaseURL: &baseURL, APIKey: &apiKey, APIType: &apiType,
	})
	if err != nil {
		return ConnectionVM{}, humanError(err)
	}
	return a.connectionView(account), nil
}

func (a *App) RenameConnection(id, name string) error {
	if err := a.ready(); err != nil {
		return err
	}
	return humanError(a.providerService().Rename(a.context(), id, name))
}

func (a *App) SetEnabled(id string, enabled bool) error {
	if err := a.ready(); err != nil {
		return err
	}
	return humanError(a.providerService().SetEnabled(a.context(), id, enabled))
}

func (a *App) Reorder(id string, delta int) error {
	if err := a.ready(); err != nil {
		return err
	}
	return humanError(a.providerService().Reorder(a.context(), id, delta))
}

func (a *App) DeleteConnection(id string) error {
	if err := a.ready(); err != nil {
		return err
	}
	return humanError(a.providerService().Delete(a.context(), id))
}

func (a *App) RefreshConnection(id string) (ConnectionVM, error) {
	if err := a.ready(); err != nil {
		return ConnectionVM{}, err
	}
	account, err := a.providerService().Refresh(a.context(), id)
	if err != nil {
		return ConnectionVM{}, humanError(err)
	}
	return a.connectionView(account), nil
}

func (a *App) TestConnection(id, model string) (ProviderTestVM, error) {
	if err := a.ready(); err != nil {
		return ProviderTestVM{}, err
	}
	account, err := a.db.MustGetAccount(a.context(), id)
	if err != nil {
		return ProviderTestVM{}, humanError(err)
	}
	result, err := a.providerService().Test(a.context(), account, model)
	if err != nil {
		return ProviderTestVM{}, humanError(err)
	}
	return ProviderTestVM{Status: result.Status, OK: result.OK, Output: result.Output}, nil
}

func (a *App) Quota(id string) (QuotaVM, error) {
	if err := a.ready(); err != nil {
		return QuotaVM{}, err
	}
	account, err := a.db.MustGetAccount(a.context(), id)
	if err != nil {
		return QuotaVM{}, humanError(err)
	}
	if account.Provider == store.ProviderCustom {
		return QuotaVM{}, humanError(errors.New("quota is not available for custom providers"))
	}
	service := a.providerService()
	if account.Provider == store.ProviderAnthropicClaude {
		a.mu.Lock()
		retryAt := a.claudeQuotaRetryAt[id]
		a.mu.Unlock()
		if retryAt.After(time.Now()) {
			return QuotaVM{Connected: true, RetryAt: formatTime(retryAt), Message: "Quota check is cooling down after an upstream 429"}, nil
		}
		info, err := service.ClaudeQuota(a.context(), account)
		if err != nil {
			return QuotaVM{}, humanError(err)
		}
		a.mu.Lock()
		if info.RetryAt.After(time.Now()) {
			a.claudeQuotaRetryAt[id] = info.RetryAt
		} else {
			delete(a.claudeQuotaRetryAt, id)
		}
		a.mu.Unlock()
		return claudeQuotaVM(info), nil
	}
	info, err := a.refreshCodexQuota(a.context(), account)
	if err != nil {
		return QuotaVM{}, humanError(err)
	}
	return codexQuotaVM(info), nil
}

func (a *App) ListKeys() ([]KeyVM, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	keys, err := (appsvc.KeyService{DB: a.db}).List(a.context())
	if err != nil {
		return nil, humanError(err)
	}
	result := make([]KeyVM, 0, len(keys))
	for _, key := range keys {
		result = append(result, keyVM(key))
	}
	return result, nil
}

func (a *App) CreateKey(name string) (CreatedKeyVM, error) {
	if err := a.ready(); err != nil {
		return CreatedKeyVM{}, err
	}
	key, err := (appsvc.KeyService{DB: a.db}).Create(a.context(), name)
	if err != nil {
		return CreatedKeyVM{}, humanError(err)
	}
	return CreatedKeyVM{KeyVM: keyVM(key), Secret: key.Secret}, nil
}

func (a *App) DeleteKey(id string) error {
	if err := a.ready(); err != nil {
		return err
	}
	return humanError((appsvc.KeyService{DB: a.db}).Delete(a.context(), id))
}

func (a *App) GetSettings() (SettingsVM, error) {
	if err := a.ready(); err != nil {
		return SettingsVM{}, err
	}
	settings, err := a.settings()
	if err != nil {
		return SettingsVM{}, humanError(err)
	}
	return settingsVM(settings), nil
}

func (a *App) SaveSettings(settings SettingsVM) (SettingsVM, error) {
	if err := a.ready(); err != nil {
		return SettingsVM{}, err
	}
	settings.Host = strings.TrimSpace(settings.Host)
	settings.DefaultModel = strings.TrimSpace(settings.DefaultModel)
	storedSettings := a.profile.applySettings(settings.storeSettings())
	if err := a.db.SaveSettings(a.context(), storedSettings); err != nil {
		return SettingsVM{}, humanError(err)
	}
	if a.tray != nil {
		a.tray.setEnabled(storedSettings.TrayEnabled)
	}
	return a.GetSettings()
}

func (a *App) Logs(filter string) ([]LogVM, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	filter = strings.ToLower(strings.TrimSpace(filter))
	entries := a.logger.Entries()
	result := make([]LogVM, 0, len(entries))
	for _, entry := range entries {
		if filter != "" && filter != "all" && entry.Source != filter {
			continue
		}
		result = append(result, LogVM{Time: entry.Time.UTC().Format(time.RFC3339Nano), Source: entry.Source, Message: entry.Message})
	}
	return result, nil
}

func (a *App) ClearLogs() error {
	if err := a.ready(); err != nil {
		return err
	}
	a.logger.Clear()
	return nil
}

func (a *App) CodexConfig() (string, error) {
	settings, key, err := a.configInputs()
	if err != nil {
		return "", err
	}
	return appsvc.CodexConfigText(settings.Port, key, settings.DefaultModel), nil
}

func (a *App) ClaudeConfig() (string, error) {
	settings, key, err := a.configInputs()
	if err != nil {
		return "", err
	}
	model := appsvc.DefaultClaudeModel
	if strings.HasPrefix(strings.ToLower(settings.DefaultModel), "claude") {
		model = settings.DefaultModel
	}
	return appsvc.ClaudeConfigText(settings.Port, key, model), nil
}

func (a *App) Version() string {
	result := fmt.Sprintf("%s · %s", iversion.Version, iversion.Commit)
	if a.profile.Name == developmentProfile {
		result += " · DEV INSTANCE"
	}
	return result
}

func (a *App) beginOAuth(provider, accountID string) (OAuthSessionVM, error) {
	if err := a.ready(); err != nil {
		return OAuthSessionVM{}, err
	}
	provider, err := store.CanonicalProvider(provider)
	if err != nil {
		return OAuthSessionVM{}, humanError(err)
	}
	if provider == store.ProviderCustom {
		return OAuthSessionVM{}, humanError(errors.New("custom provider connections do not use OAuth"))
	}
	session := a.providerService().NewAuthSessionForProvider(provider, "")
	pending := &pendingOAuth{session: session, accountID: accountID}
	loopbackError := ""
	if provider == store.ProviderOpenAICodex {
		pending.loopback, err = startOAuthLoopback()
		if err != nil {
			loopbackError = appsvc.HumanError(err.Error())
		}
	}
	id := randomID()
	a.mu.Lock()
	a.pending[id] = pending
	a.mu.Unlock()
	if err := os.MkdirAll(a.dataDir, 0o700); err == nil {
		_ = os.WriteFile(filepath.Join(a.dataDir, provider+"-auth-url.txt"), []byte(session.AuthURL+"\n"), 0o600)
	}
	if a.desktopApp != nil {
		_ = a.desktopApp.Browser.OpenURL(session.AuthURL)
	}
	return OAuthSessionVM{
		ID: id, Provider: provider, AccountID: accountID, AuthURL: session.AuthURL,
		Loopback: pending.loopback != nil, LoopbackError: loopbackError,
	}, nil
}

func (a *App) completeOAuth(sessionID, name, callbackURL string) (ConnectionVM, error) {
	pending, err := a.getPending(sessionID)
	if err != nil {
		return ConnectionVM{}, err
	}
	pending.mu.Lock()
	defer pending.mu.Unlock()
	if pending.completed {
		return ConnectionVM{}, humanError(errors.New("OAuth session has already completed"))
	}
	var account store.Account
	if pending.accountID == "" {
		account, err = a.providerService().AddFromCallback(a.context(), pending.session, name, callbackURL)
	} else {
		account, err = a.providerService().ReAuthFromCallback(a.context(), pending.session, pending.accountID, callbackURL)
	}
	if err != nil {
		return ConnectionVM{}, humanError(err)
	}
	pending.completed = true
	pending.loopback.stop()
	a.mu.Lock()
	delete(a.pending, sessionID)
	a.mu.Unlock()
	return a.connectionView(account), nil
}

func (a *App) getPending(id string) (*pendingOAuth, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pending := a.pending[id]
	if pending == nil {
		return nil, humanError(errors.New("OAuth session was not found or has expired"))
	}
	return pending, nil
}

func (a *App) providerService() appsvc.ProviderService {
	return appsvc.ProviderService{DB: a.db, Logger: a.logger}
}

func (a *App) configInputs() (store.Settings, string, error) {
	if err := a.ready(); err != nil {
		return store.Settings{}, "", err
	}
	settings, err := a.settings()
	if err != nil {
		return store.Settings{}, "", humanError(err)
	}
	keys, err := (appsvc.KeyService{DB: a.db}).List(a.context())
	if err != nil {
		return store.Settings{}, "", humanError(err)
	}
	key := "sk-lm-router-REPLACE_ME"
	if len(keys) > 0 {
		key = keys[0].Prefix + "-REPLACE_WITH_FULL_SECRET"
	}
	return settings, key, nil
}

func (a *App) settings() (store.Settings, error) {
	settings, err := a.db.GetSettings(a.context())
	if err != nil {
		return store.Settings{}, err
	}
	return a.profile.applySettings(settings), nil
}

func (a *App) ready() error {
	if a.initErr != nil {
		return humanError(a.initErr)
	}
	if a.db == nil || a.controller == nil {
		return humanError(errors.New("LM Router desktop is still starting"))
	}
	return nil
}

func (a *App) context() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func (a *App) showError(title string, err error) {
	if a.desktopApp == nil || err == nil {
		return
	}
	dialog := a.desktopApp.Dialog.Error().
		SetTitle(title).
		SetMessage(appsvc.HumanError(err.Error()))
	if a.mainWindow != nil && a.mainWindow.NativeWindow() != nil {
		dialog.AttachToWindow(a.mainWindow)
	}
	dialog.Show()
}

func (a *App) refreshTray() {
	if a.tray == nil {
		return
	}
	if a.initErr != nil {
		a.tray.refresh(ServerStatusVM{State: string(appsvc.ServerError), Error: a.initErr.Error()})
		return
	}
	if a.db == nil || a.controller == nil {
		a.tray.refresh(ServerStatusVM{State: string(appsvc.ServerStarting)})
		return
	}
	settings, err := a.settings()
	if err != nil {
		a.tray.refresh(ServerStatusVM{State: string(appsvc.ServerError), Error: err.Error()})
		return
	}
	a.tray.refresh(serverStatusVM(a.controller.Status(), settings))
}

func (a *App) updateTray(status ServerStatusVM) {
	if a.tray != nil {
		a.tray.refresh(status)
	}
}

func marshalDesktopError(err error) []byte {
	if err == nil {
		return []byte("null")
	}
	result, marshalErr := json.Marshal(appsvc.HumanError(err.Error()))
	if marshalErr != nil {
		return nil
	}
	return result
}

func configuredEndpoint(settings store.Settings) string {
	return fmt.Sprintf("http://%s:%d", displayHost(settings.Host), settings.Port)
}

func probeHealth(settings store.Settings) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(configuredEndpoint(settings) + "/health")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func humanError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(appsvc.HumanError(err.Error()))
}

func randomID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("oauth-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
