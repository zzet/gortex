package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestViewBuildGateRequiredPhaseFencesNonRequiredWork(t *testing.T) {
	gate := newViewBuildGateWithAllLimits(2, 2, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	interactive := acquireViewBuildAsync(ctx, gate, ViewBuildInteractive)
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.InteractiveQueued == 1
	})
	background := acquireViewBuildAsync(ctx, gate, ViewBuildBackground)
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.BackgroundQueued == 1
	})
	required := acquireViewBuildAsync(ctx, gate, ViewBuildRequired)
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.RequiredQueued == 1
	})

	gate.OpenRequired()
	requiredGrant := <-required
	if requiredGrant.err != nil {
		t.Fatalf("required Acquire after OpenRequired: %v", requiredGrant.err)
	}
	stats := gate.Stats()
	if stats.Open || !stats.RequiredOpen || !stats.Active {
		t.Fatalf("required-only gate state = %+v", stats)
	}
	select {
	case got := <-interactive:
		if got.release != nil {
			got.release()
		}
		t.Fatalf("interactive build crossed required-only phase: %v", got.err)
	case got := <-background:
		if got.release != nil {
			got.release()
		}
		t.Fatalf("background build crossed required-only phase: %v", got.err)
	default:
	}

	requiredGrant.release()
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return !stats.Active && stats.RequiredQueued == 0 &&
			stats.InteractiveQueued == 1 && stats.BackgroundQueued == 1
	})

	gate.Open()
	interactiveGrant := <-interactive
	if interactiveGrant.err != nil {
		t.Fatalf("interactive Acquire after Open: %v", interactiveGrant.err)
	}
	interactiveGrant.release()
	backgroundGrant := <-background
	if backgroundGrant.err != nil {
		t.Fatalf("background Acquire after Open: %v", backgroundGrant.err)
	}
	backgroundGrant.release()
}

func TestViewBuildGateRequiredAdmissionHasReservedCapacity(t *testing.T) {
	gate := newViewBuildGateWithAllLimits(1, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	interactive := acquireViewBuildAsync(ctx, gate, ViewBuildInteractive)
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.InteractiveQueued == stats.InteractiveLimit
	})
	if release, err := gate.Acquire(ctx, ViewBuildInteractive); release != nil ||
		!errors.Is(err, ErrViewBuildQueueFull) {
		if release != nil {
			release()
		}
		t.Fatalf("saturated interactive Acquire = release %v, err %v", release != nil, err)
	}

	required := acquireViewBuildAsync(ctx, gate, ViewBuildRequired)
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.RequiredQueued == 1
	})
	gate.OpenRequired()
	grant := <-required
	if grant.err != nil {
		t.Fatalf("required Acquire behind saturated interactive queue: %v", grant.err)
	}
	grant.release()

	stats := gate.Stats()
	if stats.RejectedInteractive != 1 || stats.RejectedRequired != 0 ||
		stats.InteractiveQueued != 1 {
		t.Fatalf("reserved admission stats = %+v", stats)
	}
	gate.Open()
	interactiveGrant := <-interactive
	if interactiveGrant.err != nil {
		t.Fatal(interactiveGrant.err)
	}
	interactiveGrant.release()
}

func TestViewBuildGateDefaultRequiredCohortDoesNotRejectBeyondLegacyLimit(t *testing.T) {
	gate := NewViewBuildGate()
	gate.OpenRequired()
	hold, err := gate.Acquire(context.Background(), ViewBuildRequired)
	if err != nil {
		t.Fatal(err)
	}

	const cohort = 256
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results := make(chan boundedGateAcquireResult, cohort)
	for i := 0; i < cohort; i++ {
		go func() {
			release, acquireErr := gate.Acquire(ctx, ViewBuildRequired)
			results <- boundedGateAcquireResult{release: release, err: acquireErr}
		}()
	}
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.RequiredLimit == 0 && stats.RequiredQueued == cohort &&
			stats.RejectedRequired == 0
	})
	hold()
	for i := 0; i < cohort; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("required cohort member %d rejected: %v", i, result.err)
		}
		result.release()
	}
	if stats := gate.Stats(); stats.RequiredQueued != 0 || stats.Active ||
		stats.AdmittedRequired != cohort+1 || stats.RejectedRequired != 0 {
		t.Fatalf("settled required cohort stats = %+v", stats)
	}
}

