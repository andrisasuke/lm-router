package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
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
		{ID: "acct_2", Provider: "anthropic-claude", Name: "main", Priority: 2, Enabled: true, AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)},
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

func TestAccountAliasAndPriorityAreScopedByProvider(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	for _, account := range []Account{
		{ID: "codex", Provider: ProviderOpenAICodex, Name: "main", Priority: 1, Enabled: true},
		{ID: "claude", Provider: ProviderAnthropicClaude, Name: "main", Priority: 1, Enabled: true},
	} {
		if err := db.UpsertAccount(ctx, account); err != nil {
			t.Fatalf("upsert %s: %v", account.ID, err)
		}
	}
	if err := db.UpsertAccount(ctx, Account{ID: "codex-duplicate", Provider: ProviderOpenAICodex, Name: "MAIN", Priority: 2}); err == nil {
		t.Fatal("expected duplicate alias in one provider to fail")
	}
	got, err := db.GetAccountByProviderAndName(ctx, "claude", "MAIN")
	if err != nil || got.ID != "claude" {
		t.Fatalf("provider lookup got=%+v err=%v", got, err)
	}
	if next, err := db.NextPriorityForProvider(ctx, ProviderOpenAICodex); err != nil || next != 2 {
		t.Fatalf("codex next priority=%d err=%v", next, err)
	}
	if next, err := db.NextPriorityForProvider(ctx, ProviderAnthropicClaude); err != nil || next != 2 {
		t.Fatalf("claude next priority=%d err=%v", next, err)
	}
}

