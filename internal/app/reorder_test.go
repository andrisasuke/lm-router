package app

import (
	"context"
	"testing"

	"github.com/andrisasuke/lm-router/internal/store"
)

func TestProviderServiceReorderSwapsAdjacentAccounts(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, account := range []store.Account{
		{ID: "first", Provider: store.ProviderOpenAICodex, Name: "first", Priority: 1, Enabled: true, AccessToken: "one"},
		{ID: "second", Provider: store.ProviderOpenAICodex, Name: "second", Priority: 2, Enabled: true, AccessToken: "two"},
		{ID: "claude", Provider: store.ProviderAnthropicClaude, Name: "claude", Priority: 1, Enabled: true, AccessToken: "three"},
	} {
		if err := db.UpsertAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}

	service := ProviderService{DB: db}
	if err := service.Reorder(ctx, "second", -1); err != nil {
		t.Fatal(err)
	}

	accounts, err := db.ListAccountsByProvider(ctx, store.ProviderOpenAICodex)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].ID != "second" || accounts[1].ID != "first" {
		t.Fatalf("unexpected reordered pool: %#v", accounts)
	}
	claude, err := db.MustGetAccount(ctx, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if claude.Priority != 1 {
		t.Fatalf("other provider priority changed to %d", claude.Priority)
	}
}

func TestProviderServiceReorderValidatesDeltaAndBounds(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertAccount(ctx, store.Account{ID: "only", Provider: store.ProviderOpenAICodex, Name: "only", Priority: 1, Enabled: true, AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	service := ProviderService{DB: db}
	if err := service.Reorder(ctx, "only", -1); err != nil {
		t.Fatalf("boundary reorder should be a no-op: %v", err)
	}
	if err := service.Reorder(ctx, "only", 2); err == nil {
		t.Fatal("expected invalid delta error")
	}
}
