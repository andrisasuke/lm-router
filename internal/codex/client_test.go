package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/store"
)

func TestOpenResponsesStreamForwardsSafeCodexIdentityHeaders(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	account := store.Account{
		ID:           "acct_1",
		Provider:     store.ProviderOpenAICodex,
		Name:         "main",
		Enabled:      true,
		AccessToken:  "upstream-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
		MetadataJSON: `{"chatgpt_account_id":"workspace-account"}`,
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	requestHeaders := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHeaders <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	inbound := make(http.Header)
	inbound.Set("Authorization", "Bearer local-router-key")
	inbound.Set("Cookie", "session=local")
	inbound.Set("ChatGPT-Account-Id", "client-supplied-account")
	inbound.Set("User-Agent", "codex_exec/0.149.1")
	inbound.Set("Originator", "codex_exec")
	inbound.Set("Session-Id", "session-1")
	inbound.Set("Thread-Id", "thread-1")
	inbound.Set("X-Client-Request-Id", "request-1")
	inbound.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	inbound.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	inbound.Set("X-Unrelated", "do-not-forward")

	client := NewClient(upstream.URL, NewTokenManager(db, nil))
	stream, err := client.OpenResponsesStream(ctx, ExecuteParams{
		Account: account,
		Body:    []byte(`{"model":"gpt-5.3-codex","input":"ping"}`),
		Headers: inbound,
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_ = stream.Body.Close()

	got := <-requestHeaders
	for key, want := range map[string]string{
		"Authorization":                          "Bearer upstream-token",
		"ChatGPT-Account-Id":                     "workspace-account",
		"User-Agent":                             "codex_exec/0.149.1",
		"Originator":                             "codex_exec",
		"Session-Id":                             "session-1",
		"Thread-Id":                              "thread-1",
		"X-Client-Request-Id":                    "request-1",
		"X-Codex-Beta-Features":                  "remote_compaction_v2",
		"X-OpenAI-Internal-Codex-Responses-Lite": "true",
	} {
		if value := got.Get(key); value != want {
			t.Errorf("%s=%q want %q", key, value, want)
		}
	}
	if got.Get("Cookie") != "" || got.Get("X-Unrelated") != "" {
		t.Fatalf("unsafe headers forwarded: %v", got)
	}
	formatted := formatHeaders(got)
	if strings.Contains(formatted, "upstream-token") || strings.Contains(formatted, "workspace-account") {
		t.Fatalf("sensitive identity leaked in formatted headers: %s", formatted)
	}
}

func TestPrepareRequestEnablesNativeWebSearchForCodexCustomProvider(t *testing.T) {
	headers := make(http.Header)
	headers.Set(CodexResponsesLiteHeader, "true")
	headers.Set(RouterCodexModeHeader, "full")
	headers.Set(RouterWebSearchHeader, "live")

	body, forwardedHeaders, err := prepareRequest([]byte(`{
		"model":"gpt-5.6-sol",
		"input":[{"role":"user","content":[{"type":"input_text","text":"latest news"}]}],
		"include":["reasoning.encrypted_content"]
	}`), headers)
	if err != nil {
		t.Fatalf("prepare request: %v", err)
	}
	if got := forwardedHeaders.Get(RouterWebSearchHeader); got != "" {
		t.Fatalf("private router header was forwarded=%q", got)
	}
	if got := forwardedHeaders.Get(RouterCodexModeHeader); got != "" {
		t.Fatalf("private Codex mode header was forwarded=%q", got)
	}
	if got := forwardedHeaders.Get(CodexResponsesLiteHeader); got != "" {
		t.Fatalf("responses-lite header was retained after adding hosted tools=%q", got)
	}
	if headers.Get(CodexResponsesLiteHeader) != "true" || headers.Get(RouterCodexModeHeader) != "full" || headers.Get(RouterWebSearchHeader) != "live" {
		t.Fatal("prepare request mutated inbound headers")
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%#v", payload["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "web_search" || tool["external_web_access"] != true {
		t.Fatalf("web search tool=%#v", tool)
	}
	include, _ := payload["include"].([]any)
	if len(include) != 2 || include[0] != "reasoning.encrypted_content" || include[1] != "web_search_call.action.sources" {
		t.Fatalf("include=%#v", payload["include"])
	}
}

func TestPrepareRequestFullCodexModeRemovesResponsesLiteWithoutAddingTools(t *testing.T) {
	headers := make(http.Header)
	headers.Set(CodexResponsesLiteHeader, "true")
	headers.Set(RouterCodexModeHeader, "full")
	headers.Set("X-Codex-Beta-Features", "remote_compaction_v2")

	body, forwardedHeaders, err := prepareRequest([]byte(`{
		"model":"gpt-5.6-sol",
		"input":[{"role":"user","content":[{"type":"input_file","file_id":"file_123"}]}]
	}`), headers)
	if err != nil {
		t.Fatalf("prepare request: %v", err)
	}
	if forwardedHeaders.Get(RouterCodexModeHeader) != "" {
		t.Fatalf("private Codex mode header was forwarded=%q", forwardedHeaders.Get(RouterCodexModeHeader))
	}
	if forwardedHeaders.Get(CodexResponsesLiteHeader) != "" {
		t.Fatalf("responses-lite header was retained=%q", forwardedHeaders.Get(CodexResponsesLiteHeader))
	}
	if forwardedHeaders.Get("X-Codex-Beta-Features") != "remote_compaction_v2" {
		t.Fatalf("Codex protocol header was lost=%q", forwardedHeaders.Get("X-Codex-Beta-Features"))
	}
	if headers.Get(CodexResponsesLiteHeader) != "true" || headers.Get(RouterCodexModeHeader) != "full" {
		t.Fatal("prepare request mutated inbound headers")
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if _, ok := payload["tools"]; ok {
		t.Fatalf("full mode unexpectedly added tools=%#v", payload["tools"])
	}
}

func TestPrepareRequestRejectsInvalidCodexMode(t *testing.T) {
	headers := make(http.Header)
	headers.Set(RouterCodexModeHeader, "automatic")

	if _, _, err := prepareRequest([]byte(`{"model":"gpt-5.6-sol","input":"hello"}`), headers); err == nil || !strings.Contains(err.Error(), RouterCodexModeHeader) {
		t.Fatalf("expected invalid %s error, got %v", RouterCodexModeHeader, err)
	}
}

func TestPrepareRequestLeavesGenericAndDisabledSearchRequestsUnchanged(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)

	generic, genericHeaders, err := prepareRequest(body, make(http.Header))
	if err != nil {
		t.Fatalf("prepare generic request: %v", err)
	}
	var genericPayload map[string]any
	if err := json.Unmarshal(generic, &genericPayload); err != nil {
		t.Fatalf("decode generic request: %v", err)
	}
	if _, ok := genericPayload["tools"]; ok {
		t.Fatalf("generic request unexpectedly gained tools=%#v", genericPayload["tools"])
	}
	if genericHeaders.Get(RouterWebSearchHeader) != "" {
		t.Fatalf("generic headers=%v", genericHeaders)
	}

	disabledHeaders := make(http.Header)
	disabledHeaders.Set(CodexResponsesLiteHeader, "true")
	disabledHeaders.Set(RouterWebSearchHeader, "disabled")
	disabled, forwardedHeaders, err := prepareRequest(body, disabledHeaders)
	if err != nil {
		t.Fatalf("prepare disabled request: %v", err)
	}
	var disabledPayload map[string]any
	if err := json.Unmarshal(disabled, &disabledPayload); err != nil {
		t.Fatalf("decode disabled request: %v", err)
	}
	if _, ok := disabledPayload["tools"]; ok {
		t.Fatalf("disabled request unexpectedly gained tools=%#v", disabledPayload["tools"])
	}
	if forwardedHeaders.Get(CodexResponsesLiteHeader) != "true" || forwardedHeaders.Get(RouterWebSearchHeader) != "" {
		t.Fatalf("disabled forwarded headers=%v", forwardedHeaders)
	}
}

func TestOpenResponsesStreamKeepsAccountIdentityAfterTokenRefresh(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	account := store.Account{
		ID:           "acct_1",
		Provider:     store.ProviderOpenAICodex,
		Name:         "main",
		Enabled:      true,
		AccessToken:  "expired-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
		MetadataJSON: `{"chatgpt_account_id":"workspace-account"}`,
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	attempts := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		attempts <- auth + "|" + r.Header.Get("ChatGPT-Account-Id")
		if auth == "Bearer expired-token" {
			http.Error(w, `{"error":{"message":"expired"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	refresher := RefreshFunc(func(context.Context, string) (TokenSet, error) {
		return TokenSet{AccessToken: "fresh-token", RefreshToken: "fresh-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	client := NewClient(upstream.URL, NewTokenManager(db, refresher))
	stream, err := client.OpenResponsesStream(ctx, ExecuteParams{Account: account, Body: []byte(`{"model":"gpt-5.3-codex","input":"ping"}`)})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_ = stream.Body.Close()

	for _, want := range []string{
		"Bearer expired-token|workspace-account",
		"Bearer fresh-token|workspace-account",
	} {
		if got := <-attempts; got != want {
			t.Fatalf("upstream identity=%q want %q", got, want)
		}
	}
}

func TestApplyCodexProtocolHeadersUsesSafeFallbacksForMissingMetadata(t *testing.T) {
	dst := make(http.Header)
	inbound := make(http.Header)
	inbound.Set("User-Agent", "openai-python/2.0")
	inbound.Set("X-Client-Request-Id", "generic-request")
	applyCodexProtocolHeaders(dst, inbound, store.Account{ID: "acct_fallback", MetadataJSON: "{"})

	if got := dst.Get("Session-Id"); got != "acct_fallback" {
		t.Fatalf("session id=%q", got)
	}
	if got := dst.Get("User-Agent"); got != "codex-cli/1.0.18 (macOS; arm64)" {
		t.Fatalf("generic client changed upstream user agent=%q", got)
	}
	if got := dst.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("generic request header was forwarded=%q", got)
	}
	if got := dst.Get("ChatGPT-Account-Id"); got != "" {
		t.Fatalf("unexpected account id=%q", got)
	}
}

func TestFetchQuotaUsesUsageEndpointAndIdentityHeaders(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	account := store.Account{
		ID:           "acct_1",
		Provider:     "openai-codex",
		Name:         "main",
		Enabled:      true,
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
		MetadataJSON: `{"chatgpt_account_id":"workspace-account"}`,
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	type observedRequest struct {
		method  string
		path    string
		auth    string
		account string
		body    []byte
	}
	requests := make(chan observedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- observedRequest{
			method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
			account: r.Header.Get("ChatGPT-Account-Id"), body: body,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600,"reset_at":1770000000},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":7200,"reset_at":1770003600}}}`))
	}))
	defer upstream.Close()

	client := NewClientWithUsageURL("https://inference.invalid/responses", upstream.URL+"/backend-api/wham/usage", NewTokenManager(db, nil))
	info, err := client.FetchQuota(ctx, account)
	if err != nil {
		t.Fatalf("fetch quota: %v", err)
	}
	got := <-requests
	if got.method != http.MethodGet || got.path != "/backend-api/wham/usage" {
		t.Fatalf("request=%s %s, want GET /backend-api/wham/usage", got.method, got.path)
	}
	if got.auth != "Bearer access-token" || got.account != "workspace-account" {
		t.Fatalf("identity headers auth=%q account=%q", got.auth, got.account)
	}
	if len(got.body) != 0 {
		t.Fatalf("usage request unexpectedly sent a body: %q", got.body)
	}
	if info.Primary == nil || info.Primary.WindowMinutes != 300 || info.Primary.UsedPercent != 10 || info.Primary.ResetAfterSecs != 3600 || info.Primary.ResetAt.Unix() != 1770000000 {
		t.Fatalf("primary=%+v", info.Primary)
	}
	if info.Secondary == nil || info.Secondary.WindowMinutes != 10080 || info.Secondary.UsedPercent != 20 || info.Secondary.ResetAfterSecs != 7200 || info.Secondary.ResetAt.Unix() != 1770003600 {
		t.Fatalf("secondary=%+v", info.Secondary)
	}
}

func TestFetchQuotaRetriesOnceAfterAuthFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			account := store.Account{
				ID: "acct_1", Provider: store.ProviderOpenAICodex, Name: "main", Enabled: true,
				AccessToken: "old-access", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(time.Hour),
				MetadataJSON: `{"chatgpt_account_id":"workspace-account"}`,
			}
			if err := db.UpsertAccount(ctx, account); err != nil {
				t.Fatal(err)
			}
			authorizations := make(chan string, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authorizations <- r.Header.Get("Authorization") + ":" + r.Header.Get("ChatGPT-Account-Id")
				if r.Header.Get("Authorization") == "Bearer old-access" {
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":1},"secondary_window":{"used_percent":2}}}`))
			}))
			defer upstream.Close()
			manager := NewTokenManager(db, RefreshFunc(func(context.Context, string) (TokenSet, error) {
				return TokenSet{AccessToken: "new-access", ExpiresAt: time.Now().Add(time.Hour)}, nil
			}))
			client := NewClientWithUsageURL("https://inference.invalid/responses", upstream.URL, manager)
			if _, err := client.FetchQuota(ctx, account); err != nil {
				t.Fatalf("fetch quota: %v", err)
			}
			if first, second := <-authorizations, <-authorizations; first != "Bearer old-access:workspace-account" || second != "Bearer new-access:workspace-account" {
				t.Fatalf("authorization retry=%q, %q", first, second)
			}
		})
	}
}

