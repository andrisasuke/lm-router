//go:build wailsapp

package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
	"github.com/wailsapp/wails/v3/pkg/application"
)

func TestTrayStateUsesActualEndpointAndRunningIcon(t *testing.T) {
	state := trayState(ServerStatusVM{
		State:              "ON",
		ConfiguredEndpoint: "http://127.0.0.1:19090",
		ActualEndpoint:     "http://127.0.0.1:43210",
	}, nil)
	if state.statusLabel != "Server: Running" || state.toggleLabel != "Stop Server" || state.icon != trayIconRunning {
		t.Fatalf("unexpected running state: %+v", state)
	}
	if state.endpoint != "http://127.0.0.1:43210" {
		t.Fatalf("endpoint = %q, want actual endpoint", state.endpoint)
	}
	if state.endpointLabel != "Endpoint 127.0.0.1:43210" {
		t.Fatalf("endpoint label = %q, want host and port only", state.endpointLabel)
	}
}

func TestTrayStateUsesIdleIconOutsideRunning(t *testing.T) {
	for _, stateName := range []string{"", "STARTING", "ERROR"} {
		state := trayState(ServerStatusVM{State: stateName}, nil)
		if state.icon != trayIconIdle {
			t.Fatalf("state %q icon=%v, want idle", stateName, state.icon)
		}
	}

	starting := trayState(ServerStatusVM{State: "STARTING", ConfiguredEndpoint: "http://127.0.0.1:19090"}, nil)
	if starting.toggleEnabled || starting.toggleLabel != "Starting…" {
		t.Fatalf("unexpected starting state: %+v", starting)
	}

	failed := trayState(ServerStatusVM{State: "ERROR"}, nil)
	if !failed.toggleEnabled || failed.toggleLabel != "Start Server" || failed.statusLabel != "Server: Error" {
		t.Fatalf("unexpected error state: %+v", failed)
	}
}

func TestTrayRemainingPercentClampsAndRounds(t *testing.T) {
	tests := []struct {
		used float64
		want int
	}{
		{used: -5, want: 100},
		{used: 0, want: 100},
		{used: 14.6, want: 85},
		{used: 15.5, want: 85},
		{used: 99.6, want: 0},
		{used: 100, want: 0},
		{used: 145, want: 0},
	}
	for _, test := range tests {
		if got := trayRemainingPercent(test.used); got != test.want {
			t.Errorf("used %.1f => %d, want %d", test.used, got, test.want)
		}
	}
}

func TestTrayCodexRowsFormatsCompletePartialUnknownAndStaleQuota(t *testing.T) {
	accounts := []trayCodexAccount{
		{
			Account: store.Account{ID: "complete", Name: "complete", Enabled: true},
			Quota: codexQuotaState{Info: codex.QuotaInfo{
				Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 15.4},
				Secondary: &codex.QuotaWindow{WindowMinutes: 10080, UsedPercent: 32.5},
			}},
		},
		{
			Account: store.Account{ID: "partial", Name: "partial", Enabled: true},
			Quota: codexQuotaState{Info: codex.QuotaInfo{
				Primary: &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 100},
			}},
		},
		{Account: store.Account{ID: "unknown", Name: "unknown", Enabled: true}},
		{
			Account: store.Account{ID: "stale", Name: "stale", Enabled: true},
			Quota: codexQuotaState{
				Error: "offline",
				Info: codex.QuotaInfo{
					Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 24.5},
					Secondary: &codex.QuotaWindow{WindowMinutes: 10080, UsedPercent: 100},
				},
			},
		},
	}
	want := []string{
		"complete - 5h(85%) 7d(68%)",
		"partial - 5h(0%) 7d(?)",
		"unknown - 5h(?) 7d(?)",
		"stale [stale] - 5h(76%) 7d(0%)",
	}
	if got := trayCodexRows(accounts); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%q, want %q", got, want)
	}
}

func TestTrayCodexRowsIncludesAccountStatusAndEmptyState(t *testing.T) {
	if got := trayCodexRows(nil); !reflect.DeepEqual(got, []string{"No Codex accounts"}) {
		t.Fatalf("empty rows=%q", got)
	}
	accounts := []trayCodexAccount{{
		Account: store.Account{Name: "paused", Enabled: false, NeedsReauth: true},
		Quota:   codexQuotaState{Error: "offline"},
	}}
	want := []string{"paused [disabled, reauth required, quota unavailable] - 5h(?) 7d(?)"}
	if got := trayCodexRows(accounts); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%q, want %q", got, want)
	}
}

