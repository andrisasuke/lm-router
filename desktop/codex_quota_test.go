//go:build wailsapp

package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsvc "github.com/andrisasuke/lm-router/internal/app"
	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
)

func TestCodexQuotaTrackerCoalescesRefreshes(t *testing.T) {
	tracker := newCodexQuotaTracker(nil)
	first, leader := tracker.Begin("account")
	if !leader || first == nil {
		t.Fatal("first refresh should become leader")
	}
	second, leader := tracker.Begin("account")
	if leader || second != first {
		t.Fatal("second refresh should join the active flight")
	}
	if state := tracker.Snapshot("account"); !state.Loading {
		t.Fatalf("state should be loading: %+v", state)
	}

	info := codex.QuotaInfo{
		FetchedAt: time.Now(),
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 15},
	}
	tracker.Finish("account", first, info, nil)
	select {
	case <-second.done:
	case <-time.After(time.Second):
		t.Fatal("joined refresh was not released")
	}
	state := tracker.Snapshot("account")
	if state.Loading || state.Info.Primary == nil || state.Info.Primary.UsedPercent != 15 {
		t.Fatalf("unexpected completed state: %+v", state)
	}
}

func TestCodexQuotaTrackerKeepsNewestObservation(t *testing.T) {
	tracker := newCodexQuotaTracker(nil)
	flight, leader := tracker.Begin("account")
	if !leader {
		t.Fatal("refresh should start")
	}
	base := time.Now()
	tracker.Observe("account", codex.QuotaInfo{
		FetchedAt: base.Add(time.Second),
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 20},
	})
	tracker.Finish("account", flight, codex.QuotaInfo{
		FetchedAt: base,
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 80},
	}, nil)

	state := tracker.Snapshot("account")
	if state.Loading || state.Info.Primary == nil || state.Info.Primary.UsedPercent != 20 {
		t.Fatalf("older probe replaced newer proxy observation: %+v", state)
	}
}

func TestCodexQuotaTrackerFindsOnlyExhaustedWindowsPastReset(t *testing.T) {
	now := time.Now()
	tracker := newCodexQuotaTracker(nil)
	tracker.Observe("due", codex.QuotaInfo{
		FetchedAt: now.Add(-time.Hour),
		Secondary: &codex.QuotaWindow{WindowMinutes: 10080, UsedPercent: 100, ResetAt: now.Add(-time.Second)},
	})
	tracker.Observe("future", codex.QuotaInfo{
		FetchedAt: now,
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 100, ResetAt: now.Add(time.Hour)},
	})
	tracker.Observe("available", codex.QuotaInfo{
		FetchedAt: now,
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 99, ResetAt: now.Add(-time.Second)},
	})

	due := tracker.ExhaustedDue(now)
	if len(due) != 1 || due[0] != "due" {
		t.Fatalf("due=%v, want [due]", due)
	}
}

func TestRefreshCodexQuotaJoinsActiveFetch(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	application := &App{codexQuotas: newCodexQuotaTracker(nil)}
	application.codexQuotaFetcher = func(context.Context, store.Account) (codex.QuotaInfo, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return codex.QuotaInfo{FetchedAt: time.Now(), Primary: &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 10}}, nil
	}
	account := store.Account{ID: "account", Provider: store.ProviderOpenAICodex}

	results := make(chan error, 2)
	go func() {
		_, err := application.refreshCodexQuota(context.Background(), account)
		results <- err
	}()
	<-started
	go func() {
		_, err := application.refreshCodexQuota(context.Background(), account)
		results <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("fetch calls=%d, want 1", calls.Load())
	}
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestWarmCodexQuotasUsesBoundedConcurrencyAndSkipsReauth(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for index := 0; index < 5; index++ {
		account := store.Account{
			ID: string(rune('a' + index)), Provider: store.ProviderOpenAICodex, Name: "account-" + string(rune('a'+index)), Priority: index + 1,
			Enabled: true, AccessToken: "token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
			NeedsReauth: index == 4,
		}
		if err := db.UpsertAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}

	started := make(chan struct{}, 5)
	release := make(chan struct{})
	var calls atomic.Int32
	var active atomic.Int32
	var maximum atomic.Int32
	application := &App{db: db, codexQuotas: newCodexQuotaTracker(nil)}
	application.codexQuotaFetcher = func(context.Context, store.Account) (codex.QuotaInfo, error) {
		calls.Add(1)
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return codex.QuotaInfo{FetchedAt: time.Now(), Primary: &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 10}}, nil
	}

	done := make(chan struct{})
	go func() {
		application.warmCodexQuotas(ctx)
		close(done)
	}()
	for range codexQuotaWorkers {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("quota workers did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("more than two quota requests started concurrently")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background quota refresh did not finish")
	}
	if calls.Load() != 4 {
		t.Fatalf("fetch calls=%d, want 4 non-reauth accounts", calls.Load())
	}
	if maximum.Load() > codexQuotaWorkers {
		t.Fatalf("maximum concurrency=%d, want <= %d", maximum.Load(), codexQuotaWorkers)
	}
}

