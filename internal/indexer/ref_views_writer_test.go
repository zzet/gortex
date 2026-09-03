package indexer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Writer contention is the one thing a ref-view selection must not inherit.
//
// The store's mutation gate is held for as long as a build's transactions
// run, and a build over a real tree runs for far longer than the request that
// started it may wait. Every selection of that view arrives while the gate is
// held, so every write a selection makes is a place in a queue behind the very
// pass it is trying to report on — and a request that queues there answers
// nothing at all, it just expires.
//
// The catalog's reads do not queue: the store keeps a separate read pool, so
// the view's row, its desire and the claim the build holds all answer while
// the writer is saturated. These tests hold the gate for real and pin what a
// selection does with it held: coalesce from the read pool alone, or say the
// store is busy inside its own budget.

// holdWriter takes the store's mutation gate for the rest of the test — the
// state a running build leaves it in. The release is registered as cleanup
// too, so a failing assertion cannot wedge the store's teardown.
func (f *refViewFixture) holdWriter() func() {
	f.t.Helper()
	release, err := f.store.HoldWriteGate(context.Background())
	if err != nil {
		f.t.Fatalf("hold the store's write gate: %v", err)
	}
	f.t.Cleanup(release)
	return release
}

// answerWithin runs one selection on its own goroutine and returns what it
// answered, failing the test if ctx expires first.
//
// The context is the assertion, not a timeout dressed up as one: every test
// here claims a selection answers WITHOUT waiting the writer out, and a
// context that expired is that claim failing. The goroutine exists so the
// failure is an assertion rather than a package-wide deadlock — a selection
// queued on the gate returns only once the gate is released.
func (f *refViewFixture) answerWithin(
	ctx context.Context,
	manager *RefViewManager,
	ref string,
	release func(),
) (RefViewResult, error) {
	f.t.Helper()
	type answer struct {
		result RefViewResult
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		result, err := manager.EnsureRefView(ctx, f.request(ref))
		answered <- answer{result: result, err: err}
	}()
	select {
	case got := <-answered:
		return got.result, got.err
	case <-ctx.Done():
		// Unwedge the queued selection before failing, so the goroutine is
		// not still holding the store open when the fixture tears it down.
		release()
		<-answered
		f.t.Fatal("the selection never answered while the writer was saturated")
		return RefViewResult{}, nil
	}
}

// refViewTreeC is a third tree, so a selection can be pointed at content
// neither the base corpus nor the view's published generation describes.
func refViewTreeC() map[string]string {
	tree := builderTreeB()
	tree["third.go"] = `package fixture

func Third() {
	Added()
}
`
	return tree
}

// TestRefViewCoalescingAnswerNeedsNoWriter is the bug this file exists for.
//
// A selection of a view whose build is in flight has nothing to write: the
// answer is the token of the attempt already running. It was nonetheless
// opening with an upsert on the view's row, so the cheapest answer in the
// whole surface queued behind the build it was about to report — and the
// request expired on its deadline instead of handing back a token to poll.
func TestRefViewCoalescingAnswerNeedsNoWriter(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	viewID := f.viewID("refs/heads/feature")

	parked, release := make(chan struct{}), make(chan struct{})
	var builds atomic.Int64
	manager := f.managerTuned(t, func() {
		if builds.Add(1) == 1 {
			close(parked)
			<-release
		}
	}, func(cfg *RefViewManagerConfig) {
		cfg.buildGrace = 20 * time.Millisecond
		// Well inside the selection's own context: a budget that expired
		// together with the request would make the two indistinguishable.
		cfg.writerBudget = 200 * time.Millisecond
	})

	first, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("first selection: %v", err)
	}
	if first.State != store_sqlite.RefViewBuilding || first.BuildToken == "" {
		t.Fatalf("first selection = %+v, want a building answer naming its build", first)
	}
	// Parked past its own writes, holding its claim: exactly what a build in
	// the middle of a long transaction looks like to everybody else.
	<-parked
	releaseWriter := f.holdWriter()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	second, err := f.answerWithin(ctx, manager, "refs/heads/feature", releaseWriter)
	if err != nil {
		t.Fatalf("second selection under a saturated writer: %v", err)
	}
	if second.State != store_sqlite.RefViewBuilding || second.Built {
		t.Fatalf("second selection = %+v, want a building answer it did not build", second)
	}
	if second.BuildToken != first.BuildToken {
		t.Fatalf("second selection named token %q, want the in-flight build's %q",
			second.BuildToken, first.BuildToken)
	}

	releaseWriter()
	close(release)
	f.awaitBuildState(viewID, store_sqlite.ViewGenerationReady)
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d build passes ran, want the one the first selection started", n)
	}
	if rows := f.builds(viewID); len(rows) != 1 {
		t.Fatalf("%d build rows, want the two selections to have shared one: %+v", len(rows), rows)
	}
}