func TestTrayCodexAccountsUsesProviderPriorityAndQuotaSnapshots(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	accounts := []store.Account{
		{ID: "third", Provider: store.ProviderOpenAICodex, Name: "third", Priority: 3, Enabled: true},
		{ID: "first", Provider: store.ProviderOpenAICodex, Name: "first", Priority: 1, Enabled: true, NeedsReauth: true},
		{ID: "second", Provider: store.ProviderOpenAICodex, Name: "second", Priority: 2, Enabled: false},
		{ID: "claude", Provider: store.ProviderAnthropicClaude, Name: "claude", Priority: 1, Enabled: true},
	}
	for _, account := range accounts {
		if err := db.UpsertAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	tracker := newCodexQuotaTracker(nil)
	tracker.Observe("third", codex.QuotaInfo{
		FetchedAt: time.Now(),
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 30},
		Secondary: &codex.QuotaWindow{WindowMinutes: 10080, UsedPercent: 40},
	})

	application := &App{ctx: ctx, db: db, codexQuotas: tracker}
	got := application.trayCodexAccounts()
	if len(got) != 3 {
		t.Fatalf("accounts=%+v, want three Codex accounts", got)
	}
	ids := []string{got[0].Account.ID, got[1].Account.ID, got[2].Account.ID}
	if want := []string{"first", "second", "third"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("priority order=%q, want %q", ids, want)
	}
	if got[2].Quota.Info.Primary == nil || got[2].Quota.Info.Primary.UsedPercent != 30 {
		t.Fatalf("quota snapshot=%+v", got[2].Quota)
	}
	rows := trayCodexRows(got)
	if rows[0] != "first [reauth required] - 5h(?) 7d(?)" || rows[1] != "second [disabled] - 5h(?) 7d(?)" {
		t.Fatalf("status rows=%q", rows)
	}
}

func TestTrayViewStateChangesWhenServerOrQuotaChanges(t *testing.T) {
	account := []trayCodexAccount{{
		Account: store.Account{Name: "main", Enabled: true},
		Quota: codexQuotaState{Info: codex.QuotaInfo{
			Primary: &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 10},
		}},
	}}
	idle := trayState(ServerStatusVM{State: "OFF"}, account)
	running := trayState(ServerStatusVM{State: "ON"}, account)
	if idle.equal(running) {
		t.Fatal("server state change did not produce a tray refresh")
	}
	account[0].Quota.Info.Primary.UsedPercent = 20
	quotaChanged := trayState(ServerStatusVM{State: "ON"}, account)
	if running.equal(quotaChanged) {
		t.Fatal("quota change did not produce a tray refresh")
	}
}

func TestTrayIconChangesOnlyWhenIconStateChanges(t *testing.T) {
	if !trayIconChanged(false, trayIconIdle, trayIconIdle) {
		t.Fatal("initial icon should be applied")
	}
	if trayIconChanged(true, trayIconIdle, trayIconIdle) {
		t.Fatal("unchanged icon state should not be applied")
	}
	if !trayIconChanged(true, trayIconIdle, trayIconRunning) {
		t.Fatal("changed icon state should be applied")
	}
}

func TestTrayMenuUsesEnabledInformationalCodexRows(t *testing.T) {
	app := application.New(application.Options{Name: "tray-test"})
	controller := newTrayController(app, nil, nil, desktopProfile{Title: "LM Router"}, nil, nil)
	menu := controller.menu(trayState(ServerStatusVM{}, []trayCodexAccount{{
		Account: store.Account{Name: "main", Enabled: true},
		Quota: codexQuotaState{Info: codex.QuotaInfo{
			Primary: &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 10},
		}},
	}}))
	t.Cleanup(menu.Destroy)

	codexItem := menu.FindByLabel("Codex Account")
	if codexItem == nil || !codexItem.IsSubmenu() {
		t.Fatalf("Codex Account submenu=%v", codexItem)
	}
	row := codexItem.GetSubmenu().FindByLabel("main - 5h(90%) 7d(?)")
	if row == nil {
		t.Fatal("quota row not found")
	}
	if !row.Enabled() {
		t.Fatal("quota row is disabled; native menu text would be gray")
	}
}
