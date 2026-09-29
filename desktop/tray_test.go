//go:build wailsapp

package main

import "testing"

func TestTrayStateUsesActualEndpointWhenServerIsRunning(t *testing.T) {
	state := trayState(ServerStatusVM{
		State:              "ON",
		ConfiguredEndpoint: "http://127.0.0.1:19090",
		ActualEndpoint:     "http://127.0.0.1:43210",
	})
	if state.statusLabel != "Server: Running" || state.toggleLabel != "Stop Server" {
		t.Fatalf("unexpected running state: %+v", state)
	}
	if state.endpoint != "http://127.0.0.1:43210" {
		t.Fatalf("endpoint = %q, want actual endpoint", state.endpoint)
	}
	if state.endpointLabel != "Endpoint 127.0.0.1:43210" {
		t.Fatalf("endpoint label = %q, want host and port only", state.endpointLabel)
	}
}

func TestTrayStateDisablesToggleWhileStarting(t *testing.T) {
	state := trayState(ServerStatusVM{State: "STARTING", ConfiguredEndpoint: "http://127.0.0.1:19090"})
	if state.toggleEnabled || state.toggleLabel != "Starting…" {
		t.Fatalf("unexpected starting state: %+v", state)
	}
}

func TestTrayStateKeepsRestartAvailableAfterError(t *testing.T) {
	state := trayState(ServerStatusVM{State: "ERROR"})
	if !state.toggleEnabled || state.toggleLabel != "Start Server" || state.statusLabel != "Server: Error" {
		t.Fatalf("unexpected error state: %+v", state)
	}
}
