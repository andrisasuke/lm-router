//go:build wailsapp

package main

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
	"github.com/wailsapp/wails/v3/pkg/application"
)

type trayIconState uint8

const (
	trayIconIdle trayIconState = iota
	trayIconRunning
)

type trayCodexAccount struct {
	Account store.Account
	Quota   codexQuotaState
}

type trayViewState struct {
	statusLabel   string
	endpointLabel string
	toggleLabel   string
	toggleEnabled bool
	endpoint      string
	icon          trayIconState
	codexRows     []string
}

func (s trayViewState) equal(other trayViewState) bool {
	return s.statusLabel == other.statusLabel &&
		s.endpointLabel == other.endpointLabel &&
		s.toggleLabel == other.toggleLabel &&
		s.toggleEnabled == other.toggleEnabled &&
		s.endpoint == other.endpoint &&
		s.icon == other.icon &&
		slices.Equal(s.codexRows, other.codexRows)
}

type trayController struct {
	mu sync.Mutex

	app         *application.App
	desktop     *App
	mainWindow  application.Window
	profile     desktopProfile
	idleIcon    []byte
	runningIcon []byte
	tray        *application.SystemTray

	active       bool
	enabled      bool
	creating     bool
	closed       bool
	last         trayViewState
	currentMenu  *application.Menu
	iconSet      bool
	renderedIcon trayIconState
}

func newTrayController(
	app *application.App,
	desktop *App,
	mainWindow application.Window,
	profile desktopProfile,
	idleIcon []byte,
	runningIcon []byte,
) *trayController {
	return &trayController{
		app:         app,
		desktop:     desktop,
		mainWindow:  mainWindow,
		profile:     profile,
		idleIcon:    idleIcon,
		runningIcon: runningIcon,
		enabled:     true,
	}
}

func (t *trayController) createTray() {
	t.mu.Lock()
	if t.tray != nil || t.creating || t.closed || !t.active || !t.enabled {
		t.mu.Unlock()
		return
	}
	t.creating = true
	t.mu.Unlock()

	application.InvokeAsync(func() {
		t.mu.Lock()
		if t.tray != nil || t.closed || !t.active || !t.enabled {
			t.creating = false
			t.mu.Unlock()
			return
		}
		state := t.last
		if state.statusLabel == "" {
			state = trayState(ServerStatusVM{State: "STARTING"}, nil)
			t.last = state
		}
		t.mu.Unlock()

		tray := t.app.SystemTray.New()
		menu := t.menu(state)
		tray.SetIcon(t.iconBytes(state.icon))
		tray.SetTooltip(t.profile.Title)
		tray.SetMenu(menu)

		t.mu.Lock()
		if t.closed || !t.active || !t.enabled {
			t.creating = false
			t.mu.Unlock()
			menu.Destroy()
			tray.Destroy()
			return
		}
		t.tray = tray
		t.currentMenu = menu
		t.renderedIcon = state.icon
		t.iconSet = true
		t.creating = false
		latest := t.last
		t.mu.Unlock()
		if !state.equal(latest) {
			t.applyState(tray, latest)
		}
		if t.isCurrent(tray) {
			tray.Show()
		} else {
			tray.Hide()
		}
	})
}

func (t *trayController) activate(enabled bool) {
	t.mu.Lock()
	t.active = true
	t.enabled = enabled
	t.mu.Unlock()
	if enabled {
		t.createTray()
	}
}

func (t *trayController) setEnabled(enabled bool) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.enabled = enabled
	active := t.active
	tray := t.tray
	t.mu.Unlock()

	if enabled && active && tray == nil {
		t.createTray()
		return
	}
	if tray != nil {
		t.updateVisibility(tray)
	}
}

func (t *trayController) updateVisibility(tray *application.SystemTray) {
	application.InvokeAsync(func() {
		t.mu.Lock()
		show := !t.closed && t.active && t.enabled && t.tray == tray
		t.mu.Unlock()
		if show {
			tray.Show()
			return
		}
		tray.Hide()
	})
}

func (t *trayController) isEnabled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.enabled
}

func (t *trayController) refresh(status ServerStatusVM, accounts []trayCodexAccount) {
	next := trayState(status, accounts)
	t.mu.Lock()
	if next.equal(t.last) {
		t.mu.Unlock()
		return
	}
	t.last = next
	active := t.active
	tray := t.tray
	closed := t.closed
	t.mu.Unlock()
	if closed || !active || tray == nil {
		return
	}

	application.InvokeAsync(func() {
		t.applyState(tray, next)
	})
}

func (t *trayController) applyState(tray *application.SystemTray, next trayViewState) {
	menu := t.menu(next)
	t.mu.Lock()
	current := !t.closed && t.active && t.tray == tray && t.last.equal(next)
	if !current {
		t.mu.Unlock()
		menu.Destroy()
		return
	}
	previousMenu := t.currentMenu
	iconChanged := trayIconChanged(t.iconSet, t.renderedIcon, next.icon)
	t.currentMenu = menu
	t.renderedIcon = next.icon
	t.iconSet = true
	t.mu.Unlock()

	if iconChanged {
		tray.SetIcon(t.iconBytes(next.icon))
	}
	tray.SetMenu(menu)
	if previousMenu != nil {
		previousMenu.Destroy()
	}
}

func (t *trayController) isCurrent(tray *application.SystemTray) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.closed && t.active && t.enabled && t.tray == tray
}

