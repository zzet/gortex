package indexer

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A checkout whose every build is torn by the moving tree publishes nothing.
// From the third such cycle in a row the run is a stall: the status census and
// the require_fresh rider's lookup name it, with its reason, start, the age of
// the last publication and the change set. A cycle that publishes ends it.
func TestThreeNonpublishingCyclesReportAPublicationStall(t *testing.T) {
	f := newCoordinatorFixture(t)
	var moving atomic.Bool
	var edits atomic.Int32
	cycles := make(chan CheckoutCycle, 64)
	c := f.coordinator(t, CheckoutCoordinatorConfig{
		PollInterval: -1,
		cycleDone:    func(out CheckoutCycle) { cycles <- out },
		// Every build attempt sees the tree move under it while moving is set.
		dirtyBarrier: func() {
			if moving.Load() {
				builderWriteFile(t, f.worktree, "churn.go", fmt.Sprintf("package fixture\n\n// edit %d\n", edits.Add(1)))
			}
		},
	})
	lifecycle := &CheckoutLifecycle{catalog: f.catalog, store: f.store, leases: f.leases,
		coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	next := func() CheckoutCycle {
		t.Helper()
		select {
		case out := <-cycles:
			return out
		case <-time.After(60 * time.Second):
			t.Fatal("no cycle ran")
			return CheckoutCycle{}
		}
	}

	c.Signal("initial")
	if out := next(); out.Err != nil || out.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle settled no working-tree layer: %+v", out)
	}
	published := time.Now()
	if stalls := lifecycle.PublicationStalls(); len(stalls) != 0 {
		t.Fatalf("a settled checkout reports a stall: %+v", stalls)
	}

	// Each torn cycle signals the next itself.
	moving.Store(true)
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() { _ = 1 }\n")
	c.Signal("save")
	for i := 1; i <= checkoutPublicationStallThreshold; i++ {
		if out := next(); !out.Rescheduled || out.Err != nil {
			t.Fatalf("cycle %d = %+v, want two torn builds rescheduled", i, out)
		}
		_, stalled := lifecycle.CheckoutPublicationStalled(f.checkoutID)
		if want := i >= checkoutPublicationStallThreshold; stalled != want {
			t.Fatalf("after %d torn cycles the rider's stall is %v, want %v", i, stalled, want)
		}
	}
	stall, ok := lifecycle.CheckoutPublicationStalled(f.checkoutID)
	if !ok {
		t.Fatal("no stall reported after three torn cycles")
	}
	if stall.CheckoutID != f.checkoutID || stall.ConsecutiveNonpublishingCycles != checkoutPublicationStallThreshold ||
		stall.StallReason != stallReasonTornByMotion || stall.Since < published.Unix()-1 ||
		stall.LastPublicationAgeSeconds < 0 || stall.LastPublicationAgeSeconds > int64(time.Since(published)/time.Second)+1 ||
		stall.ChangeSetSize == 0 {
		t.Fatalf("stall = %+v", stall)
	}
	health, err := lifecycle.ViewsHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(health.PublicationStalls) != 1 || health.PublicationStalls[0].CheckoutID != f.checkoutID ||
		health.PublicationStalls[0].ConsecutiveNonpublishingCycles < checkoutPublicationStallThreshold {
		t.Fatalf("status census stalls = %+v", health.PublicationStalls)
	}

	// The tree settles: the next self-signalled cycle publishes and ends the run.
	moving.Store(false)
	for {
		out := next()
		if out.Err == nil && out.DirtyBuilt {
			break
		}
		if !out.Rescheduled {
			t.Fatalf("cycle = %+v, want torn or published", out)
		}
	}
	if stalls := lifecycle.PublicationStalls(); len(stalls) != 0 {
		t.Fatalf("a publication left the stall standing: %+v", stalls)
	}
}

