package app

import (
	"context"
	"sync"
	"time"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
)

const (
	// CodexQuotaRecoveryPollInterval keeps recovery responsive after a quota
	// reset without repeatedly probing accounts that are still cooling down.
	CodexQuotaRecoveryPollInterval = 10 * time.Second
	// CodexQuotaRecoveryRetryInterval bounds retries when a due account's
	// quota endpoint is unavailable or reports incomplete usage data.
	CodexQuotaRecoveryRetryInterval = time.Minute
	CodexQuotaRecoveryWorkers       = 2
)

// CodexQuotaRecoveryResult describes one asynchronous due-cooldown probe.
// The account ID is intentionally stable so UI clients can reload their own
// cached account state from SQLite after ProviderService.Quota updates it.
type CodexQuotaRecoveryResult struct {
	AccountID string
	Info      codex.QuotaInfo
	Err       error
}

// CodexQuotaRecoveryScheduler restores Codex accounts after their persisted
// cooldowns become due. SQLite is the source of truth on every scan: neither
// UI-local quota observations nor prior scheduler results decide eligibility.
//
// Quota is an optional test/UI integration hook. Production defaults to
// ProviderService.Quota; desktop supplies its single-flight wrapper around the
// same service so startup warming, manual refreshes, and recovery cannot make
// duplicate requests for an account.
type CodexQuotaRecoveryScheduler struct {
	DB      *store.DB
	Service ProviderService

	Quota    func(context.Context, store.Account) (codex.QuotaInfo, error)
	OnResult func(CodexQuotaRecoveryResult)

	RetryInterval time.Duration
	Workers       int
	Now           func() time.Time

	configMu         sync.Mutex
	enabled          bool
	interval         time.Duration
	enableGeneration uint64
	wake             chan struct{}
}

func NewCodexQuotaRecoveryScheduler(db *store.DB, service ProviderService) *CodexQuotaRecoveryScheduler {
	if service.DB == nil {
		service.DB = db
	}
	return &CodexQuotaRecoveryScheduler{
		DB:            db,
		Service:       service,
		RetryInterval: CodexQuotaRecoveryRetryInterval,
		Workers:       CodexQuotaRecoveryWorkers,
		Now:           time.Now,
		enabled:       true,
		interval:      CodexQuotaRecoveryPollInterval,
		wake:          make(chan struct{}, 1),
	}
}

// Configure changes recovery behavior while Run is active. The wake signal
// causes the current timer to be discarded and rebuilt from the moment the
// configuration was applied. Enabling recovery also requests an immediate
// scan; disabling it never cancels a request already in flight.
func (s *CodexQuotaRecoveryScheduler) Configure(enabled bool, interval time.Duration) {
	if s == nil {
		return
	}
	if interval <= 0 {
		interval = CodexQuotaRecoveryPollInterval
	}
	s.configMu.Lock()
	if !s.enabled && enabled {
		s.enableGeneration++
	}
	s.enabled = enabled
	s.interval = interval
	wake := s.wake
	s.configMu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// Run performs an immediate scan when enabled, then continues scheduling scans
// until ctx is cancelled. Cancellation is passed through to all quota
// requests and waits for any scheduler workers to leave before returning.
func (s *CodexQuotaRecoveryScheduler) Run(ctx context.Context) {
	if s == nil || s.DB == nil || ctx == nil {
		return
	}
	retryAfter := make(map[string]time.Time)
	config := s.config()
	seenEnableGeneration := config.enableGeneration
	var timer *time.Timer
	var timerC <-chan time.Time
	if config.enabled {
		s.scan(ctx, s.now(), retryAfter)
		timer = time.NewTimer(config.interval)
		timerC = timer.C
	}
	defer stopCodexQuotaRecoveryTimer(timer)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			updated := s.config()
			if !updated.enabled {
				stopCodexQuotaRecoveryTimer(timer)
				timer = nil
				timerC = nil
				config = updated
				continue
			}
			if !config.enabled || updated.enableGeneration != seenEnableGeneration {
				config = updated
				seenEnableGeneration = updated.enableGeneration
				s.scan(ctx, s.now(), retryAfter)
				timer = resetCodexQuotaRecoveryTimer(timer, config.interval)
				timerC = timer.C
				continue
			}
			config = updated
			timer = resetCodexQuotaRecoveryTimer(timer, config.interval)
			timerC = timer.C
		case <-timerC:
			config = s.config()
			seenEnableGeneration = config.enableGeneration
			if !config.enabled {
				stopCodexQuotaRecoveryTimer(timer)
				timer = nil
				timerC = nil
				continue
			}
			s.scan(ctx, s.now(), retryAfter)
			timer = resetCodexQuotaRecoveryTimer(timer, config.interval)
			timerC = timer.C
		}
	}
}

