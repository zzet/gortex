package graphview

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestLeaseHandoffOutlivesTheAcquirersRelease is the core of the handoff
// contract: the acquirer returning does not unpin generations a joined
// consumer is still reading.
func TestLeaseHandoffOutlivesTheAcquirersRelease(t *testing.T) {
	m := NewLeaseManager()
	lease := m.Acquire(11, 12)
	if got := lease.Holders(); got != 1 {
		t.Fatalf("Holders() = %d on a fresh lease, want 1", got)
	}

	joined := lease.Handoff()
	if joined == nil {
		t.Fatal("Handoff() = nil on a live lease")
	}
	if got := lease.Holders(); got != 2 {
		t.Fatalf("Holders() = %d after Handoff, want 2", got)
	}

	// The handler returns. Its own hold is gone, the joined consumer's is not.
	lease.Release()
	lease.Release() // idempotent: a second return must not drop another holder
	for _, id := range []int64{11, 12} {
		if !m.InUse(id) {
			t.Fatalf("InUse(%d) = false while a joined consumer is live", id)
		}
	}
	if got := lease.Holders(); got != 1 {
		t.Fatalf("Holders() = %d after the acquirer released, want 1", got)
	}

	joined.Release()
	joined.Release() // idempotent
	for _, id := range []int64{11, 12} {
		if m.InUse(id) {
			t.Fatalf("InUse(%d) = true after every holder released", id)
		}
	}
	if got := lease.Holders(); got != 0 {
		t.Fatalf("Holders() = %d after every holder released, want 0", got)
	}
}

// TestLeaseHandoffReleasesInEitherOrder covers the joined consumer finishing
// first: the pins then survive until the acquirer releases.
func TestLeaseHandoffReleasesInEitherOrder(t *testing.T) {
	m := NewLeaseManager()
	lease := m.Acquire(21)
	joined := lease.Handoff()

	joined.Release()
	if !m.InUse(21) {
		t.Fatal("InUse(21) = false while the acquirer still holds the lease")
	}
	lease.Release()
	if m.InUse(21) {
		t.Fatal("InUse(21) = true after every holder released")
	}
}

// TestLeaseHandoffRefusedOnceThePinsAreGone keeps the primitive truthful: once
// the generations are unpinned the payload may already have been swept, so
// there is nothing to join and Handoff must say so rather than hand back a
// handle that pins nothing.
func TestLeaseHandoffRefusedOnceThePinsAreGone(t *testing.T) {
	m := NewLeaseManager()
	lease := m.Acquire(31)

	first := lease.Handoff()
	if first == nil {
		t.Fatal("Handoff() = nil on a live lease")
	}
	lease.Release()
	// A joined consumer is still live, so joining again is still legitimate.
	second := lease.Handoff()
	if second == nil {
		t.Fatal("Handoff() = nil while a joined consumer was still live")
	}
	first.Release()
	second.Release()
	if m.InUse(31) {
		t.Fatal("InUse(31) = true after every holder released")
	}

	if late := lease.Handoff(); late != nil {
		t.Fatal("Handoff() returned a handle after every holder released")
	}
	if m.InUse(31) {
		t.Fatal("a refused Handoff re-pinned the generation")
	}
}

// TestLeaseHandoffDoesNotDisturbOtherHolders pins the refcount arithmetic: the
// handoff must add and drop exactly one hold on the lease, never a pin in the
// manager that another lease on the same generation would notice.
func TestLeaseHandoffDoesNotDisturbOtherHolders(t *testing.T) {
	m := NewLeaseManager()
	mine := m.Acquire(41)
	theirs := m.Acquire(41)

	joined := mine.Handoff()
	mine.Release()
	mine.Release()
	joined.Release()
	joined.Release()
	if !m.InUse(41) {
		t.Fatal("InUse(41) = false while an unrelated lease still holds it")
	}

	theirs.Release()
	if m.InUse(41) {
		t.Fatal("InUse(41) = true after the last lease released")
	}
	if got := m.Held(); got != 0 {
		t.Fatalf("Held() = %d after every lease released, want 0", got)
	}
}