func TestViewBuildGateRequiredIsStrictFIFOAfterFullOpen(t *testing.T) {
	gate := newViewBuildGateWithAllLimits(4, 4, 4)
	gate.Open()
	hold, err := gate.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan boundedGateAcquireResult, 4)
	enqueue := func(name string, priority ViewBuildPriority, required, interactive, background int) {
		go func() {
			release, acquireErr := gate.Acquire(ctx, priority)
			results <- boundedGateAcquireResult{name: name, release: release, err: acquireErr}
		}()
		awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
			return stats.RequiredQueued == required &&
				stats.InteractiveQueued == interactive &&
				stats.BackgroundQueued == background
		})
	}

	enqueue("interactive", ViewBuildInteractive, 0, 1, 0)
	enqueue("background", ViewBuildBackground, 0, 1, 1)
	enqueue("required-1", ViewBuildRequired, 1, 1, 1)
	enqueue("required-2", ViewBuildRequired, 2, 1, 1)
	hold()

	for _, want := range []string{"required-1", "required-2", "interactive", "background"} {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.name != want {
				result.release()
				t.Fatalf("admitted %q, want %q", result.name, want)
			}
			result.release()
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	stats := gate.Stats()
	if stats.AdmittedRequired != 2 || stats.RequiredHighWater != 2 || stats.Active {
		t.Fatalf("required priority stats = %+v", stats)
	}
}

func TestViewBuildGateRequiredCancellationAndDirectOpen(t *testing.T) {
	gate := newViewBuildGateWithAllLimits(1, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	result := acquireViewBuildAsync(ctx, gate, ViewBuildRequired)
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.RequiredQueued == 1
	})
	cancel()
	if got := <-result; !errors.Is(got.err, context.Canceled) || got.release != nil {
		t.Fatalf("canceled required Acquire = %+v", got)
	}
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.RequiredQueued == 0 && stats.CanceledRequired == 1
	})

	// Open remains the compatibility edge for embedded users and tests: it
	// opens both phases and admits required work immediately.
	gate.Open()
	if stats := gate.Stats(); !stats.Open || !stats.RequiredOpen {
		t.Fatalf("direct Open state = %+v", stats)
	}
	release, err := gate.Acquire(context.Background(), ViewBuildRequired)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestRequiredStartupReconcilesOfflineHeadChangeBeforeFullOpen(t *testing.T) {
	f := newCoordinatorFixture(t)
	initial := coordinatorReconcile(t, f.inertCoordinator(t, CheckoutCoordinatorConfig{}))
	if initial.CommitGenerationID <= 0 || initial.DirtyGenerationID <= 0 {
		t.Fatalf("initial route = %+v", initial)
	}
	before := f.route()
	treeB := f.commitTreeB()

	gate, cycle := runRequiredRestartCycle(t, f)
	if cycle.Err != nil {
		t.Fatalf("required HEAD reconciliation: %v", cycle.Err)
	}
	if gate.IsOpen() {
		t.Fatal("full build gate opened before the required checkout became exact")
	}
	if stats := gate.Stats(); stats.AdmittedRequired != 1 || stats.AdmittedBackground != 0 {
		t.Fatalf("required HEAD admission stats = %+v", stats)
	}
	route := f.route()
	if route.CommitGenerationID == before.CommitGenerationID {
		t.Fatalf("offline HEAD change kept commit generation %d", route.CommitGenerationID)
	}
	commit, found := f.generation(route.CommitGenerationID)
	if !found || commit.TreeOID != treeB || commit.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("required commit generation = %+v, found=%v, want tree %s", commit, found, treeB)
	}
}

