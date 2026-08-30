package indexer

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

// TestExplicitRetrackDoesNotReelectPrimaryAfterClosure covers the durable
// no-primary state. Retiring the primary preserves independently dedicated
// siblings, and a later explicit track may add another dedicated corpus but
// must not silently choose either corpus as the family's new base.
func TestExplicitRetrackDoesNotReelectPrimaryAfterClosure(t *testing.T) {
	f := newFamilyFixture(t, "no-primary-retrack")
	defer f.close()
	ctx := context.Background()

	sibling, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, sibling.CatalogErr)
	siblingGraph, found, err := f.catalog.GetDedicatedGraph(ctx, sibling.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, siblingGraph.IsPrimaryBase)

	removed, err := f.lc.Untrack(ctx, f.main)
	require.NoError(t, err)
	require.Equal(t, UntrackPlanPrimaryClosure, removed.Plan)

	survivor, found, err := f.catalog.GetDedicatedGraph(ctx, sibling.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, survivor.IsPrimaryBase,
		"primary retirement must leave an independently dedicated sibling non-primary")

	retracked, err := f.lc.Register(ctx,
		config.RepoEntry{Path: f.main, Name: f.mainPrefix}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, retracked.CatalogErr)
	require.False(t, retracked.Pending)

	graphs, err := f.catalog.ListDedicatedGraphs(ctx, f.familyID)
	require.NoError(t, err)
	require.Len(t, graphs, 2)
	for _, graph := range graphs {
		assert.False(t, graph.IsPrimaryBase,
			"explicit retrack silently elected graph %s as primary", graph.GraphID)
	}
}

// TestIdempotentDedicatedBindingPreservesThePrimary proves the retry fast path
// returns the existing owner binding before initial-family eligibility is
// considered. Replaying registration for the current primary must never turn
// its graph into an ordinary dedicated sibling.
func TestIdempotentDedicatedBindingPreservesThePrimary(t *testing.T) {
	f := newFamilyFixture(t, "primary-rebind")
	defer f.close()
	ctx := context.Background()

	primary, found, err := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, primary.IsPrimaryBase)
	require.NotEmpty(t, primary.OwnerCheckoutID)

	graphID, err := f.lc.bindDedicatedGraph(
		ctx, f.familyID, primary.OwnerCheckoutID, primary.RepoPrefix,
	)
	require.NoError(t, err)
	require.Equal(t, primary.GraphID, graphID)

	rebound, found, err := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, primary, rebound)
}

// TestConcurrentFirstDedicatedBindingsChooseExactlyOnePrimary pins the race
// contract behind first-family admission. Every contender may observe the
// family before the winner commits, but the catalog's partial unique index and
// non-primary retry must leave all bindings present and exactly one primary.
func TestConcurrentFirstDedicatedBindingsChooseExactlyOnePrimary(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	const (
		familyID = "family-concurrent-primary"
		workers  = 16
	)
	require.NoError(t, f.catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          familyID,
		CommonDirIdentity: "common/concurrent-primary",
		State:             reconcile.FamilyStateReady,
		CreatedAt:         1,
		LastSeen:          1,
	}))
	for i := 0; i < workers; i++ {
		checkoutID := fmt.Sprintf("checkout-%02d", i)
		require.NoError(t, f.catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
			CheckoutID:    checkoutID,
			Incarnation:   "incarnation-" + checkoutID,
			FamilyID:      familyID,
			RootPath:      "/tmp/" + checkoutID,
			GitDir:        "/tmp/" + checkoutID + "/.git",
			AdminName:     checkoutID,
			State:         store_sqlite.CheckoutStateReady,
			DesiredMode:   store_sqlite.CheckoutModeDedicated,
			EffectiveMode: store_sqlite.CheckoutModeDedicated,
			LastSeen:      1,
		}))
	}

	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		checkoutID := fmt.Sprintf("checkout-%02d", i)
		prefix := fmt.Sprintf("repo-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.lc.bindDedicatedGraph(ctx, familyID, checkoutID, prefix)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	graphs, err := f.catalog.ListDedicatedGraphs(ctx, familyID)
	require.NoError(t, err)
	require.Len(t, graphs, workers)
	primaries := 0
	for _, graph := range graphs {
		if graph.IsPrimaryBase {
			primaries++
		}
	}
	assert.Equal(t, 1, primaries)
}
