package customprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andrisasuke/lm-router/internal/store"
)

func TestExecuteRewritesModelFieldBeforeForwarding(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := NewClient(nil)
	account := store.Account{AccessToken: "sk-test", BaseURL: server.URL, CompatType: store.CompatOpenAIStyle}
	body := []byte(`{"model":"myprefix/gpt-4o","max_tokens":1000000,"messages":[{"role":"user","content":"hi"}]}`)

	result, err := client.Execute(context.Background(), ExecuteParams{
		Account: account, Path: "/chat/completions", Model: "gpt-4o", Body: body,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Status != 200 {
		t.Fatalf("status=%d", result.Status)
	}
	if received["model"] != "gpt-4o" {
		t.Fatalf("model=%v want gpt-4o", received["model"])
	}
	if got, ok := received["max_tokens"].(float64); !ok || got != 1000000 {
		t.Fatalf("max_tokens=%v want 1000000 unchanged", received["max_tokens"])
	}
}

func TestExecuteSetsAnthropicHeadersForAnthropicCompat(t *testing.T) {
	var gotAPIKey, gotAuth, gotVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("anthropic-version")
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := NewClient(nil)
	account := store.Account{AccessToken: "sk-ant-test", BaseURL: server.URL, CompatType: store.CompatAnthropicStyle}
	_, err := client.Execute(context.Background(), ExecuteParams{
		Account: account, Path: "/messages", Model: "claude-3", Body: []byte(`{"model":"x","messages":[]}`),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotAPIKey != "sk-ant-test" {
		t.Fatalf("x-api-key=%q", gotAPIKey)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization should be unset for anthropic-compatible, got %q", gotAuth)
	}
	if gotVersion == "" {
		t.Fatal("expected anthropic-version header to be set")
	}
}

func TestExecuteSetsBearerAuthForOpenAICompat(t *testing.T) {
	var gotAuth, gotAPIKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := NewClient(nil)
	account := store.Account{AccessToken: "sk-openai-test", BaseURL: server.URL, CompatType: store.CompatOpenAIStyle}
	_, err := client.Execute(context.Background(), ExecuteParams{
		Account: account, Path: "/responses", Model: "gpt-4o", Body: []byte(`{"model":"x"}`),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotAuth != "Bearer sk-openai-test" {
		t.Fatalf("Authorization=%q", gotAuth)
	}
	if gotAPIKey != "" {
		t.Fatalf("x-api-key should be unset for openai-compatible, got %q", gotAPIKey)
	}
}

func TestOpenStreamReturnsUpstreamBodyUnaltered(t *testing.T) {
	const payload = "data: {\"chunk\":1}\n\ndata: {\"chunk\":2}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(payload))
	}))
	defer server.Close()

	client := NewClient(nil)
	account := store.Account{AccessToken: "sk-test", BaseURL: server.URL, CompatType: store.CompatOpenAIStyle}
	stream, err := client.OpenStream(context.Background(), ExecuteParams{
		Account: account, Path: "/chat/completions", Model: "gpt-4o", Body: []byte(`{"model":"x","stream":true}`),
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer stream.Body.Close()
	buf := make([]byte, len(payload))
	n, _ := stream.Body.Read(buf)
	if string(buf[:n]) != payload[:n] {
		t.Fatalf("stream body altered: got %q", buf[:n])
	}
}

func TestExecuteGetRequestSkipsModelRewrite(t *testing.T) {
	var gotMethod, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		buf := make([]byte, 1)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	client := NewClient(nil)
	account := store.Account{AccessToken: "sk-test", BaseURL: server.URL, CompatType: store.CompatOpenAIStyle}
	result, err := client.Execute(context.Background(), ExecuteParams{
		Account: account, Path: "/models", Method: http.MethodGet,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("method=%s want GET", gotMethod)
	}
	if gotBody != "" {
		t.Fatalf("expected empty body for GET probe, got %q", gotBody)
	}
	if result.Status != 200 {
		t.Fatalf("status=%d", result.Status)
	}
}