// TestLeaseHandoffOnEmptyAndNilLeases keeps the nil/no-op paths safe: an
// empty lease can still be handed off, and neither a nil lease nor a nil
// handle panics.
func TestLeaseHandoffOnEmptyAndNilLeases(t *testing.T) {
	m := NewLeaseManager()
	empty := m.Acquire()
	joined := empty.Handoff()
	if joined == nil {
		t.Fatal("Handoff() = nil on a live lease that pins nothing")
	}
	if got := joined.IDs(); len(got) != 0 {
		t.Fatalf("IDs() = %v on an empty handoff, want none", got)
	}
	empty.Release()
	joined.Release()
	joined.Release()

	var nilLease *Lease
	if got := nilLease.Handoff(); got != nil {
		t.Error("nil lease Handoff() returned a handle")
	}
	if got := nilLease.Holders(); got != 0 {
		t.Errorf("nil lease Holders() = %d, want 0", got)
	}
	var nilHandoff *LeaseHandoff
	nilHandoff.Release()
	if got := nilHandoff.IDs(); got != nil {
		t.Errorf("nil handoff IDs() = %v, want nil", got)
	}
}

// TestLeaseHandoffIDsMirrorTheLease proves the handle names the same pinned
// set the lease does, and hands out a copy.
func TestLeaseHandoffIDsMirrorTheLease(t *testing.T) {
	m := NewLeaseManager()
	lease := m.Acquire(51, 52)
	joined := lease.Handoff()
	defer joined.Release()
	lease.Release()

	ids := joined.IDs()
	if len(ids) != 2 || ids[0] != 51 || ids[1] != 52 {
		t.Fatalf("IDs() = %v, want [51 52]", ids)
	}
	ids[0] = 99
	if !m.InUse(51) || m.InUse(99) {
		t.Fatal("mutating the IDs() copy changed what the handoff pins")
	}
}

// TestWaitDrainWaitsForAJoinedConsumer is the retirement-side half of the
// contract: the drain a retirement sweep waits on must not complete while a
// detached worker still holds the payload.
func TestWaitDrainWaitsForAJoinedConsumer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewLeaseManager()
		lease := m.Acquire(61, 62)
		joined := lease.Handoff()

		done := waitDrainInBackground(context.Background(), m, 61, 62)
		synctest.Wait()
		assertStillWaiting(t, done, "while the lease and its joined consumer were held")

		lease.Release()
		synctest.Wait()
		assertStillWaiting(t, done, "while the joined consumer was still live")

		joined.Release()
		synctest.Wait()
		assertWoke(t, done, "after the joined consumer closed")
	})
}

// TestLeaseHandoffKeepsACancellationTailPinned is the shape the request path
// needs: the handler's context is cancelled and its lease released, but the
// detached worker keeps reading. The generation must stay pinned until the
// worker actually finishes, not until the handler returns.
func TestLeaseHandoffKeepsACancellationTailPinned(t *testing.T) {
	m := NewLeaseManager()
	lease := m.Acquire(71)

	requestCtx, cancel := context.WithCancel(context.Background())
	joined := lease.Handoff()
	if joined == nil {
		t.Fatal("Handoff() = nil on a live lease")
	}

	started := make(chan struct{})
	finish := make(chan struct{})
	workerDone := make(chan struct{})
	go func() {
		// The tail deliberately outlives the request context, exactly as the
		// detached workers in the request path do.
		detached := context.WithoutCancel(requestCtx)
		defer close(workerDone)
		defer joined.Release()
		close(started)
		<-finish
		if err := detached.Err(); err != nil {
			t.Errorf("detached context = %v, want no error", err)
		}
		if !m.InUse(71) {
			t.Error("InUse(71) = false while the detached worker was still running")
		}
	}()

	<-started
	cancel()
	lease.Release()
	if !m.InUse(71) {
		t.Fatal("InUse(71) = false after the handler returned, while its worker is still running")
	}
	bounded, boundedCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer boundedCancel()
	if err := m.WaitDrain(bounded, 71); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitDrain while the worker runs = %v, want %v", err, context.DeadlineExceeded)
	}

	close(finish)
	<-workerDone
	if m.InUse(71) {
		t.Fatal("InUse(71) = true after the detached worker finished")
	}
	if err := m.WaitDrain(context.Background(), 71); err != nil {
		t.Fatalf("WaitDrain after the worker finished: %v", err)
	}
}

