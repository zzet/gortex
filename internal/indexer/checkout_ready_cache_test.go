package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestCoordinatorSharesCanonicalCommitGenerationAcrossAutomaticWorktrees(t *testing.T) {
	f := newFamilyFixture(t, "ready-cache-cross-checkout")
	defer f.close()
	ctx := context.Background()

	firstCycle := f.runCoordinator(f.automatic.CheckoutID)
	firstRoute, ok := f.routeOf(f.automatic.CheckoutID)
	require.True(t, ok)
	require.Positive(t, firstRoute.CommitGenerationID)

	secondRoot := f.worktreeOf(f.main, "ready-cache-second")
	secondRoot, err := filepath.EvalSymlinks(secondRoot)
	require.NoError(t, err)
	_, err = f.lc.Sweep(ctx)
	require.NoError(t, err)
	checkouts, err := f.catalog.ListCheckouts(ctx, f.familyID)
	require.NoError(t, err)
	var second store_sqlite.Checkout
	for _, checkout := range checkouts {
		if checkout.RootPath == secondRoot {
			second = checkout
			break
		}
	}
	require.NotEmpty(t, second.CheckoutID, "second root=%q checkouts=%+v", secondRoot, checkouts)
	secondCycle := f.runCoordinator(second.CheckoutID)
	secondRoute, ok := f.routeOf(second.CheckoutID)
	require.True(t, ok)

	require.Equal(t, firstRoute.CommitGenerationID, secondRoute.CommitGenerationID,
		"compatible worktrees must route the same canonical commit payload")
	require.False(t, secondCycle.CommitBuilt,
		"the second checkout must not rebuild a compatible ready payload")
	if firstCycle.CommitBuilt {
		require.True(t, secondCycle.CommitReused)
	}
}

func TestCoordinatorKeepsAdoptedCommitDirtyGenerationStableAcrossPolls(t *testing.T) {
	f := newFamilyFixture(t, "ready-cache-dirty-polls")
	defer f.close()
	ctx := context.Background()

	firstCycle := f.runCoordinator(f.automatic.CheckoutID)
	firstRoute, ok := f.routeOf(f.automatic.CheckoutID)
	require.True(t, ok)
	require.Positive(t, firstRoute.CommitGenerationID)

	secondRoot := f.worktreeOf(f.main, "ready-cache-dirty-second")
	secondRoot, err := filepath.EvalSymlinks(secondRoot)
	require.NoError(t, err)
	_, err = f.lc.Sweep(ctx)
	require.NoError(t, err)
	checkouts, err := f.catalog.ListCheckouts(ctx, f.familyID)
	require.NoError(t, err)
	var second store_sqlite.Checkout
	for _, checkout := range checkouts {
		if checkout.RootPath == secondRoot {
			second = checkout
			break
		}
	}
	require.NotEmpty(t, second.CheckoutID, "second root=%q checkouts=%+v", secondRoot, checkouts)

	dirtyPath := filepath.Join(secondRoot, "poll_dirty.go")
	require.NoError(t, os.WriteFile(dirtyPath, []byte("package fixture\n\nfunc PollDirtyOne() {}\n"), 0o644))
	initial := f.runCoordinator(second.CheckoutID)
	initialRoute, ok := f.routeOf(second.CheckoutID)
	require.True(t, ok)
	require.Equal(t, firstRoute.CommitGenerationID, initialRoute.CommitGenerationID)
	require.Positive(t, initialRoute.DirtyGenerationID)
	require.True(t, initial.DirtyBuilt)
	if firstCycle.CommitBuilt {
		require.True(t, initial.CommitReused)
	}

	const unchangedPolls = 8
	for range unchangedPolls {
		cycle := f.runCoordinator(second.CheckoutID)
		route, exists := f.routeOf(second.CheckoutID)
		require.True(t, exists)
		require.False(t, cycle.CommitBuilt)
		require.False(t, cycle.DirtyBuilt)
		require.Equal(t, initialRoute.CommitGenerationID, route.CommitGenerationID)
		require.Equal(t, initialRoute.DirtyGenerationID, route.DirtyGenerationID)
		require.Equal(t, initialRoute.RouteEpoch, route.RouteEpoch)
	}

	require.NoError(t, os.WriteFile(dirtyPath, []byte("package fixture\n\nfunc PollDirtyTwo() {}\n"), 0o644))
	changed := f.runCoordinator(second.CheckoutID)
	changedRoute, ok := f.routeOf(second.CheckoutID)
	require.True(t, ok)
	require.False(t, changed.CommitBuilt)
	require.True(t, changed.DirtyBuilt)
	require.Equal(t, initialRoute.CommitGenerationID, changedRoute.CommitGenerationID)
	require.NotEqual(t, initialRoute.DirtyGenerationID, changedRoute.DirtyGenerationID)
	require.Equal(t, initialRoute.RouteEpoch+1, changedRoute.RouteEpoch)

	for range unchangedPolls {
		cycle := f.runCoordinator(second.CheckoutID)
		route, exists := f.routeOf(second.CheckoutID)
		require.True(t, exists)
		require.False(t, cycle.CommitBuilt)
		require.False(t, cycle.DirtyBuilt)
		require.Equal(t, changedRoute.CommitGenerationID, route.CommitGenerationID)
		require.Equal(t, changedRoute.DirtyGenerationID, route.DirtyGenerationID)
		require.Equal(t, changedRoute.RouteEpoch, route.RouteEpoch)
	}
}

