package app

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
)

func TestCodexQuotaRecoverySchedulerChecksOnlyEligibleDueAccounts(t *testing.T) {
	ctx := context.Background()
	db := openRecoveryStore(t, ctx)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for _, account := range []store.Account{
		recoveryAccount("due", now.Add(-time.Second)),
		recoveryAccount("future", now.Add(time.Hour)),
		{ID: "active", Provider: store.ProviderOpenAICodex, Name: "active", Priority: 3, Enabled: true},
		func() store.Account {
			account := recoveryAccount("disabled", now.Add(-time.Second))
			account.Enabled = false
			return account
		}(),
		func() store.Account {
			account := recoveryAccount("reauth", now.Add(-time.Second))
			account.NeedsReauth = true
			return account
		}(),
		{ID: "claude", Provider: store.ProviderAnthropicClaude, Name: "claude", Priority: 1, Enabled: true, CooldownUntil: sql.NullTime{Time: now.Add(-time.Second), Valid: true}},
	} {
		if err := db.UpsertAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}

	var checked []string
	scheduler := NewCodexQuotaRecoveryScheduler(db, ProviderService{DB: db})
	scheduler.Quota = func(_ context.Context, account store.Account) (codex.QuotaInfo, error) {
		checked = append(checked, account.ID)
		return codex.QuotaInfo{}, nil
	}
	scheduler.scan(ctx, now, make(map[string]time.Time))
	if len(checked) != 1 || checked[0] != "due" {
		t.Fatalf("checked=%v, want [due]", checked)
	}
}

func TestCodexQuotaRecoverySchedulerRunsImmediatelyAndThrottlesRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := openRecoveryStore(t, ctx)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertAccount(ctx, recoveryAccount("due", now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{}, 1)
	var calls atomic.Int32
	scheduler := NewCodexQuotaRecoveryScheduler(db, ProviderService{DB: db})
	scheduler.Now = func() time.Time { return now }
	scheduler.Quota = func(context.Context, store.Account) (codex.QuotaInfo, error) {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		return codex.QuotaInfo{}, errors.New("offline")
	}
	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("immediate startup scan did not run")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}

	retryAfter := make(map[string]time.Time)
	scheduler.scan(context.Background(), now, retryAfter)
	scheduler.scan(context.Background(), now.Add(5*time.Second), retryAfter)
	if calls.Load() != 2 { // one Run attempt, one direct scan attempt
		t.Fatalf("calls=%d, want 2", calls.Load())
	}
	scheduler.scan(context.Background(), now.Add(time.Minute), retryAfter)
	if calls.Load() != 3 {
		t.Fatalf("calls=%d, want 3 after retry interval", calls.Load())
	}
}

func TestCodexQuotaRecoverySchedulerLimitsWorkersAndCancelsQueuedJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db := openRecoveryStore(t, ctx)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := db.UpsertAccount(ctx, recoveryAccount(string(rune('a'+i)), now.Add(-time.Second))); err != nil {
			t.Fatal(err)
		}
	}

	started := make(chan struct{}, 5)
	var active atomic.Int32
	var maximum atomic.Int32
	scheduler := NewCodexQuotaRecoveryScheduler(db, ProviderService{DB: db})
	scheduler.Quota = func(ctx context.Context, _ store.Account) (codex.QuotaInfo, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		<-ctx.Done()
		return codex.QuotaInfo{}, ctx.Err()
	}

	done := make(chan struct{})
	go func() {
		scheduler.scan(ctx, now, make(map[string]time.Time))
		close(done)
	}()
	for range CodexQuotaRecoveryWorkers {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("quota workers did not start")
		}
	}
	if got := maximum.Load(); got > CodexQuotaRecoveryWorkers {
		t.Fatalf("maximum workers=%d, want <= %d", got, CodexQuotaRecoveryWorkers)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scan did not finish after cancellation")
	}
}

