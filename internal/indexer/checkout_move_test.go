package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/reconcile"
)

func TestAutomaticWorktreeMoveRebindsWithoutWritingGenerations(t *testing.T) {
	f := newFamilyFixture(t, "move-automatic")
	defer f.close()
	ctx := context.Background()

	f.runCoordinator(f.automatic.CheckoutID)
	beforeRoute, routed := f.routeOf(f.automatic.CheckoutID)
	require.True(t, routed)
	primaryBefore, found, err := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	require.NoError(t, err)
	require.True(t, found)
	beforeCoordinator := liveCoordinatorOrNil(f.lc, f.automatic.CheckoutID)
	require.NotNil(t, beforeCoordinator)
	beforeSource := requireCheckoutSourceRegistration(t, f.lc, f.automatic.CheckoutID)
	var buildAdmissions atomic.Int64
	f.lc.indexBarrier = func() { buildAdmissions.Add(1) }

	movedRoot := filepath.Join(f.dir, "automatic-renamed")
	runGit(t, f.main, "worktree", "move", f.worktree, movedRoot)
	report, err := f.lc.ReconcileFamily(ctx, f.familyID)
	require.NoError(t, err)
	requireMoveReport(t, report, f.automatic.CheckoutID, f.worktree, movedRoot)

	checkout, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, coordinatorRootEqual(checkout.RootPath, movedRoot))
	afterCoordinator := liveCoordinatorOrNil(f.lc, checkout.CheckoutID)
	require.NotNil(t, afterCoordinator)
	require.NotSame(t, beforeCoordinator, afterCoordinator)
	require.True(t, afterCoordinator.Running())
	require.True(t, coordinatorRootEqual(afterCoordinator.root, movedRoot))
	require.Equal(t, f.primaryGraph, afterCoordinator.graphID)
	afterSource := requireCheckoutSourceRegistration(t, f.lc, checkout.CheckoutID)
	require.NotSame(t, beforeSource, afterSource)
	require.True(t, coordinatorRootEqual(afterSource.identity.requestedRoot, movedRoot))
	require.Same(t, afterCoordinator, afterSource.identity.coordinator)
	select {
	case <-beforeSource.done:
	default:
		t.Fatal("old automatic source watcher was not retired")
	}

	afterRoute, routed := f.routeOf(checkout.CheckoutID)
	require.True(t, routed)
	require.Equal(t, beforeRoute, afterRoute, "root-only convergence must not publish a route")
	primaryAfter, found, err := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, primaryBefore.ActiveGenerationID, primaryAfter.ActiveGenerationID)
	require.Zero(t, buildAdmissions.Load(), "root-only convergence admitted a physical build")
	requireNoRootMoveJournal(t, f.catalog, checkout.CheckoutID)
}

func TestRootMoveTopologyEventFollowsSuccessfulJournalRelease(t *testing.T) {
	f := newFamilyFixture(t, "move-readiness-event")
	defer f.close()
	ctx := context.Background()

	type observedEvent struct {
		event       CheckoutTopologyEvent
		outsideMove bool
	}
	events := make(chan observedEvent, 2)
	f.lc.SetCheckoutTopologyObserver(func(event CheckoutTopologyEvent) {
		outside := f.lc.moveMu.TryLock()
		if outside {
			f.lc.moveMu.Unlock()
		}
		events <- observedEvent{event: event, outsideMove: outside}
	})
	defer f.lc.SetCheckoutTopologyObserver(nil)

	oldRoot := f.worktree
	newRoot := filepath.Join(f.dir, "move-readiness-event-renamed")
	runGit(t, f.main, "worktree", "move", oldRoot, newRoot)
	report, err := f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	requireMoveReport(t, report, f.automatic.CheckoutID, oldRoot, newRoot)

	raw, err := sql.Open("sqlite", f.dbPath+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.ExecContext(ctx, fmt.Sprintf(`
CREATE TRIGGER fail_root_move_completion
BEFORE DELETE ON checkout_root_moves
WHEN OLD.checkout_id = '%s'
BEGIN
  SELECT RAISE(ABORT, 'injected root move completion failure');
END`, f.automatic.CheckoutID))
	require.NoError(t, err)

	err = f.lc.applyReconcileReport(ctx, report)
	require.ErrorContains(t, err, "injected root move completion failure")
	select {
	case got := <-events:
		t.Fatalf("failed root-move journal release emitted terminal event: %+v", got)
	default:
	}
	_, pending, err := f.catalog.GetCheckoutRootMove(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, pending)

	_, err = raw.ExecContext(ctx, `DROP TRIGGER fail_root_move_completion`)
	require.NoError(t, err)
	require.NoError(t, f.lc.applyReconcileReport(ctx, report))
	select {
	case got := <-events:
		require.True(t, got.outsideMove, "topology observer ran while moveMu was held")
		require.Equal(t, CheckoutTopologyEvent{
			Kind:         CheckoutTopologyRootMoveCompleted,
			CheckoutID:   f.automatic.CheckoutID,
			Incarnation:  f.automatic.Incarnation,
			PreviousRoot: oldRoot,
			CurrentRoot:  newRoot,
		}, got.event)
	case <-time.After(5 * time.Second):
		t.Fatal("successful root-move journal release emitted no topology event")
	}
	requireNoRootMoveJournal(t, f.catalog, f.automatic.CheckoutID)
}

func TestBlockedFamilyCoordinatorConvergenceDoesNotBlockTopologyPublication(t *testing.T) {
	f := newFamilyFixture(t, "move-coordinator-lock-order")
	defer f.close()
	ctx := context.Background()

	// Model an admitted mutation without depending on a coordinator cycle: a
	// checkout topology writer must wait for this one-unit reader lease.
	gate := f.lc.ensureMutationFences().checkout(f.automatic.CheckoutID)
	require.NoError(t, gate.sem.Acquire(ctx, 1))
	gateHeld := true
	defer func() {
		if gateHeld {
			gate.sem.Release(1)
		}
	}()

	reachedApply := make(chan struct{})
	continueApply := make(chan struct{})
	f.lc.coordinatorApplyBarrier = func() {
		close(reachedApply)
		<-continueApply
	}
	defer func() { f.lc.coordinatorApplyBarrier = nil }()

	finished := make(chan error, 1)
	go func() {
		finished <- f.lc.applyReconcileReport(ctx, reconcile.FamilyReport{
			FamilyID:       f.familyID,
			PrimaryGraphID: f.primaryGraph,
			Checkouts: []reconcile.CheckoutReport{{
				CheckoutID:  f.automatic.CheckoutID,
				Incarnation: f.automatic.Incarnation,
				Durable:     true,
				State:       f.automatic.State,
			}},
		})
	}()
	select {
	case <-reachedApply:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator convergence did not reach its post-move boundary")
	}

	// Hold the global publication lock across the blocked coordinator phase.
	// The report must still finish after its own checkout drains; otherwise it
	// carried the old global-lock dependency beyond root-move publication.
	publicationFree := f.lc.topologyPublishMu.TryLock()
	if publicationFree {
		defer f.lc.topologyPublishMu.Unlock()
	}
	close(continueApply)
	gate.sem.Release(1)
	gateHeld = false
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator convergence waited on global topology publication")
	}
	if !publicationFree {
		t.Error("coordinator convergence still owns the global publication lock")
	}

	// The deferred release must not release the same reader twice.
	require.NoError(t, gate.sem.Acquire(ctx, 1))
	gateHeld = true
}

func TestRootMoveTopologyEventsCannotOvertakeDurableMoveOrder(t *testing.T) {
	f := newFamilyFixture(t, "move-readiness-order")
	defer f.close()
	ctx := context.Background()

	events := make(chan CheckoutTopologyEvent, 2)
	firstObserverEntered := make(chan struct{})
	releaseFirstObserver := make(chan struct{})
	defer func() {
		select {
		case <-releaseFirstObserver:
		default:
			close(releaseFirstObserver)
		}
	}()
	var observed atomic.Int64
	f.lc.SetCheckoutTopologyObserver(func(event CheckoutTopologyEvent) {
		events <- event
		if observed.Add(1) == 1 {
			close(firstObserverEntered)
			<-releaseFirstObserver
		}
	})
	defer f.lc.SetCheckoutTopologyObserver(nil)

	rootA := f.worktree
	rootB := filepath.Join(f.dir, "move-readiness-order-b")
	rootC := filepath.Join(f.dir, "move-readiness-order-c")
	runGit(t, f.main, "worktree", "move", rootA, rootB)
	reportAB, err := f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	requireMoveReport(t, reportAB, f.automatic.CheckoutID, rootA, rootB)

	applyABDone := make(chan error, 1)
	go func() { applyABDone <- f.lc.applyReconcileReport(ctx, reportAB) }()
	select {
	case <-firstObserverEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("A-to-B topology observer did not start")
	}
	secondPublicationAttempted := make(chan struct{})
	var publicationAttempted atomic.Bool
	f.lc.topologyPublishWaitBarrier = func() {
		if publicationAttempted.CompareAndSwap(false, true) {
			close(secondPublicationAttempted)
		}
	}
	defer func() { f.lc.topologyPublishWaitBarrier = nil }()
	runGit(t, f.main, "worktree", "move", rootB, rootC)
	reportBC, err := f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	requireMoveReport(t, reportBC, f.automatic.CheckoutID, rootB, rootC)

	applyBCDone := make(chan error, 1)
	go func() { applyBCDone <- f.lc.applyReconcileReport(ctx, reportBC) }()
	select {
	case <-secondPublicationAttempted:
	case <-time.After(5 * time.Second):
		t.Fatal("B-to-C convergence did not reach the publication fence")
	}
	select {
	case err := <-applyBCDone:
		t.Fatalf("B-to-C convergence overtook the blocked A-to-B publication: %v", err)
	default:
	}
	pending, found, err := f.catalog.GetCheckoutRootMove(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found, "B-to-C journal must remain until A-to-B is published")
	require.True(t, coordinatorRootEqual(pending.PreviousRootPath, rootB))
	require.True(t, coordinatorRootEqual(pending.CurrentRootPath, rootC))

	close(releaseFirstObserver)
	require.NoError(t, <-applyABDone)
	require.NoError(t, <-applyBCDone)
	first := <-events
	second := <-events
	require.Equal(t, rootA, first.PreviousRoot)
	require.Equal(t, rootB, first.CurrentRoot)
	require.Equal(t, rootB, second.PreviousRoot)
	require.Equal(t, rootC, second.CurrentRoot)
	requireNoRootMoveJournal(t, f.catalog, f.automatic.CheckoutID)
}

