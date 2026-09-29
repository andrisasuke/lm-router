//go:build wailsapp

package main

import (
	"path/filepath"
	"testing"

	"github.com/andrisasuke/lm-router/internal/store"
)

func TestResolveDesktopProfileProductionDefaults(t *testing.T) {
	profile, err := resolveDesktopProfile("/home/test", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if profile.Name != productionProfile || profile.Title != "LM Router" {
		t.Fatalf("unexpected production profile: %+v", profile)
	}
	if profile.DataDir != filepath.Join("/home/test", ".lm-router") || profile.PortOverride != 0 {
		t.Fatalf("unexpected production storage settings: %+v", profile)
	}
}

func TestResolveDesktopProfileDevelopmentIsIsolated(t *testing.T) {
	profile, err := resolveDesktopProfile("/home/test", func(key string) string {
		if key == "LM_ROUTER_PROFILE" {
			return "development"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Title != "LM Router Dev" || profile.DataDir != filepath.Join("/home/test", ".lm-router-dev") {
		t.Fatalf("unexpected development profile: %+v", profile)
	}
	settings := profile.applySettings(store.DefaultSettings())
	if settings.Port != 19091 {
		t.Fatalf("development port=%d want 19091", settings.Port)
	}
}

func TestResolveDesktopProfileEnvironmentOverrides(t *testing.T) {
	env := map[string]string{
		"LM_ROUTER_PROFILE":  "dev",
		"LM_ROUTER_DATA_DIR": "/tmp/lm-router-isolated",
		"LM_ROUTER_PORT":     "19191",
	}
	profile, err := resolveDesktopProfile("/home/test", func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if profile.DataDir != env["LM_ROUTER_DATA_DIR"] || profile.PortOverride != 19191 {
		t.Fatalf("environment overrides were not applied: %+v", profile)
	}
}

func TestResolveDesktopProfileRejectsInvalidPort(t *testing.T) {
	_, err := resolveDesktopProfile("/home/test", func(key string) string {
		if key == "LM_ROUTER_PORT" {
			return "70000"
		}
		return ""
	})
	if err == nil {
		t.Fatal("expected invalid development port to fail")
	}
}