func TestCoordinatorReusesCanonicalCommitGenerationAfterRestart(t *testing.T) {
	f := newFamilyFixture(t, "ready-cache-restart")
	defer f.close()
	ctx := context.Background()
	automatic := f.automatic

	f.runCoordinator(automatic.CheckoutID)
	beforeRoute, ok := f.routeOf(automatic.CheckoutID)
	require.True(t, ok)
	require.Positive(t, beforeRoute.CommitGenerationID)
	automaticPrefix := f.lc.prefixForCheckout(ctx, automatic.CheckoutID)
	beforeView := f.materialize(automatic.CheckoutID)
	beforeIdentities := contentIdentities(beforeView.Reader, automaticPrefix)
	beforeView.Close()

	f.restart()
	_, err := f.mi.TrackRepoCtx(ctx, config.RepoEntry{Path: f.main, Name: f.mainPrefix})
	require.NoError(t, err)
	require.NoError(t, f.lc.Seed(ctx))
	report, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, report.Coordinators, 2)

	// Simulate a process-local route owner forgetting the settled slot while
	// retaining the durable ready payload. Before graph-scoped reuse this path
	// rebuilt because the coordinator's owner-keyed map was empty after restart.
	route, ok := f.routeOf(automatic.CheckoutID)
	require.True(t, ok)
	err = f.catalog.FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
		CheckoutID:         automatic.CheckoutID,
		ExpectedRouteEpoch: route.RouteEpoch,
		GraphID:            route.GraphID,
		CommitGenerationID: 0,
		DirtyGenerationID:  0,
		State:              store_sqlite.RoutePending,
	})
	require.NoError(t, err)

	cycle := f.runCoordinator(automatic.CheckoutID)
	afterRoute, ok := f.routeOf(automatic.CheckoutID)
	require.True(t, ok)
	require.Equal(t, beforeRoute.CommitGenerationID, afterRoute.CommitGenerationID)
	require.False(t, cycle.CommitBuilt)
	require.True(t, cycle.CommitReused)

	afterView := f.materialize(automatic.CheckoutID)
	defer afterView.Close()
	require.Equal(t, beforeIdentities, contentIdentities(afterView.Reader, automaticPrefix))
}