func TestAutomaticMoveRebindRejectsRootThatAdvancesDuringBuild(t *testing.T) {
	f := newFamilyFixture(t, "move-automatic-build-race")
	defer f.close()
	ctx := context.Background()
	f.runCoordinator(f.automatic.CheckoutID)
	before := liveCoordinatorOrNil(f.lc, f.automatic.CheckoutID)
	require.NotNil(t, before)

	rootB := filepath.Join(f.dir, "automatic-race-b")
	rootC := filepath.Join(f.dir, "automatic-race-c")
	runGit(t, f.main, "worktree", "move", f.worktree, rootB)
	_, err := f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	checkoutB, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, coordinatorRootEqual(checkoutB.RootPath, rootB))

	built := make(chan struct{})
	allowPublish := make(chan struct{})
	f.lc.moveRebindBarrier = func() {
		close(built)
		<-allowPublish
	}
	rebindDone := make(chan error, 1)
	go func() {
		_, rebindErr := f.lc.rebindCheckoutCoordinatorRoot(
			ctx, f.primaryGraph, checkoutB, true,
		)
		rebindDone <- rebindErr
	}()
	<-built
	runGit(t, f.main, "worktree", "move", rootB, rootC)
	require.NoError(t, f.catalog.UpdateCheckoutObservation(
		ctx, moveObservationFromCheckout(checkoutB, rootC),
	))
	close(allowPublish)
	err = <-rebindDone
	require.ErrorIs(t, err, store_sqlite.ErrCatalogStaleGuard)
	require.Same(t, before, liveCoordinatorOrNil(f.lc, f.automatic.CheckoutID),
		"the stale B replacement must not displace the previous coordinator")

	f.lc.moveRebindBarrier = nil
	_, err = f.lc.ReconcileFamily(ctx, f.familyID)
	require.NoError(t, err)
	after := liveCoordinatorOrNil(f.lc, f.automatic.CheckoutID)
	require.NotNil(t, after)
	require.True(t, coordinatorRootEqual(after.root, rootC))
	requireNoRootMoveJournal(t, f.catalog, f.automatic.CheckoutID)
}

func TestDedicatedWorktreeMoveConvergesCLIAndMCPWithoutReindex(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source store_sqlite.IntentSourceKind
	}{
		{name: "cli", source: TrackSourceCLI},
		{name: "mcp", source: TrackSourceMCP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFamilyFixture(t, "move-dedicated-"+tc.name)
			defer f.close()
			ctx := context.Background()

			tracked, err := f.lc.Register(
				ctx, config.RepoEntry{Path: f.worktree}, tc.source,
			)
			require.NoError(t, err)
			require.NoError(t, tracked.CatalogErr)
			beforeRoute, routed := f.routeOf(tracked.CheckoutID)
			require.True(t, routed)
			graphBefore, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
			require.NoError(t, err)
			require.True(t, found)
			beforeCoordinator := liveCoordinatorOrNil(f.lc, tracked.CheckoutID)
			require.NotNil(t, beforeCoordinator)
			var buildAdmissions atomic.Int64
			f.lc.indexBarrier = func() { buildAdmissions.Add(1) }

			unrelated := f.gitRepo("same-prefix-unrelated-" + tc.name)
			require.NoError(t, f.cm.Global().AddRepo(config.RepoEntry{
				Path: unrelated, Name: tracked.Prefix,
			}))
			movedRoot := filepath.Join(f.dir, "dedicated-renamed-"+tc.name)
			runGit(t, f.main, "worktree", "move", f.worktree, movedRoot)
			report, err := f.lc.ReconcileFamily(ctx, f.familyID)
			require.NoError(t, err)
			requireMoveReport(t, report, tracked.CheckoutID, f.worktree, movedRoot)

			meta := f.mi.GetMetadata(tracked.Prefix)
			require.NotNil(t, meta)
			require.True(t, coordinatorRootEqual(meta.RootPath, movedRoot))
			afterCoordinator := liveCoordinatorOrNil(f.lc, tracked.CheckoutID)
			require.NotNil(t, afterCoordinator)
			require.True(t, afterCoordinator.Running())
			require.True(t, coordinatorRootEqual(afterCoordinator.root, movedRoot))
			require.Equal(t, tracked.GraphID, afterCoordinator.graphID)

			afterRoute, routed := f.routeOf(tracked.CheckoutID)
			require.True(t, routed)
			require.Equal(t, beforeRoute, afterRoute)
			graphAfter, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, graphBefore.ActiveGenerationID, graphAfter.ActiveGenerationID)
			require.Zero(t, buildAdmissions.Load(), "move convergence admitted a physical build")
			require.True(t, f.configLists(movedRoot))
			require.False(t, configContainsMovedPath(t, f.cfgPath, f.worktree))
			require.True(t, configContainsMovedPath(t, f.cfgPath, unrelated),
				"an unrelated entry reusing the same explicit Name must not move")
			requireActivePathIntent(t, f.catalog, tracked.CheckoutID, tc.source, movedRoot)
			requireNoRootMoveJournal(t, f.catalog, tracked.CheckoutID)
		})
	}
}

func TestMissingDedicatedMoveShellRefusesOccupiedRuntimeSlots(t *testing.T) {
	for _, tc := range []struct {
		name       string
		samePrefix bool
	}{
		{name: "target-owned-by-other-prefix"},
		{name: "same-prefix-without-indexer", samePrefix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFamilyFixture(t, "move-shell-collision-"+tc.name)
			defer f.close()
			ctx := context.Background()
			tracked, err := f.lc.Register(
				ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
			)
			require.NoError(t, err)
			movedRoot := filepath.Join(f.dir, "shell-collision-renamed")
			runGit(t, f.main, "worktree", "move", f.worktree, movedRoot)
			_, err = f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
			require.NoError(t, err)

			f.mi.mu.Lock()
			originalMeta := f.mi.repos[tracked.Prefix]
			originalIndexer := f.mi.indexers[tracked.Prefix]
			delete(f.mi.repos, tracked.Prefix)
			delete(f.mi.indexers, tracked.Prefix)
			collisionPrefix := "other-runtime-prefix"
			if tc.samePrefix {
				collisionPrefix = tracked.Prefix
			}
			collision := *originalMeta
			collision.RepoPrefix = collisionPrefix
			collision.RootPath = movedRoot
			f.mi.repos[collisionPrefix] = &collision
			if !tc.samePrefix {
				f.mi.indexers[collisionPrefix] = originalIndexer
			}
			f.mi.mu.Unlock()
			t.Cleanup(func() {
				f.mi.mu.Lock()
				delete(f.mi.repos, collisionPrefix)
				delete(f.mi.indexers, collisionPrefix)
				f.mi.repos[tracked.Prefix] = originalMeta
				f.mi.indexers[tracked.Prefix] = originalIndexer
				f.mi.mu.Unlock()
			})

			_, err = f.mi.RebindRouteOwnedRepoRoot(
				ctx, tracked.CheckoutID, tracked.Prefix, movedRoot,
			)
			require.ErrorIs(t, err, store_sqlite.ErrCatalogStaleGuard)
		})
	}
}

func TestDedicatedWorktreeSwapConvergesOneAtomicConfigBatch(t *testing.T) {
	f := newFamilyFixture(t, "move-dedicated-swap")
	defer f.close()
	ctx := context.Background()
	first, err := f.lc.Register(
		ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
	)
	require.NoError(t, err)

	secondRoot := filepath.Join(f.dir, "move-swap-second")
	runGit(t, f.main, "branch", "move-swap-second")
	runGit(t, f.main, "worktree", "add", secondRoot, "move-swap-second")
	_, err = f.lc.ReconcileFamily(ctx, f.familyID)
	require.NoError(t, err)
	second, err := f.lc.Register(
		ctx, config.RepoEntry{Path: secondRoot}, TrackSourceCLI,
	)
	require.NoError(t, err)
	firstRoute, routed := f.routeOf(first.CheckoutID)
	require.True(t, routed)
	secondRoute, routed := f.routeOf(second.CheckoutID)
	require.True(t, routed)
	var buildAdmissions atomic.Int64
	f.lc.indexBarrier = func() { buildAdmissions.Add(1) }

	temporaryRoot := filepath.Join(f.dir, "move-swap-temporary")
	runGit(t, f.main, "worktree", "move", f.worktree, temporaryRoot)
	runGit(t, f.main, "worktree", "move", secondRoot, f.worktree)
	runGit(t, f.main, "worktree", "move", temporaryRoot, secondRoot)
	_, err = f.lc.ReconcileFamily(ctx, f.familyID)
	require.NoError(t, err)

	require.True(t, configNamesRoot(t, f.cfgPath, first.Prefix, secondRoot))
	require.True(t, configNamesRoot(t, f.cfgPath, second.Prefix, f.worktree))
	afterFirstRoute, routed := f.routeOf(first.CheckoutID)
	require.True(t, routed)
	afterSecondRoute, routed := f.routeOf(second.CheckoutID)
	require.True(t, routed)
	require.Equal(t, firstRoute, afterFirstRoute)
	require.Equal(t, secondRoute, afterSecondRoute)
	require.Zero(t, buildAdmissions.Load())
	requireNoRootMoveJournal(t, f.catalog, first.CheckoutID)
	requireNoRootMoveJournal(t, f.catalog, second.CheckoutID)
}

