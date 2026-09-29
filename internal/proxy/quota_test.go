package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
)

type observedQuota struct {
	accountID string
	info      codex.QuotaInfo
}

type quotaObservations struct {
	mu     sync.Mutex
	values []observedQuota
}

func (o *quotaObservations) callback(accountID string, info codex.QuotaInfo) {
	o.mu.Lock()
	o.values = append(o.values, observedQuota{accountID: accountID, info: info})
	o.mu.Unlock()
}

func (o *quotaObservations) snapshot() []observedQuota {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]observedQuota(nil), o.values...)
}

func TestRouteResponsesObservesCodexQuotaHeaders(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account := store.Account{
		ID: "codex", Provider: store.ProviderOpenAICodex, Name: "main", Priority: 1, Enabled: true,
		AccessToken: "token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-codex-primary-used-percent", "15")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		w.Header().Set("x-codex-secondary-used-percent", "25")
		w.Header().Set("x-codex-secondary-window-minutes", "10080")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	observations := &quotaObservations{}
	server := &Server{
		store:        db,
		codex:        codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		onCodexQuota: observations.callback,
	}
	_, _, status, err := server.routeResponses(ctx, []byte(`{"model":"gpt-5.3-codex","input":"hi","stream":false}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	got := observations.snapshot()
	if len(got) != 1 || got[0].accountID != account.ID {
		t.Fatalf("observations=%+v", got)
	}
	if got[0].info.Primary == nil || got[0].info.Primary.UsedPercent != 15 || got[0].info.Primary.WindowMinutes != 300 {
		t.Fatalf("primary quota=%+v", got[0].info.Primary)
	}
	if got[0].info.Secondary == nil || got[0].info.Secondary.UsedPercent != 25 || got[0].info.Secondary.WindowMinutes != 10080 {
		t.Fatalf("secondary quota=%+v", got[0].info.Secondary)
	}
}

func TestResponseStreamObservesQuotaForEachFailoverAttempt(t *testing.T) {
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
		w.Header().Set("x-codex-primary-window-minutes", "300")
		if r.Header.Get("Authorization") == "Bearer first-token" {
			w.Header().Set("x-codex-primary-used-percent", "90")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"limited"}`))
			return
		}
		w.Header().Set("x-codex-primary-used-percent", "20")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	observations := &quotaObservations{}
	server := &Server{
		store:        db,
		codex:        codex.NewClient(upstream.URL, codex.NewTokenManager(db, nil)),
		onCodexQuota: observations.callback,
	}
	stream, status, err := server.openResponseStream(ctx, []byte(`{"model":"gpt-5.3-codex","input":"hi","stream":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	defer stream.Body.Close()

	got := observations.snapshot()
	if len(got) != 2 {
		t.Fatalf("observations=%+v", got)
	}
	if got[0].accountID != "first" || got[0].info.Primary == nil || got[0].info.Primary.UsedPercent != 90 {
		t.Fatalf("first observation=%+v", got[0])
	}
	if got[1].accountID != "second" || got[1].info.Primary == nil || got[1].info.Primary.UsedPercent != 20 {
		t.Fatalf("second observation=%+v", got[1])
	}
}

func TestObserveCodexQuotaIgnoresResponsesWithoutQuotaHeaders(t *testing.T) {
	called := false
	server := &Server{onCodexQuota: func(string, codex.QuotaInfo) { called = true }}
	server.observeCodexQuota(context.Background(), "codex", http.Header{"X-Request-Id": []string{"request"}})
	if called {
		t.Fatal("callback should not run without x-codex quota headers")
	}
}

func TestObserveCodexQuotaPersistsWeeklyCooldownWithoutUICallback(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	account := store.Account{
		ID: "codex", Provider: store.ProviderOpenAICodex, Name: "main", Priority: 1, Enabled: true,
		AccessToken: "token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	resetAt := time.Now().Add(24 * time.Hour).Unix()
	header := http.Header{}
	header.Set("x-codex-primary-used-percent", "0")
	header.Set("x-codex-primary-window-minutes", "300")
	header.Set("x-codex-secondary-used-percent", "100")
	header.Set("x-codex-secondary-window-minutes", "10080")
	header.Set("x-codex-secondary-reset-at", strconv.FormatInt(resetAt, 10))

	server := &Server{store: db}
	server.observeCodexQuota(ctx, account.ID, header)

	updated, err := db.MustGetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.CooldownUntil.Valid || updated.CooldownUntil.Time.Unix() != resetAt {
		t.Fatalf("weekly cooldown was not persisted: %+v", updated.CooldownUntil)
	}
}
