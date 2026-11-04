package tui

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/andrisasuke/lm-router/internal/app"
	"github.com/andrisasuke/lm-router/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

func Run(ctx context.Context, dataDir, host string, port int) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	db, err := store.Open(runCtx, dataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	settings, err := db.GetSettings(runCtx)
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
	previousLogOutput := log.Writer()
	previousLogFlags := log.Flags()
	previousLogPrefix := log.Prefix()
	log.SetOutput(logger)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(previousLogOutput)
		log.SetFlags(previousLogFlags)
		log.SetPrefix(previousLogPrefix)
	}()
	controller := app.NewServerController(app.ServerControllerConfig{
		Logger: logger,
		HandlerFactory: func() (http.Handler, error) {
			current, err := db.GetSettings(runCtx)
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
	scheduler := app.NewCodexQuotaRecoveryScheduler(db, app.ProviderService{DB: db, Logger: logger})
	scheduler.Configure(
		settings.CodexQuotaRecoveryEnabled,
		time.Duration(settings.CodexQuotaRecoveryIntervalSeconds)*time.Second,
	)
	model := NewWithDataDir(runCtx, db, logger, controller, settings, dataDir)
	model.quotaRecovery = scheduler
	recoveryDone := startCodexQuotaRecovery(runCtx, scheduler, model.quotaRecoveryResults)
	defer stopCodexQuotaRecovery(cancel, recoveryDone)
	_, err = tea.NewProgram(model, tea.WithAltScreen()).Run()
	stopCodexQuotaRecovery(cancel, recoveryDone)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = controller.Stop(stopCtx)
	stopCancel()
	if err != nil {
		return fmt.Errorf("run tui: %w", err)
	}
	return nil
}

// startCodexQuotaRecovery gives Run explicit ownership of the scheduler
// goroutine. Its completion channel is waited before the database's deferred
// Close, preventing an in-flight cancellation path from using SQLite after it
// has been closed.
func startCodexQuotaRecovery(ctx context.Context, scheduler *app.CodexQuotaRecoveryScheduler, results chan<- app.CodexQuotaRecoveryResult) <-chan struct{} {
	done := make(chan struct{})
	scheduler.OnResult = func(result app.CodexQuotaRecoveryResult) {
		select {
		case results <- result:
		case <-ctx.Done():
		}
	}
	go func() {
		defer close(done)
		scheduler.Run(ctx)
	}()
	return done
}

func stopCodexQuotaRecovery(cancel context.CancelFunc, done <-chan struct{}) {
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}
