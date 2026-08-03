package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/anthropic"
	"github.com/andrisasuke/lm-router/internal/app"
	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

func TestHomeNavigationMovesSelectionAndOpensProviders(t *testing.T) {
	model := NewTestModel()

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = next.(Model)
	if model.selected != 1 {
		t.Fatalf("selected=%d", model.selected)
	}

	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model = next.(Model)
	if model.selected != 0 {
		t.Fatalf("selected=%d", model.selected)
	}

	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if model.screen != screenProviderTypes {
		t.Fatalf("screen=%v", model.screen)
	}
}

func TestProvidersViewShowsDefaultModelAndPassThroughRouting(t *testing.T) {
	model := NewTestModel()
	model.screen = screenProviders

	view := model.View()

	if !strings.Contains(view, "Provider: OpenAI Codex") {
		t.Fatalf("provider label missing: %s", view)
	}
	if !strings.Contains(view, "Default model: gpt-5.3-codex") {
		t.Fatalf("default model missing: %s", view)
	}
	if !strings.Contains(view, "Model routing: gpt* via Codex Responses") {
		t.Fatalf("prefix routing label missing: %s", view)
	}
	if strings.Contains(view, "Alias: cx") || strings.Contains(view, "cx/") || strings.Contains(view, "Models:") {
		t.Fatalf("unexpected legacy model display in view: %s", view)
	}
}

func TestProviderTypeSelectionOpensFilteredClaudePool(t *testing.T) {
	model := NewTestModel()
	model.screen = screenProviderTypes
	model.selected = 2

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if model.screen != screenProviders || model.selectedProvider != store.ProviderAnthropicClaude {
		t.Fatalf("screen=%v provider=%q", model.screen, model.selectedProvider)
	}
	view := model.View()
	if !strings.Contains(view, "Provider: Anthropic Claude") ||
		!strings.Contains(view, "Default model: claude-opus-4-8") ||
		!strings.Contains(view, "claude* via native Messages API") {
		t.Fatalf("view=%s", view)
	}
}

func TestProviderTypeSelectionClearsRowsAndIgnoresStaleLoads(t *testing.T) {
	model := NewTestModel()
	model.screen = screenProviderTypes
	model.selected = 2
	model.providerLoadSeq = 7
	model.accounts = []store.Account{{ID: "codex", Provider: store.ProviderOpenAICodex, Name: "old"}}

	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if cmd == nil || model.screen != screenProviders || len(model.accounts) != 0 {
		t.Fatalf("screen=%v accounts=%+v cmd_nil=%t", model.screen, model.accounts, cmd == nil)
	}
	currentSeq := model.providerLoadSeq
	next, _ = model.Update(loadProvidersMsg{
		provider: store.ProviderOpenAICodex,
		seq:      currentSeq - 1,
		accounts: []store.Account{{ID: "stale", Provider: store.ProviderOpenAICodex}},
	})
	model = next.(Model)
	if len(model.accounts) != 0 {
		t.Fatalf("stale provider load was applied: %+v", model.accounts)
	}

	next, _ = model.Update(loadProvidersMsg{
		provider: store.ProviderAnthropicClaude,
		seq:      currentSeq,
		accounts: []store.Account{{ID: "claude", Provider: store.ProviderAnthropicClaude}},
	})
	model = next.(Model)
	if len(model.accounts) != 1 || model.accounts[0].ID != "claude" {
		t.Fatalf("current provider load was not applied: %+v", model.accounts)
	}
}

