package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The dormancy tests are about one decision: a linked worktree the startup
// inventory saw is not given a coordinator until something selects it, and the
// selection that wakes it builds the same composed view the eager path would.
// Everything under them is the real lifecycle over real git families and a real
// store — the gate lives between the reconcile verdict and the coordinator, so
// a stub anywhere there removes exactly the seam the tests exist to check.

// activateAndWait activates one checkout and blocks until its coordinator is
// registered — the point at which SignalCheckout will find it, which is the
// precondition the tests that go on to signal or read the checkout rely on. A
// coordinator whose build loop has merely started is not yet in the registry,
// so the poll waits on registry membership rather than on a live loop.
// Activation is fire-and-forget, so the build lands on the lifecycle's own
// goroutine; the poll is what lets a test read the result deterministically
// without reaching into the activation's internals.
func (f *lifecycleFixture) activateAndWait(checkoutID string) {
	f.t.Helper()
	if !f.lc.ActivateCheckout(checkoutID, "test activation") {
		f.t.Fatalf("activation of %s was rejected", checkoutID)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !f.lc.coordinatorRegistered(checkoutID) {
		if time.Now().After(deadline) {
			f.t.Fatalf("coordinator for %s never came up after activation", checkoutID)
		}
		time.Sleep(time.Millisecond)
	}
}

// automaticCheckoutID returns the family's checkout administered under one
// worktree's name. Git spells a worktree by its directory's base name, which is
// the name passed to worktreeOf, so the admin name is the stable handle a test
// can name a specific automatic checkout by.
func (f *lifecycleFixture) automaticCheckoutID(familyID, adminName string) string {
	f.t.Helper()
	checkouts, err := f.catalog.ListCheckouts(context.Background(), familyID)
	require.NoError(f.t, err)
	for i := range checkouts {
		if checkouts[i].AdminName == adminName {
			return checkouts[i].CheckoutID
		}
	}
	f.t.Fatalf("family %s holds no checkout administered as %q", familyID, adminName)
	return ""
}

// contentEdges renders one reader's edges with the repo prefix stripped, so two
// composed views over identical content can be compared endpoint-for-endpoint
// regardless of which checkout produced them.
func contentEdges(r graph.Reader, prefix string) []string {
	edges := r.AllEdges()
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		from := strings.TrimPrefix(e.From, prefix+"/")
		to := strings.TrimPrefix(e.To, prefix+"/")
		out = append(out, from+" -"+string(e.Kind)+"-> "+to)
	}
	slices.Sort(out)
	return out
}

// TestDormancyColdFanoutInstallsNoAutomaticCoordinators is the load-bearing
// regression: the startup inventory of a worktree-heavy family costs no
// coordinators at all. Every worktree still gets a durable catalog row — the
// identity is what a later selection wakes — but the fan-out of one coordinator
// plus a commit and dirty build per worktree, serialised through the build
// gate, is exactly what dormancy removes from boot.
func TestDormancyColdFanoutInstallsNoAutomaticCoordinators(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("fanout-main")
	const worktrees = 4
	for i := 0; i < worktrees; i++ {
		f.worktreeOf(main, fmt.Sprintf("fanout-wt-%d", i))
	}

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "fanout-main"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	report, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, report.Coordinators,
		"the startup inventory installs no automatic coordinators")

	checkouts, err := f.catalog.ListCheckouts(ctx, tracked.FamilyID)
	require.NoError(t, err)
	require.Len(t, checkouts, worktrees+1,
		"every worktree plus the primary keeps a durable catalog row")

	require.Equal(t, 0, f.lc.LiveCoordinators(""),
		"no coordinator is live for a fully dormant family")
	gauge := viewmetrics.Read().Gauges[viewmetrics.Coordinators]
	require.Equal(t, int64(f.lc.LiveCoordinators("")), gauge,
		"the coordinators gauge tracks the live count, not the catalog size")
}