func TestFetchQuotaHTTPErrorsAreSafe(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			account := store.Account{
				ID: "acct_1", Provider: store.ProviderOpenAICodex, Name: "main", Enabled: true,
				AccessToken: "access-token", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(time.Hour),
			}
			if err := db.UpsertAccount(ctx, account); err != nil {
				t.Fatal(err)
			}
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"access-token workspace-account"}`))
			}))
			defer upstream.Close()

			client := NewClientWithUsageURL("https://inference.invalid/responses", upstream.URL, NewTokenManager(db, nil))
			info, err := client.FetchQuota(ctx, account)
			if err == nil {
				t.Fatal("expected quota error")
			}
			var quotaErr *QuotaHTTPError
			if !errors.As(err, &quotaErr) || quotaErr.Status != status {
				t.Fatalf("error=%v, want HTTP %d quota error", err, status)
			}
			if info.Primary != nil || info.Secondary != nil || info.FetchedAt != (time.Time{}) {
				t.Fatalf("failed request returned quota info: %+v", info)
			}
			if strings.Contains(err.Error(), "access-token") || strings.Contains(err.Error(), "workspace-account") {
				t.Fatalf("error leaked upstream body: %v", err)
			}
			wantCalls := 1
			if status == http.StatusForbidden {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d, want %d", calls, wantCalls)
			}
		})
	}
}

func TestFetchQuotaNetworkFailureIsDistinctFromEmptyPayload(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	account := store.Account{
		ID: "acct_1", Provider: store.ProviderOpenAICodex, Name: "main", Enabled: true,
		AccessToken: "access-token", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.NotFoundHandler())
	usageURL := server.URL
	server.Close()
	client := NewClientWithUsageURL("https://inference.invalid/responses", usageURL, NewTokenManager(db, nil))
	info, err := client.FetchQuota(ctx, account)
	if err == nil || !strings.Contains(err.Error(), "network error") {
		t.Fatalf("error=%v, want network error", err)
	}
	if info.FetchedAt != (time.Time{}) || info.Primary != nil || info.Secondary != nil {
		t.Fatalf("network error returned quota info: %+v", info)
	}
}

func TestFetchQuotaClearsCooldownWhenBothWindowsRecovered(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	account := store.Account{
		ID: "acct_1", Provider: store.ProviderOpenAICodex, Name: "main", Enabled: true,
		AccessToken: "access-token", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	if _, err := db.RecordRetryableFailure(ctx, account.ID, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("set cooldown: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000},"secondary_window":{"used_percent":0,"limit_window_seconds":604800}}}`))
	}))
	defer upstream.Close()

	client := NewClientWithUsageURL("https://inference.invalid/responses", upstream.URL, NewTokenManager(db, nil))
	if _, err := client.FetchQuota(ctx, account); err != nil {
		t.Fatalf("fetch quota: %v", err)
	}
	updated, err := db.MustGetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CooldownUntil.Valid || updated.LastFailureAt.Valid || updated.ConsecutiveFailures != 0 {
		t.Fatalf("failure state was not cleared: %+v", updated)
	}
}

