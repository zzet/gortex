package indexer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// The warmup gate.
//
// A restart is the case these tests are about. The graph is fully persisted,
// the catalog remembers every checkout and every ref view, and all of it wants
// to build at the moment the warmup tail is re-resolving the graph through the
// same writer. What the gate has to give is a shape, not a delay: build work
// waits, everything else — registering a checkout, reading a route, serving a
// generation that is already published — carries on, and the work that waited
// starts on its own when the warmup says so.

// TestViewBuildGateAdmitsWhatItWasNeverGiven pins the default every surface
// without a warmup runs on — an embedded server, a CLI pass, a test — and the
// idempotence the daemon's single Open relies on.
func TestViewBuildGateAdmitsWhatItWasNeverGiven(t *testing.T) {
	var absent *ViewBuildGate
	if !absent.Admitted() {
		t.Fatal("a missing gate held a build back")
	}
	select {
	case <-absent.Opened():
	default:
		t.Fatal("a missing gate has something to wait for")
	}
	absent.Open()

	gate := NewViewBuildGate()
	if gate.Admitted() {
		t.Fatal("a new gate admitted a build before warmup finished")
	}
	select {
	case <-gate.Opened():
		t.Fatal("a closed gate reported itself open")
	default:
	}

	gate.Open()
	gate.Open()
	if !gate.Admitted() {
		t.Fatal("an opened gate still holds builds")
	}
	<-gate.Opened()
}

