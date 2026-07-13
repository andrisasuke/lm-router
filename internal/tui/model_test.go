package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if model.screen != screenProviders {
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
	if !strings.Contains(view, "Model routing: pass-through") {
		t.Fatalf("pass-through routing label missing: %s", view)
	}
	if strings.Contains(view, "Alias: cx") || strings.Contains(view, "cx/") || strings.Contains(view, "Models:") {
		t.Fatalf("unexpected legacy model display in view: %s", view)
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