func TestFetchQuotaDoesNotClearFailureStateOnIncompleteOrFailedResponse(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{name: "partial payload", status: http.StatusOK, body: `{"rate_limit":{"primary_window":{"used_percent":0},"secondary_window":{"limit_window_seconds":604800}}}`},
		{name: "both windows missing used percent", status: http.StatusOK, body: `{"rate_limit":{"primary_window":{"limit_window_seconds":18000},"secondary_window":{"limit_window_seconds":604800}}}`},
		{name: "both windows null used percent", status: http.StatusOK, body: `{"rate_limit":{"primary_window":{"used_percent":null},"secondary_window":{"used_percent":null}}}`},
		{name: "failed", status: http.StatusInternalServerError, body: `{"message":"temporary failure"}`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			account := store.Account{
				ID: "acct_1", Provider: store.ProviderOpenAICodex, Name: "main", Enabled: true,
				AccessToken: "access-token", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(time.Hour),
			}
			if err := db.UpsertAccount(ctx, account); err != nil {
				t.Fatal(err)
			}
			if _, err := db.RecordRetryableFailure(ctx, account.ID, time.Now(), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			before, err := db.MustGetAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer upstream.Close()
			client := NewClientWithUsageURL("https://inference.invalid/responses", upstream.URL, NewTokenManager(db, nil))
			_, fetchErr := client.FetchQuota(ctx, account)
			if test.wantError != (fetchErr != nil) {
				t.Fatalf("fetch error=%v, want error=%t", fetchErr, test.wantError)
			}
			updated, err := db.MustGetAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if updated.ConsecutiveFailures != before.ConsecutiveFailures ||
				updated.LastFailureAt.Valid != before.LastFailureAt.Valid ||
				(updated.LastFailureAt.Valid && !updated.LastFailureAt.Time.Equal(before.LastFailureAt.Time)) ||
				updated.CooldownUntil.Valid != before.CooldownUntil.Valid ||
				(updated.CooldownUntil.Valid && !updated.CooldownUntil.Time.Equal(before.CooldownUntil.Time)) {
				t.Fatalf("failure state changed: before=%+v after=%+v", before, updated)
			}
		})
	}
}