func TestKeyboardBackResetsProviderPoolAndReloadsHomeAccounts(t *testing.T) {
	model := NewTestModel()
	model.screen = screenProviders
	model.stack = []screen{screenHome, screenProviderTypes}
	model.selectedProvider = store.ProviderAnthropicClaude
	model.providerLoadSeq = 4
	model.accounts = []store.Account{{ID: "claude", Provider: store.ProviderAnthropicClaude}}

	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = next.(Model)
	if cmd != nil || model.screen != screenProviderTypes || model.selectedProvider != "" || len(model.accounts) != 0 {
		t.Fatalf("provider exit screen=%v provider=%q accounts=%+v", model.screen, model.selectedProvider, model.accounts)
	}
	invalidatedSeq := model.providerLoadSeq

	next, cmd = model.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	model = next.(Model)
	if cmd == nil || model.screen != screenHome || model.selectedProvider != "" || model.providerLoadSeq <= invalidatedSeq {
		t.Fatalf("home exit screen=%v provider=%q seq=%d cmd_nil=%t", model.screen, model.selectedProvider, model.providerLoadSeq, cmd == nil)
	}
	next, _ = model.Update(loadProvidersMsg{
		seq: model.providerLoadSeq,
		accounts: []store.Account{
			{ID: "codex", Provider: store.ProviderOpenAICodex},
			{ID: "claude", Provider: store.ProviderAnthropicClaude},
		},
	})
	model = next.(Model)
	if len(model.accounts) != 2 {
		t.Fatalf("home accounts were not restored: %+v", model.accounts)
	}
}

func TestClaudeAddRequiresRiskConfirmationAndWritesProviderAuthURL(t *testing.T) {
	dataDir := t.TempDir()
	model := NewWithDataDir(context.Background(), nil, app.NewRingLogger(10, nil), app.NewServerController(app.ServerControllerConfig{}), store.DefaultSettings(), dataDir)
	model.screen = screenProviders
	model.selectedProvider = store.ProviderAnthropicClaude
	model.selected = 1

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if model.screen != screenClaudeRisk || !strings.Contains(model.View(), "I understand, continue") || !strings.Contains(model.View(), "combines capacity") {
		t.Fatalf("risk screen missing: %s", model.View())
	}
	model.selected = 1
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if model.screen != screenAddProvider || model.authSession.Provider != store.ProviderAnthropicClaude {
		t.Fatalf("screen=%v session=%+v", model.screen, model.authSession)
	}
	wantPath := filepath.Join(dataDir, "anthropic-claude-auth-url.txt")
	if model.authURLPath != wantPath {
		t.Fatalf("path=%s want=%s", model.authURLPath, wantPath)
	}
}

func TestClaudeQuota429UsesSeparateThreeMinuteUICooldown(t *testing.T) {
	model := NewTestModel()
	model.screen = screenProviderDetail
	model.selected = 3
	model.selectedAccount = 0
	model.accounts = []store.Account{{ID: "claude", Provider: store.ProviderAnthropicClaude, Name: "main"}}
	quota := anthropic.UsageInfo{
		Connected: true,
		Available: false,
		Status:    429,
		RetryAt:   time.Now().Add(anthropic.UsageRetryCooldown),
	}
	next, _ := model.Update(providerQuotaDoneMsg{accountID: "claude", claude: &quota})
	model = next.(Model)
	model.claudeQuota = nil // Simulate leaving and re-entering the detail page.
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if cmd == nil || model.providerQuotaLoading || !strings.Contains(model.statusLine, "quota check is cooling down") {
		t.Fatalf("loading=%t status=%q", model.providerQuotaLoading, model.statusLine)
	}
	if model.accounts[0].CooldownUntil.Valid {
		t.Fatal("quota cooldown leaked into inference account state")
	}
}

func TestAliasValidationAllowsSameNameAcrossProviders(t *testing.T) {
	model := NewTestModel()
	model.selectedProvider = store.ProviderAnthropicClaude
	model.accounts = []store.Account{{ID: "codex", Provider: store.ProviderOpenAICodex, Name: "main"}}
	if err := model.validateAliasName("MAIN", ""); err != nil {
		t.Fatalf("cross-provider alias should be allowed: %v", err)
	}
}

func TestClaudeConfigViewPrintsEnvironmentWithoutEditingFiles(t *testing.T) {
	model := NewTestModel()
	model.screen = screenClaudeConfig
	view := model.View()
	if !strings.Contains(view, "ANTHROPIC_BASE_URL=http://127.0.0.1:19090") ||
		!strings.Contains(view, "ANTHROPIC_AUTH_TOKEN=") ||
		!strings.Contains(view, "ANTHROPIC_MODEL=claude-opus-4-8") {
		t.Fatalf("view=%s", view)
	}
}