func TestRouteOwnedStartupRestoreReusesStaleAutomaticRootShell(t *testing.T) {
	t.Parallel()
	f := newFamilyFixture(t, "move-startup-stale-automatic-shell")
	defer f.close()
	ctx := context.Background()
	tracked, err := f.lc.Register(
		ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
	)
	require.NoError(t, err)

	movedRoot := filepath.Join(f.dir, "startup-stale-automatic-target")
	runGit(t, f.main, "worktree", "move", f.worktree, movedRoot)
	_, err = f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)

	f.mi.mu.Lock()
	originalMeta := f.mi.repos[tracked.Prefix]
	originalIndexer := f.mi.indexers[tracked.Prefix]
	delete(f.mi.repos, tracked.Prefix)
	delete(f.mi.indexers, tracked.Prefix)
	stalePrefix := "stale-automatic-startup-shell"
	stale := *originalMeta
	stale.RepoPrefix = stalePrefix
	stale.RootPath = movedRoot
	f.mi.repos[stalePrefix] = &stale
	f.mi.indexers[stalePrefix] = originalIndexer
	f.mi.mu.Unlock()

	identity, err := DetectIdentity(movedRoot)
	require.NoError(t, err)
	_, restored, err := f.mi.restoreRouteOwnedRepoCtx(
		ctx,
		config.RepoEntry{Path: movedRoot, Name: tracked.Prefix},
		movedRoot,
		tracked.Prefix,
		config.Default(),
		identity,
		nil,
		false,
	)
	require.NoError(t, err)
	require.True(t, restored)
	meta := f.mi.GetMetadata(tracked.Prefix)
	require.NotNil(t, meta)
	require.True(t, coordinatorRootEqual(meta.RootPath, movedRoot))
	require.NotNil(t, f.mi.GetMetadata(stalePrefix),
		"startup restoration must not silently delete the stale shell")
}

