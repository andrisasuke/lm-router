package codex

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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

func TestProviderTokenManagerUsesProviderRefreshLeadAndRefresher(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, account := range []store.Account{
		{ID: "codex", Provider: store.ProviderOpenAICodex, Name: "main", Enabled: true, AccessToken: "codex-old", RefreshToken: "codex-refresh", ExpiresAt: time.Now().Add(2 * time.Hour)},
		{ID: "claude", Provider: store.ProviderAnthropicClaude, Name: "main", Enabled: true, AccessToken: "claude-old", RefreshToken: "claude-refresh", ExpiresAt: time.Now().Add(2 * time.Hour)},
	} {
		if err := db.UpsertAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	var codexCalls, claudeCalls int
	manager := NewProviderTokenManager(db, map[string]Refresher{
		store.ProviderOpenAICodex: RefreshFunc(func(context.Context, string) (TokenSet, error) {
			codexCalls++
			return TokenSet{AccessToken: "codex-new", ExpiresAt: time.Now().Add(time.Hour)}, nil
		}),
		store.ProviderAnthropicClaude: RefreshFunc(func(context.Context, string) (TokenSet, error) {
			claudeCalls++
			return TokenSet{AccessToken: "claude-new", RefreshToken: "claude-rotated", ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
		}),
	}, map[string]time.Duration{
		store.ProviderOpenAICodex:     5 * time.Minute,
		store.ProviderAnthropicClaude: 4 * time.Hour,
	})

	if got, err := manager.EnsureFresh(ctx, "codex"); err != nil || got.AccessToken != "codex-old" {
		t.Fatalf("codex got=%+v err=%v", got, err)
	}
	got, err := manager.EnsureFresh(ctx, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if codexCalls != 0 || claudeCalls != 1 || got.AccessToken != "claude-new" || got.RefreshToken != "claude-rotated" {
		t.Fatalf("calls codex=%d claude=%d account=%+v", codexCalls, claudeCalls, got)
	}
}

func TestProviderTokenManagerMarksPermanentClaudeRefreshFailure(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertAccount(ctx, store.Account{ID: "claude", Provider: store.ProviderAnthropicClaude, Name: "main", Enabled: true, RefreshToken: "bad", ExpiresAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	manager := NewProviderTokenManager(db, map[string]Refresher{
		store.ProviderAnthropicClaude: RefreshFunc(func(context.Context, string) (TokenSet, error) {
			return TokenSet{}, errors.New(`{"error":"invalid_grant"}`)
		}),
	}, nil)
	if _, err := manager.EnsureFresh(ctx, "claude"); err == nil {
		t.Fatal("expected refresh failure")
	}
	got, _ := db.GetAccount(ctx, "claude")
	if !got.NeedsReauth {
		t.Fatal("expected needs reauth")
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
	transformed, err := TransformRequest([]byte(`{"model":"gpt-5.3-codex","input":"ping","stream":false,"max_output_tokens":100,"tools":[{"type":"web_search","external_web_access":true}],"tool_choice":"auto","client_metadata":{"session_id":"session-1"}}`))
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
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("web search tool was not preserved: %#v", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "web_search" || tool["external_web_access"] != true {
		t.Fatalf("unexpected web search tool: %#v", tool)
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice=%#v", body["tool_choice"])
	}
	metadata, _ := body["client_metadata"].(map[string]any)
	if metadata["session_id"] != "session-1" {
		t.Fatalf("client_metadata=%#v", metadata)
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("unexpected input: %#v", body["input"])
	}
}

func TestTransformRequestPreservesFullResponsesFields(t *testing.T) {
	original := []byte(`{
		"model":"gpt-5.6-sol",
		"instructions":[{"type":"input_text","text":"Use the workbook"}],
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_text","text":"Summarize this file"},
			{"type":"input_image","image_url":"data:image/png;base64,aW1hZ2U="},
			{"type":"input_file","file_id":"file_123","filename":"report.xlsx"},
			{"type":"future_input_part","payload":{"keep":true}}
		]}],
		"tools":[{"type":"function","name":"inspect_sheet","parameters":{"type":"object"}}],
		"tool_choice":"auto",
		"include":["reasoning.encrypted_content","web_search_call.action.sources"]
	}`)

	var want map[string]any
	if err := json.Unmarshal(original, &want); err != nil {
		t.Fatalf("decode original: %v", err)
	}
	transformed, err := TransformRequest(original)
	if err != nil {
		t.Fatalf("transform request: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(transformed, &got); err != nil {
		t.Fatalf("decode transformed: %v", err)
	}

	for _, field := range []string{"instructions", "input", "tools", "tool_choice", "include"} {
		if !reflect.DeepEqual(got[field], want[field]) {
			t.Errorf("%s changed:\n got %#v\nwant %#v", field, got[field], want[field])
		}
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