func TestCompletedProviderAddReturnsToSingleProviderListLevel(t *testing.T) {
	model := NewTestModel()
	model.screen = screenAddProvider
	model.stack = []screen{screenProviderTypes, screenProviders}
	next, _ := model.Update(addProviderDoneMsg{account: store.Account{ID: "claude", Provider: store.ProviderAnthropicClaude, Name: "main"}})
	model = next.(Model)
	if model.screen != screenProviders || len(model.stack) != 1 || model.stack[0] != screenProviderTypes {
		t.Fatalf("screen=%v stack=%v", model.screen, model.stack)
	}
}

func TestAddProviderWritesFullAuthURLToDataDir(t *testing.T) {
	dataDir := t.TempDir()
	model := NewWithDataDir(
		context.Background(),
		nil,
		app.NewRingLogger(10, nil),
		app.NewServerController(app.ServerControllerConfig{}),
		store.DefaultSettings(),
		dataDir,
	)
	model.screen = screenProviders
	model.selected = 1

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)

	if model.authURLPath == "" {
		t.Fatalf("auth URL path missing")
	}
	if filepath.Dir(model.authURLPath) != dataDir {
		t.Fatalf("auth URL path=%s want dir %s", model.authURLPath, dataDir)
	}
	data, err := os.ReadFile(model.authURLPath)
	if err != nil {
		t.Fatalf("read auth URL file: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != model.authSession.AuthURL {
		t.Fatalf("file URL mismatch\nfile=%s\nmodel=%s", got, model.authSession.AuthURL)
	}
	if !strings.Contains(model.View(), "Full URL saved to: "+model.authURLPath) {
		t.Fatalf("saved path missing from view: %s", model.View())
	}
}

func TestAuthViewsSeparateSavedURLWithBlankLine(t *testing.T) {
	model := NewTestModel()
	model.authSession.AuthURL = "https://example.com/authorize"
	model.authURLPath = "/tmp/lm-router-auth-url.txt"
	want := "https://example.com/authorize\n\nFull URL saved to: /tmp/lm-router-auth-url.txt"

	for name, view := range map[string]string{
		"add provider":    model.viewAddProvider(),
		"reauth provider": model.viewReauthProvider(),
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(view, want) {
				t.Fatalf("saved URL should be separated by a blank line:\n%s", view)
			}
		})
	}
}

func TestProviderDetailShowsQuotaHeaderDiagnostics(t *testing.T) {
	model := NewTestModel()
	model.accounts = []store.Account{{
		ID:        "acct_1",
		Name:      "main",
		Enabled:   true,
		ExpiresAt: time.Now().Add(time.Hour),
	}}
	model.screen = screenProviderDetail
	model.selectedAccount = 0
	model.providerQuota = &codex.QuotaInfo{
		HeaderKeys: []string{"x-request-id", "x-ratelimit-limit"},
	}

	view := model.View()
	if !strings.Contains(view, "Quota:      no data (no x-codex-*-used-percent header)") {
		t.Fatalf("missing quota diagnostic: %s", view)
	}
	if !strings.Contains(view, "x-* headers seen: x-request-id, x-ratelimit-limit") {
		t.Fatalf("missing observed header keys: %s", view)
	}
}

func TestAddProviderViewRequiresConnectionName(t *testing.T) {
	model := NewTestModel()
	model.screen = screenAddProvider

	view := model.View()

	if !strings.Contains(view, "Connection name:") {
		t.Fatalf("connection name input missing: %s", view)
	}
}

func TestAddProviderRejectsDuplicateConnectionName(t *testing.T) {
	model := NewTestModel()
	model.screen = screenAddProvider
	model.addProviderField = addProviderCallbackField
	model.accounts = []store.Account{{Name: "main"}}
	model.providerNameInput.SetValue("main")
	model.callbackInput.SetValue("http://localhost:1455/auth/callback?code=test&state=test")

	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)

	if cmd != nil {
		t.Fatalf("duplicate name should not start oauth exchange")
	}
	if !strings.Contains(model.statusLine, "already exists") {
		t.Fatalf("expected duplicate error, got %q", model.statusLine)
	}
}

