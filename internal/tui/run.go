package tui

import (
	"context"
	"fmt"
	"net/http"

	"github.com/andrisasuke/lm-router/internal/app"
	"github.com/andrisasuke/lm-router/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

func Run(ctx context.Context, dataDir, host string, port int) error {
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	settings, err := db.GetSettings(ctx)
	if err != nil {
		return err
	}
	if host != "" {
		settings.Host = host
	}
	if port > 0 {
		settings.Port = port
	}
	logger := app.NewRingLogger(500, nil)
	controller := app.NewServerController(app.ServerControllerConfig{
		Logger: logger,
		HandlerFactory: func() (http.Handler, error) {
			current, err := db.GetSettings(ctx)
			if err != nil {
				return nil, err
			}
			if host != "" {
				current.Host = host
			}
			if port > 0 {
				current.Port = port
			}
			return app.NewProxyHandler(db, current, logger), nil
		},
	})
	model := NewWithDataDir(ctx, db, logger, controller, settings, dataDir)
	_, err = tea.NewProgram(model, tea.WithAltScreen()).Run()
	_ = controller.Stop(ctx)
	if err != nil {
		return fmt.Errorf("run tui: %w", err)
	}
	return nil
}
