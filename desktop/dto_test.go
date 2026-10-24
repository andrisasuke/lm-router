//go:build wailsapp

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsvc "github.com/andrisasuke/lm-router/internal/app"
	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
)

func TestConnectionVMDoesNotExposeStoredCredentials(t *testing.T) {
	account := store.Account{
		ID: "acct", Provider: store.ProviderCustom, Name: "private", Priority: 1, Enabled: true,
		AccessToken: "custom-secret", RefreshToken: "refresh-secret", MetadataJSON: `{"token":"metadata-secret"}`,
		Prefix: "local", BaseURL: "https://example.test/v1", CompatType: store.CompatOpenAIStyle, APIType: store.CustomAPITypeResponses,
	}
	payload, err := json.Marshal(connectionVM(account))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"custom-secret", "refresh-secret", "metadata-secret", "accessToken", "refreshToken"} {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("connection DTO leaked %q: %s", secret, payload)
		}
	}
}

func TestConnectionViewIncludesCurrentRequestActivity(t *testing.T) {
	application := &App{}
	application.activity = appsvc.NewAccountActivityTracker(nil)
	account := store.Account{ID: "acct", Provider: store.ProviderOpenAICodex, Enabled: true}

	application.activity.Update(account.ID, true)
	if view := application.connectionView(account); !view.Requesting {
		t.Fatalf("active account should be requesting: %+v", view)
	}
	application.activity.Update(account.ID, false)
	if view := application.connectionView(account); view.Requesting {
		t.Fatalf("idle account should not be requesting: %+v", view)
	}
}

func TestKeyVMOnlyExposesPrefix(t *testing.T) {
	key := store.APIKey{ID: "key", Name: "local", Prefix: "sk-lm-router-abc", Secret: "full-secret", CreatedAt: time.Now()}
	payload, err := json.Marshal(keyVM(key))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), key.Secret) {
		t.Fatalf("key list DTO leaked secret: %s", payload)
	}
	if !strings.Contains(string(payload), key.Prefix) {
		t.Fatalf("key list DTO omitted prefix: %s", payload)
	}
}

func TestServerStatusAlwaysIncludesConfiguredEndpoint(t *testing.T) {
	status := serverStatusVM(appsvc.ServerStatus{State: appsvc.ServerOff}, store.Settings{Host: "0.0.0.0", Port: 0})
	if status.ConfiguredEndpoint != "http://127.0.0.1:0" || status.ActualEndpoint != "" {
		t.Fatalf("unexpected server status: %+v", status)
	}
}

func TestSettingsVMPreservesTrayPreference(t *testing.T) {
	settings := store.DefaultSettings()
	settings.TrayEnabled = false

	view := settingsVM(settings)
	if view.TrayEnabled {
		t.Fatalf("tray preference should be disabled: %+v", view)
	}
	if view.storeSettings().TrayEnabled {
		t.Fatalf("tray preference was lost during DTO round trip: %+v", view.storeSettings())
	}
}

func TestSettingsVMRoundTripsCodexQuotaRecovery(t *testing.T) {
	settings := store.DefaultSettings()
	settings.CodexQuotaRecoveryEnabled = false
	settings.CodexQuotaRecoveryIntervalSeconds = 300

	view := settingsVM(settings)
	got := view.storeSettings()
	if got.CodexQuotaRecoveryEnabled || got.CodexQuotaRecoveryIntervalSeconds != 300 {
		t.Fatalf("quota recovery preference was lost: %+v", got)
	}
}

func TestSaveSettingsConfiguresCodexQuotaRecovery(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	scheduler := appsvc.NewCodexQuotaRecoveryScheduler(db, appsvc.ProviderService{DB: db})
	now := time.Now().UTC()
	if err := db.UpsertAccount(ctx, store.Account{
		ID: "due", Provider: store.ProviderOpenAICodex, Name: "due", Priority: 1, Enabled: true,
		CooldownUntil: sql.NullTime{Time: now.Add(-time.Second), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	scheduler.Now = func() time.Time { return now }
	scheduler.Quota = func(context.Context, store.Account) (codex.QuotaInfo, error) {
		calls.Add(1)
		return codex.QuotaInfo{}, nil
	}
	application := &App{
		ctx:                ctx,
		db:                 db,
		controller:         appsvc.NewServerController(appsvc.ServerControllerConfig{}),
		profile:            desktopProfile{},
		codexQuotaRecovery: scheduler,
	}
	if _, err := application.SaveSettings(SettingsVM{
		CodexQuotaRecoveryEnabled:         false,
		CodexQuotaRecoveryIntervalSeconds: 300,
	}); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		scheduler.Run(runCtx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("disabled SaveSettings still allowed recovery calls=%d", got)
	}
	if _, err := application.SaveSettings(SettingsVM{
		CodexQuotaRecoveryEnabled:         true,
		CodexQuotaRecoveryIntervalSeconds: 5,
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
	if calls.Load() == 0 {
		t.Fatal("runtime SaveSettings did not re-enable quota recovery")
	}
}

func TestCodexQuotaVMUsesHumanReadableWindowNames(t *testing.T) {
	quota := codexQuotaVM(codex.QuotaInfo{
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 15},
		Secondary: &codex.QuotaWindow{WindowMinutes: 10080, UsedPercent: 15},
	})
	if len(quota.Windows) != 2 {
		t.Fatalf("unexpected quota windows: %+v", quota.Windows)
	}
	if quota.Windows[0].Name != "5h" || quota.Windows[1].Name != "weekly" {
		t.Fatalf("unexpected quota window names: %+v", quota.Windows)
	}
}

func TestCodexQuotaVMEmptyUsagePayload(t *testing.T) {
	quota := codexQuotaVM(codex.QuotaInfo{FetchedAt: time.Now()})
	if quota.Available || quota.Message != "No usage windows returned" || len(quota.Windows) != 0 {
		t.Fatalf("quota=%+v", quota)
	}
}

func TestConnectionQuotaVMSummarizesCardQuota(t *testing.T) {
	primaryReset := time.Now().Add(time.Hour)
	secondaryReset := time.Now().Add(24 * time.Hour)
	view := connectionQuotaVM(codexQuotaState{Info: codex.QuotaInfo{
		FetchedAt: time.Now(),
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 15, ResetAt: primaryReset},
		Secondary: &codex.QuotaWindow{WindowMinutes: 10080, UsedPercent: 15, ResetAt: secondaryReset},
	}})
	if view.State != quotaStateAvailable || view.Summary != "5h: 15% - Weekly: 15%" {
		t.Fatalf("unexpected card quota: %+v", view)
	}
	if len(view.Windows) != 2 || view.Windows[0].Critical || view.Windows[1].Critical {
		t.Fatalf("unexpected quota parts: %+v", view.Windows)
	}
	if view.Windows[0].ResetsAt != formatTime(primaryReset) || view.Windows[1].ResetsAt != formatTime(secondaryReset) {
		t.Fatalf("quota reset times were not preserved: %+v", view.Windows)
	}
}

func TestConnectionQuotaVMMarksOnlyWindowsAboveNinetyPercentCritical(t *testing.T) {
	view := connectionQuotaVM(codexQuotaState{Info: codex.QuotaInfo{
		FetchedAt: time.Now(),
		Primary:   &codex.QuotaWindow{WindowMinutes: 300, UsedPercent: 90},
		Secondary: &codex.QuotaWindow{WindowMinutes: 10080, UsedPercent: 91},
	}})
	if view.Windows[0].Critical {
		t.Fatal("90% must not be critical")
	}
	if !view.Windows[1].Critical {
		t.Fatal("values above 90% must be critical")
	}
}
