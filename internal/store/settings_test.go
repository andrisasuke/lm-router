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
	if settings.LogBodyLimit != 64*1024 {
		t.Fatalf("body limit=%d", settings.LogBodyLimit)
	}
	if settings.DefaultModel != "gpt-5.3-codex" {
		t.Fatalf("default model=%q", settings.DefaultModel)
	}

	settings.Host = "0.0.0.0"
	settings.Port = 19191
	settings.LogUpstream = false
	settings.LogBodyLimit = 1024
	settings.DefaultModel = "gpt-5.4"
	if err := db.SaveSettings(ctx, settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	got, err := db.GetSettings(ctx)
	if err != nil {
		t.Fatalf("get saved settings: %v", err)
	}
	if got.Host != "0.0.0.0" || got.Port != 19191 || got.LogUpstream || got.LogBodyLimit != 1024 || got.DefaultModel != "gpt-5.4" {
		t.Fatalf("settings not persisted: %+v", got)
	}
}