func TestFetchQuotaSetsCooldownWhenAnyWindowIsExhausted(t *testing.T) {
	for _, test := range []struct {
		name      string
		primary   string
		secondary string
	}{
		{name: "primary", primary: "100", secondary: "0"},
		{name: "secondary", primary: "0", secondary: "100"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(ctx, t.TempDir())
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			defer db.Close()

			account := store.Account{
				ID: "acct_1", Provider: store.ProviderOpenAICodex, Name: "main", Enabled: true,
				AccessToken: "access-token", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(time.Hour),
			}
			if err := db.UpsertAccount(ctx, account); err != nil {
				t.Fatalf("upsert account: %v", err)
			}
			resetAt := time.Now().Add(time.Hour).Unix()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%s,"limit_window_seconds":18000,"reset_at":%d},"secondary_window":{"used_percent":%s,"limit_window_seconds":604800,"reset_at":%d}}}`,
					test.primary, resetAt, test.secondary, resetAt)))
			}))
			defer upstream.Close()

			client := NewClientWithUsageURL("https://inference.invalid/responses", upstream.URL, NewTokenManager(db, nil))
			if _, err := client.FetchQuota(ctx, account); err != nil {
				t.Fatalf("fetch quota: %v", err)
			}
			updated, err := db.MustGetAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !updated.CooldownUntil.Valid || updated.CooldownUntil.Time.Unix() != resetAt {
				t.Fatalf("quota cooldown was not persisted: %+v", updated.CooldownUntil)
			}
		})
	}
}

// SSE sample mirroring a real backend reply that emits a function_call.
const functionCallSSE = `event: response.output_item.added
data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_abc","name":"get_weather"},"output_index":0,"sequence_number":2}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","delta":"{\"city\":\"Jakarta\"}","item_id":"fc_1","output_index":0,"sequence_number":3}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","arguments":"{\"city\":\"Jakarta\"}","item_id":"fc_1","output_index":0,"sequence_number":4}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","status":"completed","arguments":"{\"city\":\"Jakarta\"}","call_id":"call_abc","name":"get_weather"},"output_index":0,"sequence_number":5}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":93,"output_tokens":19,"total_tokens":112}}}

