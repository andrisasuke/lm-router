package codex

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/store"
)

func TestTokenRefreshIsAtomicPerAccount(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	acct := store.Account{
		ID:           "acct_1",
		Provider:     "openai-codex",
		Name:         "main",
		Priority:     1,
		Enabled:      true,
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(time.Minute),
	}
	if err := db.UpsertAccount(ctx, acct); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	var calls int32
	refresher := NewTokenManager(db, RefreshFunc(func(ctx context.Context, refreshToken string) (TokenSet, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(30 * time.Millisecond)
		return TokenSet{
			AccessToken:  "new-access",
			RefreshToken: "new-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	}))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := refresher.EnsureFresh(ctx, "acct_1")
			if err != nil {
				t.Errorf("ensure fresh: %v", err)
				return
			}
			if got.AccessToken != "new-access" {
				t.Errorf("got access %q", got.AccessToken)
			}
		}()
	}
	wg.Wait()

	if calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
}

func TestUnrecoverableRefreshMarksNeedsReauth(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	acct := store.Account{
		ID:           "acct_1",
		Provider:     "openai-codex",
		Name:         "main",
		Priority:     1,
		Enabled:      true,
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(time.Minute),
	}
	if err := db.UpsertAccount(ctx, acct); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	refresher := NewTokenManager(db, RefreshFunc(func(ctx context.Context, refreshToken string) (TokenSet, error) {
		return TokenSet{}, ErrNeedsReauth
	}))

	if _, err := refresher.EnsureFresh(ctx, "acct_1"); err == nil {
		t.Fatal("expected refresh error")
	}
	got, err := db.GetAccount(ctx, "acct_1")
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if !got.NeedsReauth {
		t.Fatal("expected account to be marked needs_reauth")
	}
}

func TestTransformRequestAddsCodexRequiredFields(t *testing.T) {
	transformed, err := TransformRequest([]byte(`{"model":"gpt-5.3-codex","input":"ping","stream":false,"max_output_tokens":100}`))
	if err != nil {
		t.Fatalf("transform request: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(transformed, &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if body["instructions"] == "" {
		t.Fatal("expected default instructions")
	}
	if body["stream"] != true {
		t.Fatalf("stream=%v want true", body["stream"])
	}
	if body["store"] != false {
		t.Fatalf("store=%v want false", body["store"])
	}
	if _, ok := body["max_output_tokens"]; ok {
		t.Fatal("max_output_tokens should be stripped")
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("unexpected input: %#v", body["input"])
	}
}

func TestConvertResponsesSSEToOutputReadsEventDataDelta(t *testing.T) {
	body := []byte("event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\"}\n\n")

	if got := ConvertResponsesSSEToOutput(body); got != "pong" {
		t.Fatalf("got %q want pong", got)
	}
}
