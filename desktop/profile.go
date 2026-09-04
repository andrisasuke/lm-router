//go:build wailsapp

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/andrisasuke/lm-router/internal/store"
)

const (
	productionProfile  = "production"
	developmentProfile = "development"
)

type desktopProfile struct {
	Name         string
	Title        string
	DataDir      string
	PortOverride int
}

func loadDesktopProfile() (desktopProfile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return resolveDesktopProfile(home, os.Getenv)
}

func resolveDesktopProfile(home string, getenv func(string) string) (desktopProfile, error) {
	profile := desktopProfile{
		Name:    productionProfile,
		Title:   "LM Router",
		DataDir: filepath.Join(home, ".lm-router"),
	}

	switch strings.ToLower(strings.TrimSpace(getenv("LM_ROUTER_PROFILE"))) {
	case "", "prod", productionProfile:
	case "dev", developmentProfile:
		profile.Name = developmentProfile
		profile.Title = "LM Router Dev"
		profile.DataDir = filepath.Join(home, ".lm-router-dev")
		profile.PortOverride = 19091
	default:
		return desktopProfile{}, fmt.Errorf("unsupported LM_ROUTER_PROFILE; use production or development")
	}

	if dataDir := strings.TrimSpace(getenv("LM_ROUTER_DATA_DIR")); dataDir != "" {
		profile.DataDir = filepath.Clean(dataDir)
	}
	if value := strings.TrimSpace(getenv("LM_ROUTER_PORT")); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return desktopProfile{}, fmt.Errorf("LM_ROUTER_PORT must be between 1 and 65535")
		}
		profile.PortOverride = port
	}
	return profile, nil
}

func (profile desktopProfile) applySettings(settings store.Settings) store.Settings {
	if profile.PortOverride > 0 {
		settings.Port = profile.PortOverride
	}
	return settings
}