data: [DONE]
`

const textSSE = `event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello there"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_2","object":"response","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}

data: [DONE]
`

func TestConvertResponsesSSEToItems_FunctionCall(t *testing.T) {
	items, final := ConvertResponsesSSEToItems([]byte(functionCallSSE))
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	item := items[0]
	if item["type"] != "function_call" {
		t.Fatalf("want function_call, got %v", item["type"])
	}
	if item["name"] != "get_weather" {
		t.Errorf("name: want get_weather, got %v", item["name"])
	}
	if item["call_id"] != "call_abc" {
		t.Errorf("call_id: want call_abc, got %v", item["call_id"])
	}
	if item["arguments"] != `{"city":"Jakarta"}` {
		t.Errorf("arguments: got %v", item["arguments"])
	}
	if final == nil {
		t.Fatal("expected final response.completed object")
	}
	usage, _ := final["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 112 {
		t.Errorf("usage total_tokens: got %v", usage["total_tokens"])
	}
}

func TestOutputTextFromItems_IgnoresFunctionCall(t *testing.T) {
	items, _ := ConvertResponsesSSEToItems([]byte(functionCallSSE))
	if got := OutputTextFromItems(items); got != "" {
		t.Errorf("function_call should yield no text, got %q", got)
	}

	items, _ = ConvertResponsesSSEToItems([]byte(textSSE))
	if got := OutputTextFromItems(items); got != "Hello there" {
		t.Errorf("text: want %q, got %q", "Hello there", got)
	}
}

func TestConvertResponsesSSEToItemsPreservesFileCitations(t *testing.T) {
	sse := []byte(`event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"See workbook","annotations":[{"type":"file_citation","file_id":"file_123","filename":"report.xlsx","index":0}]}]}}

