package indexer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A source-mutation lease builds nothing, so it needs its own checkout's cycle
// lock and not the daemon's one physical build lane. Holding the lane here
// stands in for another checkout's build in progress: the edit must be
// admitted without waiting for it. Any success while the lane stays held is
// the proof; the deadline only bounds a regression.
func TestCheckoutMutationAdmissionDoesNotWaitForAnotherCheckoutsBuild(t *testing.T) {
	f, c, lifecycle := newCheckoutMutationFixture(t)
	before := f.route()
	releaseLane, err := c.gate.Acquire(t.Context(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLane()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	lease, err := lifecycle.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatalf("admission waited on another checkout's build: %v", err)
	}
	lease.Close()
	if !c.gate.Stats().Active {
		t.Fatal("closing a lease released a lane it never held")
	}
	if f.route() != before {
		t.Fatal("dry-run admission changed the route")
	}
}

// A tree the coordinator has to rebuild for is refused at admission BEFORE the
// wait for the cycle lock. While that rebuild is queued or running the route
// still serves the last coherent generation, so reads look exact and an edit
// reaches admission; the coordinator holds the lock for the whole wait, and
// the edit would be refused as stale the moment it got the lock anyway. The
// held lock here stands in for that queued coordinator.
func TestCheckoutMutationAdmissionRefusesAStaleTreeBeforeWaitingForTheLock(t *testing.T) {
	f, c, lifecycle := newCheckoutMutationFixture(t)
	before := f.route()
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc ExternallyEdited() {}\n")
	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	lease, err := lifecycle.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, before.RouteEpoch)
	if lease != nil {
		lease.Close()
		t.Fatal("admitted an edit against a tree the routed view does not describe")
	}
	if !errors.Is(err, ErrCheckoutMutationStale) {
		t.Fatalf("expected the stale refusal, got: %v", err)
	}
	if f.route() != before {
		t.Fatal("a refused admission changed the route")
	}
	c.mu.Lock()
	active := c.sourceMutations
	c.mu.Unlock()
	if active != 0 {
		t.Fatalf("refused admission leaked %d tracked source mutations", active)
	}
}

// The synchronous Refresh is the one lease operation that builds, so it is the
// one that queues for the lane. Its wait carries the busy identity and the
// stage a caller can act on, and it leaves no waiter behind when it gives up.
func TestCheckoutMutationRefreshQueuesForTheSharedLane(t *testing.T) {
	f, c, lifecycle := newCheckoutMutationFixture(t)
	before := f.route()
	releaseLane, err := c.gate.Acquire(t.Context(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			releaseLane()
		}
	}()

	lease, err := lifecycle.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := lease.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc LaneQueuedHelper() {}\n")

	// Generous enough that the checks before the lane acquire cannot be the
	// ones that run out of time on a slow shard; the lane is what expires it.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = lease.Refresh(ctx)
	if !errors.Is(err, ErrCheckoutMutationBusy) || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "shared view-build gate") {
		t.Fatalf("refresh behind a busy lane: %v", err)
	}
	if stats := c.gate.Stats(); stats.InteractiveQueued != 0 {
		t.Fatalf("timed-out refresh left a queued waiter: %+v", stats)
	}
	if route := f.route(); route.State != store_sqlite.RoutePending {
		t.Fatalf("a refresh that never built changed the route: %+v", route)
	}

	releaseLane()
	held = false
	out, err := lease.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !out.DirtyBuilt || f.route().DirtyGenerationID != out.DirtyGenerationID {
		t.Fatalf("refresh after the lane freed did not publish: %+v route=%+v", out, f.route())
	}
	if c.gate.Stats().Active {
		t.Fatal("refresh leaked the lane")
	}
}

// The coordinator takes its cycle lock before it queues for the lane, so a
// lease holding the lock never leaves a coordinator parked on the lane: other
// builders keep moving for as long as the lease's request runs, and the cycle
// publishes the lease's edit once the lock is released.
func TestCheckoutCoordinatorCycleDoesNotHoldTheLaneWhileALeaseHoldsTheLock(t *testing.T) {
	f, c, lifecycle := newCheckoutMutationFixture(t)
	before := f.route()
	lease, err := lifecycle.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			lease.Close()
		}
	}()
	// A withdrawn dirty slot is what makes the cycle need a build rather than
	// settle in its preflight.
	if err := lease.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc OffLaneHelper() {}\n")

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.cycle(t.Context())
	}()

	// Long enough for the cycle to run its preflight and reach the lock. In
	// all that time it must neither take the lane nor complete.
	settle := time.Now().Add(time.Second)
	for time.Now().Before(settle) {
		if c.gate.Stats().Active {
			t.Fatal("the cycle took the lane while a lease held the cycle lock")
		}
		select {
		case <-done:
			t.Fatal("the cycle completed while the lease still held the cycle lock")
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	otherCtx, otherCancel := context.WithTimeout(t.Context(), time.Second)
	defer otherCancel()
	releaseOther, err := c.gate.Acquire(otherCtx, ViewBuildInteractive)
	if err != nil {
		t.Fatalf("another checkout's build could not take the lane while the cycle waited for the lease: %v", err)
	}
	releaseOther()

	lease.Close()
	closed = true
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the cycle did not run after the lease released the lock")
	}
	route := f.route()
	if route.State != store_sqlite.RouteActive || route.DirtyGenerationID == before.DirtyGenerationID || route.DirtyGenerationID == 0 {
		t.Fatalf("the cycle did not publish the lease's edit: %+v", route)
	}
	if c.gate.Stats().Active {
		t.Fatal("the cycle leaked the lane")
	}
}
