package indexer

import "sync"

// checkoutBuildFailures is the process-local fallback for a terminal build
// error that the store was too full or unavailable to persist. Durable catalog
// state remains authoritative whenever it can be written; this registry only
// prevents a failed building row from presenting as permanently in progress.
type checkoutBuildFailures struct {
	mu       sync.RWMutex
	attempts map[string]checkoutBuildAttempt
}

type checkoutBuildAttempt struct {
	generationID int64
	failed       bool
	reason       string
}

func newCheckoutBuildFailures() *checkoutBuildFailures {
	return &checkoutBuildFailures{attempts: make(map[string]checkoutBuildAttempt)}
}

func (f *checkoutBuildFailures) start(checkoutID string, generationID int64) {
	if f == nil || checkoutID == "" || generationID <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if current, found := f.attempts[checkoutID]; found && current.generationID > generationID {
		return
	}
	f.attempts[checkoutID] = checkoutBuildAttempt{generationID: generationID}
}

func (f *checkoutBuildFailures) record(checkoutID string, generationID int64, reason string) {
	if f == nil || checkoutID == "" || generationID <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, found := f.attempts[checkoutID]
	if !found || current.generationID != generationID {
		return
	}
	current.failed = true
	current.reason = reason
	f.attempts[checkoutID] = current
}

func (f *checkoutBuildFailures) clearThrough(checkoutID string, generationID int64) {
	if f == nil || checkoutID == "" || generationID <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if current, found := f.attempts[checkoutID]; found && current.generationID <= generationID {
		delete(f.attempts, checkoutID)
	}
}

func (f *checkoutBuildFailures) failure(checkoutID string, generationID int64) (string, bool) {
	if f == nil || checkoutID == "" || generationID <= 0 {
		return "", false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	current, found := f.attempts[checkoutID]
	if !found || current.generationID != generationID || !current.failed {
		return "", false
	}
	return current.reason, true
}