func TestRefreshDueCodexQuotasRefreshesExpiredWindow(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account := store.Account{
		ID: "account", Provider: store.ProviderOpenAICodex, Name: "main", Priority: 1, Enabled: true,
		AccessToken: "token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tracker := newCodexQuotaTracker(nil)
	tracker.Observe(account.ID, codex.QuotaInfo{
		FetchedAt: now.Add(-time.Hour),
		Secondary: &codex.QuotaWindow{WindowMinutes: 10080, UsedPercent: 100, ResetAt: now.Add(-time.Second)},
	})
	var calls atomic.Int32
	application := &App{ctx: ctx, db: db, codexQuotas: tracker}
	application.codexQuotaFetcher = func(context.Context, store.Account) (codex.QuotaInfo, error) {
		calls.Add(1)
		return codex.QuotaInfo{
			FetchedAt: time.Now(),
			Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 4},
			Secondary: &codex.QuotaWindow{WindowMinutes: 10080, UsedPercent: 0},
		}, nil
	}

	application.refreshDueCodexQuotas(ctx, now, make(map[string]time.Time))
	if calls.Load() != 1 {
		t.Fatalf("quota refresh calls=%d, want 1", calls.Load())
	}
	state := tracker.Snapshot(account.ID)
	if state.Info.Secondary == nil || state.Info.Secondary.UsedPercent != 0 {
		t.Fatalf("quota state was not refreshed: %+v", state.Info)
	}
}

func TestRefreshDueCodexQuotasRateLimitsFailedRetry(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account := store.Account{
		ID: "account", Provider: store.ProviderOpenAICodex, Name: "main", Priority: 1, Enabled: true,
		AccessToken: "token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tracker := newCodexQuotaTracker(nil)
	tracker.Observe(account.ID, codex.QuotaInfo{
		FetchedAt: now.Add(-time.Hour),
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 100, ResetAt: now.Add(-time.Second)},
	})
	var calls atomic.Int32
	application := &App{ctx: ctx, db: db, codexQuotas: tracker}
	application.codexQuotaFetcher = func(context.Context, store.Account) (codex.QuotaInfo, error) {
		calls.Add(1)
		return codex.QuotaInfo{}, errors.New("offline")
	}
	retryAfter := make(map[string]time.Time)

	application.refreshDueCodexQuotas(ctx, now, retryAfter)
	application.refreshDueCodexQuotas(ctx, now.Add(codexQuotaResetPollInterval), retryAfter)
	if calls.Load() != 1 {
		t.Fatalf("quota refresh calls=%d, want one retry per minute", calls.Load())
	}
}

func TestCodexQuotaTrackerReportsErrorWithoutCachedQuota(t *testing.T) {
	tracker := newCodexQuotaTracker(nil)
	flight, _ := tracker.Begin("account")
	tracker.Finish("account", flight, codex.QuotaInfo{}, errors.New("offline"))
	view := connectionQuotaVM(tracker.Snapshot("account"))
	if view.State != quotaStateError || view.Summary != "Quota unavailable" {
		t.Fatalf("view=%+v", view)
	}
}

func TestQuotaActionUpdatesConnectionQuotaState(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account := store.Account{
		ID: "account", Provider: store.ProviderOpenAICodex, Name: "main", Priority: 1, Enabled: true,
		AccessToken: "token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	application := &App{
		ctx:         ctx,
		db:          db,
		controller:  appsvc.NewServerController(appsvc.ServerControllerConfig{}),
		codexQuotas: newCodexQuotaTracker(nil),
	}
	application.codexQuotaFetcher = func(context.Context, store.Account) (codex.QuotaInfo, error) {
		return codex.QuotaInfo{
			FetchedAt: time.Now(),
			Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 15},
			Secondary: &codex.QuotaWindow{WindowMinutes: 10080, UsedPercent: 25},
		}, nil
	}

	result, err := application.Quota(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Windows) != 2 {
		t.Fatalf("quota result=%+v", result)
	}
	view := application.connectionView(account)
	if view.Quota.State != quotaStateAvailable || view.Quota.Summary != "5h: 15% - Weekly: 25%" {
		t.Fatalf("connection quota=%+v", view.Quota)
	}
}

func TestConnectionQuotaEventIncludesCurrentCooldownStatus(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account := store.Account{
		ID: "account", Provider: store.ProviderOpenAICodex, Name: "main", Priority: 1, Enabled: true,
		AccessToken: "token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	cooldownUntil := time.Now().Add(30 * time.Minute).UTC()
	if err := db.SetCooldown(ctx, account.ID, cooldownUntil); err != nil {
		t.Fatal(err)
	}

	application := &App{ctx: ctx, db: db}
	event := application.connectionQuotaEvent(account.ID, codexQuotaState{Info: codex.QuotaInfo{
		FetchedAt: time.Now(),
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 100, ResetAt: cooldownUntil},
	}})
	if event.Status != "Cooldown" {
		t.Fatalf("status=%q, want Cooldown", event.Status)
	}
	if event.CooldownUntil == "" {
		t.Fatal("cooldown timestamp was not included")
	}
	if event.Quota.State != quotaStateAvailable || len(event.Quota.Windows) != 1 {
		t.Fatalf("quota=%+v", event.Quota)
	}
}

func TestCodexQuotaTrackerNotificationsAreSerialized(t *testing.T) {
	var mu sync.Mutex
	var states []string
	tracker := newCodexQuotaTracker(func(_ string, state codexQuotaState) {
		mu.Lock()
		defer mu.Unlock()
		if state.Loading {
			states = append(states, quotaStateLoading)
		} else {
			states = append(states, quotaStateAvailable)
		}
	})
	flight, _ := tracker.Begin("account")
	tracker.Finish("account", flight, codex.QuotaInfo{Primary: &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 10}}, nil)
	if len(states) != 2 || states[0] != quotaStateLoading || states[1] != quotaStateAvailable {
		t.Fatalf("states=%v", states)
	}
}