func (s *CodexQuotaRecoveryScheduler) scan(ctx context.Context, now time.Time, retryAfter map[string]time.Time) {
	if s == nil || s.DB == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	accounts, err := s.DB.ListAccountsByProvider(ctx, store.ProviderOpenAICodex)
	if err != nil || ctx.Err() != nil {
		return
	}

	jobs := make(chan store.Account)
	workers := s.workers()
	done := make(chan struct{}, workers)
	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			for {
				select {
				case <-ctx.Done():
					return
				case account, ok := <-jobs:
					if !ok {
						return
					}
					info, quotaErr := s.quota(ctx, account)
					if s.OnResult != nil {
						s.OnResult(CodexQuotaRecoveryResult{AccountID: account.ID, Info: info, Err: quotaErr})
					}
				}
			}
		}()
	}

	for _, account := range accounts {
		if !eligibleForCodexQuotaRecovery(account, now) || retryAfter[account.ID].After(now) {
			continue
		}
		// Throttle from dispatch time, not completion, so slow or failed probes
		// cannot create an unbounded queue on successive configured scans.
		retryAfter[account.ID] = now.Add(s.retryInterval())
		select {
		case <-ctx.Done():
			close(jobs)
			for range workers {
				<-done
			}
			return
		case jobs <- account:
		}
	}
	close(jobs)
	for range workers {
		<-done
	}
}

func eligibleForCodexQuotaRecovery(account store.Account, now time.Time) bool {
	return account.Enabled && !account.NeedsReauth && account.CooldownUntil.Valid && !account.CooldownUntil.Time.After(now)
}

func (s *CodexQuotaRecoveryScheduler) quota(ctx context.Context, account store.Account) (codex.QuotaInfo, error) {
	if s.Quota != nil {
		return s.Quota(ctx, account)
	}
	return s.Service.Quota(ctx, account)
}

func (s *CodexQuotaRecoveryScheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *CodexQuotaRecoveryScheduler) retryInterval() time.Duration {
	if s.RetryInterval > 0 {
		return s.RetryInterval
	}
	return CodexQuotaRecoveryRetryInterval
}

type codexQuotaRecoveryConfig struct {
	enabled          bool
	interval         time.Duration
	enableGeneration uint64
}

func (s *CodexQuotaRecoveryScheduler) config() codexQuotaRecoveryConfig {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	interval := s.interval
	if interval <= 0 {
		interval = CodexQuotaRecoveryPollInterval
	}
	return codexQuotaRecoveryConfig{enabled: s.enabled, interval: interval, enableGeneration: s.enableGeneration}
}

func resetCodexQuotaRecoveryTimer(timer *time.Timer, interval time.Duration) *time.Timer {
	if interval <= 0 {
		interval = CodexQuotaRecoveryPollInterval
	}
	if timer == nil {
		return time.NewTimer(interval)
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
	return timer
}

func stopCodexQuotaRecoveryTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (s *CodexQuotaRecoveryScheduler) workers() int {
	if s.Workers > 0 {
		return s.Workers
	}
	return CodexQuotaRecoveryWorkers
}
