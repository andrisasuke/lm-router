package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
)

type activityEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *activityEvents) callback(accountID string, active bool) {
	state := "idle"
	if active {
		state = "active"
	}
	e.mu.Lock()
	e.events = append(e.events, accountID+":"+state)
	e.mu.Unlock()
}

func (e *activityEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

func TestResponseStreamActivityEndsWhenBodyCloses(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account := store.Account{
		ID: "codex-stream", Provider: store.ProviderOpenAICodex, Name: "stream", Priority: 1, Enabled: true,
		AccessToken: "token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseUpstream)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
	}))
	t.Cleanup(upstream.Close)

	events := &activityEvents{}
	server := &Server{
		store:             db,
		codex:             codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		onAccountActivity: events.callback,
	}
	stream, status, err := server.openResponseStream(ctx, []byte(`{"model":"gpt-5.3-codex","input":"hi","stream":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	if got, want := events.snapshot(), []string{"codex-stream:active"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events before close=%v, want %v", got, want)
	}
	if err := stream.Body.Close(); err != nil {
		t.Fatal(err)
	}
	releaseUpstream()
	if got, want := events.snapshot(), []string{"codex-stream:active", "codex-stream:idle"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events after close=%v, want %v", got, want)
	}
}

func TestResponseStreamActivityTransfersDuringFailover(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, account := range []store.Account{
		{ID: "first", Provider: store.ProviderOpenAICodex, Name: "first", Priority: 1, Enabled: true, AccessToken: "first-token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)},
		{ID: "second", Provider: store.ProviderOpenAICodex, Name: "second", Priority: 2, Enabled: true, AccessToken: "second-token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)},
	} {
		if err := db.UpsertAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer first-token" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"limited"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	events := &activityEvents{}
	server := &Server{
		store:             db,
		codex:             codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		onAccountActivity: events.callback,
	}
	stream, status, err := server.openResponseStream(ctx, []byte(`{"model":"gpt-5.3-codex","input":"hi","stream":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	wantBeforeClose := []string{"first:active", "first:idle", "second:active"}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantBeforeClose) {
		t.Fatalf("events before close=%v, want %v", got, wantBeforeClose)
	}
	if err := stream.Body.Close(); err != nil {
		t.Fatal(err)
	}
	wantAfterClose := append(wantBeforeClose, "second:idle")
	if got := events.snapshot(); !reflect.DeepEqual(got, wantAfterClose) {
		t.Fatalf("events after close=%v, want %v", got, wantAfterClose)
	}
}