func TestMoveComponentRejectsNonparticipantDedicatedTarget(t *testing.T) {
	t.Parallel()
	f := newFamilyFixture(t, "move-component-dedicated-occupant")
	defer f.close()
	ctx := context.Background()
	first, err := f.lc.Register(
		ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
	)
	require.NoError(t, err)

	secondRoot := filepath.Join(f.dir, "move-component-dedicated-occupant-second")
	runGit(t, f.main, "branch", "move-component-dedicated-occupant-second")
	runGit(t, f.main, "worktree", "add", secondRoot, "move-component-dedicated-occupant-second")
	_, err = f.lc.ReconcileFamily(ctx, f.familyID)
	require.NoError(t, err)
	second, err := f.lc.Register(
		ctx, config.RepoEntry{Path: secondRoot}, TrackSourceCLI,
	)
	require.NoError(t, err)
	require.NotEmpty(t, second.Prefix)

	checkout, found, err := f.catalog.GetCheckout(ctx, first.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, f.catalog.UpdateCheckoutObservation(
		ctx, moveObservationFromCheckout(checkout, secondRoot),
	))
	checkout, found, err = f.catalog.GetCheckout(ctx, first.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	move, found, err := f.catalog.GetCheckoutRootMove(ctx, first.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	err = f.lc.validateCheckoutRootMoveComponentOccupants(ctx, []checkoutRootMoveParticipant{{
		checkout: checkout,
		move:     move,
		graphID:  first.GraphID,
		prefix:   first.Prefix,
	}})
	require.ErrorIs(t, err, store_sqlite.ErrCatalogStaleGuard)
	require.Contains(t, err.Error(), "nonparticipant")
	firstMeta := f.mi.GetMetadata(first.Prefix)
	require.NotNil(t, firstMeta)
	require.True(t, coordinatorRootEqual(firstMeta.RootPath, f.worktree),
		"refused source shell must remain at its last coherent root")
	secondMeta := f.mi.GetMetadata(second.Prefix)
	require.NotNil(t, secondMeta)
	require.True(t, coordinatorRootEqual(secondMeta.RootPath, secondRoot),
		"destination route-owned shell must remain untouched")
	_, routed := f.routeOf(second.CheckoutID)
	require.True(t, routed, "destination route/corpus was withdrawn")
	_, pending, journalErr := f.catalog.GetCheckoutRootMove(ctx, first.CheckoutID)
	require.NoError(t, journalErr)
	require.True(t, pending, "refused move must remain durably retryable")

	// The durable guard must survive a process restart where neither the
	// destination shell nor its coordinator has been restored yet.
	f.restart()
	require.Nil(t, liveCoordinatorOrNil(f.lc, second.CheckoutID))
	require.Nil(t, f.mi.GetMetadata(second.Prefix))
	err = f.lc.validateCheckoutRootMoveComponentOccupants(ctx, []checkoutRootMoveParticipant{{
		checkout: checkout,
		move:     move,
		graphID:  first.GraphID,
		prefix:   first.Prefix,
	}})
	require.ErrorIs(t, err, store_sqlite.ErrCatalogStaleGuard)
	require.Contains(t, err.Error(), "nonparticipant durable checkout")
	secondCheckout, found, catalogErr := f.catalog.GetCheckout(ctx, second.CheckoutID)
	require.NoError(t, catalogErr)
	require.True(t, found)
	require.True(t, coordinatorRootEqual(secondCheckout.RootPath, secondRoot))
	_, routed = f.routeOf(second.CheckoutID)
	require.True(t, routed, "catalog-only destination route was withdrawn")
	_, pending, journalErr = f.catalog.GetCheckoutRootMove(ctx, first.CheckoutID)
	require.NoError(t, journalErr)
	require.True(t, pending, "catalog-only collision consumed the retry journal")
}

func TestDedicatedSwapSecondShellFailureStaysQuiescedUntilCoherentRecovery(t *testing.T) {
	t.Parallel()
	f := newFamilyFixture(t, "move-dedicated-swap-publication-failure")
	defer f.close()
	ctx := context.Background()
	first, err := f.lc.Register(
		ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
	)
	require.NoError(t, err)

	secondRoot := filepath.Join(f.dir, "move-dedicated-swap-publication-second")
	unrelatedRoot := filepath.Join(f.dir, "move-dedicated-swap-publication-unrelated")
	runGit(t, f.main, "branch", "move-dedicated-swap-publication-second")
	runGit(t, f.main, "branch", "move-dedicated-swap-publication-unrelated")
	runGit(t, f.main, "worktree", "add", secondRoot, "move-dedicated-swap-publication-second")
	runGit(t, f.main, "worktree", "add", unrelatedRoot, "move-dedicated-swap-publication-unrelated")
	_, err = f.lc.ReconcileFamily(ctx, f.familyID)
	require.NoError(t, err)
	second, err := f.lc.Register(
		ctx, config.RepoEntry{Path: secondRoot}, TrackSourceCLI,
	)
	require.NoError(t, err)

	checkouts, err := f.catalog.ListCheckouts(ctx, f.familyID)
	require.NoError(t, err)
	var unrelated store_sqlite.Checkout
	for _, checkout := range checkouts {
		if coordinatorRootEqual(checkout.RootPath, unrelatedRoot) {
			unrelated = checkout
			break
		}
	}
	require.NotEmpty(t, unrelated.CheckoutID)
	require.Equal(t, store_sqlite.CheckoutModeAutomatic, unrelated.EffectiveMode)
	require.NotNil(t, liveCoordinatorOrNil(f.lc, first.CheckoutID))
	require.NotNil(t, liveCoordinatorOrNil(f.lc, second.CheckoutID))
	require.NotNil(t, liveCoordinatorOrNil(f.lc, unrelated.CheckoutID))

	firstGraphBefore, found, err := f.catalog.GetDedicatedGraph(ctx, first.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	secondGraphBefore, found, err := f.catalog.GetDedicatedGraph(ctx, second.GraphID)
	require.NoError(t, err)
	require.True(t, found)

	temporaryRoot := filepath.Join(f.dir, "move-dedicated-swap-publication-temporary")
	runGit(t, f.main, "worktree", "move", f.worktree, temporaryRoot)
	runGit(t, f.main, "worktree", "move", secondRoot, f.worktree)
	runGit(t, f.main, "worktree", "move", temporaryRoot, secondRoot)
	report, err := f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	requireMoveReport(t, report, first.CheckoutID, f.worktree, secondRoot)
	requireMoveReport(t, report, second.CheckoutID, secondRoot, f.worktree)

	f.lc.dropCoordinator(unrelated.CheckoutID)
	require.Nil(t, liveCoordinatorOrNil(f.lc, unrelated.CheckoutID))
	phases := make([]string, 0, 4)
	f.lc.moveComponentBarrier = func(phase string, ids []string) {
		if len(ids) == 2 {
			phases = append(phases, phase)
		}
	}
	publishOrder := make([]string, 0, 6)
	blockedID := ""
	f.lc.moveShellPublishBarrier = func(checkoutID string) error {
		publishOrder = append(publishOrder, checkoutID)
		if blockedID == "" && len(publishOrder) == 2 {
			blockedID = checkoutID
		}
		if checkoutID == blockedID {
			return fmt.Errorf("injected second shell publication failure")
		}
		return nil
	}
	var buildAdmissions atomic.Int64
	f.lc.indexBarrier = func() { buildAdmissions.Add(1) }

	err = f.lc.applyReconcileReport(ctx, report)
	require.Error(t, err)
	require.Contains(t, err.Error(), "injected second shell publication failure")
	require.Equal(t, []string{"discovered", "quiesced", "revalidated"}, phases)
	require.Len(t, publishOrder, 4,
		"recovery must retry every shell before considering coordinator admission")
	require.NotEmpty(t, blockedID)
	require.Nil(t, liveCoordinatorOrNil(f.lc, first.CheckoutID))
	require.Nil(t, liveCoordinatorOrNil(f.lc, second.CheckoutID))
	require.NotNil(t, liveCoordinatorOrNil(f.lc, unrelated.CheckoutID),
		"an unresolved component blocked unrelated same-family admission")
	require.Zero(t, buildAdmissions.Load(),
		"failed swap admitted a build at an old or peer-owned root")

	prefixByID := map[string]string{
		first.CheckoutID:  first.Prefix,
		second.CheckoutID: second.Prefix,
	}
	oldRootByID := map[string]string{
		first.CheckoutID:  f.worktree,
		second.CheckoutID: secondRoot,
	}
	targetRootByID := map[string]string{
		first.CheckoutID:  secondRoot,
		second.CheckoutID: f.worktree,
	}
	publishedID := publishOrder[0]
	publishedMeta := f.mi.GetMetadata(prefixByID[publishedID])
	require.NotNil(t, publishedMeta)
	require.True(t, coordinatorRootEqual(
		publishedMeta.RootPath, targetRootByID[publishedID],
	))
	blockedMeta := f.mi.GetMetadata(prefixByID[blockedID])
	require.NotNil(t, blockedMeta)
	require.True(t, coordinatorRootEqual(
		blockedMeta.RootPath, oldRootByID[blockedID],
	))
	_, firstRouted := f.routeOf(first.CheckoutID)
	_, secondRouted := f.routeOf(second.CheckoutID)
	require.True(t, firstRouted)
	require.True(t, secondRouted)
	_, firstPending, err := f.catalog.GetCheckoutRootMove(ctx, first.CheckoutID)
	require.NoError(t, err)
	require.True(t, firstPending)
	_, secondPending, err := f.catalog.GetCheckoutRootMove(ctx, second.CheckoutID)
	require.NoError(t, err)
	require.True(t, secondPending)

	f.lc.moveShellPublishBarrier = nil
	f.lc.moveComponentBarrier = nil
	require.NoError(t, f.lc.applyReconcileReport(ctx, report))
	for checkoutID, targetRoot := range targetRootByID {
		coordinator := liveCoordinatorOrNil(f.lc, checkoutID)
		require.NotNil(t, coordinator)
		require.True(t, coordinatorRootEqual(coordinator.root, targetRoot))
		meta := f.mi.GetMetadata(prefixByID[checkoutID])
		require.NotNil(t, meta)
		require.True(t, coordinatorRootEqual(meta.RootPath, targetRoot))
		requireNoRootMoveJournal(t, f.catalog, checkoutID)
	}
	require.True(t, configNamesRoot(t, f.cfgPath, first.Prefix, secondRoot))
	require.True(t, configNamesRoot(t, f.cfgPath, second.Prefix, f.worktree))
	firstGraphAfter, found, err := f.catalog.GetDedicatedGraph(ctx, first.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	secondGraphAfter, found, err := f.catalog.GetDedicatedGraph(ctx, second.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, firstGraphBefore.ActiveGenerationID, firstGraphAfter.ActiveGenerationID)
	require.Equal(t, secondGraphBefore.ActiveGenerationID, secondGraphAfter.ActiveGenerationID)
	require.Zero(t, buildAdmissions.Load())
}

func TestDedicatedAutomaticSwapQuiescesWholeComponent(t *testing.T) {
	t.Parallel()
	f := newFamilyFixture(t, "move-dedicated-automatic-swap")
	defer f.close()
	ctx := context.Background()
	f.runCoordinator(f.automatic.CheckoutID)

	dedicatedRoot := filepath.Join(f.dir, "move-dedicated-automatic-second")
	runGit(t, f.main, "branch", "move-dedicated-automatic-second")
	runGit(t, f.main, "worktree", "add", dedicatedRoot, "move-dedicated-automatic-second")
	_, err := f.lc.ReconcileFamily(ctx, f.familyID)
	require.NoError(t, err)
	dedicated, err := f.lc.Register(
		ctx, config.RepoEntry{Path: dedicatedRoot}, TrackSourceCLI,
	)
	require.NoError(t, err)
	require.NotNil(t, liveCoordinatorOrNil(f.lc, dedicated.CheckoutID))

	participantIDs := map[string]struct{}{
		f.automatic.CheckoutID: {},
		dedicated.CheckoutID:   {},
	}
	phases := make([]string, 0, 6)
	f.lc.moveComponentBarrier = func(phase string, ids []string) {
		if len(ids) != 2 {
			return
		}
		for _, id := range ids {
			if _, participant := participantIDs[id]; !participant {
				return
			}
		}
		phases = append(phases, phase)
		switch phase {
		case "quiesced", "revalidated", "published":
			require.Nil(t, liveCoordinatorOrNil(f.lc, f.automatic.CheckoutID), phase)
			require.Nil(t, liveCoordinatorOrNil(f.lc, dedicated.CheckoutID), phase)
		case "reinstalled":
			require.NotNil(t, liveCoordinatorOrNil(f.lc, f.automatic.CheckoutID), phase)
			require.NotNil(t, liveCoordinatorOrNil(f.lc, dedicated.CheckoutID), phase)
		}
	}
	var buildAdmissions atomic.Int64
	f.lc.indexBarrier = func() { buildAdmissions.Add(1) }

	temporaryRoot := filepath.Join(f.dir, "move-dedicated-automatic-temporary")
	runGit(t, f.main, "worktree", "move", f.worktree, temporaryRoot)
	runGit(t, f.main, "worktree", "move", dedicatedRoot, f.worktree)
	runGit(t, f.main, "worktree", "move", temporaryRoot, dedicatedRoot)
	_, err = f.lc.ReconcileFamily(ctx, f.familyID)
	require.NoError(t, err)

	require.Equal(t,
		[]string{"discovered", "quiesced", "revalidated", "published", "reinstalled", "completed"},
		phases,
	)
	automatic, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, coordinatorRootEqual(automatic.RootPath, dedicatedRoot))
	dedicatedCheckout, found, err := f.catalog.GetCheckout(ctx, dedicated.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, coordinatorRootEqual(dedicatedCheckout.RootPath, f.worktree))
	require.True(t, configNamesRoot(t, f.cfgPath, dedicated.Prefix, f.worktree))
	require.Zero(t, buildAdmissions.Load())
	requireNoRootMoveJournal(t, f.catalog, automatic.CheckoutID)
	requireNoRootMoveJournal(t, f.catalog, dedicated.CheckoutID)
}

func TestFailedMoveComponentDoesNotBlockDisjointMove(t *testing.T) {
	f := newFamilyFixture(t, "move-component-no-head-of-line")
	defer f.close()
	ctx := context.Background()
	first, err := f.lc.Register(
		ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
	)
	require.NoError(t, err)

	secondRoot := filepath.Join(f.dir, "move-component-healthy-second")
	runGit(t, f.main, "branch", "move-component-healthy-second")
	runGit(t, f.main, "worktree", "add", secondRoot, "move-component-healthy-second")
	_, err = f.lc.ReconcileFamily(ctx, f.familyID)
	require.NoError(t, err)
	second, err := f.lc.Register(
		ctx, config.RepoEntry{Path: secondRoot}, TrackSourceCLI,
	)
	require.NoError(t, err)

	global := f.cm.Global()
	global.Repos = removeConfigPath(global.Repos, f.worktree)
	require.NoError(t, global.Save())

	firstMoved := filepath.Join(f.dir, "move-component-broken-target")
	secondMoved := filepath.Join(f.dir, "move-component-healthy-target")
	runGit(t, f.main, "worktree", "move", f.worktree, firstMoved)
	runGit(t, f.main, "worktree", "move", secondRoot, secondMoved)
	_, err = f.lc.ReconcileFamily(ctx, f.familyID)
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrRepoRelocationSourceMissing)

	firstMove, firstPending, journalErr := f.catalog.GetCheckoutRootMove(ctx, first.CheckoutID)
	require.NoError(t, journalErr)
	require.True(t, firstPending)
	require.True(t, coordinatorRootEqual(firstMove.CurrentRootPath, firstMoved))
	requireNoRootMoveJournal(t, f.catalog, second.CheckoutID)
	require.True(t, configNamesRoot(t, f.cfgPath, second.Prefix, secondMoved))
	secondMeta := f.mi.GetMetadata(second.Prefix)
	require.NotNil(t, secondMeta)
	require.True(t, coordinatorRootEqual(secondMeta.RootPath, secondMoved))
}

func TestFailedMoveComponentRetriesEveryParticipantFamily(t *testing.T) {
	f := newFamilyFixture(t, "move-component-family-retries")
	defer f.close()
	component := []checkoutRootMoveParticipant{
		{checkout: store_sqlite.Checkout{CheckoutID: "retry-a", FamilyID: "family-a"}},
		{checkout: store_sqlite.Checkout{CheckoutID: "retry-b", FamilyID: "family-b"}},
		{checkout: store_sqlite.Checkout{CheckoutID: "retry-a-duplicate", FamilyID: "family-a"}},
	}
	f.lc.scheduleCheckoutMoveComponentRetries(component)
	f.lc.retryMu.Lock()
	_, familyA := f.lc.familyRetries["family-a"]
	_, familyB := f.lc.familyRetries["family-b"]
	count := len(f.lc.familyRetries)
	f.lc.retryMu.Unlock()
	require.True(t, familyA)
	require.True(t, familyB)
	require.Equal(t, 2, count)
}

type moveTeardownFailWatcher struct {
	removeErr   error
	removeCalls atomic.Int64
	addCalls    atomic.Int64
}

func (w *moveTeardownFailWatcher) AddRepo(string, config.WatchConfig) error {
	w.addCalls.Add(1)
	return nil
}

func (w *moveTeardownFailWatcher) RemoveRepo(string) error {
	w.removeCalls.Add(1)
	return w.removeErr
}

func TestMoveComponentWatcherTeardownFailureAbortsBeforePublish(t *testing.T) {
	t.Parallel()
	f := newFamilyFixture(t, "move-component-watcher-teardown")
	defer f.close()
	ctx := context.Background()
	tracked, err := f.lc.Register(
		ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
	)
	require.NoError(t, err)
	beforeRoute, routed := f.routeOf(tracked.CheckoutID)
	require.True(t, routed)
	beforeCoordinator := liveCoordinatorOrNil(f.lc, tracked.CheckoutID)
	require.NotNil(t, beforeCoordinator)
	beforeGraph, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, found)

	movedRoot := filepath.Join(f.dir, "watcher-teardown-target")
	runGit(t, f.main, "worktree", "move", f.worktree, movedRoot)
	report, err := f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	requireMoveReport(t, report, tracked.CheckoutID, f.worktree, movedRoot)

	originalWatcher := f.lc.watcher()
	failingWatcher := &moveTeardownFailWatcher{removeErr: fmt.Errorf("injected watcher teardown failure")}
	f.lc.SetWatcherSource(func() RepoWatcher { return failingWatcher })
	t.Cleanup(func() {
		f.lc.SetWatcherSource(func() RepoWatcher { return originalWatcher })
	})
	phases := make([]string, 0, 2)
	f.lc.moveComponentBarrier = func(phase string, _ []string) {
		phases = append(phases, phase)
	}
	var buildAdmissions atomic.Int64
	f.lc.indexBarrier = func() { buildAdmissions.Add(1) }

	err = f.lc.applyReconcileReport(ctx, report)
	require.Error(t, err)
	require.Contains(t, err.Error(), "injected watcher teardown failure")
	require.Equal(t, []string{"discovered"}, phases,
		"teardown failure crossed the publish barrier")
	require.EqualValues(t, 1, failingWatcher.removeCalls.Load())
	require.Zero(t, failingWatcher.addCalls.Load(),
		"a watcher that never retired was spuriously reinstalled")
	require.Same(t, beforeCoordinator, liveCoordinatorOrNil(f.lc, tracked.CheckoutID),
		"teardown failure replaced the still-coherent coordinator")
	require.Zero(t, buildAdmissions.Load(), "teardown failure admitted a physical build")
	meta := f.mi.GetMetadata(tracked.Prefix)
	require.NotNil(t, meta)
	require.True(t, coordinatorRootEqual(meta.RootPath, f.worktree),
		"runtime shell published despite watcher teardown failure")
	afterRoute, routed := f.routeOf(tracked.CheckoutID)
	require.True(t, routed)
	require.Equal(t, beforeRoute, afterRoute)
	afterGraph, found, graphErr := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, graphErr)
	require.True(t, found)
	require.Equal(t, beforeGraph.ActiveGenerationID, afterGraph.ActiveGenerationID)
	_, pending, journalErr := f.catalog.GetCheckoutRootMove(ctx, tracked.CheckoutID)
	require.NoError(t, journalErr)
	require.True(t, pending, "teardown failure consumed the retry journal")

	f.lc.SetWatcherSource(func() RepoWatcher { return originalWatcher })
	f.lc.moveComponentBarrier = nil
	require.NoError(t, f.lc.applyReconcileReport(ctx, report))
	meta = f.mi.GetMetadata(tracked.Prefix)
	require.NotNil(t, meta)
	require.True(t, coordinatorRootEqual(meta.RootPath, movedRoot))
	requireNoRootMoveJournal(t, f.catalog, tracked.CheckoutID)
}