func (t *trayController) shutdown() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.active = false
	t.enabled = false
	tray := t.tray
	menu := t.currentMenu
	t.tray = nil
	t.currentMenu = nil
	t.mu.Unlock()

	if tray == nil && menu == nil {
		return
	}
	application.InvokeAsync(func() {
		if menu != nil {
			menu.Destroy()
		}
		if tray != nil {
			tray.Destroy()
		}
	})
}

func (t *trayController) menu(state trayViewState) *application.Menu {
	menu := t.app.Menu.New()
	menu.Add(t.profile.Title).SetEnabled(false)
	menu.AddSeparator()
	menu.Add("Open").OnClick(func(_ *application.Context) {
		t.openMainWindow()
	})
	menu.AddSeparator()
	menu.Add(state.statusLabel).SetEnabled(false)
	menu.Add(state.endpointLabel).SetEnabled(false)
	menu.Add(state.toggleLabel).SetEnabled(state.toggleEnabled).OnClick(func(_ *application.Context) {
		t.toggleServer()
	})
	menu.Add("Copy Endpoint").SetEnabled(state.endpoint != "").OnClick(func(_ *application.Context) {
		t.copyEndpoint()
	})
	menu.AddSeparator()
	codexMenu := menu.AddSubmenu("Codex Account")
	for _, row := range state.codexRows {
		// Keep informational quota rows enabled so native menus render them
		// with normal text. They intentionally have no callback, so selecting
		// one cannot trigger an account action.
		codexMenu.Add(row)
	}
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(_ *application.Context) {
		t.app.Quit()
	})
	return menu
}

func (t *trayController) iconBytes(state trayIconState) []byte {
	if state == trayIconRunning {
		return t.runningIcon
	}
	return t.idleIcon
}

func trayIconChanged(set bool, rendered, next trayIconState) bool {
	return !set || rendered != next
}

func (t *trayController) openMainWindow() {
	if t.mainWindow.IsMinimised() {
		t.mainWindow.UnMinimise()
	}
	t.mainWindow.Show().Focus()
}

func (t *trayController) toggleServer() {
	status, err := t.desktop.ServerStatus()
	if err != nil {
		t.desktop.showError("Could not read server status", err)
		return
	}
	if status.State == "ON" || status.State == "STARTING" {
		_, err = t.desktop.StopServer()
		if err != nil {
			t.desktop.showError("Could not stop server", err)
		}
	} else {
		_, _ = t.desktop.StartServer()
	}
}

func (t *trayController) copyEndpoint() {
	status, err := t.desktop.ServerStatus()
	if err != nil {
		t.desktop.showError("Could not read endpoint", err)
		return
	}
	endpoint := status.ActualEndpoint
	if endpoint == "" {
		endpoint = status.ConfiguredEndpoint
	}
	if endpoint == "" || !t.app.Clipboard.SetText(endpoint) {
		t.desktop.showError("Could not copy endpoint", errors.New("the endpoint could not be copied to the clipboard"))
	}
}

func trayState(status ServerStatusVM, accounts []trayCodexAccount) trayViewState {
	endpoint := status.ActualEndpoint
	if endpoint == "" {
		endpoint = status.ConfiguredEndpoint
	}

	result := trayViewState{
		statusLabel:   "Server: Stopped",
		endpointLabel: "—",
		toggleLabel:   "Start Server",
		toggleEnabled: true,
		endpoint:      endpoint,
		icon:          trayIconIdle,
		codexRows:     trayCodexRows(accounts),
	}
	if endpoint != "" {
		result.endpointLabel = "Endpoint " + strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	}

	switch status.State {
	case "ON":
		result.statusLabel = "Server: Running"
		result.toggleLabel = "Stop Server"
		result.icon = trayIconRunning
	case "STARTING":
		result.statusLabel = "Server: Starting…"
		result.toggleLabel = "Starting…"
		result.toggleEnabled = false
	case "ERROR":
		result.statusLabel = "Server: Error"
	}
	return result
}

func trayCodexRows(accounts []trayCodexAccount) []string {
	if len(accounts) == 0 {
		return []string{"No Codex accounts"}
	}
	rows := make([]string, 0, len(accounts))
	for _, account := range accounts {
		rows = append(rows, fmt.Sprintf("%s - 5h(%s) 7d(%s)",
			trayCodexAccountName(account),
			trayQuotaRemaining(account.Quota.Info.Primary),
			trayQuotaRemaining(account.Quota.Info.Secondary),
		))
	}
	return rows
}

func trayCodexAccountName(account trayCodexAccount) string {
	markers := make([]string, 0, 3)
	if !account.Account.Enabled {
		markers = append(markers, "disabled")
	}
	if account.Account.NeedsReauth {
		markers = append(markers, "reauth required")
	}
	if account.Quota.Loading {
		if hasCodexQuota(account.Quota.Info) {
			markers = append(markers, "stale")
		} else {
			markers = append(markers, "loading")
		}
	} else if account.Quota.Error != "" {
		if hasCodexQuota(account.Quota.Info) {
			markers = append(markers, "stale")
		} else {
			markers = append(markers, "quota unavailable")
		}
	}
	if len(markers) == 0 {
		return account.Account.Name
	}
	return account.Account.Name + " [" + strings.Join(markers, ", ") + "]"
}

func trayQuotaRemaining(window *codex.QuotaWindow) string {
	if window == nil || math.IsNaN(window.UsedPercent) {
		return "?"
	}
	return fmt.Sprintf("%d%%", trayRemainingPercent(window.UsedPercent))
}

func trayRemainingPercent(usedPercent float64) int {
	remaining := 100 - usedPercent
	if remaining < 0 {
		remaining = 0
	} else if remaining > 100 {
		remaining = 100
	}
	return int(math.Round(remaining))
}