// TestDormancyActivatedViewMatchesEagerView guards the wake-up against serving a
// sparse overlay in place of the composed base+overlay. It builds the same
// content two ways — a worktree the inventory left dormant and then activated,
// and a control worktree added at runtime and built eagerly — and pins their
// materialised views node-for-node and edge-for-edge. A wake-up that served
// only the overlay delta would come back smaller than the eager control.
func TestDormancyActivatedViewMatchesEagerView(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("activated-main")
	dormant := f.worktreeOf(main, "activated-dormant")

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "activated-main"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	report, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, report.Coordinators, "the startup worktree is dormant")

	// The control is added after the inventory, so it is eager on discovery —
	// the same content built through the path startup never deferred.
	eager := f.worktreeOf(main, "activated-eager")

	// Give both worktrees byte-identical content: one committed file over the
	// shared base, and one dirty file over that. A correct composed view of
	// either is then the same graph.
	for _, wt := range []string{dormant, eager} {
		require.NoError(t, os.Remove(filepath.Join(wt, filepath.Base(wt)+".go")))
		writeFile(t, filepath.Join(wt, "feature.go"), "package a\n\nfunc Feature() {}\n")
		runGit(t, wt, "add", "-A")
		runGit(t, wt, "commit", "-q", "-m", "feature")
		writeFile(t, filepath.Join(wt, "overlay.go"), "package a\n\nfunc Overlay() {}\n")
	}

	report, err = f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, report.Coordinators, "the runtime-added worktree is eager on discovery")

	dormantID := f.automaticCheckoutID(tracked.FamilyID, filepath.Base(dormant))
	eagerID := f.automaticCheckoutID(tracked.FamilyID, filepath.Base(eager))

	f.activateAndWait(dormantID)
	f.runCoordinator(dormantID)
	f.runCoordinator(eagerID)

	activatedView := f.materialize(dormantID)
	defer activatedView.Close()
	eagerView := f.materialize(eagerID)
	defer eagerView.Close()

	activatedNodes := contentIdentities(activatedView.Reader, tracked.Prefix)
	require.NotEmpty(t, activatedNodes,
		"the activated view is empty — a sparse overlay was served instead of the composed base+overlay")
	require.Contains(t, activatedNodes, "feature.go::Feature",
		"the activated view is missing the committed layer — the base was not composed")
	require.Contains(t, activatedNodes, "overlay.go::Overlay",
		"the activated view is missing the dirty layer")
	require.Equal(t, contentIdentities(eagerView.Reader, tracked.Prefix), activatedNodes,
		"the activated view's nodes differ from the eager control")
	require.Equal(t, contentEdges(eagerView.Reader, tracked.Prefix), contentEdges(activatedView.Reader, tracked.Prefix),
		"the activated view's edges differ from the eager control")
}

// TestDormancyRuntimeDiscovery pins the one default the config flag flips: a
// worktree added while the daemon is up is eager on discovery, and the same
// worktree is left dormant when lazy activation is configured. The initial
// inventory is dormant either way; the flag only decides what a later
// `git worktree add` costs.
func TestDormancyRuntimeDiscovery(t *testing.T) {
	runtimeDiscovery := func(t *testing.T, lazy bool, wantCoordinators int) {
		f := newLifecycleFixture(t)
		f.lc.cfgLazyWorktrees = lazy
		defer f.close()
		ctx := context.Background()

		main := f.gitRepo("runtime-main")
		f.worktreeOf(main, "runtime-startup")
		tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "runtime-main"}, TrackSourceCLI)
		require.NoError(t, err)
		require.NoError(t, tracked.CatalogErr)

		report, err := f.lc.Sweep(ctx)
		require.NoError(t, err)
		require.Equal(t, 0, report.Coordinators, "the startup worktree is always dormant")

		// A worktree minted after the inventory is a runtime addition.
		f.worktreeOf(main, "runtime-added")
		report, err = f.lc.Sweep(ctx)
		require.NoError(t, err)
		require.Equal(t, wantCoordinators, report.Coordinators)
	}

	t.Run("eager by default", func(t *testing.T) {
		runtimeDiscovery(t, false, 1)
	})
	t.Run("dormant under lazy activation", func(t *testing.T) {
		runtimeDiscovery(t, true, 0)
	})
}

// TestDormancyAdmitsRoutedAndConvergingButNotStartupAutomatic pins the
// admission predicate itself: a routed checkout resumes across a restart, a
// checkout a track or promote is converging keeps building, and a freshly
// minted worktree after the inventory is a runtime addition — while a plain
// automatic checkout re-seen on a later pass stays dormant. The primary is a
// dedicated checkout the gate never reaches, so it is never dormant.
func TestDormancyAdmitsRoutedAndConvergingButNotStartupAutomatic(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()

	const family = "family-admission"
	f.lc.markInitialInventory(reconcile.FamilyReport{FamilyID: family, PrimaryGraphID: "graph-primary"})

	base := store_sqlite.Checkout{
		CheckoutID:    "checkout-automatic",
		FamilyID:      family,
		State:         store_sqlite.CheckoutStateReady,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
	}

	// A user-tracked checkout converging — an intent transition is in flight —
	// keeps its coordinator so the build the transition drives has one to run.
	converging := base
	converging.CheckoutID = "checkout-converging"
	converging.ActiveIntentTransitionID = "transition-1"
	require.True(t, f.lc.coordinatorAdmitted(converging, false, reconcile.ActionReadyConfirmed),
		"a checkout with an active intent transition must keep building")

	// A routed checkout resumes across a restart — the route is what marks the
	// view it was serving worth resuming without a fresh selection.
	routed := base
	routed.CheckoutID = "checkout-routed"
	require.True(t, f.lc.coordinatorAdmitted(routed, true, reconcile.ActionReadyConfirmed),
		"a routed checkout resumes across a restart")

	// A freshly minted automatic after the inventory is a runtime addition.
	minted := base
	minted.CheckoutID = "checkout-minted"
	require.True(t, f.lc.coordinatorAdmitted(minted, false, reconcile.ActionIdentityAllocated),
		"a worktree minted after the inventory is eager by default")

	// A plain automatic re-seen on a later pass is startup inventory and stays
	// dormant until it is selected.
	reseen := base
	reseen.CheckoutID = "checkout-reseen"
	require.False(t, f.lc.coordinatorAdmitted(reseen, false, reconcile.ActionReadyConfirmed),
		"a re-seen startup worktree stays dormant")

	// The lazy flag suppresses even a runtime addition.
	f.lc.cfgLazyWorktrees = true
	require.False(t, f.lc.coordinatorAdmitted(minted, false, reconcile.ActionIdentityAllocated),
		"lazy activation keeps a runtime addition dormant too")
}