func TestSeedAutomaticMoveDoesNotSuppressExplicitOldRootReplacement(t *testing.T) {
	t.Parallel()
	f := newFamilyFixture(t, "move-automatic-seed-old-root-replacement")
	defer f.close()
	ctx := context.Background()

	movedRoot := filepath.Join(f.dir, "automatic-seed-moved")
	runGit(t, f.main, "worktree", "move", f.worktree, movedRoot)
	report, err := f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	requireMoveReport(t, report, f.automatic.CheckoutID, f.worktree, movedRoot)
	_, pending, err := f.catalog.GetCheckoutRootMove(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, pending)

	replacement := f.gitRepo("automatic-old-root-replacement-source")
	require.NoError(t, os.Rename(replacement, f.worktree))
	const replacementPrefix = "automatic-old-root-replacement"
	require.NoError(t, f.cm.Global().AddRepo(config.RepoEntry{
		Path: f.worktree,
		Name: replacementPrefix,
	}))
	require.NoError(t, f.cm.Global().Save())
	staleConfigRoots, err := f.lc.preparePendingRootMovesForSeed(ctx)
	require.NoError(t, err)
	for _, staleRoot := range staleConfigRoots {
		require.False(t, coordinatorRootEqual(staleRoot, f.worktree),
			"automatic physical history became config suppression authority")
	}
	_, pending, err = f.catalog.GetCheckoutRootMove(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, pending,
		"config preparation consumed the independent automatic move journal")

	f.restart()
	require.NoError(t, f.lc.Seed(ctx))
	var replacementMeta *RepoMetadata
	f.mi.mu.RLock()
	for _, meta := range f.mi.repos {
		if meta != nil && coordinatorRootEqual(meta.RootPath, f.worktree) {
			replacementMeta = meta
			break
		}
	}
	f.mi.mu.RUnlock()
	require.NotNil(t, replacementMeta,
		"automatic move history suppressed a new explicit old-root registration")
	replacementGraph, graphFound, err := f.catalog.GetDedicatedGraph(
		ctx, GraphIDFor(replacementMeta.RepoPrefix),
	)
	require.NoError(t, err)
	require.True(t, graphFound)
	require.NotEmpty(t, replacementGraph.OwnerCheckoutID)
	requireActivePathIntent(t, f.catalog, replacementGraph.OwnerCheckoutID,
		TrackSourceConfig, f.worktree)
	moved, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, coordinatorRootEqual(moved.RootPath, movedRoot))
	requireNoRootMoveJournal(t, f.catalog, moved.CheckoutID)
	families, err := f.catalog.ListRepositoryFamilies(ctx)
	require.NoError(t, err)
	require.Len(t, families, 2, "explicit replacement repository did not get its own family")
}

func TestSeedRecoversAutomaticMoveWithoutBuildOrIdentityChurn(t *testing.T) {
	f := newFamilyFixture(t, "move-automatic-restart")
	defer f.close()
	ctx := context.Background()
	f.runCoordinator(f.automatic.CheckoutID)
	beforeRoute, routed := f.routeOf(f.automatic.CheckoutID)
	require.True(t, routed)
	primaryBefore, found, err := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	require.NoError(t, err)
	require.True(t, found)
	beforeSource := requireCheckoutSourceRegistration(t, f.lc, f.automatic.CheckoutID)
	beforeIdentity := f.automatic

	movedRoot := filepath.Join(f.dir, "automatic-restart-renamed")
	runGit(t, f.main, "worktree", "move", f.worktree, movedRoot)
	report, err := f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	requireMoveReport(t, report, f.automatic.CheckoutID, f.worktree, movedRoot)
	if _, pending, err := f.catalog.GetCheckoutRootMove(ctx, f.automatic.CheckoutID); err != nil || !pending {
		t.Fatalf("crash-cut automatic journal = pending %t, err %v", pending, err)
	}

	f.restart()
	select {
	case <-beforeSource.done:
	default:
		t.Fatal("restart did not retire the old automatic source watcher")
	}
	var buildAdmissions atomic.Int64
	f.lc.indexBarrier = func() { buildAdmissions.Add(1) }
	require.NoError(t, f.lc.Seed(ctx))

	after, found, err := f.catalog.GetCheckout(ctx, beforeIdentity.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, beforeIdentity.CheckoutID, after.CheckoutID)
	require.Equal(t, beforeIdentity.Incarnation, after.Incarnation)
	require.True(t, coordinatorRootEqual(after.RootPath, movedRoot))
	coordinator := liveCoordinatorOrNil(f.lc, after.CheckoutID)
	require.NotNil(t, coordinator)
	require.True(t, coordinator.Running())
	require.True(t, coordinatorRootEqual(coordinator.root, movedRoot))
	afterSource := requireCheckoutSourceRegistration(t, f.lc, after.CheckoutID)
	require.True(t, coordinatorRootEqual(afterSource.identity.requestedRoot, movedRoot))
	require.Same(t, coordinator, afterSource.identity.coordinator)
	afterRoute, routed := f.routeOf(after.CheckoutID)
	require.True(t, routed)
	require.Equal(t, beforeRoute, afterRoute)
	primaryAfter, found, err := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, primaryBefore.ActiveGenerationID, primaryAfter.ActiveGenerationID)
	require.Zero(t, buildAdmissions.Load(), "startup move repair admitted a physical build")
	requireNoRootMoveJournal(t, f.catalog, after.CheckoutID)
}

func TestSeedRecoversPreparedConfigCrashCuts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		commitConfig bool
	}{
		{name: "before-atomic-save"},
		{name: "after-atomic-save-before-ack", commitConfig: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFamilyFixture(t, "move-prepared-crash-"+tc.name)
			defer f.close()
			ctx := context.Background()
			tracked, err := f.lc.Register(
				ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
			)
			require.NoError(t, err)
			movedRoot := filepath.Join(f.dir, "prepared-crash-renamed")
			runGit(t, f.main, "worktree", "move", f.worktree, movedRoot)
			_, err = f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
			require.NoError(t, err)
			checkout, found, err := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
			require.NoError(t, err)
			require.True(t, found)
			batch, err := f.cm.PrepareRepoRelocationBatch([]config.RepoRelocation{{
				ID: tracked.CheckoutID, ConfigRoot: f.worktree,
				CurrentRoot: movedRoot, Prefix: tracked.Prefix,
				Sources: config.RepoRelocationSources{TopLevel: true},
			}})
			require.NoError(t, err)
			require.NoError(t, f.catalog.PrepareCheckoutRootMoveConfig(
				ctx, tracked.CheckoutID, checkout.Incarnation,
				f.worktree, movedRoot, batch.BeforeHash(), batch.AfterHash(),
			))
			if tc.commitConfig {
				committed, commitErr := f.cm.CommitRepoRelocationBatch(batch)
				require.NoError(t, commitErr)
				require.True(t, committed)
				require.True(t, configContainsMovedPath(t, f.cfgPath, movedRoot))
			} else {
				require.True(t, configContainsMovedPath(t, f.cfgPath, f.worktree))
			}

			f.restart()
			var buildAdmissions atomic.Int64
			f.lc.indexBarrier = func() { buildAdmissions.Add(1) }
			require.NoError(t, f.lc.Seed(ctx))
			require.True(t, configContainsMovedPath(t, f.cfgPath, movedRoot))
			require.False(t, configContainsMovedPath(t, f.cfgPath, f.worktree))
			require.Zero(t, buildAdmissions.Load())
			requireNoRootMoveJournal(t, f.catalog, tracked.CheckoutID)
		})
	}
}

