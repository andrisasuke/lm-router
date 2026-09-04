//go:build wailsapp

package main

import (
	"encoding/json"
	"strings"
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
