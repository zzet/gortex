package indexer

import (
	"context"
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