func TestDedicatedWorktreeMoveConfigFailureKeepsJournalButRebindsRuntime(t *testing.T) {
	f := newFamilyFixture(t, "move-config-failure")
	defer f.close()
	ctx := context.Background()
	tracked, err := f.lc.Register(
		ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
	)
	require.NoError(t, err)
	beforeRoute, routed := f.routeOf(tracked.CheckoutID)
	require.True(t, routed)

	movedRoot := filepath.Join(f.dir, "config-failure-renamed")
	runGit(t, f.main, "worktree", "move", f.worktree, movedRoot)
	// Renaming an atomic temp file over an existing directory fails on every
	// supported platform without modifying the real durable config.
	f.cm.Global().SetConfigPath(f.dir)
	_, err = f.lc.ReconcileFamily(ctx, f.familyID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "moved repository config")

	meta := f.mi.GetMetadata(tracked.Prefix)
	require.NotNil(t, meta)
	require.True(t, coordinatorRootEqual(meta.RootPath, movedRoot),
		"config persistence must not roll runtime back to the dead root")
	coordinator := liveCoordinatorOrNil(f.lc, tracked.CheckoutID)
	require.NotNil(t, coordinator)
	require.True(t, coordinatorRootEqual(coordinator.root, movedRoot))
	afterFailureRoute, routed := f.routeOf(tracked.CheckoutID)
	require.True(t, routed)
	require.Equal(t, beforeRoute, afterFailureRoute)
	require.True(t, configContainsMovedPath(t, f.cfgPath, f.worktree))
	requireActivePathIntent(t, f.catalog, tracked.CheckoutID, TrackSourceCLI, f.worktree)
	if _, pending, journalErr := f.catalog.GetCheckoutRootMove(ctx, tracked.CheckoutID); journalErr != nil || !pending {
		t.Fatalf("failed config journal = pending %t, err %v", pending, journalErr)
	}

	f.cm.Global().SetConfigPath(f.cfgPath)
	_, err = f.lc.ReconcileFamily(ctx, f.familyID)
	require.NoError(t, err)
	require.True(t, f.configLists(movedRoot))
	requireActivePathIntent(t, f.catalog, tracked.CheckoutID, TrackSourceCLI, movedRoot)
	requireNoRootMoveJournal(t, f.catalog, tracked.CheckoutID)
}

func TestDedicatedWorktreeMoveAToBToCConvergesNewestRootFromStaleReport(t *testing.T) {
	f := newFamilyFixture(t, "move-a-b-c")
	defer f.close()
	ctx := context.Background()
	tracked, err := f.lc.Register(
		ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
	)
	require.NoError(t, err)
	beforeRoute, routed := f.routeOf(tracked.CheckoutID)
	require.True(t, routed)

	rootB := filepath.Join(f.dir, "move-chain-b")
	rootC := filepath.Join(f.dir, "move-chain-c")
	runGit(t, f.main, "worktree", "move", f.worktree, rootB)
	reportB, err := f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	requireMoveReport(t, reportB, tracked.CheckoutID, f.worktree, rootB)
	runGit(t, f.main, "worktree", "move", rootB, rootC)
	reportC, err := f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	requireMoveReport(t, reportC, tracked.CheckoutID, rootB, rootC)

	// The delayed A -> B report must consume the current B -> C marker and
	// converge to C. Its own stale target is evidence only, never authority.
	require.NoError(t, f.lc.applyReconcileReport(ctx, reportB))
	checkout, found, err := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, coordinatorRootEqual(checkout.RootPath, rootC))
	meta := f.mi.GetMetadata(tracked.Prefix)
	require.NotNil(t, meta)
	require.True(t, coordinatorRootEqual(meta.RootPath, rootC))
	require.True(t, f.configLists(rootC),
		"the still-at-A config is found through the active path intent")
	requireActivePathIntent(t, f.catalog, tracked.CheckoutID, TrackSourceCLI, rootC)
	afterRoute, routed := f.routeOf(tracked.CheckoutID)
	require.True(t, routed)
	require.Equal(t, beforeRoute, afterRoute)
	requireNoRootMoveJournal(t, f.catalog, tracked.CheckoutID)
}

func TestDedicatedAliasSpellingDoesNotCreateMoveOrRewriteConfig(t *testing.T) {
	f := newFamilyFixture(t, "move-alias")
	defer f.close()
	ctx := context.Background()
	tracked, err := f.lc.Register(
		ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
	)
	require.NoError(t, err)

	aliasParent := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(f.dir, aliasParent))
	aliasRoot := filepath.Join(aliasParent, filepath.Base(f.worktree))
	checkout, found, err := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, f.catalog.UpdateCheckoutObservation(
		ctx, moveObservationFromCheckout(checkout, aliasRoot),
	))
	requireNoRootMoveJournal(t, f.catalog, tracked.CheckoutID)
	configBefore, err := os.ReadFile(f.cfgPath)
	require.NoError(t, err)

	report, err := f.lc.ReconcileFamily(ctx, f.familyID)
	require.NoError(t, err)
	for _, observed := range report.Checkouts {
		if observed.CheckoutID == tracked.CheckoutID {
			require.False(t, observed.RootMoved, "alias-only spelling was classified as a move")
		}
	}
	requireNoRootMoveJournal(t, f.catalog, tracked.CheckoutID)
	configAfter, err := os.ReadFile(f.cfgPath)
	require.NoError(t, err)
	require.Equal(t, configBefore, configAfter, "alias-only observation rewrote durable config")
}

func TestMoveConfigNoMatchKeepsJournalAndSeedDefersAmbiguousSource(t *testing.T) {
	f := newFamilyFixture(t, "move-config-no-match")
	defer f.close()
	ctx := context.Background()
	tracked, err := f.lc.Register(
		ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
	)
	require.NoError(t, err)
	unrelated := f.gitRepo("move-config-no-match-unrelated")
	global := f.cm.Global()
	global.Repos = removeConfigPath(global.Repos, f.worktree)
	global.Repos = append(global.Repos, config.RepoEntry{
		Path: unrelated, Name: "healthy-unrelated",
	})
	require.NoError(t, global.Save())

	movedRoot := filepath.Join(f.dir, "move-config-no-match-renamed")
	runGit(t, f.main, "worktree", "move", f.worktree, movedRoot)
	_, err = f.lc.ReconcileFamily(ctx, f.familyID)
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrRepoRelocationSourceMissing)
	meta := f.mi.GetMetadata(tracked.Prefix)
	require.NotNil(t, meta)
	require.True(t, coordinatorRootEqual(meta.RootPath, movedRoot),
		"runtime still converges before a config inconsistency")
	require.True(t, configContainsMovedPath(t, f.cfgPath, unrelated))
	if _, pending, journalErr := f.catalog.GetCheckoutRootMove(ctx, tracked.CheckoutID); journalErr != nil || !pending {
		t.Fatalf("no-match journal = pending %t, err %v", pending, journalErr)
	}

	f.restart()
	err = f.lc.Seed(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrRepoRelocationSourceMissing)
	if _, pending, journalErr := f.catalog.GetCheckoutRootMove(ctx, tracked.CheckoutID); journalErr != nil || !pending {
		t.Fatalf("restart no-match journal = pending %t, err %v", pending, journalErr)
	}
	families, listErr := f.catalog.ListRepositoryFamilies(ctx)
	require.NoError(t, listErr)
	require.Len(t, families, 2,
		"one inconsistent move must not block an unrelated healthy registration")
	checkouts, listErr := f.catalog.ListCheckouts(ctx, f.familyID)
	require.NoError(t, listErr)
	require.Len(t, checkouts, 2, "stale moved root seeded a phantom checkout")
	for _, checkout := range checkouts {
		require.False(t, coordinatorRootEqual(checkout.RootPath, f.worktree),
			"stale moved root was seeded as a phantom checkout: %+v", checkout)
	}
	require.True(t, configContainsMovedPath(t, f.cfgPath, unrelated),
		"fail-closed recovery must not rewrite the unrelated same-prefix entry")
}

func TestDisappearingDedicatedMoveRemovesEveryJournalConfigRoot(t *testing.T) {
	for _, tc := range []struct {
		name               string
		configIntermediate bool
	}{
		{name: "config-at-origin"},
		{name: "config-at-intermediate", configIntermediate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFamilyFixture(t, "move-disappears-"+tc.name)
			defer f.close()
			ctx := context.Background()
			tracked, err := f.lc.Register(
				ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
			)
			require.NoError(t, err)
			checkout, found, err := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
			require.NoError(t, err)
			require.True(t, found)

			rootB := filepath.Join(f.dir, "disappearing-b")
			runGit(t, f.main, "worktree", "move", f.worktree, rootB)
			require.NoError(t, f.catalog.UpdateCheckoutObservation(
				ctx, moveObservationFromCheckout(checkout, rootB),
			))
			currentRoot := rootB
			if tc.configIntermediate {
				require.NoError(t, f.catalog.PrepareCheckoutRootMoveConfig(
					ctx, tracked.CheckoutID, checkout.Incarnation, f.worktree, rootB,
					"before-disappearing-intermediate", "after-disappearing-intermediate",
				))
				moved, moveErr := f.cm.RelocateRepoAndSaveIfPresent(
					[]string{f.worktree}, rootB, tracked.Prefix,
					config.RepoRelocationSources{TopLevel: true},
				)
				require.NoError(t, moveErr)
				require.True(t, moved)
				require.NoError(t, f.catalog.AcknowledgeCheckoutRootMoveConfig(
					ctx, tracked.CheckoutID, checkout.Incarnation, f.worktree, rootB,
					"before-disappearing-intermediate", "after-disappearing-intermediate",
				))
				checkout, found, err = f.catalog.GetCheckout(ctx, tracked.CheckoutID)
				require.NoError(t, err)
				require.True(t, found)
				rootC := filepath.Join(f.dir, "disappearing-c")
				runGit(t, f.main, "worktree", "move", rootB, rootC)
				require.NoError(t, f.catalog.UpdateCheckoutObservation(
					ctx, moveObservationFromCheckout(checkout, rootC),
				))
				currentRoot = rootC
			}
			require.NoError(t, f.catalog.UpsertTrackingIntent(ctx, store_sqlite.TrackingIntent{
				IntentID:      "historical-project-move-cleanup",
				CheckoutID:    tracked.CheckoutID,
				SourceKind:    store_sqlite.IntentSourceProjectMembership,
				SourceLocator: "project:already-removed",
				Active:        false,
				CreatedAt:     f.clock.Now().Unix(),
				RevokedAt:     f.clock.Now().Unix(),
			}))

			require.NoError(t, f.catalog.DeleteCheckoutRoute(ctx, tracked.CheckoutID))
			err = (cleanupHooks{l: f.lc}).ReleaseGraph(ctx, reconcile.GraphReleaseTarget{
				GraphID:     tracked.GraphID,
				CheckoutID:  tracked.CheckoutID,
				Incarnation: checkout.Incarnation,
				RepoPrefix:  tracked.Prefix,
				RootPath:    currentRoot,
			}, func() error {
				deleted, deleteErr := f.catalog.DeleteDedicatedGraphForIncarnation(
					ctx, tracked.GraphID, tracked.CheckoutID, checkout.Incarnation,
				)
				if deleteErr != nil {
					return deleteErr
				}
				if !deleted {
					return store_sqlite.ErrCatalogStaleGuard
				}
				return nil
			})
			require.NoError(t, err)
			require.False(t, configContainsMovedPath(t, f.cfgPath, f.worktree))
			require.False(t, configContainsMovedPath(t, f.cfgPath, rootB))
			require.False(t, configContainsMovedPath(t, f.cfgPath, currentRoot))
		})
	}
}

