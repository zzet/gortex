package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
