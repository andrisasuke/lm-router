//go:build wailsapp

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/andrisasuke/lm-router/internal/anthropic"
	appsvc "github.com/andrisasuke/lm-router/internal/app"
	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
)

type ServerStatusVM struct {
	State              string `json:"state"`
	ConfiguredEndpoint string `json:"configuredEndpoint"`
	ActualEndpoint     string `json:"actualEndpoint"`
	Host               string `json:"host"`
	Port               int    `json:"port"`
	Error              string `json:"error"`
}

type ConnectionVM struct {
	ID                  string `json:"id"`
	Provider            string `json:"provider"`
	Name                string `json:"name"`
	Priority            int    `json:"priority"`
	Enabled             bool   `json:"enabled"`
	Status              string `json:"status"`
	NeedsReauth         bool   `json:"needsReauth"`
	Routable            bool   `json:"routable"`
	CooldownUntil       string `json:"cooldownUntil"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	Prefix              string `json:"prefix"`
	BaseURL             string `json:"baseUrl"`
	CompatType          string `json:"compatType"`
	APIType             string `json:"apiType"`
	CanQuota            bool   `json:"canQuota"`
	CanRefresh          bool   `json:"canRefresh"`
	CanReauth           bool   `json:"canReauth"`
	Requesting          bool   `json:"requesting"`
}

type ConnectionActivityVM struct {
	ID         string `json:"id"`
	Requesting bool   `json:"requesting"`
}

type KeyVM struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	CreatedAt string `json:"createdAt"`
}

type CreatedKeyVM struct {
	KeyVM
	Secret string `json:"secret"`
}

type SettingsVM struct {
	Host                   string `json:"host"`
	Port                   int    `json:"port"`
	TrayEnabled            bool   `json:"trayEnabled"`
	LogRequests            bool   `json:"logRequests"`
	LogUpstream            bool   `json:"logUpstream"`
	LogBodyLimit           int    `json:"logBodyLimit"`
	DefaultModel           string `json:"defaultModel"`
	UpstreamTimeoutSeconds int    `json:"upstreamTimeoutSeconds"`
}

type LogVM struct {
	Time    string `json:"time"`
	Source  string `json:"source"`
	Message string `json:"message"`
}

type ProviderTestVM struct {
	Status int    `json:"status"`
	OK     bool   `json:"ok"`
	Output string `json:"output"`
}

type QuotaWindowVM struct {
	Name        string  `json:"name"`
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resetsAt"`
	Summary     string  `json:"summary"`
}

type QuotaVM struct {
	Connected bool            `json:"connected"`
	Available bool            `json:"available"`
	Status    int             `json:"status"`
	FetchedAt string          `json:"fetchedAt"`
	RetryAt   string          `json:"retryAt"`
	Message   string          `json:"message"`
	Windows   []QuotaWindowVM `json:"windows"`
}

type OAuthSessionVM struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	AccountID     string `json:"accountId"`
	AuthURL       string `json:"authUrl"`
	Loopback      bool   `json:"loopback"`
	LoopbackError string `json:"loopbackError"`
}

type CustomProviderInput struct {
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	BaseURL    string `json:"baseUrl"`
	APIKey     string `json:"apiKey"`
	CompatType string `json:"compatType"`
	APIType    string `json:"apiType"`
}

type CustomProviderUpdate struct {
	Name    string `json:"name"`
	Prefix  string `json:"prefix"`
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey"`
	APIType string `json:"apiType"`
}

func serverStatusVM(status appsvc.ServerStatus, settings store.Settings) ServerStatusVM {
	host := displayHost(settings.Host)
	configured := fmt.Sprintf("http://%s:%d", host, settings.Port)
	return ServerStatusVM{
		State:              string(status.State),
		ConfiguredEndpoint: configured,
		ActualEndpoint:     status.Endpoint,
		Host:               settings.Host,
		Port:               settings.Port,
		Error:              status.Error,
	}
}