func TestDedicatedMoveDefersWhileDemotionOwnsCheckout(t *testing.T) {
	f := newFamilyFixture(t, "move-during-demotion")
	defer f.close()
	ctx := context.Background()
	tracked, err := f.lc.Register(
		ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI,
	)
	require.NoError(t, err)
	movedRoot := filepath.Join(f.dir, "move-during-demotion-renamed")
	runGit(t, f.main, "worktree", "move", f.worktree, movedRoot)
	report, err := f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	requireMoveReport(t, report, tracked.CheckoutID, f.worktree, movedRoot)
	_, blocked, err := f.catalog.RevokeTrackingIntents(
		ctx, tracked.CheckoutID, f.clock.Now().Unix(),
		[]store_sqlite.IntentSourceKind{TrackSourceCLI},
	)
	require.NoError(t, err)
	require.Empty(t, blocked)
	require.NoError(t, f.catalog.BeginIntentTransition(ctx, store_sqlite.IntentTransition{
		TransitionID:       "move-during-demotion-transition",
		CheckoutID:         tracked.CheckoutID,
		Cause:              "test-demotion-interleave",
		PriorDesiredMode:   store_sqlite.CheckoutModeDedicated,
		PriorEffectiveMode: store_sqlite.CheckoutModeDedicated,
		RequestedMode:      store_sqlite.CheckoutModeAutomatic,
		PriorCheckoutState: store_sqlite.CheckoutStateReady,
		State:              store_sqlite.IntentTransitionPending,
		CreatedAt:          f.clock.Now().Unix(),
		LastProgress:       f.clock.Now().Unix(),
	}))

	err = f.lc.applyReconcileReport(ctx, report)
	require.Error(t, err)
	require.Contains(t, err.Error(), "deferred behind intent transition")
	require.True(t, configContainsMovedPath(t, f.cfgPath, f.worktree),
		"move repair must not clear config after the transition revoked its intent")
	if _, pending, journalErr := f.catalog.GetCheckoutRootMove(ctx, tracked.CheckoutID); journalErr != nil || !pending {
		t.Fatalf("transition-interleaved journal = pending %t, err %v", pending, journalErr)
	}
}

func TestSeedRecoversProjectOnlyMoveBeforeStaleConfigRegistration(t *testing.T) {
	f := newFamilyFixture(t, "move-project-restart")
	defer f.close()
	ctx := context.Background()
	tracked, err := f.lc.Register(
		ctx, config.RepoEntry{Path: f.worktree}, TrackSourceMCP,
	)
	require.NoError(t, err)
	beforeRoute, routed := f.routeOf(tracked.CheckoutID)
	require.True(t, routed)
	graphBefore, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, found)

	revoked, blocked, err := f.catalog.RevokeTrackingIntents(
		ctx, tracked.CheckoutID, f.clock.Now().Unix(), []store_sqlite.IntentSourceKind{TrackSourceMCP},
	)
	require.NoError(t, err)
	require.Empty(t, blocked)
	require.Len(t, revoked, 1)
	require.NoError(t, f.catalog.UpsertTrackingIntent(ctx, store_sqlite.TrackingIntent{
		IntentID:      "project-only-move",
		CheckoutID:    tracked.CheckoutID,
		SourceKind:    store_sqlite.IntentSourceProjectMembership,
		SourceLocator: "project:alpha",
		Active:        true,
		CreatedAt:     f.clock.Now().Unix(),
	}))
	global := f.cm.Global()
	global.Repos = removeConfigPath(global.Repos, f.worktree)
	if global.Projects == nil {
		global.Projects = map[string]config.ProjectConfig{}
	}
	global.Projects["alpha"] = config.ProjectConfig{Repos: []config.RepoEntry{{
		Path: f.worktree, Name: tracked.Prefix,
	}}}
	require.NoError(t, global.Save())

	movedRoot := filepath.Join(f.dir, "project-only-renamed")
	runGit(t, f.main, "worktree", "move", f.worktree, movedRoot)
	// Crash cut: inventory and root+journal commit, but no lifecycle
	// convergence, config save, runtime rebind, or locator relocation.
	report, err := f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	requireMoveReport(t, report, tracked.CheckoutID, f.worktree, movedRoot)
	require.True(t, configContainsMovedPath(t, f.cfgPath, f.worktree))
	if _, pending, err := f.catalog.GetCheckoutRootMove(ctx, tracked.CheckoutID); err != nil || !pending {
		t.Fatalf("crash-cut journal = pending %t, err %v", pending, err)
	}

	f.restart()
	require.NoError(t, f.lc.Seed(ctx))
	require.False(t, configContainsMovedPath(t, f.cfgPath, f.worktree))
	require.True(t, configContainsMovedPath(t, f.cfgPath, movedRoot))
	meta := f.mi.GetMetadata(tracked.Prefix)
	require.NotNil(t, meta)
	require.True(t, coordinatorRootEqual(meta.RootPath, movedRoot))
	requireNoRootMoveJournal(t, f.catalog, tracked.CheckoutID)

	afterRoute, routed := f.routeOf(tracked.CheckoutID)
	require.True(t, routed)
	require.Equal(t, beforeRoute, afterRoute, "restart repair must reuse the published route")
	graphAfter, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, graphBefore.ActiveGenerationID, graphAfter.ActiveGenerationID)
	families, err := f.catalog.ListRepositoryFamilies(ctx)
	require.NoError(t, err)
	require.Len(t, families, 1, "stale A config seeded a phantom family")
	checkouts, err := f.catalog.ListCheckouts(ctx, f.familyID)
	require.NoError(t, err)
	require.Len(t, checkouts, 2, "stale A config seeded a phantom checkout")
	for _, checkout := range checkouts {
		require.False(t, coordinatorRootEqual(checkout.RootPath, f.worktree),
			"catalog retained a phantom checkout at A: %+v", checkout)
	}
}

func TestSeedRecoversProjectOnlyMoveWhoseConfigReachedIntermediateRoot(t *testing.T) {
	f := newFamilyFixture(t, "move-project-intermediate")
	defer f.close()
	ctx := context.Background()
	tracked, err := f.lc.Register(
		ctx, config.RepoEntry{Path: f.worktree}, TrackSourceMCP,
	)
	require.NoError(t, err)
	makeCheckoutProjectOnly(t, f, tracked, "beta")

	rootB := filepath.Join(f.dir, "project-intermediate-b")
	rootC := filepath.Join(f.dir, "project-intermediate-c")
	rootD := filepath.Join(f.dir, "project-intermediate-d")
	runGit(t, f.main, "worktree", "move", f.worktree, rootB)
	_, err = f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	// Simulate the crash cut after config publication but before runtime,
	// intent, and journal completion.
	_, err = f.lc.preparePendingRootMovesForSeed(ctx)
	require.NoError(t, err)
	require.True(t, configContainsMovedPath(t, f.cfgPath, rootB))
	moveB, pending, err := f.catalog.GetCheckoutRootMove(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, pending)
	require.True(t, coordinatorRootEqual(moveB.PreviousRootPath, f.worktree))
	require.True(t, coordinatorRootEqual(moveB.LatestPreviousRootPath, f.worktree))
	require.True(t, coordinatorRootEqual(moveB.ConfigRootPath, rootB))

	runGit(t, f.main, "worktree", "move", rootB, rootC)
	f.cm.Global().SetConfigPath(f.dir)
	_, err = f.lc.ReconcileFamily(ctx, f.familyID)
	require.Error(t, err)
	f.cm.Global().SetConfigPath(f.cfgPath)
	require.True(t, configContainsMovedPath(t, f.cfgPath, rootB),
		"failed C save must retain the acknowledged B config")
	moveC, pending, err := f.catalog.GetCheckoutRootMove(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, pending)
	require.True(t, coordinatorRootEqual(moveC.PreviousRootPath, f.worktree),
		"journal lost earliest origin: %+v", moveC)
	require.True(t, coordinatorRootEqual(moveC.LatestPreviousRootPath, rootB),
		"journal lost exact intermediate recovery token: %+v", moveC)
	require.True(t, coordinatorRootEqual(moveC.ConfigRootPath, rootB),
		"failed config save advanced durable config token: %+v", moveC)
	require.True(t, coordinatorRootEqual(moveC.CurrentRootPath, rootC))

	runGit(t, f.main, "worktree", "move", rootC, rootD)
	_, err = f.lc.Reconciler().ReconcileFamily(ctx, f.familyID, f.main)
	require.NoError(t, err)
	moveD, pending, err := f.catalog.GetCheckoutRootMove(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, pending)
	require.True(t, coordinatorRootEqual(moveD.PreviousRootPath, f.worktree))
	require.True(t, coordinatorRootEqual(moveD.LatestPreviousRootPath, rootC))
	require.True(t, coordinatorRootEqual(moveD.ConfigRootPath, rootB))
	require.True(t, coordinatorRootEqual(moveD.CurrentRootPath, rootD))

	f.restart()
	require.NoError(t, f.lc.Seed(ctx))
	require.False(t, configContainsMovedPath(t, f.cfgPath, rootB))
	require.False(t, configContainsMovedPath(t, f.cfgPath, rootC))
	require.True(t, configContainsMovedPath(t, f.cfgPath, rootD))
	meta := f.mi.GetMetadata(tracked.Prefix)
	require.NotNil(t, meta)
	require.True(t, coordinatorRootEqual(meta.RootPath, rootD))
	requireNoRootMoveJournal(t, f.catalog, tracked.CheckoutID)
	families, err := f.catalog.ListRepositoryFamilies(ctx)
	require.NoError(t, err)
	require.Len(t, families, 1)
	checkouts, err := f.catalog.ListCheckouts(ctx, f.familyID)
	require.NoError(t, err)
	require.Len(t, checkouts, 2)
}