// TestRefViewFreshClaimAnswersWhileTheWriterIsSaturated pins the other half: a
// selection that genuinely needs the writer — the ref moved, so the desire has
// to be re-stamped and a new build claimed — must still answer.
//
// It cannot answer with a view, and it does not pretend to. What it must not
// do is hold the request open for the whole pass in front of it: the caller
// retries either way, and a typed "the store is busy" inside a couple of
// seconds is an answer it can act on where a generic deadline is not.
func TestRefViewFreshClaimAnswersWhileTheWriterIsSaturated(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)

	manager := f.managerTuned(t, nil, func(cfg *RefViewManagerConfig) {
		cfg.writerBudget = 50 * time.Millisecond
	})
	first, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("first selection: %v", err)
	}
	if first.State != store_sqlite.RefViewReady || !first.Built {
		t.Fatalf("first selection = %+v, want a ready view it built itself", first)
	}

	// The ref moves to a tree the view has never served, so the selection has
	// real bookkeeping to do and no live build to coalesce onto.
	commitC, _ := f.commitTree(refViewTreeC(), "C")
	f.setRef("refs/heads/feature", commitC)

	releaseWriter := f.holdWriter()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := f.answerWithin(ctx, manager, "refs/heads/feature", releaseWriter)
	if !errors.Is(err, ErrRefViewStoreBusy) {
		t.Fatalf("selection under a saturated writer = (%+v, %v), want %v",
			result, err, ErrRefViewStoreBusy)
	}

	// The saturation was the whole of it: with the gate free the same
	// selection claims its build and runs it.
	releaseWriter()
	next, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection after the writer freed up: %v", err)
	}
	if next.State != store_sqlite.RefViewReady || !next.Built {
		t.Fatalf("selection = %+v, want a ready view it built itself", next)
	}
}

// TestRefViewUncontendedSelectionStillClaimsAndBuilds is the control arm for
// the two above: reading before writing must not swallow a selection that has
// work to do. With the writer free, a view nobody is building claims its
// build, runs it and adopts it — no busy answer, no coalescing onto a claim
// that does not exist.
func TestRefViewUncontendedSelectionStillClaimsAndBuilds(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, treeB := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	viewID := f.viewID("refs/heads/feature")

	manager := f.manager(t, nil)
	result, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection: %v", err)
	}
	if result.State != store_sqlite.RefViewReady || !result.Built || result.GenerationID == 0 {
		t.Fatalf("selection = %+v, want a ready view it built itself", result)
	}
	if result.Resolved.TreeOID != treeB {
		t.Fatalf("selection resolved %+v, want tree %s", result.Resolved, treeB)
	}
	rows := f.builds(viewID)
	if len(rows) != 1 || rows[0].State != store_sqlite.ViewGenerationReady {
		t.Fatalf("build rows = %+v, want the one attempt this selection finished", rows)
	}
	if rows[0].GenerationID != result.GenerationID {
		t.Fatalf("build row = %+v, want it closed on generation %d", rows[0], result.GenerationID)
	}
	if view := f.view(viewID); view.ActiveGenerationID != result.GenerationID {
		t.Fatalf("ref view = %+v, want it serving generation %d", view, result.GenerationID)
	}
}
