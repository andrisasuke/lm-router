package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/andrisasuke/lm-router/internal/app"
	"github.com/andrisasuke/lm-router/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

func TestProviderTypesMenuIncludesCustomProviderOption(t *testing.T) {
	model := NewTestModel()
	model.screen = screenProviderTypes
	if view := model.View(); !strings.Contains(view, "Custom Provider") {
		t.Fatalf("view=%s", view)
	}

	model.selected = 3
	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if model.screen != screenProviders || model.selectedProvider != store.ProviderCustom {
		t.Fatalf("screen=%v provider=%q", model.screen, model.selectedProvider)
	}
}

func TestBeginProviderAuthPushesCustomWizardInsteadOfOAuth(t *testing.T) {
	model := NewTestModel()
	model.screen = screenProviders
	model.selectedProvider = store.ProviderCustom
	model.selected = 1 // "Add New Connection"

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if model.screen != screenAddCustomProvider {
		t.Fatalf("screen=%v want screenAddCustomProvider", model.screen)
	}
	if model.authSession.AuthURL != "" {
		t.Fatalf("unexpected OAuth auth URL populated: %q", model.authSession.AuthURL)
	}
}

func TestCustomProviderWizardSavesConnection(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	model := New(ctx, db, app.NewRingLogger(10, nil), app.NewServerController(app.ServerControllerConfig{}), store.DefaultSettings())

	next, _ := model.beginAddCustomProvider("")
	model = next.(Model)
	if model.screen != screenAddCustomProvider || model.customStep != customStepCompatType {
		t.Fatalf("screen=%v step=%v", model.screen, model.customStep)
	}

	// Compat type: default selection 0 = openai-compatible.
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if model.customStep != customStepName {
		t.Fatalf("step=%v want name", model.customStep)
	}

	model.customNameInput.SetValue("my-server")
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)

	model.customPrefixInput.SetValue("myapi")
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)

	model.customBaseURLInput.SetValue("https://api.example.com/v1")
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)

	model.customAPIKeyInput.SetValue("sk-secret")
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if model.customStep != customStepAPIType {
		t.Fatalf("step=%v want api type", model.customStep)
	}

	// API type: default selection 0 = chat.
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if cmd == nil {
		t.Fatal("expected a save command")
	}
	next, _ = model.Update(cmd())
	model = next.(Model)

	if model.screen != screenProviders {
		t.Fatalf("screen=%v want screenProviders", model.screen)
	}
	if len(model.accounts) != 1 {
		t.Fatalf("accounts=%+v", model.accounts)
	}
	saved := model.accounts[0]
	if saved.Prefix != "myapi" || saved.BaseURL != "https://api.example.com/v1" ||
		saved.AccessToken != "sk-secret" || saved.CompatType != store.CompatOpenAIStyle || saved.APIType != store.CustomAPITypeChat {
		t.Fatalf("saved=%+v", saved)
	}
	stored, err := db.GetAccountByPrefix(ctx, "myapi")
	if err != nil {
		t.Fatalf("get by prefix: %v", err)
	}
	if stored.Name != "my-server" {
		t.Fatalf("stored=%+v", stored)
	}
}

func TestCustomProviderWizardRejectsDuplicatePrefix(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertAccount(ctx, store.Account{
		ID: "existing", Provider: store.ProviderCustom, Name: "existing", Enabled: true,
		Prefix: "myapi", BaseURL: "https://a.example.com", CompatType: store.CompatOpenAIStyle, APIType: store.CustomAPITypeChat,
	}); err != nil {
		t.Fatal(err)
	}
	model := New(ctx, db, app.NewRingLogger(10, nil), app.NewServerController(app.ServerControllerConfig{}), store.DefaultSettings())

	next, _ := model.beginAddCustomProvider("")
	model = next.(Model)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter}) // compat type -> name
	model = next.(Model)
	model.customNameInput.SetValue("new-one")
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter}) // name -> prefix
	model = next.(Model)

	model.customPrefixInput.SetValue("myapi")
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter}) // duplicate prefix, should stay put
	model = next.(Model)
	if model.customStep != customStepPrefix {
		t.Fatalf("step=%v want still at prefix after duplicate rejected", model.customStep)
	}
	if !strings.Contains(model.statusLine, "already in use") {
		t.Fatalf("statusLine=%q", model.statusLine)
	}
}