func TestPartitionCheckoutRootMoveComponentsPreservesCanonicalAndPreparedEdges(t *testing.T) {
	physical := t.TempDir()
	alias := physical + "-alias"
	require.NoError(t, os.Symlink(physical, alias))
	participants := []checkoutRootMoveParticipant{
		{move: store_sqlite.CheckoutRootMove{CheckoutID: "physical", CurrentRootPath: physical}},
		{move: store_sqlite.CheckoutRootMove{CheckoutID: "alias", ConfigRootPath: alias}},
		{move: store_sqlite.CheckoutRootMove{
			CheckoutID: "prepared-a", CurrentRootPath: "/benchmark/prepared-a",
			ConfigPreparedBeforeHash: "before", ConfigPreparedAfterHash: "after",
		}},
		{move: store_sqlite.CheckoutRootMove{
			CheckoutID: "prepared-b", CurrentRootPath: "/benchmark/prepared-b",
			ConfigPreparedBeforeHash: "before", ConfigPreparedAfterHash: "after",
		}},
		{move: store_sqlite.CheckoutRootMove{CheckoutID: "disjoint", CurrentRootPath: "/benchmark/disjoint"}},
	}
	components := partitionCheckoutRootMoveComponents(participants)
	require.Len(t, components, 3)
	componentByCheckout := make(map[string]int)
	for index, component := range components {
		for _, participant := range component {
			componentByCheckout[participant.move.CheckoutID] = index
		}
	}
	require.Equal(t, componentByCheckout["physical"], componentByCheckout["alias"])
	require.Equal(t, componentByCheckout["prepared-a"], componentByCheckout["prepared-b"])
	require.NotEqual(t, componentByCheckout["physical"], componentByCheckout["prepared-a"])
	require.NotEqual(t, componentByCheckout["physical"], componentByCheckout["disjoint"])
	require.NotEqual(t, componentByCheckout["prepared-a"], componentByCheckout["disjoint"])
}

func BenchmarkCheckoutMovePartition(b *testing.B) {
	makeParticipants := func(participantCount int, sharedPreparedHash bool) ([]checkoutRootMoveParticipant, []dedicatedCheckoutMove) {
		participants := make([]checkoutRootMoveParticipant, 0, participantCount)
		dedicated := make([]dedicatedCheckoutMove, 0, participantCount)
		for i := 0; i < participantCount; i++ {
			move := store_sqlite.CheckoutRootMove{
				CheckoutID:      fmt.Sprintf("benchmark-checkout-%03d", i),
				ConfigRootPath:  fmt.Sprintf("/benchmark/source-%03d", i),
				CurrentRootPath: fmt.Sprintf("/benchmark/target-%03d", i),
			}
			if sharedPreparedHash {
				move.ConfigPreparedBeforeHash = "benchmark-before"
				move.ConfigPreparedAfterHash = "benchmark-after"
			}
			participant := checkoutRootMoveParticipant{
				checkout: store_sqlite.Checkout{
					CheckoutID: move.CheckoutID,
					RootPath:   move.CurrentRootPath,
				},
				move: move,
			}
			participants = append(participants, participant)
			dedicated = append(dedicated, dedicatedCheckoutMove{
				checkout: participant.checkout,
				move:     move,
				sources:  config.RepoRelocationSources{TopLevel: true},
			})
		}
		return participants, dedicated
	}
	for _, participantCount := range []int{64, 256} {
		for _, prepared := range []bool{false, true} {
			name := "disjoint"
			if prepared {
				name = "shared_prepared_hash"
			}
			participants, dedicated := makeParticipants(participantCount, prepared)
			b.Run(fmt.Sprintf("component_%d_%s", participantCount, name), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					_ = partitionCheckoutRootMoveComponents(participants)
				}
			})
			b.Run(fmt.Sprintf("legacy_%d_%s", participantCount, name), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					_ = partitionDedicatedMoveComponents(dedicated)
				}
			})
		}
	}
}

func requireMoveReport(
	t *testing.T,
	report reconcile.FamilyReport,
	checkoutID, previousRoot, currentRoot string,
) {
	t.Helper()
	for _, checkout := range report.Checkouts {
		if checkout.CheckoutID != checkoutID {
			continue
		}
		require.True(t, checkout.RootMoved, "checkout report did not mark the root move: %+v", checkout)
		require.True(t, coordinatorRootEqual(checkout.PreviousRootPath, previousRoot),
			"previous root = %q, want %q", checkout.PreviousRootPath, previousRoot)
		require.True(t, coordinatorRootEqual(checkout.RootPath, currentRoot),
			"current root = %q, want %q", checkout.RootPath, currentRoot)
		return
	}
	t.Fatalf("report has no checkout %s: %+v", checkoutID, report.Checkouts)
}

func requireNoRootMoveJournal(t *testing.T, catalog *store_sqlite.Catalog, checkoutID string) {
	t.Helper()
	_, found, err := catalog.GetCheckoutRootMove(context.Background(), checkoutID)
	require.NoError(t, err)
	require.False(t, found)
}

func requireCheckoutSourceRegistration(
	t *testing.T,
	lifecycle *CheckoutLifecycle,
	checkoutID string,
) *checkoutSourceSignalRegistration {
	t.Helper()
	lifecycle.checkoutSignalWatchMu.Lock()
	watchers := lifecycle.checkoutSignalWatchers
	lifecycle.checkoutSignalWatchMu.Unlock()
	require.NotNil(t, watchers, "automatic checkout has no source watcher set")
	watchers.mu.Lock()
	registration := watchers.watchers[checkoutID]
	watchers.mu.Unlock()
	require.NotNil(t, registration, "automatic checkout has no source watcher registration")
	return registration
}

func requireActivePathIntent(
	t *testing.T,
	catalog *store_sqlite.Catalog,
	checkoutID string,
	kind store_sqlite.IntentSourceKind,
	root string,
) {
	t.Helper()
	intents, err := catalog.ListTrackingIntents(context.Background(), checkoutID)
	require.NoError(t, err)
	for _, intent := range intents {
		if intent.Active && intent.SourceKind == kind && coordinatorRootEqual(intent.SourceLocator, root) {
			return
		}
	}
	t.Fatalf("no active %s intent at %s: %+v", kind, root, intents)
}

func configContainsMovedPath(t *testing.T, configPath, root string) bool {
	t.Helper()
	global, err := config.LoadGlobal(configPath)
	require.NoError(t, err)
	for _, entry := range global.Repos {
		if configEntryMatchesRoot(entry, root) {
			return true
		}
	}
	for _, project := range global.Projects {
		for _, entry := range project.Repos {
			if configEntryMatchesRoot(entry, root) {
				return true
			}
		}
	}
	return false
}

func configNamesRoot(t *testing.T, configPath, prefix, root string) bool {
	t.Helper()
	global, err := config.LoadGlobal(configPath)
	require.NoError(t, err)
	for _, entry := range global.Repos {
		if entry.Name == prefix && configEntryMatchesRoot(entry, root) {
			return true
		}
	}
	for _, project := range global.Projects {
		for _, entry := range project.Repos {
			if entry.Name == prefix && configEntryMatchesRoot(entry, root) {
				return true
			}
		}
	}
	return false
}

func configEntryMatchesRoot(entry config.RepoEntry, root string) bool {
	if coordinatorRootEqual(entry.Path, root) {
		return true
	}
	entryInfo, entryErr := os.Stat(entry.Path)
	rootInfo, rootErr := os.Stat(root)
	return entryErr == nil && rootErr == nil && os.SameFile(entryInfo, rootInfo)
}

func removeConfigPath(entries []config.RepoEntry, root string) []config.RepoEntry {
	out := make([]config.RepoEntry, 0, len(entries))
	for _, entry := range entries {
		if !pathkey.EqualPaths(
			pathkey.CanonicalExistingRoot(entry.Path), pathkey.CanonicalExistingRoot(root),
		) {
			out = append(out, entry)
		}
	}
	return out
}

func makeCheckoutProjectOnly(
	t *testing.T,
	f *familyFixture,
	tracked RegisterResult,
	projectName string,
) {
	t.Helper()
	ctx := context.Background()
	revoked, blocked, err := f.catalog.RevokeTrackingIntents(
		ctx, tracked.CheckoutID, f.clock.Now().Unix(), []store_sqlite.IntentSourceKind{TrackSourceMCP},
	)
	require.NoError(t, err)
	require.Empty(t, blocked)
	require.Len(t, revoked, 1)
	require.NoError(t, f.catalog.UpsertTrackingIntent(ctx, store_sqlite.TrackingIntent{
		IntentID:      "project-only-" + projectName,
		CheckoutID:    tracked.CheckoutID,
		SourceKind:    store_sqlite.IntentSourceProjectMembership,
		SourceLocator: "project:" + projectName,
		Active:        true,
		CreatedAt:     f.clock.Now().Unix(),
	}))
	global := f.cm.Global()
	global.Repos = removeConfigPath(global.Repos, f.worktree)
	if global.Projects == nil {
		global.Projects = map[string]config.ProjectConfig{}
	}
	global.Projects[projectName] = config.ProjectConfig{Repos: []config.RepoEntry{{
		Path: f.worktree, Name: tracked.Prefix,
	}}}
	require.NoError(t, global.Save())
}

func moveObservationFromCheckout(
	checkout store_sqlite.Checkout,
	root string,
) store_sqlite.UpdateCheckoutObservationRequest {
	return store_sqlite.UpdateCheckoutObservationRequest{
		CheckoutID:           checkout.CheckoutID,
		Incarnation:          checkout.Incarnation,
		ExpectedRootPath:     checkout.RootPath,
		State:                checkout.State,
		RootPath:             root,
		GitDir:               checkout.GitDir,
		Locked:               checkout.Locked,
		Prunable:             checkout.Prunable,
		HeadRef:              checkout.HeadRef,
		HeadCommit:           checkout.HeadCommit,
		HeadTree:             checkout.HeadTree,
		LastAccessible:       checkout.LastAccessible,
		UnavailableSince:     checkout.UnavailableSince,
		AvailabilityDeadline: checkout.AvailabilityDeadline,
		RemovalDetectedAt:    checkout.RemovalDetectedAt,
		RemovalDeadline:      checkout.RemovalDeadline,
		RemovalEvidence:      checkout.RemovalEvidence,
		LastSeen:             checkout.LastSeen + 1,
		LastError:            checkout.LastError,
	}
}
