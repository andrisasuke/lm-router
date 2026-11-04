package tui

import (
	"context"
	"testing"
	"time"
)

func TestStopCodexQuotaRecoveryWaitsBeforeDatabaseClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	closed := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		stopCodexQuotaRecovery(cancel, done)
		// This represents Run's deferred db.Close. It must happen only after
		// the scheduler has acknowledged cancellation by closing done.
		close(closed)
		close(returned)
	}()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("scheduler context was not cancelled")
	}
	select {
	case <-closed:
		t.Fatal("database close ran before scheduler completion")
	default:
	}

	close(done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("scheduler shutdown did not complete")
	}
	select {
	case <-closed:
	default:
		t.Fatal("database close did not follow scheduler completion")
	}
}