func TestCustomProviderEditReusesWizardAndPreseedsFields(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	account := store.Account{
		ID: "acct_1", Provider: store.ProviderCustom, Name: "my-server", Enabled: true,
		AccessToken: "sk-original", Prefix: "myapi", BaseURL: "https://api.example.com/v1",
		CompatType: store.CompatOpenAIStyle, APIType: store.CustomAPITypeChat,
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	model := New(ctx, db, app.NewRingLogger(10, nil), app.NewServerController(app.ServerControllerConfig{}), store.DefaultSettings())
	model.accounts = []store.Account{account}

	next, _ := model.beginAddCustomProvider("acct_1")
	model = next.(Model)
	if model.customStep != customStepName {
		t.Fatalf("step=%v want name (compat type step skipped on edit)", model.customStep)
	}
	if model.customNameInput.Value() != "my-server" || model.customPrefixInput.Value() != "myapi" ||
		model.customBaseURLInput.Value() != "https://api.example.com/v1" {
		t.Fatalf("wizard not pre-seeded: name=%q prefix=%q baseURL=%q",
			model.customNameInput.Value(), model.customPrefixInput.Value(), model.customBaseURLInput.Value())
	}
	if model.customAPIKeyInput.Value() != "" {
		t.Fatalf("api key input should start blank, got %q", model.customAPIKeyInput.Value())
	}

	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter}) // name -> prefix
	model = next.(Model)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter}) // prefix -> baseURL
	model = next.(Model)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter}) // baseURL -> apikey
	model = next.(Model)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter}) // apikey (left blank) -> apitype
	model = next.(Model)
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter}) // apitype -> submit
	model = next.(Model)
	if cmd == nil {
		t.Fatal("expected save command")
	}
	next, _ = model.Update(cmd())
	model = next.(Model)

	updated, err := db.GetAccount(ctx, "acct_1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.AccessToken != "sk-original" {
		t.Fatalf("access token=%q want unchanged sk-original", updated.AccessToken)
	}
}

func TestCustomProviderDetailMenuOmitsOAuthActions(t *testing.T) {
	account := store.Account{ID: "c1", Provider: store.ProviderCustom, Name: "my-server", Enabled: true, Prefix: "myapi"}
	model := NewTestModel()
	model.accounts = []store.Account{account}
	model.selectedAccount = 0
	model.screen = screenProviderDetail

	if got := model.itemCount(); got != 5 {
		t.Fatalf("itemCount=%d want 5", got)
	}
	view := model.View()
	for _, forbidden := range []string{"Show Quota Limit", "Refresh Token", "Re-authenticate"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("view unexpectedly contains %q: %s", forbidden, view)
		}
	}
	if !strings.Contains(view, "Edit Connection") {
		t.Fatalf("view missing Edit Connection: %s", view)
	}
}

// TestCustomProviderDetailDeleteAtCorrectIndex guards against the
// index/action desync: a 5-item custom menu must map "Delete Connection"
// (index 4) to detailDelete, not to whatever index 4 means in the 8-item
// OAuth menu ("Refresh Token").
func TestCustomProviderDetailDeleteAtCorrectIndex(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	account := store.Account{
		ID: "c1", Provider: store.ProviderCustom, Name: "my-server", Enabled: true,
		Prefix: "myapi", BaseURL: "https://x", CompatType: store.CompatOpenAIStyle, APIType: store.CustomAPITypeChat,
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	model := New(ctx, db, app.NewRingLogger(10, nil), app.NewServerController(app.ServerControllerConfig{}), store.DefaultSettings())
	model.accounts = []store.Account{account}
	model.selectedAccount = 0
	model.screen = screenProviderDetail
	model.selected = 4 // "Delete Connection" in the 5-item custom menu

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(Model)
	if len(model.accounts) != 0 {
		t.Fatalf("expected account deleted, accounts=%+v", model.accounts)
	}
	if _, err := db.GetAccount(ctx, "c1"); err == nil {
		t.Fatal("expected account removed from store")
	}
}

func TestProviderTitleRoutingAndDefaultModelHandleCustomProvider(t *testing.T) {
	if got := providerTitle(store.ProviderCustom); got != "Custom Provider" {
		t.Fatalf("title=%q", got)
	}
	if got := providerRouting(store.ProviderCustom); !strings.Contains(got, "passthrough") {
		t.Fatalf("routing=%q", got)
	}
	if got := providerDefaultModel(store.ProviderCustom, "gpt-5.3-codex"); !strings.Contains(got, "per connection") {
		t.Fatalf("defaultModel=%q", got)
	}
}