// TestCoordinatorDefersItsCycleUntilBuildsAreAdmitted is the coordinator half.
//
// A signal during warmup is a real state change and must not be dropped, so
// the claim is in two parts: while the gate is closed the window fires and no
// build runs, and when the gate opens the cycle the window claimed runs —
// without a second signal, because nothing is going to send one.
func TestCoordinatorDefersItsCycleUntilBuildsAreAdmitted(t *testing.T) {
	f := newCoordinatorFixture(t)

	synctest.Test(t, func(t *testing.T) {
		// Built inside the bubble: the loop selects on the gate's channel, and
		// a channel from outside the bubble is never durably blocking there.
		gate := NewViewBuildGate()
		var mu sync.Mutex
		var cycles []CheckoutCycle
		c := f.coordinator(t, CheckoutCoordinatorConfig{
			Debounce: 300 * time.Millisecond,
			Gate:     gate,
			cycleDone: func(cycle CheckoutCycle) {
				mu.Lock()
				cycles = append(cycles, cycle)
				mu.Unlock()
			},
		})

		c.Signal("checkout registered")
		time.Sleep(400 * time.Millisecond)
		synctest.Wait()

		mu.Lock()
		held := append([]CheckoutCycle(nil), cycles...)
		mu.Unlock()
		if len(held) != 1 || !held[0].Deferred {
			t.Fatalf("cycles while the gate held builds = %+v, want one deferred cycle", held)
		}
		if held[0].CommitBuilt || held[0].DirtyBuilt || held[0].CommitGenerationID != 0 {
			t.Fatalf("a deferred cycle built something: %+v", held[0])
		}
		if generations := f.generations(); len(generations) != 0 {
			t.Fatalf("%d generations were built during warmup: %+v", len(generations), generations)
		}

		// Nothing signals the coordinator again: the gate opening is the whole
		// of the wake, exactly as the daemon's warmup leaves it.
		gate.Open()
		synctest.Wait()

		mu.Lock()
		defer mu.Unlock()
		if len(cycles) != 2 {
			t.Fatalf("%d cycles after builds were admitted, want the deferred one to have run: %+v",
				len(cycles), cycles)
		}
		ran := cycles[1]
		if ran.Err != nil {
			t.Fatalf("the admitted cycle failed: %v", ran.Err)
		}
		if !ran.CommitBuilt || !ran.DirtyBuilt {
			t.Fatalf("the admitted cycle built commit=%v dirty=%v, want both", ran.CommitBuilt, ran.DirtyBuilt)
		}
		if route := f.route(); route.State != store_sqlite.RouteActive ||
			route.CommitGenerationID != ran.CommitGenerationID ||
			route.DirtyGenerationID != ran.DirtyGenerationID {
			t.Fatalf("route = %+v, want it serving what the admitted cycle built: %+v", route, ran)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestCoordinatorServesItsPublishedRouteWhileBuildsAreDeferred is the other
// half of the same claim: what the last run published is what a warming daemon
// serves.
//
// The route, its two generations and the corpus under them are all durable, so
// deferring build work costs a restart nothing a reader can see. The gated
// coordinator must leave every one of them exactly where it found them, and
// the view materialized while it is holding still has to agree with a flat
// index of the working tree it describes.
func TestCoordinatorServesItsPublishedRouteWhileBuildsAreDeferred(t *testing.T) {
	f := newCoordinatorFixture(t)
	published := coordinatorReconcile(t, f.inertCoordinator(t, CheckoutCoordinatorConfig{}))
	if published.CommitGenerationID == 0 || published.DirtyGenerationID == 0 {
		t.Fatalf("the run before the restart did not publish a route: %+v", published)
	}
	before := f.route()

	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		var mu sync.Mutex
		var cycles []CheckoutCycle
		c := f.coordinator(t, CheckoutCoordinatorConfig{
			Debounce: 300 * time.Millisecond,
			Gate:     gate,
			cycleDone: func(cycle CheckoutCycle) {
				mu.Lock()
				cycles = append(cycles, cycle)
				mu.Unlock()
			},
		})
		c.Signal("checkout registered")
		time.Sleep(400 * time.Millisecond)
		synctest.Wait()

		mu.Lock()
		held := append([]CheckoutCycle(nil), cycles...)
		mu.Unlock()
		if len(held) != 1 || !held[0].Deferred {
			t.Fatalf("cycles while the gate held builds = %+v, want one deferred cycle", held)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	after := f.route()
	if after != before {
		t.Fatalf("route = %+v, want the published one untouched: %+v", after, before)
	}
	if generations := f.generations(); len(generations) != 2 {
		t.Fatalf("%d generations, want only the two the published route names: %+v",
			len(generations), generations)
	}

	materializer := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
	view, err := materializer.MaterializeCheckout(context.Background(), f.checkoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout while builds are deferred: %v", err)
	}
	defer view.Close()

	flat := builderOpenStore(t, "flat-deferred")
	builderIndex(t, flat, f.worktree)
	builderAssertReadersAgree(t, view.Reader, flat)
}

// TestLifecycleRegistersCheckoutsWhileBuildsAreDeferred pins what the gate
// must NOT reach. Registration is how a checkout gets an identity, a route row
// and a coordinator to signal, and a daemon that deferred any of that would
// come back with no idea what it serves — the deferral is over build passes
// alone.
func TestLifecycleRegistersCheckoutsWhileBuildsAreDeferred(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	f.lc.SetBuildGate(NewViewBuildGate())

	main := f.gitRepo("gated-main")
	f.worktreeOf(main, "gated-wt")

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "gated-main"}, TrackSourceCLI)
	if err != nil || tracked.CatalogErr != nil {
		t.Fatalf("register the primary while builds are deferred: %v / %v", err, tracked.CatalogErr)
	}
	report, err := f.lc.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Coordinators != 0 {
		t.Fatalf("%d coordinators at startup while builds are deferred, want the dormant worktree's 0", report.Coordinators)
	}

	checkouts, err := f.catalog.ListCheckouts(ctx, tracked.FamilyID)
	if err != nil {
		t.Fatalf("list checkouts: %v", err)
	}
	var automatic *store_sqlite.Checkout
	for i := range checkouts {
		if checkouts[i].CheckoutID != tracked.CheckoutID {
			automatic = &checkouts[i]
		}
	}
	if automatic == nil {
		t.Fatal("the observed worktree got no catalog identity while builds were deferred")
	}
	if automatic.State != store_sqlite.CheckoutStateReady {
		t.Fatalf("the observed worktree is %q, want a ready identity", automatic.State)
	}
	// The deferral is over build passes, not over the coordinator that runs
	// them: selecting the worktree registers a coordinator whose build waits on
	// the gate, so the signal listener comes up even while the gate is closed.
	f.activateAndWait(automatic.CheckoutID)
	if !f.lc.SignalCheckout(automatic.CheckoutID, "test") {
		t.Fatal("the automatic checkout has no coordinator listening for signals")
	}
}

// TestRefViewDefersItsBuildUntilBuildsAreAdmitted is the ref-view half.
//
// A selection during warmup still claims its build — that claim is the token
// every later selection coalesces onto — and answers with it, so the caller
// polls one build rather than starting a second. What waits is the pass, and
// it runs when the gate opens: nobody selects again, and the view publishes
// anyway.
func TestRefViewDefersItsBuildUntilBuildsAreAdmitted(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, treeB := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	viewID := f.viewID("refs/heads/feature")

	gate := NewViewBuildGate()
	var builds atomic.Int64
	manager := f.managerTuned(t, func() { builds.Add(1) },
		func(cfg *RefViewManagerConfig) { cfg.Gate = gate })
	ctx := context.Background()

	deferred, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection while builds are deferred: %v", err)
	}
	if deferred.State != store_sqlite.RefViewBuilding || deferred.Built {
		t.Fatalf("selection = %+v, want a building answer from a pass that has not run", deferred)
	}
	if deferred.BuildToken == "" {
		t.Fatal("a deferred selection named no build to poll")
	}
	if n := builds.Load(); n != 0 {
		t.Fatalf("%d build passes ran while the gate held them", n)
	}
	if generations := f.generations(); len(generations) != 0 {
		t.Fatalf("%d generations were built during warmup: %+v", len(generations), generations)
	}
	rows := f.builds(viewID)
	if len(rows) != 1 || rows[0].State != store_sqlite.ViewGenerationBuilding {
		t.Fatalf("build rows = %+v, want the claim the deferred pass holds", rows)
	}
	if rows[0].BuildToken != deferred.BuildToken {
		t.Fatalf("selection named token %q, want the claimed build's %q",
			deferred.BuildToken, rows[0].BuildToken)
	}

	// Nothing selects again: the gate opening is the whole of the wake.
	gate.Open()
	f.awaitBuildState(viewID, store_sqlite.ViewGenerationReady)

	view := f.view(viewID)
	if view.State != store_sqlite.RefViewReady || view.ActiveGenerationID == 0 {
		t.Fatalf("ref view = %+v, want it serving what the deferred build published", view)
	}
	if view.ActiveTree != treeB {
		t.Fatalf("ref view serves tree %s, want the selector's %s", view.ActiveTree, treeB)
	}

	next, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selection after the deferred build published: %v", err)
	}
	if next.State != store_sqlite.RefViewReady || next.Built {
		t.Fatalf("selection = %+v, want the deferred build's view served as it stands", next)
	}
	if next.GenerationID != view.ActiveGenerationID {
		t.Fatalf("selection serves generation %d, want the published %d",
			next.GenerationID, view.ActiveGenerationID)
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d build passes ran, want the one the deferred selection claimed", n)
	}

	// Serving a published generation is not build work. A manager under a gate
	// of its own — the next restart, mid-warmup — answers the same selector
	// from what is already stored, without claiming a build at all.
	warming := f.managerTuned(t, nil, func(cfg *RefViewManagerConfig) { cfg.Gate = NewViewBuildGate() })
	served, err := warming.EnsureRefView(ctx, f.request("refs/heads/feature"))
	if err != nil {
		t.Fatalf("selecting a published view while builds are deferred: %v", err)
	}
	if served.State != store_sqlite.RefViewReady || served.Built {
		t.Fatalf("selection = %+v, want the published view served during warmup", served)
	}
	if served.GenerationID != view.ActiveGenerationID {
		t.Fatalf("a warming selection serves generation %d, want the published %d",
			served.GenerationID, view.ActiveGenerationID)
	}
	if rows := f.builds(viewID); len(rows) != 1 {
		t.Fatalf("build rows = %+v, want the single deferred attempt", rows)
	}
}

type viewBuildAcquireResult struct {
	release func()
	err     error
}

func acquireViewBuildAsync(ctx context.Context, gate *ViewBuildGate, priority ViewBuildPriority) <-chan viewBuildAcquireResult {
	result := make(chan viewBuildAcquireResult, 1)
	go func() {
		release, err := gate.Acquire(ctx, priority)
		result <- viewBuildAcquireResult{release: release, err: err}
	}()
	return result
}

// TestViewBuildGateWarmupAndCapacity pins the two independent responsibilities
// of the gate: Open is a one-way warmup latch, while Acquire remains a
// single-build lane for the rest of the daemon's lifetime.
func TestViewBuildGateWarmupAndCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		firstResult := acquireViewBuildAsync(context.Background(), gate, ViewBuildBackground)
		synctest.Wait()
		select {
		case got := <-firstResult:
			if got.release != nil {
				got.release()
			}
			t.Fatalf("Acquire crossed the warmup latch before Open: err=%v", got.err)
		default:
		}

		gate.Open()
		synctest.Wait()
		first := <-firstResult
		if first.err != nil {
			t.Fatalf("first Acquire after Open: %v", first.err)
		}

		secondResult := acquireViewBuildAsync(context.Background(), gate, ViewBuildBackground)
		synctest.Wait()
		select {
		case got := <-secondResult:
			if got.release != nil {
				got.release()
			}
			t.Fatalf("second build entered the active lane: err=%v", got.err)
		default:
		}

		first.release()
		synctest.Wait()
		second := <-secondResult
		if second.err != nil {
			t.Fatalf("second Acquire after release: %v", second.err)
		}
		second.release()
		second.release() // A caller may safely defer and explicitly release.

		third, err := gate.Acquire(context.Background(), ViewBuildBackground)
		if err != nil {
			t.Fatalf("Acquire after idempotent release: %v", err)
		}
		third()
	})
}