// TestDormancyConcurrentActivationInstallsOneCoordinator drives the wake-up from
// many goroutines at once, the shape a burst of selections lands in. Exactly one
// build is spawned, the rest fold onto it, and the start WaitGroup is left
// balanced — a leak there would hang Close forever, and an over-release would
// panic the race detector.
func TestDormancyConcurrentActivationInstallsOneCoordinator(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("concurrent-main")
	f.worktreeOf(main, "concurrent-wt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "concurrent-main"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	report, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, report.Coordinators, "the startup worktree is dormant")

	checkoutID := f.automaticCheckoutID(tracked.FamilyID, "concurrent-wt")

	const racers = 8
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.lc.ActivateCheckout(checkoutID, "race")
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(15 * time.Second)
	for !f.lc.hasCoordinator(checkoutID) {
		if time.Now().After(deadline) {
			t.Fatal("no coordinator came up under concurrent activation")
		}
		time.Sleep(time.Millisecond)
	}
	// Join the one activation that won, then confirm it is the only one.
	f.lc.coordinatorStartWG.Wait()
	require.Equal(t, 1, f.lc.LiveCoordinators(""),
		"concurrent activation installed more than one coordinator")
}

// TestDormancySweepOverDormantMembersFaultsNothing runs the retirement sweep
// over a family whose worktrees are all dormant. A dormant member has no
// coordinator, no route and no generation, so the sweep that hunts orphaned
// generations must find nothing to retire and no coordinator to fault on.
func TestDormancySweepOverDormantMembersFaultsNothing(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("retire-main")
	for i := 0; i < 3; i++ {
		f.worktreeOf(main, fmt.Sprintf("retire-wt-%d", i))
	}
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "retire-main"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	report, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, report.Coordinators, "every worktree is dormant")
	require.Equal(t, 0, report.Retired, "dormant members have nothing built to retire")

	// The retirement sweep, run directly against the dormant family, collects
	// nothing and faults on no missing coordinator.
	require.Equal(t, 0, f.lc.sweepRetirements(ctx))
	require.Equal(t, 0, f.lc.LiveCoordinators(""), "the sweep minted no coordinator for a dormant member")
}

// TestDormancyRepeatedActivationDoesNotStarveTheBuild is the livelock guard.
//
// A production client polling a not-yet-routed view kicks activation on every
// poll, and it retries faster than the coordinator's 300ms quiet window. The
// coordinator's build loop re-arms that window on every signal and only builds
// once it elapses quiet, so if each activation re-signalled the live coordinator
// the window would reset on every poll and the build would never run — the route
// would never settle and the client would poll forever. This drives exactly that
// cadence and requires the route to become ready anyway: the second activation
// onward must not signal the coordinator.
func TestDormancyRepeatedActivationDoesNotStarveTheBuild(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("livelock-main")
	f.worktreeOf(main, "livelock-wt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "livelock-main"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	report, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, report.Coordinators, "the worktree is dormant until selected")

	checkoutID := f.automaticCheckoutID(tracked.FamilyID, "livelock-wt")

	// Poll faster than the 300ms quiet window, kicking activation each time — the
	// shape a retrying client's not-routed fallback produces — and require the
	// route to settle regardless.
	deadline := time.Now().Add(30 * time.Second)
	for {
		require.True(t, f.lc.ActivateCheckout(checkoutID, "poll"),
			"activation of a ready automatic checkout was rejected")
		route, found, err := f.catalog.GetCheckoutRoute(ctx, checkoutID)
		require.NoError(t, err)
		if found && graphview.RouteReady(route) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("repeated activation starved the build: the route never settled")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