// TestLeaseHandoffConcurrentJoinAndRelease exercises the handle refcount from
// many goroutines: with -race this is where a torn count or a double release
// would show up.
func TestLeaseHandoffConcurrentJoinAndRelease(t *testing.T) {
	m := NewLeaseManager()
	lease := m.Acquire(81)

	const workers = 32
	var wg sync.WaitGroup
	release := make(chan struct{})
	for range workers {
		joined := lease.Handoff()
		if joined == nil {
			t.Fatal("Handoff() = nil on a live lease")
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-release
			joined.Release()
			joined.Release()
		}()
	}
	// Joins racing with the acquirer's own return.
	var joinWG sync.WaitGroup
	joined := make(chan *LeaseHandoff, workers)
	for range workers {
		joinWG.Add(1)
		go func() {
			defer joinWG.Done()
			joined <- lease.Handoff()
		}()
	}
	lease.Release()
	joinWG.Wait()
	close(joined)

	if !m.InUse(81) {
		t.Fatal("InUse(81) = false while joined consumers are live")
	}
	close(release)
	wg.Wait()
	for h := range joined {
		// A join that lost the race with the last release is refused, not a
		// handle over an unpinned generation.
		h.Release()
	}
	if m.InUse(81) {
		t.Fatal("InUse(81) = true after every holder released")
	}
	if got := lease.Holders(); got != 0 {
		t.Fatalf("Holders() = %d after every holder released, want 0", got)
	}
}

// TestViewHandoffKeepsTheStackReadableAfterClose is the view-level proof the
// plan asks for: a detached worker that reads a GenerationSource.Handle after
// the request's view was closed must observe a live pin. Without the handoff
// the retirement sweep collects the generation under it.
func TestViewHandoffKeepsTheStackReadableAfterClose(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "view-handoff")
	commit, dirty := seedRoutedStack(t, store)

	materializer := newTestMaterializer(store)
	view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}

	handoff := view.Handoff()
	if handoff == nil {
		t.Fatal("RepoView.Handoff() = nil on a live view")
	}
	if !handoff.ID.Equal(view.ID) {
		t.Fatalf("handoff ID = %v, want the view's %v", handoff.ID, view.ID)
	}
	if !maps.Equal(handoff.Completeness, view.Completeness) {
		t.Fatalf("handoff Completeness = %v, want the view's %v", handoff.Completeness, view.Completeness)
	}
	if got, want := handoff.Generations(), view.Generations(); !slicesEqualInt64(got, want) {
		t.Fatalf("handoff Generations() = %v, want the view's %v", got, want)
	}
	if got, want := len(handoff.GenerationSources()), len(view.GenerationSources()); got != want {
		t.Fatalf("handoff GenerationSources() has %d entries, want the view's %d", got, want)
	}

	// The handler returns.
	view.Close()
	view.Close()

	for _, id := range []int64{commit, dirty} {
		if !materializer.Leases.InUse(id) {
			t.Fatalf("generation %d is unpinned while a handed-off consumer is live", id)
		}
	}
	// Un-route the working-tree generation so only the lease can refuse the
	// retire, then prove the sweep is refused.
	unrouteDirty(t, store)
	if err := store.RetirePayloadGeneration(ctx, dirty, materializer.Leases.InUse); !errors.Is(err, store_sqlite.ErrPayloadGenerationInUse) {
		t.Fatalf("retire while the handoff is open = %v, want %v", err, store_sqlite.ErrPayloadGenerationInUse)
	}
	bounded, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := materializer.Leases.WaitDrain(bounded, handoff.Generations()...); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitDrain while the handoff is open = %v, want %v", err, context.DeadlineExceeded)
	}

	// The detached worker reads through both doors after the handler returned.
	// Keeper is the dirty layer's own row (line 2), so serving it proves the
	// whole composed stack is still readable, not just the corpus underneath.
	if got := handoff.Reader.GetNode(stackKeeperID); got == nil || got.StartLine != 2 {
		t.Fatalf("handoff Reader.GetNode(%s) = %v after the view closed, want the dirty row", stackKeeperID, got)
	}
	sources := handoff.GenerationSources()
	if len(sources) == 0 {
		t.Fatal("handoff GenerationSources() is empty")
	}
	found := false
	for _, source := range sources {
		if source.Handle == nil {
			t.Fatalf("generation %d source has no handle", source.Generation)
		}
		if source.Handle.GetNode(stackFreshID) != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("no generation source served %s after the view closed", stackFreshID)
	}

	handoff.Close()
	handoff.Close()
	if err := materializer.Leases.WaitDrain(ctx, commit, dirty); err != nil {
		t.Fatalf("WaitDrain after the handoff closed: %v", err)
	}
	if err := store.RetirePayloadGeneration(ctx, dirty, materializer.Leases.InUse); err != nil {
		t.Fatalf("retire after the handoff closed: %v", err)
	}
}

