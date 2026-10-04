package indexer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A mutation that arrives while its checkout's coordinator is not registered
// yet (the daemon is still activating it) waits for the coordinator instead of
// being refused as read-only, and is admitted once it registers.
func TestCheckoutMutationWaitsForItsCoordinatorToRegister(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	route := f.route()
	requested := observeMutationActivation(t)
	l.coordMu.Lock()
	delete(l.coordinators, f.checkoutID)
	l.coordMu.Unlock()
	go func() {
		time.Sleep(200 * time.Millisecond)
		l.coordMu.Lock()
		l.coordinators[f.checkoutID] = c
		l.coordMu.Unlock()
	}()
	started := time.Now()
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, route.RouteEpoch)
	if err != nil {
		t.Fatalf("a mutation sent before activation was refused after %s: %v", time.Since(started), err)
	}
	m.Close()
	if waited := time.Since(started); waited < 150*time.Millisecond {
		t.Fatalf("admitted after %s, before the coordinator registered", waited)
	}
	if got := requested(); len(got) != 1 || got[0] != f.checkoutID {
		t.Fatalf("activation requests = %v, want one for %s", got, f.checkoutID)
	}
}

// observeMutationActivation replaces the activation a coordinator-less
// mutation asks for with a recorder, for the test's duration.
func observeMutationActivation(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var requests []string
	previous := activateCheckoutForMutation
	activateCheckoutForMutation = func(_ *CheckoutLifecycle, checkoutID, _ string) bool {
		mu.Lock()
		requests = append(requests, checkoutID)
		mu.Unlock()
		return true
	}
	t.Cleanup(func() { activateCheckoutForMutation = previous })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), requests...)
	}
}

// The wait is bounded by the caller: a coordinator that never registers is
// refused as stale once the caller's deadline passes.
func TestCheckoutMutationWaitForACoordinatorIsBounded(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	route := f.route()
	observeMutationActivation(t)
	l.coordMu.Lock()
	delete(l.coordinators, f.checkoutID)
	l.coordMu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := l.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, route.RouteEpoch)
	if !errors.Is(err, ErrCheckoutMutationStale) {
		t.Fatalf("a mutation with no coordinator returned %v, want %v", err, ErrCheckoutMutationStale)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatalf("the refusal took %s; the caller's deadline was 300ms", waited)
	}
}