func TestProviderViewsUseCodexEmailAsPrimaryLabel(t *testing.T) {
	account := store.Account{
		ID:           "acct_1",
		Name:         "main",
		Enabled:      true,
		MetadataJSON: `{"email":"andri.sasuki@gmail.com","chatgpt_account_id":"acct_openai"}`,
	}
	model := NewTestModel()
	model.accounts = []store.Account{account}
	model.screen = screenProviders

	providersView := model.View()
	if !strings.Contains(providersView, "andri.sasuki@gmail.com (main, Active)") {
		t.Fatalf("provider list should show email and alias: %s", providersView)
	}

	model.screen = screenProviderDetail
	model.selectedAccount = 0
	detailView := model.View()
	if !strings.Contains(detailView, "Connection: andri.sasuki@gmail.com") {
		t.Fatalf("detail should show email as connection: %s", detailView)
	}
	if !strings.Contains(detailView, "Alias:      main") {
		t.Fatalf("detail should show alias: %s", detailView)
	}
	if !strings.Contains(detailView, "Edit Alias") {
		t.Fatalf("detail should include edit alias action: %s", detailView)
	}
}

func TestProviderDisplayFallsBackToChatGPTAccountIDThenAlias(t *testing.T) {
	withAccountID := store.Account{Name: "main", MetadataJSON: `{"chatgpt_account_id":"acct_openai"}`}
	if got := providerDisplayName(withAccountID); got != "acct_openai" {
		t.Fatalf("display=%q want acct_openai", got)
	}
	aliasOnly := store.Account{Name: "main", MetadataJSON: `{}`}
	if got := providerDisplayName(aliasOnly); got != "main" {
		t.Fatalf("display=%q want main", got)
	}
}

func TestEditAliasRenamesProvider(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	account := store.Account{
		ID:           "acct_1",
		Provider:     "openai-codex",
		Name:         "main",
		Priority:     1,
		Enabled:      true,
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now(),
		MetadataJSON: `{"email":"andri.sasuki@gmail.com"}`,
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	model := New(ctx, db, app.NewRingLogger(10, nil), app.NewServerController(app.ServerControllerConfig{}), store.DefaultSettings())
	model.accounts = []store.Account{account}
	model.screen = screenProviderDetail
	model.selectedAccount = 0
	model.selected = 1

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if !model.aliasEditing {
		t.Fatalf("expected alias editing mode")
	}
	model.aliasInput.SetValue("work")
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)

	if model.accounts[0].Name != "work" {
		t.Fatalf("model alias=%q want work", model.accounts[0].Name)
	}
	got, err := db.GetAccount(ctx, "acct_1")
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Name != "work" {
		t.Fatalf("stored alias=%q want work", got.Name)
	}
}

func TestProvidersShiftArrowReordersAccountsAndPersistsPriority(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	accounts := []store.Account{
		{ID: "acct_1", Provider: "openai-codex", Name: "main", Priority: 1, Enabled: true, AccessToken: "a1", RefreshToken: "r1", ExpiresAt: time.Now()},
		{ID: "acct_2", Provider: "openai-codex", Name: "backup", Priority: 2, Enabled: true, AccessToken: "a2", RefreshToken: "r2", ExpiresAt: time.Now()},
	}
	for _, account := range accounts {
		if err := db.UpsertAccount(ctx, account); err != nil {
			t.Fatalf("upsert account: %v", err)
		}
	}
	model := New(ctx, db, app.NewRingLogger(10, nil), app.NewServerController(app.ServerControllerConfig{}), store.DefaultSettings())
	model.screen = screenProviders
	model.accounts = accounts
	model.selected = 3

	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyShiftUp})
	model = next.(Model)

	if cmd == nil {
		t.Fatalf("expected clear status timer command")
	}
	if model.accounts[0].ID != "acct_2" || model.accounts[1].ID != "acct_1" {
		t.Fatalf("accounts not reordered: %+v", model.accounts)
	}
	if model.selected != 2 {
		t.Fatalf("selected=%d want 2", model.selected)
	}
	got, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if got[0].ID != "acct_2" || got[0].Priority != 1 || got[1].ID != "acct_1" || got[1].Priority != 2 {
		t.Fatalf("stored priority not swapped: %+v", got)
	}

	seq := model.statusSeq
	next, _ = model.Update(clearStatusMsg{seq: seq})
	model = next.(Model)
	if model.statusLine != "" {
		t.Fatalf("status should clear after timeout, got %q", model.statusLine)
	}

	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyShiftDown})
	model = next.(Model)
	if model.accounts[0].ID != "acct_1" || model.accounts[1].ID != "acct_2" {
		t.Fatalf("accounts not moved down: %+v", model.accounts)
	}
}