// TestViewHandoffRefusedAfterTheStackDrained keeps the view-level refusal
// truthful: once the pins are gone there is no view left to hand off.
func TestViewHandoffRefusedAfterTheStackDrained(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "view-handoff-refused")
	commit, _ := seedRoutedStack(t, store)

	materializer := newTestMaterializer(store)
	view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	view.Close()
	if materializer.Leases.InUse(commit) {
		t.Fatal("the commit generation is still pinned after Close with no handoff")
	}
	if got := view.Handoff(); got != nil {
		t.Fatal("RepoView.Handoff() returned a handle after the view drained")
	}
	if materializer.Leases.InUse(commit) {
		t.Fatal("a refused Handoff re-pinned the commit generation")
	}

	var nilView *RepoView
	if got := nilView.Handoff(); got != nil {
		t.Error("nil RepoView Handoff() returned a handle")
	}
	var nilHandoff *ViewHandoff
	nilHandoff.Close()
	if got := nilHandoff.Generations(); got != nil {
		t.Errorf("nil ViewHandoff Generations() = %v, want nil", got)
	}
	if got := nilHandoff.GenerationSources(); got != nil {
		t.Errorf("nil ViewHandoff GenerationSources() = %v, want nil", got)
	}
}

// TestViewHandoffSurvivesAConcurrentDetachedWorker runs the whole shape once
// more with real goroutines so -race covers the view-level handle too.
func TestViewHandoffSurvivesAConcurrentDetachedWorker(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "view-handoff-race")
	commit, dirty := seedRoutedStack(t, store)

	materializer := newTestMaterializer(store)
	view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	handoff := view.Handoff()
	if handoff == nil {
		t.Fatal("RepoView.Handoff() = nil on a live view")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer handoff.Close()
		close(started)
		<-release
		for _, source := range handoff.GenerationSources() {
			source.Handle.GetNode(stackFreshID)
		}
		if handoff.Reader.GetNode(stackKeeperID) == nil {
			t.Errorf("detached worker read %s = nil", stackKeeperID)
		}
	}()

	<-started
	view.Close()
	for _, id := range []int64{commit, dirty} {
		if !materializer.Leases.InUse(id) {
			t.Fatalf("generation %d is unpinned while the detached worker runs", id)
		}
	}
	close(release)
	<-done
	if err := materializer.Leases.WaitDrain(ctx, commit, dirty); err != nil {
		t.Fatalf("WaitDrain after the detached worker finished: %v", err)
	}
}
