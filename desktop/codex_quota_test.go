//go:build wailsapp

package main

import (
	"context"
	"encoding/json"
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

func TestCodexQuotaTrackerReportsErrorWithoutCachedQuota(t *testing.T) {
	tracker := newCodexQuotaTracker(nil)
	flight, _ := tracker.Begin("account")
	tracker.Finish("account", flight, codex.QuotaInfo{}, errors.New("offline"))
	view := connectionQuotaVM(tracker.Snapshot("account"))
	if view.State != quotaStateError || view.Summary != "Quota unavailable" {
		t.Fatalf("view=%+v", view)
	}
}

func TestCodexQuotaTrackerKeepsCachedQuotaOnRefreshFailure(t *testing.T) {
	tracker := newCodexQuotaTracker(nil)
	fetchedAt := time.Now()
	tracker.Observe("account", codex.QuotaInfo{
		FetchedAt: fetchedAt,
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 25},
		Secondary: &codex.QuotaWindow{WindowMinutes: 10080, UsedPercent: 35},
	})
	flight, leader := tracker.Begin("account")
	if !leader {
		t.Fatal("expected refresh leader")
	}
	tracker.Finish("account", flight, codex.QuotaInfo{}, errors.New("offline"))
	state := tracker.Snapshot("account")
	if state.Loading || state.Error != "offline" || state.Info.Primary == nil || state.Info.Secondary == nil {
		t.Fatalf("cached quota was lost: %+v", state)
	}
	if state.Info.Primary.UsedPercent != 25 || state.Info.Secondary.UsedPercent != 35 || !state.Info.FetchedAt.Equal(fetchedAt) {
		t.Fatalf("cached quota changed: %+v", state.Info)
	}
	if view := connectionQuotaVM(state); view.State != quotaStateError || view.Summary != "Quota refresh failed; showing last known quota" || len(view.Windows) != 2 {
		t.Fatalf("cached quota view=%+v", view)
	}
}

func TestCodexQuotaTrackerKeepsStaleTrayStateDuringRetry(t *testing.T) {
	tracker := newCodexQuotaTracker(nil)
	tracker.Observe("account", codex.QuotaInfo{
		FetchedAt: time.Unix(100, 0),
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 25},
		Secondary: &codex.QuotaWindow{WindowMinutes: 10080, UsedPercent: 35},
	})
	flight, leader := tracker.Begin("account")
	if !leader {
		t.Fatal("initial refresh should become leader")
	}
	tracker.Finish("account", flight, codex.QuotaInfo{}, errors.New("offline"))

	retry, leader := tracker.Begin("account")
	if !leader || retry == nil {
		t.Fatal("retry should become leader")
	}
	state := tracker.Snapshot("account")
	if !state.Loading || state.Error != "offline" {
		t.Fatalf("retry state=%+v, want loading with the previous error", state)
	}
	rows := trayCodexRows([]trayCodexAccount{{
		Account: store.Account{Name: "main", Enabled: true},
		Quota:   state,
	}})
	if want := "main [stale] - 5h(75%) 7d(65%)"; len(rows) != 1 || rows[0] != want {
		t.Fatalf("retry rows=%q, want %q", rows, want)
	}

	tracker.Finish("account", retry, codex.QuotaInfo{
		FetchedAt: time.Unix(200, 0),
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 10},
		Secondary: &codex.QuotaWindow{WindowMinutes: 10080, UsedPercent: 20},
	}, nil)
	state = tracker.Snapshot("account")
	rows = trayCodexRows([]trayCodexAccount{{
		Account: store.Account{Name: "main", Enabled: true},
		Quota:   state,
	}})
	if want := "main - 5h(90%) 7d(80%)"; len(rows) != 1 || rows[0] != want {
		t.Fatalf("recovered rows=%q, want %q", rows, want)
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
	if event.Status == nil || *event.Status != "Cooldown" {
		t.Fatalf("status=%v, want Cooldown", event.Status)
	}
	if event.CooldownUntil == nil || *event.CooldownUntil == "" {
		t.Fatal("cooldown timestamp was not included")
	}
	if event.Quota.State != quotaStateAvailable || len(event.Quota.Windows) != 1 {
		t.Fatalf("quota=%+v", event.Quota)
	}
}

func TestConnectionQuotaEventSerializesRecoveryResetFields(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account := store.Account{ID: "account", Provider: store.ProviderOpenAICodex, Name: "main", Priority: 1, Enabled: true}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordRetryableFailure(ctx, account.ID, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.ResetFailureState(ctx, account.ID); err != nil {
		t.Fatal(err)
	}

	application := &App{ctx: ctx, db: db}
	payload, err := json.Marshal(application.connectionQuotaEvent(account.ID, codexQuotaState{}))
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event["status"] != "Active" || event["cooldownUntil"] != "" || event["consecutiveFailures"] != float64(0) {
		t.Fatalf("recovery event=%s", payload)
	}
}

func TestConnectionQuotaEventDoesNotClearStateWhenAccountReloadFails(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	application := &App{ctx: ctx, db: db}
	payload, err := json.Marshal(application.connectionQuotaEvent("missing", codexQuotaState{}))
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"status", "cooldownUntil", "consecutiveFailures"} {
		if _, ok := event[field]; ok {
			t.Fatalf("reload failure incorrectly emitted %s in %s", field, payload)
		}
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
