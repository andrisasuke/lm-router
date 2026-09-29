package app

import "sync"

// AccountActivityTracker keeps an in-memory count of active upstream requests
// per provider account. The callback runs only when an account transitions
// between idle and active.
type AccountActivityTracker struct {
	mu       sync.RWMutex
	notifyMu sync.Mutex
	counts   map[string]int
	onChange func(accountID string, active bool)
}

func NewAccountActivityTracker(onChange func(accountID string, active bool)) *AccountActivityTracker {
	return &AccountActivityTracker{
		counts:   make(map[string]int),
		onChange: onChange,
	}
}

func (t *AccountActivityTracker) Update(accountID string, active bool) {
	if t == nil || accountID == "" {
		return
	}

	// Serialize the state transition together with its notification. Without
	// this lock, a rapid finish could notify before the matching start and
	// leave the frontend with an out-of-order absolute state.
	t.notifyMu.Lock()
	defer t.notifyMu.Unlock()

	t.mu.Lock()
	previous := t.counts[accountID]
	next := previous
	if active {
		next++
	} else if next > 0 {
		next--
	}
	if next == 0 {
		delete(t.counts, accountID)
	} else {
		t.counts[accountID] = next
	}
	changed := (previous == 0) != (next == 0)
	t.mu.Unlock()

	if changed && t.onChange != nil {
		t.onChange(accountID, next > 0)
	}
}

func (t *AccountActivityTracker) Active(accountID string) bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.counts[accountID] > 0
}
