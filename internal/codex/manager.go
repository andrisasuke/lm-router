package codex

import (
	"context"
	"sync"
	"time"

	"github.com/andrisasuke/lm-router/internal/oauth"
	"github.com/andrisasuke/lm-router/internal/store"
)

type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

type Refresher interface {
	Refresh(context.Context, string) (TokenSet, error)
}

type RefreshFunc func(context.Context, string) (TokenSet, error)

func (f RefreshFunc) Refresh(ctx context.Context, refreshToken string) (TokenSet, error) {
	return f(ctx, refreshToken)
}

type TokenManager struct {
	db        *store.DB
	refresher Refresher
	mu        sync.Mutex
	locks     map[string]*sync.Mutex
}

func NewTokenManager(db *store.DB, refresher Refresher) *TokenManager {
	return &TokenManager{
		db:        db,
		refresher: refresher,
		locks:     make(map[string]*sync.Mutex),
	}
}

func (m *TokenManager) EnsureFresh(ctx context.Context, accountID string) (store.Account, error) {
	account, err := m.db.MustGetAccount(ctx, accountID)
	if err != nil {
		return store.Account{}, err
	}
	if !needsRefresh(account) || m.refresher == nil {
		return account, nil
	}
	return m.refreshLocked(ctx, accountID, false)
}

func (m *TokenManager) RefreshNow(ctx context.Context, accountID string) (store.Account, error) {
	if m.refresher == nil {
		return m.db.MustGetAccount(ctx, accountID)
	}
	return m.refreshLocked(ctx, accountID, true)
}

func (m *TokenManager) refreshLocked(ctx context.Context, accountID string, force bool) (store.Account, error) {
	lock := m.accountLock(accountID)
	lock.Lock()
	defer lock.Unlock()

	account, err := m.db.MustGetAccount(ctx, accountID)
	if err != nil {
		return store.Account{}, err
	}
	if !force && !needsRefresh(account) {
		return account, nil
	}

	tokenSet, err := m.refresher.Refresh(ctx, account.RefreshToken)
	if err != nil {
		if err == ErrNeedsReauth || oauth.IsUnrecoverableRefreshError(err) {
			_ = m.db.MarkNeedsReauth(ctx, account.ID)
		}
		return store.Account{}, err
	}
	if tokenSet.RefreshToken == "" {
		tokenSet.RefreshToken = account.RefreshToken
	}
	if err := m.db.UpdateTokens(ctx, account.ID, tokenSet.AccessToken, tokenSet.RefreshToken, tokenSet.ExpiresAt); err != nil {
		return store.Account{}, err
	}
	return m.db.MustGetAccount(ctx, accountID)
}

func (m *TokenManager) MarkNeedsReauth(ctx context.Context, accountID string) error {
	return m.db.MarkNeedsReauth(ctx, accountID)
}

func needsRefresh(account store.Account) bool {
	if account.ExpiresAt.IsZero() {
		return false
	}
	return time.Until(account.ExpiresAt) < 5*time.Minute
}

func (m *TokenManager) accountLock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lock, ok := m.locks[id]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	m.locks[id] = lock
	return lock
}

var ErrNeedsReauth = oauthError("needs reauth")

type oauthError string

func (e oauthError) Error() string { return string(e) }