// TestViewBuildGatePrefersInteractiveWaiters proves that priority applies only
// to queued work: the active background build finishes, then the interactive
// ref request overtakes a background refresh that was already waiting.
func TestViewBuildGatePrefersInteractiveWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		gate.Open()
		active, err := gate.Acquire(context.Background(), ViewBuildBackground)
		if err != nil {
			t.Fatalf("active Acquire: %v", err)
		}

		backgroundResult := acquireViewBuildAsync(context.Background(), gate, ViewBuildBackground)
		synctest.Wait()
		interactiveResult := acquireViewBuildAsync(context.Background(), gate, ViewBuildInteractive)
		synctest.Wait()

		active()
		synctest.Wait()
		var interactive viewBuildAcquireResult
		select {
		case got := <-backgroundResult:
			if got.release != nil {
				got.release()
			}
			t.Fatalf("background waiter overtook interactive waiter: err=%v", got.err)
		case interactive = <-interactiveResult:
		}
		if interactive.err != nil {
			t.Fatalf("interactive Acquire: %v", interactive.err)
		}
		select {
		case got := <-backgroundResult:
			if got.release != nil {
				got.release()
			}
			t.Fatalf("background waiter entered beside interactive waiter: err=%v", got.err)
		default:
		}

		interactive.release()
		synctest.Wait()
		background := <-backgroundResult
		if background.err != nil {
			t.Fatalf("background Acquire after interactive release: %v", background.err)
		}
		background.release()
	})
}