// The reconcile-failed warning names what admitted the cycle (reason) apart
// from what failed it (cause), and the failure starts a stall run of its own.
func TestReconcileFailedLogCarriesItsCause(t *testing.T) {
	root, sampler := motionRepo(t)
	gate := NewViewBuildGate()
	gate.Open()
	core, logs := observer.New(zap.InfoLevel)
	c, outcomes := startMotionCycle(t, gate, root, sampler, func(c *CheckoutCoordinator) {
		c.logger = zap.New(core)
		c.Signal("poll")
	}, func(context.Context) {})
	out := awaitMotionOutcome(t, outcomes)
	if out.Err == nil {
		t.Fatalf("cycle = %+v, want it failed", out)
	}
	entries := logs.FilterMessage("checkout coordinator: reconcile failed").All()
	if len(entries) != 1 {
		t.Fatalf("reconcile-failed lines = %d, want 1", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["cause"] != checkoutCycleFailureClass(out.Err) || fields["cause"] == "" {
		t.Fatalf("cause = %v, want the class of %v", fields["cause"], out.Err)
	}
	if _, ok := fields["reason"]; !ok {
		t.Fatalf("the admission reason is missing: %v", fields)
	}
	stall, ok := c.publicationStall(time.Now())
	if !ok || stall.StallReason != stallReasonFailedPrefix+checkoutCycleFailureClass(out.Err) || stall.ConsecutiveNonpublishingCycles != 1 {
		t.Fatalf("stall = %+v (%v), want one failed cycle", stall, ok)
	}
}

func TestCheckoutCycleFailureClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("indexer: per-file delta pass: %w", store_sqlite.ErrCatalogStaleGuard), "catalog_guard"},
		{&DirtySnapshotChangedError{GenerationID: 4}, "working_tree_moved"},
		{errCheckoutUnsettled, "working_tree_moved"},
		{fmt.Errorf("flip: %w", errRouteMoved), "route_moved"},
		{errBaseMoved, "base_moved"},
		{context.Canceled, "canceled"},
		{fmt.Errorf("disk full"), "other"},
	} {
		if got := checkoutCycleFailureClass(tc.err); got != tc.want {
			t.Errorf("class(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// An import of a large working tree routes one batch per cycle and reschedules
// the next: progress, not a stall. Every batch ends a run that was already
// standing and stamps the last publication, so neither the status census nor
// the require_fresh rider reports a stall while the import advances.
func TestAnImportInBatchesIsNotAPublicationStall(t *testing.T) {
	old := importInteractivePaths
	importInteractivePaths = 2
	t.Cleanup(func() { importInteractivePaths = old })
	f := newCoordinatorFixtureWithTree(t, retentionTree())
	// Each cycle's run, read on the loop right after the cycle folded into it.
	type folded struct {
		out         CheckoutCycle
		consecutive int
		last        time.Time
	}
	cycles := make(chan folded, 64)
	var c *CheckoutCoordinator
	c = f.coordinator(t, CheckoutCoordinatorConfig{
		PollInterval: -1,
		cycleDone: func(out CheckoutCycle) {
			c.publication.mu.Lock()
			run := folded{out: out, consecutive: c.publication.consecutive, last: c.publication.lastPublished}
			c.publication.mu.Unlock()
			cycles <- run
		},
	})
	lifecycle := &CheckoutLifecycle{catalog: f.catalog, store: f.store, leases: f.leases,
		coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	next := func() folded {
		t.Helper()
		select {
		case run := <-cycles:
			return run
		case <-time.After(60 * time.Second):
			t.Fatal("no cycle ran")
			return folded{}
		}
	}
	c.Signal("initial")
	first := next()
	if first.out.Err != nil || first.out.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle settled no working-tree layer: %+v", first.out)
	}

	// A run already standing when the import starts: the first batch ends it.
	for i := 0; i < checkoutPublicationStallThreshold; i++ {
		c.notePublicationOutcome(CheckoutCycle{Rescheduled: true, rescheduledBy: stallReasonTornByMotion}, time.Now())
	}
	if _, stalled := lifecycle.CheckoutPublicationStalled(f.checkoutID); !stalled {
		t.Fatal("the seeded run is not a stall")
	}

	const units = 3 // six paths, past importInteractivePaths
	for i := 0; i < units; i++ {
		for _, layout := range []accumulatedDirtyLayout{accumulatedDirtyIndependent, accumulatedDirtySamePackage} {
			accumulatedDirtyWriteUnit(t, f.worktree, layout, i, true, false)
		}
	}
	c.Signal("save")
	batches := 0
	previous := first.last
	for {
		run := next()
		out := run.out
		if out.Err != nil {
			t.Fatalf("an import cycle failed: %v", out.Err)
		}
		if !out.DirtyBuilt {
			continue // held while the writes settle, or stepped aside
		}
		if out.DirtyBatchRemaining > 0 {
			batches++
			if !out.Rescheduled {
				t.Fatalf("batch %d is not rescheduled: %+v", batches, out)
			}
		}
		if run.consecutive != 0 {
			t.Fatalf("after batch %d (remaining %d) the run counts %d non-publishing cycles", batches, out.DirtyBatchRemaining, run.consecutive)
		}
		if !run.last.After(previous) {
			t.Fatalf("batch %d did not stamp the last publication: %v, previous %v", batches, run.last, previous)
		}
		previous = run.last
		if out.DirtyBatchRemaining == 0 {
			break
		}
	}
	if batches < checkoutPublicationStallThreshold {
		t.Fatalf("the import ran %d batches, want at least %d to cover a stall's threshold", batches, checkoutPublicationStallThreshold)
	}
	if stalls := lifecycle.PublicationStalls(); len(stalls) != 0 {
		t.Fatalf("after the import the census reports a stall: %+v", stalls)
	}
}

// Each way a cycle can end, folded into the run: what counts, under which
// reason, what ends the run, and what leaves it alone.
func TestPublicationOutcomeClassifiesEveryCycleEnding(t *testing.T) {
	failed := fmt.Errorf("indexer: per-file delta pass: %w", store_sqlite.ErrCatalogStaleGuard)
	for _, tc := range []struct {
		name   string
		out    CheckoutCycle
		reason string // "" with ends: the run is over; "" without: untouched
		ends   bool
	}{
		{"failed", CheckoutCycle{Err: failed}, "failed:catalog_guard", false},
		{"abandoned for a moving tree", CheckoutCycle{Rescheduled: true, YieldedTo: treeMovedReason}, stallReasonTornByMotion, false},
		{"torn twice", CheckoutCycle{Rescheduled: true, rescheduledBy: stallReasonTornByMotion}, stallReasonTornByMotion, false},
		{"lost route flip", CheckoutCycle{Rescheduled: true, rescheduledBy: stallReasonRouteMoved}, stallReasonRouteMoved, false},
		{"base moved", CheckoutCycle{Rescheduled: true, rescheduledBy: stallReasonBaseMoved}, stallReasonBaseMoved, false},
		{"HEAD moved", CheckoutCycle{Rescheduled: true, rescheduledBy: stallReasonHeadMoved}, stallReasonHeadMoved, false},
		{"one import batch", CheckoutCycle{Rescheduled: true, DirtyBuilt: true, DirtyGenerationID: 9, DirtyBatchRemaining: 4}, "", true},
		{"published", CheckoutCycle{DirtyBuilt: true, DirtyGenerationID: 9}, "", true},
		{"settled no-op", CheckoutCycle{DirtyGenerationID: 9}, "", true},
		{"yielded to an interactive build", CheckoutCycle{Rescheduled: true, YieldedTo: laneYieldedToInteractive}, "", false},
		{"held", CheckoutCycle{Rescheduled: true, Held: true}, "", false},
		{"deferred", CheckoutCycle{Deferred: true}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &CheckoutCoordinator{checkoutID: "co"}
			start := time.Unix(1_800_000_000, 0)
			c.notePublicationOutcome(CheckoutCycle{Err: failed}, start)
			c.notePublicationOutcome(tc.out, start.Add(time.Second))
			stall, ok := c.publicationStall(start.Add(2 * time.Second))
			switch {
			case tc.ends:
				if ok {
					t.Fatalf("the run is still standing: %+v", stall)
				}
				if got := c.publication.lastPublished; !got.Equal(start.Add(time.Second)) {
					t.Fatalf("last publication = %v, want the cycle's end", got)
				}
			case tc.reason == "":
				if !ok || stall.ConsecutiveNonpublishingCycles != 1 || stall.StallReason != "failed:catalog_guard" {
					t.Fatalf("stall = %+v (%v), want the seeded run untouched", stall, ok)
				}
			default:
				if !ok || stall.ConsecutiveNonpublishingCycles != 2 || stall.StallReason != tc.reason || stall.Since != start.Unix() {
					t.Fatalf("stall = %+v (%v), want a second cycle with reason %q", stall, ok, tc.reason)
				}
			}
		})
	}
}
