package indexer

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
)

// startLaneYieldCycle runs one background coordinator cycle through the real
// admission path. The cycle's work is the barrier: it stands in for a long
// background build (a commit-layer build, a working-tree build of another
// checkout's filesystem edit) and ends when its context does.
func startLaneYieldCycle(t *testing.T, gate *ViewBuildGate, laneYields int, work func(ctx context.Context)) (*CheckoutCoordinator, <-chan CheckoutCycle, context.CancelFunc) {
	t.Helper()
	lifetime, cancel := context.WithCancel(t.Context())
	outcomes := make(chan CheckoutCycle, 1)
	c := &CheckoutCoordinator{
		checkoutID:     "background",
		gate:           gate,
		logger:         zap.NewNop(),
		signal:         make(chan struct{}, 1),
		done:           make(chan struct{}),
		lifetime:       lifetime,
		cyclePreflight: func(context.Context) (CheckoutCycle, bool) { return CheckoutCycle{}, false },
		cycleBarrier:   work,
		cycleDone:      func(out CheckoutCycle) { outcomes <- out },
		laneYields:     laneYields,
	}
	go func() {
		defer close(c.done)
		c.cycle(lifetime)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-c.done:
		case <-time.After(3 * time.Second):
			t.Error("background cycle did not stop")
		}
	})
	return c, outcomes, cancel
}

func awaitLaneYieldOutcome(t *testing.T, outcomes <-chan CheckoutCycle) CheckoutCycle {
	t.Helper()
	select {
	case out := <-outcomes:
		return out
	case <-time.After(3 * time.Second):
		t.Fatal("background cycle reported no outcome")
		return CheckoutCycle{}
	}
}

// An interactive build that starts waiting while a background cycle builds
// gets the lane as soon as the background work observes its canceled
// context; the background cycle publishes nothing, reports the yield, and
// asks for itself again.
func TestABackgroundCycleYieldsTheLaneToAnInteractiveBuild(t *testing.T) {
	gate := NewViewBuildGate()
	gate.Open()
	building := make(chan struct{})
	c, outcomes, _ := startLaneYieldCycle(t, gate, 0, func(ctx context.Context) {
		close(building)
		<-ctx.Done()
	})
	select {
	case <-building:
	case <-time.After(3 * time.Second):
		t.Fatal("background cycle was not admitted")
	}

	asked := time.Now()
	// Bounded, so a background build that never yields fails here rather
	// than hanging the package.
	actx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	release, err := gate.Acquire(actx, ViewBuildInteractive)
	if err != nil {
		t.Fatalf("interactive build was not admitted: %v", err)
	}
	waited := time.Since(asked)
	release()
	t.Logf("interactive build waited %s for the lane a background build held", waited)
	if waited > 100*time.Millisecond {
		t.Errorf("interactive build waited %s for a yielding background build, want ≤ 100ms", waited)
	}

	out := awaitLaneYieldOutcome(t, outcomes)
	if out.Err != nil || !out.Rescheduled || out.YieldedTo != laneYieldedToInteractive {
		t.Fatalf("yielded cycle = %+v, want rescheduled with yielded_to=%s and no error", out, laneYieldedToInteractive)
	}
	if got := c.backgroundLaneYields(); got != 1 {
		t.Fatalf("yield count = %d, want 1", got)
	}
	select {
	case <-c.signal:
	default:
		t.Fatal("yielded cycle did not queue itself again")
	}
	if stats := gate.Stats(); stats.YieldRequests != 1 {
		t.Fatalf("gate yield requests = %d, want 1", stats.YieldRequests)
	}
}

// A background build that has reached its commit point is never abandoned:
// the interactive build waits for it.
func TestABackgroundCycleDoesNotYieldPastItsCommitPoint(t *testing.T) {
	gate := NewViewBuildGate()
	gate.Open()
	publishing := make(chan struct{})
	finish := make(chan struct{})
	canceledWhilePublishing := make(chan bool, 1)
	c, outcomes, cancelLifetime := startLaneYieldCycle(t, gate, 0, func(ctx context.Context) {
		reachBuildCommitPoint(ctx)
		close(publishing)
		<-finish
		canceledWhilePublishing <- ctx.Err() != nil
		// Hold until the test stops the cycle: this seam has no catalog
		// for reconcile to read.
		<-ctx.Done()
	})
	select {
	case <-publishing:
	case <-time.After(3 * time.Second):
		t.Fatal("background cycle was not admitted")
	}

	granted := make(chan func(), 1)
	go func() {
		release, err := gate.Acquire(t.Context(), ViewBuildInteractive)
		if err == nil {
			granted <- release
		}
	}()
	select {
	case release := <-granted:
		release()
		t.Fatal("interactive build took the lane from a build past its commit point")
	case <-time.After(150 * time.Millisecond):
	}
	close(finish)
	if <-canceledWhilePublishing {
		t.Fatal("a build past its commit point was canceled")
	}
	cancelLifetime()
	select {
	case release := <-granted:
		release()
	case <-time.After(3 * time.Second):
		t.Fatal("interactive build never got the lane")
	}
	if out := awaitLaneYieldOutcome(t, outcomes); out.YieldedTo != "" || out.Rescheduled {
		t.Fatalf("committed cycle reported a yield: %+v", out)
	}
	if got := c.backgroundLaneYields(); got != 0 {
		t.Fatalf("yield count = %d, want 0", got)
	}
}