func TestClearStatusIgnoresStaleTimer(t *testing.T) {
	model := NewTestModel()
	model, _ = model.setTimedStatus("first")
	oldSeq := model.statusSeq
	model, _ = model.setTimedStatus("second")

	next, _ := model.Update(clearStatusMsg{seq: oldSeq})
	model = next.(Model)

	if model.statusLine != "second" {
		t.Fatalf("stale timer cleared current status: %q", model.statusLine)
	}
}

func TestStatusTimeoutIsThreeSeconds(t *testing.T) {
	if statusTimeout != 3*time.Second {
		t.Fatalf("statusTimeout=%s want 3s", statusTimeout)
	}
}

func TestDeleteAPIKeyRequiresConfirmation(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	key, err := db.CreateAPIKey(ctx, "prod-key")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	model := New(ctx, db, app.NewRingLogger(10, nil), app.NewServerController(app.ServerControllerConfig{}), store.DefaultSettings())
	model.keys = []store.APIKey{key}
	model.screen = screenKeys
	model.selected = 2 // first key's delete row ("<- Back", "Create Key", <keys...>)

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if len(model.keys) != 1 {
		t.Fatalf("expected key not yet deleted, keys=%+v", model.keys)
	}
	if model.confirmAction != confirmDeleteKey {
		t.Fatalf("expected confirmDeleteKey armed, got %v", model.confirmAction)
	}
	if view := model.View(); !strings.Contains(view, `Delete API key "prod-key"?`) {
		t.Fatalf("confirm prompt not rendered: %q", view)
	}

	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	model = next.(Model)
	if len(model.keys) != 0 {
		t.Fatalf("expected key deleted, keys=%+v", model.keys)
	}
	stored, err := db.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("expected key removed from store, got %+v", stored)
	}
}

func TestClearLogsRequiresConfirmation(t *testing.T) {
	logger := app.NewRingLogger(10, nil)
	logger.Printf("hello world")
	model := NewTestModel()
	model.logger = logger
	model.screen = screenLogs
	model.selected = 2 // "Clear Logs" row

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if len(model.logger.Entries()) == 0 {
		t.Fatal("expected log entries to survive Enter (not yet confirmed)")
	}
	if model.confirmAction != confirmClearLogs {
		t.Fatalf("expected confirmClearLogs armed, got %v", model.confirmAction)
	}
	if view := model.View(); !strings.Contains(view, "Clear all logs?") {
		t.Fatalf("confirm prompt not rendered: %q", view)
	}

	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	model = next.(Model)
	if len(model.logger.Entries()) != 0 {
		t.Fatalf("expected logs cleared, entries=%+v", model.logger.Entries())
	}
}

func TestConfirmCancelledByEscOrEnter(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyEnter}} {
		model := NewTestModel()
		model.logger.Printf("keep me")
		model.confirmAction = confirmClearLogs
		model.confirmLabel = "Clear all logs? (y/N)"

		next, _ := model.Update(key)
		model = next.(Model)
		if model.confirmAction != confirmNone {
			t.Fatalf("key %v: expected confirmation cancelled, got %v", key.Type, model.confirmAction)
		}
		if len(model.logger.Entries()) == 0 {
			t.Fatalf("key %v: expected logs to survive cancellation, not be cleared", key.Type)
		}
	}
}