func TestCoordinatorRestartSweepRetainsInactiveCommitForSwitchBack(t *testing.T) {
	f := newFamilyFixture(t, "ready-cache-inactive-restart")
	defer f.close()
	ctx := context.Background()
	checkoutID := f.automatic.CheckoutID
	runGit(t, f.worktree, "add", ".")
	runGit(t, f.worktree, "commit", "-q", "-m", "tree A")
	commitA := runGitWatcherGit(t, f.worktree, "rev-parse", "HEAD")

	f.runCoordinator(checkoutID)
	routeA, ok := f.routeOf(checkoutID)
	require.True(t, ok)
	require.Positive(t, routeA.CommitGenerationID)
	generationA := routeA.CommitGenerationID

	require.NoError(t, os.WriteFile(filepath.Join(f.worktree, "branch_b.go"),
		[]byte("package a\n\nfunc BranchB() {}\n"), 0o644))
	runGit(t, f.worktree, "add", ".")
	runGit(t, f.worktree, "commit", "-q", "-m", "tree B")
	f.runCoordinator(checkoutID)
	routeB, ok := f.routeOf(checkoutID)
	require.True(t, ok)
	require.Positive(t, routeB.CommitGenerationID)
	require.NotEqual(t, generationA, routeB.CommitGenerationID)

	pinnedBefore, err := f.catalog.ListCheckoutCommitCachePins(ctx, routeB.GraphID)
	require.NoError(t, err)
	require.True(t, checkoutCommitPinnedBy(pinnedBefore, checkoutID, generationA),
		"switching A -> B must durably retain A before restart")
	require.True(t, checkoutCommitPinnedBy(pinnedBefore, checkoutID, routeB.CommitGenerationID),
		"the selected B generation must bridge restart")

	f.restart()
	_, err = f.mi.TrackRepoCtx(ctx, config.RepoEntry{Path: f.main, Name: f.mainPrefix})
	require.NoError(t, err)
	require.NoError(t, f.lc.Seed(ctx))
	_, err = f.lc.Sweep(ctx)
	require.NoError(t, err)

	pinnedAfter, err := f.catalog.ListCheckoutCommitCachePins(ctx, routeB.GraphID)
	require.NoError(t, err)
	require.True(t, checkoutCommitPinnedBy(pinnedAfter, checkoutID, generationA),
		"startup retirement sweep discarded inactive A generation %d", generationA)
	_, found, err := f.catalog.GetViewGeneration(ctx, generationA)
	require.NoError(t, err)
	require.True(t, found)

	runGitWatcherGit(t, f.worktree, "checkout", "--detach", commitA)
	third := f.runCoordinator(checkoutID)
	routeAgain, ok := f.routeOf(checkoutID)
	require.True(t, ok)
	require.Equal(t, generationA, routeAgain.CommitGenerationID)
	require.False(t, third.CommitBuilt, "restart-safe B -> A switch rebuilt A")
	require.True(t, third.CommitReused, "restart-safe B -> A switch did not report cache reuse")
}

func TestCheckoutCommitCacheEvictionSurvivesRefusalAndLostRetryMemory(t *testing.T) {
	f := newFamilyFixture(t, "ready-cache-eviction-retry")
	defer f.close()
	ctx := context.Background()
	checkoutID := f.automatic.CheckoutID
	runGit(t, f.worktree, "add", ".")
	runGit(t, f.worktree, "commit", "-q", "-m", "tree A")

	f.runCoordinator(checkoutID)
	routeA, ok := f.routeOf(checkoutID)
	require.True(t, ok)
	require.Positive(t, routeA.CommitGenerationID)
	generationA := routeA.CommitGenerationID

	require.NoError(t, os.WriteFile(filepath.Join(f.worktree, "branch_b.go"),
		[]byte("package a\n\nfunc BranchB() {}\n"), 0o644))
	runGit(t, f.worktree, "add", ".")
	runGit(t, f.worktree, "commit", "-q", "-m", "tree B")
	f.runCoordinator(checkoutID)
	routeB, ok := f.routeOf(checkoutID)
	require.True(t, ok)
	require.NotEqual(t, generationA, routeB.CommitGenerationID)

	retention := DefaultRefViewRetention()
	retention.RetainInactive = time.Hour
	f.lc.refViewRetention = retention
	f.clock.advance(2 * 365 * 24 * time.Hour)
	lease := f.lc.ViewLeases().Acquire(generationA)
	f.lc.sweepRetirements(ctx)

	pins, err := f.catalog.ListCheckoutCommitCachePins(ctx, routeA.GraphID)
	require.NoError(t, err)
	require.False(t, checkoutCommitPinnedBy(pins, checkoutID, generationA),
		"expired A pin survived retention")
	_, found, err := f.catalog.GetViewGeneration(ctx, generationA)
	require.NoError(t, err)
	require.True(t, found, "live lease did not refuse the first retirement")
	f.lc.coordMu.Lock()
	_, owed := f.lc.owed[generationA]
	delete(f.lc.owed, generationA)
	f.lc.coordMu.Unlock()
	require.True(t, owed, "pin eviction was not handed to the retry backlog")

	// Dropping the process-local retry entry models a crash after the pin
	// transaction committed. The durable retirement queue must still hand off
	// the unpinned, unrouted layer even though this checkout has a live
	// coordinator and runtime orphan discovery deliberately skips READY rows.
	lease.Release()
	f.lc.sweepRetirements(ctx)
	_, found, err = f.catalog.GetViewGeneration(ctx, generationA)
	require.NoError(t, err)
	require.False(t, found, "evicted A was lost after retry memory disappeared")
}

func checkoutCommitPinnedBy(
	pins []store_sqlite.CheckoutCommitCachePin,
	checkoutID string,
	generationID int64,
) bool {
	for _, pin := range pins {
		if pin.CheckoutID == checkoutID && pin.GenerationID == generationID {
			return true
		}
	}
	return false
}