func TestRequiredStartupReconcilesOfflineDirtyChangesBeforeFullOpen(t *testing.T) {
	f := newCoordinatorFixture(t)
	initial := coordinatorReconcile(t, f.inertCoordinator(t, CheckoutCoordinatorConfig{}))
	before := f.route()
	headBefore := builderGit(t, f.worktree, "rev-parse", "HEAD")

	// Change a tracked file, delete tracked files, and add untracked source while
	// keeping HEAD fixed. This is the warm-restart case readiness metadata alone
	// cannot detect; the required coordinator must sample the working copy.
	builderWriteTree(t, f.worktree, builderTreeB())
	if headAfter := builderGit(t, f.worktree, "rev-parse", "HEAD"); headAfter != headBefore {
		t.Fatalf("dirty fixture moved HEAD from %s to %s", headBefore, headAfter)
	}

	gate, cycle := runRequiredRestartCycle(t, f)
	if cycle.Err != nil {
		t.Fatalf("required dirty reconciliation: %v", cycle.Err)
	}
	if gate.IsOpen() {
		t.Fatal("full build gate opened before dirty working-copy publication")
	}
	route := f.route()
	if route.CommitGenerationID != before.CommitGenerationID {
		t.Fatalf("dirty-only restart moved commit generation from %d to %d",
			before.CommitGenerationID, route.CommitGenerationID)
	}
	if route.DirtyGenerationID == before.DirtyGenerationID || !cycle.DirtyBuilt {
		t.Fatalf("dirty-only restart did not publish a new dirty layer: before=%+v after=%+v cycle=%+v",
			before, route, cycle)
	}
	dirty, found := f.generation(route.DirtyGenerationID)
	if !found || dirty.LowerViewFingerprint == "" || dirty.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("required dirty generation = %+v, found=%v", dirty, found)
	}
	if initial.CommitGenerationID != route.CommitGenerationID {
		t.Fatalf("dirty-only route lost its original commit layer %d: %+v",
			initial.CommitGenerationID, route)
	}
}

func TestRequiredStartupPreGenerationFailureBecomesTerminalDegraded(t *testing.T) {
	f := newCoordinatorFixture(t)
	gate := NewViewBuildGate()
	cycles := make(chan CheckoutCycle, 4)
	coordinator := f.coordinator(t, CheckoutCoordinatorConfig{
		Gate:            gate,
		StartupRequired: true,
		Debounce:        time.Millisecond,
		cycleDone:       func(cycle CheckoutCycle) { cycles <- cycle },
	})
	coordinator.cyclePreflight = func(context.Context) (CheckoutCycle, bool) {
		return CheckoutCycle{Err: errors.New("synthetic pre-generation failure")}, true
	}
	coordinator.Signal("cold startup")
	deferred := awaitRequiredCycle(t, cycles)
	if !deferred.Deferred {
		t.Fatalf("cycle before required admission = %+v", deferred)
	}
	gate.OpenRequired()
	failed := awaitRequiredCycle(t, cycles)
	if failed.Err == nil {
		t.Fatalf("required pre-generation attempt = %+v", failed)
	}
	pending, reason := coordinator.startupBuildStatus()
	if pending || reason != "startup checkout reconciliation failed; see daemon log" {
		t.Fatalf("terminal startup status = pending %v, reason %q", pending, reason)
	}
	if gate.IsOpen() {
		t.Fatal("coordinator failure opened the full gate without the daemon readiness decision")
	}

	// The readiness monitor owns this fail-open edge. Once it opens normal
	// service, a later exact background cycle can retry and clear the failure.
	gate.Open()
	if !gate.IsOpen() {
		t.Fatal("terminal degraded startup could not open normal service")
	}
}

func runRequiredRestartCycle(
	t *testing.T, f *coordinatorFixture,
) (*ViewBuildGate, CheckoutCycle) {
	t.Helper()
	gate := NewViewBuildGate()
	cycles := make(chan CheckoutCycle, 4)
	coordinator := f.coordinator(t, CheckoutCoordinatorConfig{
		Gate:            gate,
		StartupRequired: true,
		Debounce:        time.Millisecond,
		cycleDone:       func(cycle CheckoutCycle) { cycles <- cycle },
	})
	coordinator.Signal("cold restart")
	deferred := awaitRequiredCycle(t, cycles)
	if !deferred.Deferred {
		t.Fatalf("cycle before required admission = %+v", deferred)
	}
	gate.OpenRequired()
	cycle := awaitRequiredCycle(t, cycles)
	if cycle.Deferred {
		t.Fatalf("required cycle stayed deferred after OpenRequired: %+v", cycle)
	}
	pending, failure := coordinator.startupBuildStatus()
	if pending || failure != "" {
		t.Fatalf("required cycle status = pending %v, failure %q", pending, failure)
	}
	return gate, cycle
}

func awaitRequiredCycle(t *testing.T, cycles <-chan CheckoutCycle) CheckoutCycle {
	t.Helper()
	select {
	case cycle := <-cycles:
		return cycle
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for required checkout cycle")
		return CheckoutCycle{}
	}
}

func BenchmarkViewBuildGateRequiredHandoff(b *testing.B) {
	gate := NewViewBuildGate()
	gate.OpenRequired()
	ctx := context.Background()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			release, err := gate.Acquire(ctx, ViewBuildRequired)
			if err != nil {
				b.Fatal(err)
			}
			release()
		}
	})
}