func TestCanonicalProviderAcceptsCLIClaudeAlias(t *testing.T) {
	if got, err := CanonicalProvider(" Claude "); err != nil || got != ProviderAnthropicClaude {
		t.Fatalf("got=%q err=%v", got, err)
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

func TestRetryStateMigrationPreservesExistingAccount(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	legacy, err := sql.Open("sqlite", filepath.Join(dir, "lm-router.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `
		create table accounts (
			id text primary key, provider text not null, name text not null,
			priority integer not null default 0, enabled integer not null default 1,
			needs_reauth integer not null default 0, access_token text not null default '',
			refresh_token text not null default '', expires_at text not null default '',
			cooldown_until text, metadata_json text not null default '{}'
		);
		insert into accounts (id, provider, name, priority, enabled, access_token)
		values ('legacy', 'openai-codex', 'main', 1, 1, 'kept');
	`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	account, err := db.GetAccount(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if account.AccessToken != "kept" || account.ConsecutiveFailures != 0 || account.LastFailureAt.Valid {
		t.Fatalf("migrated account=%+v", account)
	}
	// Re-running init exercises migration idempotence.
	if err := db.init(ctx); err != nil {
		t.Fatalf("second migration: %v", err)
	}
}

func TestLegacyDuplicateAliasesAreMigratedBeforeReauthentication(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	legacy, err := sql.Open("sqlite", filepath.Join(dir, "lm-router.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `
		create table accounts (
			id text primary key, provider text not null, name text not null,
			priority integer not null default 0, enabled integer not null default 1,
			needs_reauth integer not null default 0, access_token text not null default '',
			refresh_token text not null default '', expires_at text not null default '',
			cooldown_until text, metadata_json text not null default '{}'
		);
		insert into accounts (id, provider, name, priority, enabled, access_token)
		values
			('first', 'openai-codex', 'openai-codex', 1, 1, 'old-first'),
			('second', 'openai-codex', 'OPENAI-CODEX', 2, 1, 'old-second'),
			('reserved', 'openai-codex', 'openai-codex-2', 3, 1, 'reserved');
	`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	accounts, err := db.ListAccountsByProvider(ctx, ProviderOpenAICodex)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 3 || accounts[0].Name != "openai-codex" || accounts[1].Name != "OPENAI-CODEX-3" || accounts[2].Name != "openai-codex-2" {
		t.Fatalf("migrated aliases=%+v", accounts)
	}

	// Re-authentication upserts the existing row while preserving its migrated
	// identity. It must no longer collide with the sibling legacy account.
	second := accounts[1]
	second.AccessToken = "fresh-second"
	if err := db.UpsertAccount(ctx, second); err != nil {
		t.Fatalf("save re-authenticated account: %v", err)
	}
	got, err := db.GetAccount(ctx, "second")
	if err != nil || got.AccessToken != "fresh-second" {
		t.Fatalf("re-authenticated account=%+v err=%v", got, err)
	}
	if err := db.UpsertAccount(ctx, Account{ID: "new", Provider: ProviderOpenAICodex, Name: "OPENAI-CODEX"}); err == nil {
		t.Fatal("provider-scoped unique alias should be enforced after migration")
	}
}

func TestRetryFailureStatePersistsBackoffAndResets(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertAccount(ctx, Account{ID: "claude", Provider: ProviderAnthropicClaude, Name: "main", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	until, err := db.RecordRetryableFailure(ctx, "claude", now, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if delay := until.Sub(now); delay < 2*time.Second || delay > 2500*time.Millisecond {
		t.Fatalf("first backoff=%s", delay)
	}
	hint := now.Add(2 * time.Minute)
	if got, err := db.RecordRetryableFailure(ctx, "claude", now.Add(time.Second), hint); err != nil || !got.Equal(hint) {
		t.Fatalf("hint cooldown=%v err=%v", got, err)
	}
	account, _ := db.GetAccount(ctx, "claude")
	if account.ConsecutiveFailures != 2 || !account.LastFailureAt.Valid || !account.CooldownUntil.Valid {
		t.Fatalf("failure state=%+v", account)
	}
	if err := db.ResetFailureState(ctx, "claude"); err != nil {
		t.Fatal(err)
	}
	account, _ = db.GetAccount(ctx, "claude")
	if account.ConsecutiveFailures != 0 || account.LastFailureAt.Valid || account.CooldownUntil.Valid {
		t.Fatalf("reset state=%+v", account)
	}
}

func TestPrioritySwapCASLeavesIntermediateAndCannotReverse(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, account := range []Account{
		{ID: "first", Provider: ProviderAnthropicClaude, Name: "first", Priority: 1},
		{ID: "middle", Provider: ProviderAnthropicClaude, Name: "middle", Priority: 2},
		{ID: "success", Provider: ProviderAnthropicClaude, Name: "success", Priority: 3},
	} {
		if err := db.UpsertAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	swapped, err := db.SwapAccountPrioritiesCAS(ctx, ProviderAnthropicClaude, "first", 1, "success", 3)
	if err != nil || !swapped {
		t.Fatalf("first swap=%t err=%v", swapped, err)
	}
	if swapped, err = db.SwapAccountPrioritiesCAS(ctx, ProviderAnthropicClaude, "first", 1, "success", 3); err != nil || swapped {
		t.Fatalf("stale swap=%t err=%v", swapped, err)
	}
	first, _ := db.GetAccount(ctx, "first")
	middle, _ := db.GetAccount(ctx, "middle")
	success, _ := db.GetAccount(ctx, "success")
	if first.Priority != 3 || middle.Priority != 2 || success.Priority != 1 {
		t.Fatalf("priorities first=%d middle=%d success=%d", first.Priority, middle.Priority, success.Priority)
	}
}

func TestConcurrentPrioritySwapCASSucceedsOnce(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, account := range []Account{
		{ID: "first", Provider: ProviderAnthropicClaude, Name: "first", Priority: 1},
		{ID: "success", Provider: ProviderAnthropicClaude, Name: "success", Priority: 2},
	} {
		if err := db.UpsertAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	var successes atomic.Int32
	errCh := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			swapped, err := db.SwapAccountPrioritiesCAS(ctx, ProviderAnthropicClaude, "first", 1, "success", 2)
			if err != nil {
				errCh <- err
				return
			}
			if swapped {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if successes.Load() != 1 {
		t.Fatalf("successful swaps=%d", successes.Load())
	}
}
