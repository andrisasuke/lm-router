package app

import (
	"context"
	"testing"

	"github.com/andrisasuke/lm-router/internal/store"
)

func TestProviderServiceNotifiesAfterSuccessfulAccountMutations(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var notifications int
	service := ProviderService{
		DB: db,
		OnAccountChange: func() {
			notifications++
		},
	}

	account, err := service.AddCustomProvider(ctx, AddCustomProviderParams{
		Name: "main", Prefix: "myapi", BaseURL: "https://api.example.com/v1",
		APIKey: "sk-secret", CompatType: store.CompatOpenAIStyle, APIType: store.CustomAPITypeChat,
	})
	if err != nil {
		t.Fatalf("add custom provider: %v", err)
	}
	if notifications != 1 {
		t.Fatalf("notifications after add=%d, want 1", notifications)
	}

	name := "renamed"
	if _, err := service.UpdateCustomProvider(ctx, account.ID, UpdateCustomProviderParams{Name: &name}); err != nil {
		t.Fatalf("update custom provider: %v", err)
	}
	if err := service.Rename(ctx, account.ID, "final"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := service.SetEnabled(ctx, account.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if notifications != 4 {
		t.Fatalf("notifications after updates=%d, want 4", notifications)
	}

	if err := service.Rename(ctx, account.ID, ""); err == nil {
		t.Fatal("expected blank rename to fail")
	}
	if notifications != 4 {
		t.Fatalf("notifications after failed rename=%d, want 4", notifications)
	}

	if err := service.Delete(ctx, account.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if notifications != 5 {
		t.Fatalf("notifications after delete=%d, want 5", notifications)
	}
}
