package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestServerControllerStartStopRestart(t *testing.T) {
	ctx := context.Background()
	controller := NewServerController(ServerControllerConfig{
		HandlerFactory: func() (http.Handler, error) {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}), nil
		},
		Logger: NewRingLogger(10, nil),
	})

	if controller.Status().State != ServerOff {
		t.Fatalf("initial status=%+v", controller.Status())
	}
	if err := controller.Start(ctx, "127.0.0.1", 0); err != nil {
		t.Fatalf("start: %v", err)
	}
	first := controller.Status()
	if first.State != ServerOn || first.Endpoint == "" {
		t.Fatalf("started status=%+v", first)
	}

	resp, err := http.Get(first.Endpoint + "/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	if err := controller.Restart(ctx, "127.0.0.1", 0); err != nil {
		t.Fatalf("restart: %v", err)
	}
	second := controller.Status()
	if second.State != ServerOn || second.Endpoint == first.Endpoint {
		t.Fatalf("restart did not update endpoint: first=%+v second=%+v", first, second)
	}

	if err := controller.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if controller.Status().State != ServerOff {
		t.Fatalf("stopped status=%+v", controller.Status())
	}
}

func TestServerControllerRoutesHTTPErrorLogToConfiguredLogger(t *testing.T) {
	ctx := context.Background()
	logger := NewRingLogger(10, nil)
	controller := NewServerController(ServerControllerConfig{
		HandlerFactory: func() (http.Handler, error) {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}), nil
		},
		Logger: logger,
	})

	if err := controller.Start(ctx, "127.0.0.1", 0); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer controller.Stop(ctx)

	controller.server.ErrorLog.Print("http: test internal error")
	entries := logger.Entries()
	if len(entries) == 0 || !strings.Contains(entries[len(entries)-1].Message, "http: test internal error") {
		t.Fatalf("HTTP error was not routed to configured logger: %+v", entries)
	}
}