func connectionVM(account store.Account) ConnectionVM {
	cooldown := ""
	if account.CooldownUntil.Valid {
		cooldown = account.CooldownUntil.Time.UTC().Format(time.RFC3339)
	}
	custom := account.Provider == store.ProviderCustom
	return ConnectionVM{
		ID:                  account.ID,
		Provider:            account.Provider,
		Name:                account.Name,
		Priority:            account.Priority,
		Enabled:             account.Enabled,
		Status:              appsvc.FormatProviderStatus(account),
		NeedsReauth:         account.NeedsReauth,
		Routable:            account.Enabled && !account.NeedsReauth,
		CooldownUntil:       cooldown,
		ConsecutiveFailures: account.ConsecutiveFailures,
		Prefix:              account.Prefix,
		BaseURL:             account.BaseURL,
		CompatType:          account.CompatType,
		APIType:             account.APIType,
		CanQuota:            !custom,
		CanRefresh:          !custom,
		CanReauth:           !custom,
	}
}

func keyVM(key store.APIKey) KeyVM {
	return KeyVM{ID: key.ID, Name: key.Name, Prefix: key.Prefix, CreatedAt: key.CreatedAt.UTC().Format(time.RFC3339)}
}

func settingsVM(settings store.Settings) SettingsVM {
	return SettingsVM{
		Host: settings.Host, Port: settings.Port,
		TrayEnabled: settings.TrayEnabled,
		LogRequests: settings.LogRequests, LogUpstream: settings.LogUpstream,
		LogBodyLimit: settings.LogBodyLimit, DefaultModel: settings.DefaultModel,
		UpstreamTimeoutSeconds: settings.UpstreamTimeoutSeconds,
	}
}

func (settings SettingsVM) storeSettings() store.Settings {
	return store.Settings{
		Host: settings.Host, Port: settings.Port,
		TrayEnabled: settings.TrayEnabled,
		LogRequests: settings.LogRequests, LogUpstream: settings.LogUpstream,
		LogBodyLimit: settings.LogBodyLimit, DefaultModel: settings.DefaultModel,
		UpstreamTimeoutSeconds: settings.UpstreamTimeoutSeconds,
	}
}

func codexQuotaVM(info codex.QuotaInfo) QuotaVM {
	windows := make([]QuotaWindowVM, 0, 2)
	for _, window := range []*codex.QuotaWindow{info.Primary, info.Secondary} {
		if window == nil {
			continue
		}
		reset := ""
		if !window.ResetAt.IsZero() {
			reset = window.ResetAt.UTC().Format(time.RFC3339)
		}
		windows = append(windows, QuotaWindowVM{
			Name: codexQuotaWindowName(window.WindowMinutes), Utilization: window.UsedPercent,
			ResetsAt: reset, Summary: codex.FormatQuotaWindow(window),
		})
	}
	message := ""
	if len(windows) == 0 {
		message = "Quota headers were not returned"
		if len(info.HeaderKeys) > 0 {
			message += "; available headers: " + strings.Join(info.HeaderKeys, ", ")
		}
	}
	return QuotaVM{Connected: true, Available: len(windows) > 0, FetchedAt: formatTime(info.FetchedAt), Message: message, Windows: windows}
}

func codexQuotaWindowName(minutes int) string {
	switch minutes {
	case 300:
		return "5h"
	case 10080:
		return "weekly"
	case 0:
		return "quota"
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

func claudeQuotaVM(info anthropic.UsageInfo) QuotaVM {
	windows := make([]QuotaWindowVM, 0, len(info.Windows))
	for _, window := range info.Windows {
		windows = append(windows, QuotaWindowVM{
			Name: window.Name, Utilization: window.Utilization,
			ResetsAt: formatTime(window.ResetsAt),
			Summary:  fmt.Sprintf("%s (%.0f%%)", window.Name, window.Utilization),
		})
	}
	message := ""
	if info.Connected && !info.Available {
		message = "Connected; quota is temporarily unavailable"
	}
	return QuotaVM{
		Connected: info.Connected, Available: info.Available, Status: info.Status,
		FetchedAt: formatTime(info.FetchedAt), RetryAt: formatTime(info.RetryAt),
		Message: message, Windows: windows,
	}
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func displayHost(host string) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "127.0.0.1"
	}
	return host
}