// A checkout cycle never becomes non-preemptible, however often it has
// yielded: past maxViewBuildYields it still gives the lane up to a waiting
// interactive build within one safe point. (Its work is bounded instead — a
// large working tree is built in batches that survive a yield — so it still
// completes between edits.) Live, a cycle at the old bound held another
// checkout's edits for 95 s and 313 s.
func TestBackgroundCyclePastTheYieldBoundStillGivesTheLaneUp(t *testing.T) {
	gate := NewViewBuildGate()
	gate.Open()
	building := make(chan struct{})
	_, outcomes, _ := startLaneYieldCycle(t, gate, maxViewBuildYields+5, func(ctx context.Context) {
		close(building)
		<-ctx.Done()
	})
	select {
	case <-building:
	case <-time.After(3 * time.Second):
		t.Fatal("background cycle was not admitted")
	}
	waited := time.Now()
	acquireCtx, cancelAcquire := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancelAcquire()
	release, err := gate.Acquire(acquireCtx, ViewBuildInteractive)
	if err != nil {
		t.Fatalf("the interactive build never got the lane from a background cycle past the yield bound: %v", err)
	}
	release()
	if wait := time.Since(waited); wait > time.Second {
		t.Fatalf("the interactive build waited %v for a background cycle past the yield bound", wait)
	}
	if stats := gate.Stats(); stats.YieldRefusals != 0 || stats.YieldRequests != 1 {
		t.Fatalf("gate yield stats = requests %d refusals %d, want 1 and 0", stats.YieldRequests, stats.YieldRefusals)
	}
	if out := awaitLaneYieldOutcome(t, outcomes); out.YieldedTo != laneYieldedToInteractive {
		t.Fatalf("the cycle past the bound did not yield: %+v", out)
	}
}

// commit and fire exclude each other: whichever comes first decides, and a
// commit that lost finds its context already canceled.
func TestBackgroundLaneYieldCommitAndFireExclude(t *testing.T) {
	for _, commitFirst := range []bool{true, false} {
		name := "fire_first"
		if commitFirst {
			name = "commit_first"
		}
		t.Run(name, func(t *testing.T) {
			gate := NewViewBuildGate()
			gate.Open()
			release, err := gate.Acquire(t.Context(), ViewBuildBackground)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			ctx, y := armBackgroundLaneYield(t.Context(), gate, 0)
			if y == nil {
				t.Fatal("background holder was not armed")
			}
			defer y.close()
			if commitFirst {
				reachBuildCommitPoint(ctx)
			}
			waiter, cancelWaiter := context.WithCancel(t.Context())
			defer cancelWaiter()
			go func() { _, _ = gate.Acquire(waiter, ViewBuildInteractive) }()
			if commitFirst {
				time.Sleep(50 * time.Millisecond)
				// A yield request that lost the race to the commit (its channel
				// closed before withdraw ran) must not cancel either.
				y.fire()
				if ctx.Err() != nil || y.Yielded() {
					t.Fatal("a committed holder was canceled")
				}
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("armed holder was not canceled for an interactive waiter")
			}
			if y.commit() {
				t.Fatal("commit after a yield reported success")
			}
			if !y.Yielded() {
				t.Fatal("yield not recorded")
			}
		})
	}
	// An interactive holder is never armed.
	gate := NewViewBuildGate()
	gate.Open()
	release, err := gate.Acquire(t.Context(), ViewBuildInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, y := armBackgroundLaneYield(t.Context(), gate, 0)
	if y != nil || ctx != t.Context() {
		t.Fatal("an interactive holder was armed")
	}
	y.close()
	reachBuildCommitPoint(ctx)
}