func TestCodexQuotaRecoverySchedulerDisabledAndReenabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := openRecoveryStore(t, ctx)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertAccount(ctx, recoveryAccount("due", now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	scheduler := NewCodexQuotaRecoveryScheduler(db, ProviderService{DB: db})
	scheduler.Now = func() time.Time { return now }
	scheduler.Configure(false, 20*time.Millisecond)
	scheduler.Quota = func(context.Context, store.Account) (codex.QuotaInfo, error) {
		calls.Add(1)
		return codex.QuotaInfo{}, nil
	}
	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()
	time.Sleep(60 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("disabled scheduler made %d calls", got)
	}
	scheduler.Configure(true, 20*time.Millisecond)
	waitForQuotaCalls(t, &calls, 1)
	if got := calls.Load(); got != 1 {
		t.Fatalf("re-enable made %d calls, want immediate single scan", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
}

func TestCodexQuotaRecoverySchedulerIntervalReconfigureResetsTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := openRecoveryStore(t, ctx)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertAccount(ctx, recoveryAccount("due", now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	scheduler := NewCodexQuotaRecoveryScheduler(db, ProviderService{DB: db})
	scheduler.Now = time.Now
	scheduler.RetryInterval = time.Millisecond
	scheduler.Configure(true, time.Hour)
	scheduler.Quota = func(context.Context, store.Account) (codex.QuotaInfo, error) {
		calls.Add(1)
		return codex.QuotaInfo{}, nil
	}
	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()
	waitForQuotaCalls(t, &calls, 1)
	scheduler.Configure(true, 80*time.Millisecond)
	time.Sleep(25 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("reconfigured timer fired too early with %d calls", got)
	}
	waitForQuotaCalls(t, &calls, 2)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
}

func TestCodexQuotaRecoverySchedulerRapidDisableEnableStillScans(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := openRecoveryStore(t, ctx)
	now := time.Now().UTC()
	if err := db.UpsertAccount(ctx, recoveryAccount("due", now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	scheduler := NewCodexQuotaRecoveryScheduler(db, ProviderService{DB: db})
	scheduler.Now = time.Now
	scheduler.RetryInterval = time.Millisecond
	scheduler.Configure(true, time.Hour)
	scheduler.Quota = func(context.Context, store.Account) (codex.QuotaInfo, error) {
		calls.Add(1)
		return codex.QuotaInfo{}, nil
	}
	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()
	waitForQuotaCalls(t, &calls, 1)
	scheduler.Configure(false, time.Hour)
	scheduler.Configure(true, time.Hour)
	waitForQuotaCalls(t, &calls, 2)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
}

func TestCodexQuotaRecoverySchedulerDisableLeavesInFlightProbeRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := openRecoveryStore(t, ctx)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertAccount(ctx, recoveryAccount("due", now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	scheduler := NewCodexQuotaRecoveryScheduler(db, ProviderService{DB: db})
	scheduler.Now = func() time.Time { return now }
	scheduler.Quota = func(context.Context, store.Account) (codex.QuotaInfo, error) {
		calls.Add(1)
		close(started)
		<-release
		return codex.QuotaInfo{}, nil
	}
	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	scheduler.Configure(false, 20*time.Millisecond)
	close(release)
	time.Sleep(60 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("disable cancelled or duplicated in-flight probe: calls=%d", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
}

func TestCodexQuotaRecoverySchedulerConfigureConcurrentWithRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db := openRecoveryStore(t, ctx)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertAccount(ctx, recoveryAccount("due", now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}

	scheduler := NewCodexQuotaRecoveryScheduler(db, ProviderService{DB: db})
	scheduler.Now = func() time.Time { return now }
	scheduler.RetryInterval = time.Millisecond
	scheduler.Quota = func(ctx context.Context, _ store.Account) (codex.QuotaInfo, error) {
		select {
		case <-ctx.Done():
			return codex.QuotaInfo{}, ctx.Err()
		default:
			return codex.QuotaInfo{}, nil
		}
	}
	done := make(chan struct{})
	go func() {
		scheduler.Run(ctx)
		close(done)
	}()
	for i := 0; i < 100; i++ {
		scheduler.Configure(i%2 == 0, time.Duration(1+i%3)*time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after concurrent configuration")
	}
}

func waitForQuotaCalls(t *testing.T, calls *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("calls=%d, want at least %d", calls.Load(), want)
}

func openRecoveryStore(t *testing.T, ctx context.Context) *store.DB {
	t.Helper()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func recoveryAccount(id string, cooldown time.Time) store.Account {
	return store.Account{
		ID: id, Provider: store.ProviderOpenAICodex, Name: id, Priority: 1, Enabled: true,
		AccessToken: "token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
		CooldownUntil: sql.NullTime{Time: cooldown, Valid: true},
	}
}
