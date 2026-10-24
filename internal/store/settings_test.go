package store

import (
	"context"
	"testing"
)

func TestSettingsDefaultsAndPersistence(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	settings, err := db.GetSettings(ctx)
	if err != nil {
		t.Fatalf("get defaults: %v", err)
	}
	if settings.Host != "127.0.0.1" || settings.Port != 19090 {
		t.Fatalf("unexpected default bind: %+v", settings)
	}
	if !settings.LogRequests || !settings.LogUpstream {
		t.Fatalf("logging should default on: %+v", settings)
	}
	if !settings.TrayEnabled {
		t.Fatalf("tray should default on: %+v", settings)
	}
	if settings.LogBodyLimit != 64*1024 {
		t.Fatalf("body limit=%d", settings.LogBodyLimit)
	}
	if settings.DefaultModel != "gpt-5.3-codex" {
		t.Fatalf("default model=%q", settings.DefaultModel)
	}
	if !settings.CodexQuotaRecoveryEnabled || settings.CodexQuotaRecoveryIntervalSeconds != DefaultCodexQuotaRecoveryIntervalSeconds {
		t.Fatalf("unexpected quota recovery defaults: %+v", settings)
	}

	settings.Host = "0.0.0.0"
	settings.Port = 19191
	settings.TrayEnabled = false
	settings.LogUpstream = false
	settings.LogBodyLimit = 1024
	settings.DefaultModel = "gpt-5.4"
	settings.CodexQuotaRecoveryEnabled = false
	settings.CodexQuotaRecoveryIntervalSeconds = 300
	if err := db.SaveSettings(ctx, settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	got, err := db.GetSettings(ctx)
	if err != nil {
		t.Fatalf("get saved settings: %v", err)
	}
	if got.Host != "0.0.0.0" || got.Port != 19191 || got.TrayEnabled || got.LogUpstream || got.LogBodyLimit != 1024 || got.DefaultModel != "gpt-5.4" || got.CodexQuotaRecoveryEnabled || got.CodexQuotaRecoveryIntervalSeconds != 300 {
		t.Fatalf("settings not persisted: %+v", got)
	}

	got.Port = 0
	if err := db.SaveSettings(ctx, got); err != nil {
		t.Fatalf("save dynamic port: %v", err)
	}
	dynamic, err := db.GetSettings(ctx)
	if err != nil {
		t.Fatalf("get dynamic port: %v", err)
	}
	if dynamic.Port != 0 {
		t.Fatalf("dynamic port should persist as 0, got %d", dynamic.Port)
	}
}

func TestSettingsInvalidCodexQuotaRecoveryIntervalUsesDefault(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	if _, err := db.sql.ExecContext(ctx, `insert into settings (key, value) values (?, ?), (?, ?)`,
		"codex_quota_recovery_enabled", "false",
		"codex_quota_recovery_interval_seconds", "17"); err != nil {
		t.Fatalf("insert invalid settings: %v", err)
	}
	settings, err := db.GetSettings(ctx)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if settings.CodexQuotaRecoveryEnabled || settings.CodexQuotaRecoveryIntervalSeconds != DefaultCodexQuotaRecoveryIntervalSeconds {
		t.Fatalf("invalid interval was not normalized: %+v", settings)
	}
}

func TestNormalizeCodexQuotaRecoveryIntervalSeconds(t *testing.T) {
	for _, valid := range ValidCodexQuotaRecoveryIntervals() {
		if got := NormalizeCodexQuotaRecoveryIntervalSeconds(valid); got != valid {
			t.Fatalf("valid interval %d normalized to %d", valid, got)
		}
	}
	if got := NormalizeCodexQuotaRecoveryIntervalSeconds(0); got != DefaultCodexQuotaRecoveryIntervalSeconds {
		t.Fatalf("invalid interval normalized to %d", got)
	}
}