data: [DONE]

`)
	items, _ := ConvertResponsesSSEToItems(sse)
	if len(items) != 1 {
		t.Fatalf("items=%#v", items)
	}
	content, _ := items[0]["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%#v", content)
	}
	output, _ := content[0].(map[string]any)
	annotations, _ := output["annotations"].([]any)
	if len(annotations) != 1 {
		t.Fatalf("annotations=%#v", annotations)
	}
	citation, _ := annotations[0].(map[string]any)
	if citation["type"] != "file_citation" || citation["file_id"] != "file_123" || citation["filename"] != "report.xlsx" {
		t.Fatalf("citation was not preserved: %#v", citation)
	}
}

func TestCopyStreamPreservesAnnotationsAndUnknownEvents(t *testing.T) {
	want := []byte("event: response.output_text.annotation.added\n" +
		"data: {\"type\":\"response.output_text.annotation.added\",\"annotation\":{\"type\":\"file_citation\",\"file_id\":\"file_123\"}}\n\n" +
		"event: response.future_event\n" +
		"data: {\"type\":\"response.future_event\",\"payload\":{\"kept\":true}}\n\n")
	var got bytes.Buffer
	if err := CopyStream(&got, bytes.NewReader(want)); err != nil {
		t.Fatalf("copy stream: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("stream changed:\n got %q\nwant %q", got.Bytes(), want)
	}
}
