//go:build wailsapp

package main

import (
	"errors"
	"runtime"
	"strings"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
)

type trayViewState struct {
	statusLabel   string
	endpointLabel string
	toggleLabel   string
	toggleEnabled bool
	endpoint      string
}

type trayController struct {
	mu sync.Mutex

	app        *application.App
	desktop    *App
	mainWindow application.Window
	profile    desktopProfile
	trayIcon   []byte
	appIcon    []byte
	tray       *application.SystemTray

	statusItem   *application.MenuItem
	endpointItem *application.MenuItem
	toggleItem   *application.MenuItem
	copyItem     *application.MenuItem

	active  bool
	enabled bool
	last    trayViewState
}

func newTrayController(
	app *application.App,
	desktop *App,
	mainWindow application.Window,
	profile desktopProfile,
	trayIcon []byte,
	appIcon []byte,
) *trayController {
	return &trayController{
		app:        app,
		desktop:    desktop,
		mainWindow: mainWindow,
		profile:    profile,
		trayIcon:   trayIcon,
		appIcon:    appIcon,
		enabled:    true,
	}
}

func (t *trayController) createTrayLocked() *application.SystemTray {
	if t.tray != nil {
		return t.tray
	}

	state := t.last
	if state.statusLabel == "" {
		state = trayState(ServerStatusVM{State: "STARTING"})
	}

	tray := t.app.SystemTray.New()
	menu := t.app.Menu.New()
	menu.Add(t.profile.Title).SetEnabled(false)
	menu.AddSeparator()
	menu.Add("Open").OnClick(func(_ *application.Context) {
		t.openMainWindow()
	})
	menu.AddSeparator()
	t.statusItem = menu.Add(state.statusLabel).SetEnabled(false)
	t.endpointItem = menu.Add(state.endpointLabel).SetEnabled(false)
	t.toggleItem = menu.Add(state.toggleLabel).SetEnabled(state.toggleEnabled).OnClick(func(_ *application.Context) {
		t.toggleServer()
	})
	t.copyItem = menu.Add("Copy Endpoint").SetEnabled(state.endpoint != "").OnClick(func(_ *application.Context) {
		t.copyEndpoint()
	})
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(_ *application.Context) {
		t.app.Quit()
	})

	if runtime.GOOS == "darwin" {
		tray.SetTemplateIcon(t.trayIcon)
	} else {
		tray.SetIcon(t.appIcon)
	}
	tray.SetTooltip(t.profile.Title)
	tray.SetMenu(menu)
	t.tray = tray
	return tray
}

func (t *trayController) activate(enabled bool) {
	t.mu.Lock()
	t.active = true
	t.enabled = enabled
	if enabled {
		t.createTrayLocked()
	}
	t.mu.Unlock()
}

func (t *trayController) setEnabled(enabled bool) {
	t.mu.Lock()
	t.enabled = enabled
	if !t.active {
		t.mu.Unlock()
		return
	}
	tray := t.tray
	if enabled && tray == nil {
		tray = t.createTrayLocked()
	}
	t.mu.Unlock()

	if tray == nil {
		return
	}
	if enabled {
		tray.Show()
		return
	}
	tray.Hide()
}

func (t *trayController) isEnabled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.enabled
}

func (t *trayController) refresh(status ServerStatusVM) {
	next := trayState(status)
	t.mu.Lock()
	if next == t.last {
		t.mu.Unlock()
		return
	}
	t.last = next
	active := t.active
	statusItem := t.statusItem
	endpointItem := t.endpointItem
	toggleItem := t.toggleItem
	copyItem := t.copyItem
	t.mu.Unlock()
	if statusItem == nil || endpointItem == nil || toggleItem == nil || copyItem == nil {
		return
	}

	apply := func() {
		statusItem.SetLabel(next.statusLabel)
		endpointItem.SetLabel(next.endpointLabel)
		toggleItem.SetLabel(next.toggleLabel).SetEnabled(next.toggleEnabled)
		copyItem.SetEnabled(next.endpoint != "")
	}
	if active {
		application.InvokeAsync(apply)
		return
	}
	apply()
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

func trayState(status ServerStatusVM) trayViewState {
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
	}
	if endpoint != "" {
		result.endpointLabel = "Endpoint " + strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	}

	switch status.State {
	case "ON":
		result.statusLabel = "Server: Running"
		result.toggleLabel = "Stop Server"
	case "STARTING":
		result.statusLabel = "Server: Starting…"
		result.toggleLabel = "Starting…"
		result.toggleEnabled = false
	case "ERROR":
		result.statusLabel = "Server: Error"
	}
	return result
}
