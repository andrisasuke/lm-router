//go:build wailsapp

package main

import (
	_ "embed"
	"log"
	"os"

	frontendassets "github.com/andrisasuke/lm-router/frontend"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed build/appicon.png
var appIcon []byte

//go:embed build/trayicon.png
var trayIcon []byte

func main() {
	profile, err := loadDesktopProfile()
	if err != nil {
		println("LM Router desktop configuration error:", err.Error())
		return
	}
	desktop := NewApp(profile)
	app := application.New(application.Options{
		Name:        profile.Title,
		Description: "Local model switchboard for Codex and Claude",
		Icon:        appIcon,
		Services: []application.Service{
			application.NewService(desktop),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(frontendassets.Assets),
		},
		MarshalError: marshalDesktopError,
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		Windows: application.WindowsOptions{DisableQuitOnLastWindowClosed: true},
		Linux:   application.LinuxOptions{DisableQuitOnLastWindowClosed: true},
	})

	mainWindow := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "main",
		Title:            profile.Title,
		Width:            980,
		Height:           640,
		DisableResize:    true,
		MinWidth:         980,
		MinHeight:        640,
		MaxWidth:         980,
		MaxHeight:        640,
		InitialPosition:  application.WindowCentered,
		BackgroundColour: application.NewRGB(13, 15, 18),
		URL:              "/",
		Mac: application.MacWindow{
			TitleBar:   application.MacTitleBarHiddenInset,
			Appearance: application.NSAppearanceNameDarkAqua,
		},
	})
	tray := newTrayController(app, desktop, mainWindow, profile, trayIcon, appIcon)
	mainWindow.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		if !tray.isEnabled() {
			app.Quit()
			return
		}
		mainWindow.Hide()
		event.Cancel()
	})
	app.Event.OnApplicationEvent(events.Mac.ApplicationDidFinishLaunching, func(_ *application.ApplicationEvent) {
		application.InvokeAsync(func() {
			app.Show()
			mainWindow.Show().Focus()
		})
	})

	desktop.attachApplication(app, mainWindow, tray)

	if err := app.Run(); err != nil {
		log.Printf("LM Router desktop error: %v", err)
		os.Exit(1)
	}
}
