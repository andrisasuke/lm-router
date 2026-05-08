package store

import (
	"context"
	"testing"
	"time"
)

func TestAccountCRUDAndKeyValidation(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	account := Account{
		ID:           "acct_1",
		Provider:     "openai-codex",
		Name:         "main",
		Priority:     1,
		Enabled:      true,
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
		MetadataJSON: `{"email":"a@example.com"}`,
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	accounts, err := db.ListRoutableAccounts(ctx, "openai-codex")
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "acct_1" {
		t.Fatalf("unexpected accounts: %+v", accounts)
	}

	key, err := db.CreateAPIKey(ctx, "test")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if !db.ValidateAPIKey(ctx, key.Secret) {
		t.Fatal("expected api key to validate")
	}
	if db.ValidateAPIKey(ctx, "bad-key") {
		t.Fatal("bad key validated")
	}
}

func TestGetAccountByNameReturnsLowestPriority(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	for _, account := range []Account{
		{ID: "acct_2", Provider: "openai-codex", Name: "main", Priority: 2, Enabled: true, AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)},
		{ID: "acct_1", Provider: "openai-codex", Name: "main", Priority: 1, Enabled: true, AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)},
	} {
		if err := db.UpsertAccount(ctx, account); err != nil {
			t.Fatalf("upsert account: %v", err)
		}
	}

	got, err := db.GetAccountByName(ctx, "main")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if got.ID != "acct_1" {
		t.Fatalf("got %s want acct_1", got.ID)
	}
}

func TestRenameAccountUpdatesName(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	account := Account{
		ID:           "acct_1",
		Provider:     "openai-codex",
		Name:         "main",
		Priority:     1,
		Enabled:      true,
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	if err := db.RenameAccount(ctx, "acct_1", "work"); err != nil {
		t.Fatalf("rename account: %v", err)
	}
	got, err := db.GetAccount(ctx, "acct_1")
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Name != "work" {
		t.Fatalf("name=%q want work", got.Name)
	}
}
