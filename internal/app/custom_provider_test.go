package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andrisasuke/lm-router/internal/store"
)

func TestAddCustomProviderPersistsConnection(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	service := ProviderService{DB: db}
	account, err := service.AddCustomProvider(ctx, AddCustomProviderParams{
		Name: "my-server", Prefix: "myapi", BaseURL: "https://api.example.com/v1",
		APIKey: "sk-secret", CompatType: store.CompatOpenAIStyle, APIType: store.CustomAPITypeChat,
	})
	if err != nil {
		t.Fatalf("add custom provider: %v", err)
	}
	got, err := db.GetAccountByPrefix(ctx, "myapi")
	if err != nil {
		t.Fatalf("get by prefix: %v", err)
	}
	if got.ID != account.ID || got.AccessToken != "sk-secret" || got.BaseURL != "https://api.example.com/v1" {
		t.Fatalf("stored account=%+v", got)
	}
}

func TestUpdateCustomProviderRejectsNonCustomAccount(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertAccount(ctx, store.Account{ID: "codex-1", Provider: store.ProviderOpenAICodex, Name: "main", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	service := ProviderService{DB: db}
	newName := "renamed"
	if _, err := service.UpdateCustomProvider(ctx, "codex-1", UpdateCustomProviderParams{Name: &newName}); err == nil {
		t.Fatal("expected update to be rejected for a non-custom account")
	}
}

func TestUpdateCustomProviderBlankAPIKeyKeepsExistingKey(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	service := ProviderService{DB: db}
	account, err := service.AddCustomProvider(ctx, AddCustomProviderParams{
		Name: "my-server", Prefix: "myapi", BaseURL: "https://api.example.com/v1",
		APIKey: "sk-original", CompatType: store.CompatOpenAIStyle, APIType: store.CustomAPITypeChat,
	})
	if err != nil {
		t.Fatal(err)
	}

	newBaseURL := "https://api.example.com/v2"
	blank := ""
	updated, err := service.UpdateCustomProvider(ctx, account.ID, UpdateCustomProviderParams{
		BaseURL: &newBaseURL, APIKey: &blank,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.AccessToken != "sk-original" {
		t.Fatalf("access token=%q want unchanged sk-original", updated.AccessToken)
	}
	if updated.BaseURL != newBaseURL {
		t.Fatalf("base url=%q want %q", updated.BaseURL, newBaseURL)
	}
}

func TestTestDispatchesToCustomProviderForOpenAICompat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	service := ProviderService{DB: db}
	account, err := service.AddCustomProvider(ctx, AddCustomProviderParams{
		Name: "my-server", Prefix: "myapi", BaseURL: server.URL,
		APIKey: "sk-secret", CompatType: store.CompatOpenAIStyle, APIType: store.CustomAPITypeChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Test(ctx, account, "")
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if !result.OK || result.Status != 200 {
		t.Fatalf("result=%+v", result)
	}
}

func TestTestReportsAuthFailureForCustomProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer server.Close()

	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	service := ProviderService{DB: db}
	account, err := service.AddCustomProvider(ctx, AddCustomProviderParams{
		Name: "my-server", Prefix: "myapi", BaseURL: server.URL,
		APIKey: "sk-bad", CompatType: store.CompatAnthropicStyle,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Test(ctx, account, "")
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if result.OK || result.Status != http.StatusUnauthorized {
		t.Fatalf("result=%+v", result)
	}
}