// TestViewBuildGateSkipsCanceledWaiters verifies that cancellation removes a
// queued claim logically: it returns context.Canceled and cannot consume the
// lane when the active build later releases it.
func TestViewBuildGateSkipsCanceledWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		gate.Open()
		active, err := gate.Acquire(context.Background(), ViewBuildBackground)
		if err != nil {
			t.Fatalf("active Acquire: %v", err)
		}

		canceledCtx, cancel := context.WithCancel(context.Background())
		canceledResult := acquireViewBuildAsync(canceledCtx, gate, ViewBuildInteractive)
		synctest.Wait()
		cancel()
		synctest.Wait()
		canceled := <-canceledResult
		if canceled.err != context.Canceled {
			t.Fatalf("canceled Acquire error = %v, want context.Canceled", canceled.err)
		}
		if canceled.release != nil {
			t.Fatal("canceled Acquire returned a release function")
		}
		gate.mu.Lock()
		queued := len(gate.interactive) + len(gate.background)
		gate.mu.Unlock()
		if queued != 0 {
			t.Fatalf("canceled waiter remains in the admission queues: %d", queued)
		}

		survivorResult := acquireViewBuildAsync(context.Background(), gate, ViewBuildBackground)
		synctest.Wait()
		active()
		synctest.Wait()
		survivor := <-survivorResult
		if survivor.err != nil {
			t.Fatalf("Acquire behind canceled waiter: %v", survivor.err)
		}
		survivor.release()
	})
}

