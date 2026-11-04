//go:build wailsapp

package main

import (
	"context"
	"errors"
	"sync"
	"time"

	appsvc "github.com/andrisasuke/lm-router/internal/app"
	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
)

const (
	codexQuotaWorkers = appsvc.CodexQuotaRecoveryWorkers
)

type codexQuotaState struct {
	Info    codex.QuotaInfo
	Loading bool
	Error   string
	flight  *codexQuotaFlight
}

type codexQuotaFlight struct {
	done chan struct{}
	info codex.QuotaInfo
	err  error
}

type codexQuotaTracker struct {
	mu       sync.RWMutex
	notifyMu sync.Mutex
	states   map[string]codexQuotaState
	onChange func(accountID string, state codexQuotaState)
}

func newCodexQuotaTracker(onChange func(accountID string, state codexQuotaState)) *codexQuotaTracker {
	return &codexQuotaTracker{
		states:   make(map[string]codexQuotaState),
		onChange: onChange,
	}
}

func (t *codexQuotaTracker) Begin(accountID string) (*codexQuotaFlight, bool) {
	if t == nil || accountID == "" {
		return nil, false
	}

	t.notifyMu.Lock()
	defer t.notifyMu.Unlock()

	t.mu.Lock()
	state := t.states[accountID]
	if state.flight != nil {
		flight := state.flight
		t.mu.Unlock()
		return flight, false
	}
	flight := &codexQuotaFlight{done: make(chan struct{})}
	state.Loading = true
	state.flight = flight
	t.states[accountID] = state
	view := cloneCodexQuotaState(state)
	t.mu.Unlock()

	t.notify(accountID, view)
	return flight, true
}

func (t *codexQuotaTracker) Finish(accountID string, flight *codexQuotaFlight, info codex.QuotaInfo, err error) {
	if t == nil || accountID == "" || flight == nil {
		return
	}

	t.notifyMu.Lock()
	defer t.notifyMu.Unlock()

	normalizeQuotaFetchedAt(&info)
	t.mu.Lock()
	state := t.states[accountID]
	if state.flight != flight {
		t.mu.Unlock()
		return
	}
	flight.info = cloneCodexQuotaInfo(info)
	flight.err = err
	state.flight = nil
	state.Loading = false
	if err != nil {
		state.Error = err.Error()
	} else {
		state.Error = ""
		if hasCodexQuota(info) || !hasCodexQuota(state.Info) {
			if quotaInfoIsNewer(info, state.Info) {
				state.Info = cloneCodexQuotaInfo(info)
			}
		}
	}
	t.states[accountID] = state
	view := cloneCodexQuotaState(state)
	close(flight.done)
	t.mu.Unlock()

	t.notify(accountID, view)
}

func (t *codexQuotaTracker) Observe(accountID string, info codex.QuotaInfo) {
	if t == nil || accountID == "" || !hasCodexQuota(info) {
		return
	}

	t.notifyMu.Lock()
	defer t.notifyMu.Unlock()

	normalizeQuotaFetchedAt(&info)
	t.mu.Lock()
	state := t.states[accountID]
	if !quotaInfoIsNewer(info, state.Info) {
		t.mu.Unlock()
		return
	}
	state.Info = cloneCodexQuotaInfo(info)
	state.Error = ""
	t.states[accountID] = state
	view := cloneCodexQuotaState(state)
	t.mu.Unlock()

	t.notify(accountID, view)
}

func (t *codexQuotaTracker) Snapshot(accountID string) codexQuotaState {
	if t == nil || accountID == "" {
		return codexQuotaState{}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return cloneCodexQuotaState(t.states[accountID])
}

func (t *codexQuotaTracker) notify(accountID string, state codexQuotaState) {
	if t.onChange != nil {
		t.onChange(accountID, state)
	}
}

func hasCodexQuota(info codex.QuotaInfo) bool {
	return info.Primary != nil || info.Secondary != nil
}

func normalizeQuotaFetchedAt(info *codex.QuotaInfo) {
	if info != nil && info.FetchedAt.IsZero() {
		info.FetchedAt = time.Now()
	}
}

func quotaInfoIsNewer(incoming, current codex.QuotaInfo) bool {
	return current.FetchedAt.IsZero() || !incoming.FetchedAt.Before(current.FetchedAt)
}

func cloneCodexQuotaState(state codexQuotaState) codexQuotaState {
	state.Info = cloneCodexQuotaInfo(state.Info)
	return state
}

func cloneCodexQuotaInfo(info codex.QuotaInfo) codex.QuotaInfo {
	result := info
	if info.Primary != nil {
		primary := *info.Primary
		result.Primary = &primary
	}
	if info.Secondary != nil {
		secondary := *info.Secondary
		result.Secondary = &secondary
	}
	result.HeaderKeys = append([]string(nil), info.HeaderKeys...)
	return result
}

func (a *App) refreshCodexQuota(ctx context.Context, account store.Account) (codex.QuotaInfo, error) {
	if a.codexQuotas == nil {
		return codex.QuotaInfo{}, errors.New("Codex quota tracker is not initialized")
	}
	flight, leader := a.codexQuotas.Begin(account.ID)
	if !leader {
		if flight == nil {
			return codex.QuotaInfo{}, errors.New("Codex quota refresh could not start")
		}
		select {
		case <-flight.done:
			return cloneCodexQuotaInfo(flight.info), flight.err
		case <-ctx.Done():
			return codex.QuotaInfo{}, ctx.Err()
		}
	}

	fetch := a.codexQuotaFetcher
	if fetch == nil {
		fetch = func(ctx context.Context, account store.Account) (codex.QuotaInfo, error) {
			return a.providerService().Quota(ctx, account)
		}
	}
	info, err := fetch(ctx, account)
	a.codexQuotas.Finish(account.ID, flight, info, err)
	return info, err
}

func (a *App) warmCodexQuotas(ctx context.Context) {
	accounts, err := a.providerService().ListProvider(ctx, store.ProviderOpenAICodex)
	if err != nil {
		if ctx.Err() == nil && a.logger != nil {
			a.logger.Printf("[quota] list Codex accounts: %v", err)
		}
		return
	}
	eligible := make([]store.Account, 0, len(accounts))
	for _, account := range accounts {
		if !account.NeedsReauth {
			eligible = append(eligible, account)
		}
	}
	a.refreshCodexQuotaAccounts(ctx, eligible)
}

// recoverCodexQuotas delegates persisted cooldown recovery to the shared
// scheduler. Its quota hook preserves this UI's tracker single-flight: startup
// warming, manual checks, proxy observations, and recovery all coalesce per
// account while the underlying fetch remains ProviderService.Quota.
func (a *App) recoverCodexQuotas(ctx context.Context) {
	if a.codexQuotaRecovery != nil {
		a.codexQuotaRecovery.Run(ctx)
	}
}

func (a *App) refreshCodexQuotaAccounts(ctx context.Context, accounts []store.Account) {
	jobs := make(chan store.Account)
	var workers sync.WaitGroup
	workerCount := codexQuotaWorkers
	if len(accounts) < workerCount {
		workerCount = len(accounts)
	}
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for account := range jobs {
				_, _ = a.refreshCodexQuota(ctx, account)
			}
		}()
	}
	for _, account := range accounts {
		select {
		case jobs <- account:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		}
	}
	close(jobs)
	workers.Wait()
}

func (a *App) observeCodexQuota(accountID string, info codex.QuotaInfo) {
	if a.codexQuotas != nil {
		a.codexQuotas.Observe(accountID, info)
	}
}
