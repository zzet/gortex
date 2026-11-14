package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"
)

// A checkout whose cycles keep failing backs its poll off instead of
// re-entering the mutation lane every interval (#1458), and a cycle that
// succeeds puts it back on the plain interval.
func TestFailedCyclesBackThePollOff(t *testing.T) {
	gate := NewViewBuildGate()
	gate.Open()
	store := builderOpenStoreAt(t, filepath.Join(t.TempDir(), "catalog.sqlite"))
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	outcomes := make(chan CheckoutCycle, 1)
	succeed := false
	c := &CheckoutCoordinator{
		checkoutID: "failing",
		// A family with no primary dedicated graph: reconcile fails at its
		// first catalog read, every time, on unchanged input.
		familyID: "family-without-a-primary",
		catalog:  store.Catalog(),
		gate:     gate,
		logger:   zap.NewNop(),
		signal:   make(chan struct{}, 1),
		done:     make(chan struct{}),
		lifetime: t.Context(),
		poll:     15 * time.Second,
		cyclePreflight: func(context.Context) (CheckoutCycle, bool) {
			return CheckoutCycle{}, succeed
		},
		cycleDone: func(out CheckoutCycle) { outcomes <- out },
	}
	if got := c.failedCyclePollDelay(); got != 15*time.Second {
		t.Fatalf("poll delay before any failure = %v, want the interval", got)
	}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		c.cycle(t.Context())
		if out := awaitLaneYieldOutcome(t, outcomes); out.Err == nil {
			t.Fatalf("cycle %d was expected to fail: %+v", i+1, out)
		}
		if got := c.failedCyclePollDelay(); got != w {
			t.Fatalf("poll delay after %d failed cycles = %v, want %v", i+1, got, w)
		}
	}

	succeed = true
	c.cycle(t.Context())
	if out := awaitLaneYieldOutcome(t, outcomes); out.Err != nil {
		t.Fatalf("the preflight-answered cycle failed: %v", out.Err)
	}
	if got := c.failedCyclePollDelay(); got != 15*time.Second {
		t.Fatalf("poll delay after a successful cycle = %v, want the interval", got)
	}

	// An interval longer than the cap is never shortened by it.
	c.poll = 10 * time.Minute
	c.failedCycles.Store(3)
	if got := c.failedCyclePollDelay(); got != 10*time.Minute {
		t.Fatalf("poll delay with a 10m interval = %v, want 10m", got)
	}
}

// A cycle that was canceled, deferred or gave the lane up says nothing about
// whether the checkout builds, so it neither extends nor ends the failure run;
// only a cycle that ran to an error-free end does.
func TestCycleOutcomesWithoutEvidenceKeepTheFailureRun(t *testing.T) {
	c := &CheckoutCoordinator{poll: 15 * time.Second}
	c.failedCycles.Store(2)
	for name, out := range map[string]CheckoutCycle{
		"canceled":         {Err: context.Canceled},
		"wrapped canceled": {Err: fmt.Errorf("reconcile: %w", context.Canceled)},
		"lane yield":       {Rescheduled: true, YieldedTo: laneYieldedToInteractive},
		"tree moved":       {Rescheduled: true, YieldedTo: treeMovedReason},
		"failed then yield": {
			Err: errors.New("presence check"), Rescheduled: true, YieldedTo: laneYieldedToInteractive,
		},
		"deferred": {Deferred: true},
	} {
		c.noteCycleFailure(out)
		if got := c.failedCycles.Load(); got != 2 {
			t.Fatalf("%s: failed cycles = %d, want 2 (unchanged)", name, got)
		}
	}
	c.noteCycleFailure(CheckoutCycle{Err: errors.New("presence check")})
	if got := c.failedCycles.Load(); got != 3 {
		t.Fatalf("a failed cycle left the run at %d, want 3", got)
	}
	// A large working tree's batch publishes and is rescheduled for the next
	// one: that is a successful build, and it ends the run.
	c.noteCycleFailure(CheckoutCycle{Rescheduled: true, DirtyBatchRemaining: 1})
	if got := c.failedCycles.Load(); got != 0 {
		t.Fatalf("a successful batch left the run at %d, want 0", got)
	}
}

// The coordinator's own poll is what spaces the cycles of a checkout that
// fails on unchanged input: the gap doubles from the interval to the cap, and
// the first success restores the interval for the poll after the one it armed.
func TestFailingCheckoutPollBacksOffOnFakeClock(t *testing.T) {
	const (
		interval = 15 * time.Second
		quiet    = 10 * time.Millisecond
		failures = 7
	)
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var at []time.Time
		var n atomic.Int64
		c := &CheckoutCoordinator{
			checkoutID: "failing-on-fake-clock",
			root:       ".",
			quiet:      quiet,
			poll:       interval,
			signal:     make(chan struct{}, 1),
			stop:       make(chan struct{}),
			done:       make(chan struct{}),
			cyclePreflight: func(context.Context) (CheckoutCycle, bool) {
				if n.Add(1) <= failures {
					return CheckoutCycle{Err: errors.New("change set names a path the target does not hold")}, true
				}
				return CheckoutCycle{}, true
			},
			cycleDone: func(CheckoutCycle) {
				mu.Lock()
				at = append(at, time.Now())
				mu.Unlock()
			},
		}
		go c.run()

		time.Sleep(initialCheckoutPollDelay(c.checkoutID, interval) + quiet + 30*time.Minute)
		synctest.Wait()
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		// Each poll arms the next from the failures counted when it fires, so
		// the doubling trails the failures by one poll, and so does the reset.
		want := []time.Duration{
			interval, 2 * interval, 4 * interval, 8 * interval, 16 * interval,
			maxCheckoutPollBackoff, maxCheckoutPollBackoff, maxCheckoutPollBackoff,
			interval, interval,
		}
		mu.Lock()
		defer mu.Unlock()
		if len(at) < len(want)+1 {
			t.Fatalf("only %d cycles ran, want at least %d", len(at), len(want)+1)
		}
		for i, w := range want {
			if got := at[i+1].Sub(at[i]); got != w {
				t.Fatalf("gap after cycle %d = %v, want %v", i+1, got, w)
			}
		}
	})
}