// TestViewBuildGateBoundsInteractiveOvertaking proves that priority is
// responsive rather than absolute: a continuously arriving interactive stream
// cannot leave an already queued checkout refresh behind forever.
func TestViewBuildGateBoundsInteractiveOvertaking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		gate.Open()
		active, err := gate.Acquire(context.Background(), ViewBuildBackground)
		if err != nil {
			t.Fatalf("active Acquire: %v", err)
		}

		type orderedGrant struct {
			priority ViewBuildPriority
			release  func()
			err      error
		}
		grants := make(chan orderedGrant, maxInteractiveBuildBurst+2)
		queue := func(priority ViewBuildPriority) {
			go func() {
				release, err := gate.Acquire(context.Background(), priority)
				grants <- orderedGrant{priority: priority, release: release, err: err}
			}()
			synctest.Wait()
		}

		queue(ViewBuildBackground)
		for range maxInteractiveBuildBurst + 1 {
			queue(ViewBuildInteractive)
		}
		active()

		got := make([]ViewBuildPriority, 0, maxInteractiveBuildBurst+2)
		for range maxInteractiveBuildBurst + 2 {
			synctest.Wait()
			grant := <-grants
			if grant.err != nil {
				t.Fatalf("queued Acquire: %v", grant.err)
			}
			got = append(got, grant.priority)
			grant.release()
		}
		for i := range maxInteractiveBuildBurst {
			if got[i] != ViewBuildInteractive {
				t.Fatalf("grant %d = %v, want interactive; order=%v", i, got[i], got)
			}
		}
		if got[maxInteractiveBuildBurst] != ViewBuildBackground {
			t.Fatalf("background grant remained starved after %d interactive grants: %v",
				maxInteractiveBuildBurst, got)
		}
	})
}

func BenchmarkViewBuildGateHandoff(b *testing.B) {
	gate := NewViewBuildGate()
	gate.Open()
	ctx := context.Background()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			release, err := gate.Acquire(ctx, ViewBuildBackground)
			if err != nil {
				b.Fatal(err)
			}
			release()
		}
	})
}

func BenchmarkViewBuildGateCanceledAdmission(b *testing.B) {
	gate := NewViewBuildGate()
	gate.Open()
	b.ReportAllocs()
	for b.Loop() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := gate.Acquire(ctx, ViewBuildBackground); err != context.Canceled {
			b.Fatalf("Acquire error = %v, want context.Canceled", err)
		}
	}
}

func BenchmarkCheckoutCoordinatorSettledCycle(b *testing.B) {
	gate := NewViewBuildGate() // Deliberately closed: settled cycles must not acquire it.
	c := &CheckoutCoordinator{
		gate: gate,
		cyclePreflight: func(context.Context) (CheckoutCycle, bool) {
			return CheckoutCycle{CommitGenerationID: 1, DirtyGenerationID: 2}, true
		},
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		c.cycle(ctx)
	}
}
