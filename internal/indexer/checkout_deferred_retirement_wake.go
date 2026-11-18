package indexer

import "sync"

// The deferred retirement worker's wake signal.
//
// With nothing pending the worker sleeps its idle pause. Work that arrives in
// that sleep — a reference released for a parked generation — used to wait the
// pause out. The signal is a broadcast: every waiter holds the channel current
// when it last looked, and a notification closes it, so a notification that
// lands between a pass and its wait is not lost.

var deferredRetirementWakeState struct {
	mu sync.Mutex
	ch chan struct{}
}

// DeferredRetirementWake returns a channel closed by the next notification of
// deferred retirement work.
func DeferredRetirementWake() <-chan struct{} {
	deferredRetirementWakeState.mu.Lock()
	defer deferredRetirementWakeState.mu.Unlock()
	if deferredRetirementWakeState.ch == nil {
		deferredRetirementWakeState.ch = make(chan struct{})
	}
	return deferredRetirementWakeState.ch
}

// notifyDeferredRetirementWork wakes every worker waiting on its idle pause.
func notifyDeferredRetirementWork() {
	deferredRetirementWakeState.mu.Lock()
	defer deferredRetirementWakeState.mu.Unlock()
	// Nobody has looked since the last notification: there is no waiter to
	// wake, and the next look starts a fresh channel.
	if deferredRetirementWakeState.ch != nil {
		close(deferredRetirementWakeState.ch)
		deferredRetirementWakeState.ch = nil
	}
}
