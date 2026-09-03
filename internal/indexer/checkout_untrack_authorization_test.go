package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

// TestApplyUntrackStalePrimaryPreviewKeepsIntent exercises the production
// preview/confirm path across the exact race the atomic authorization closes:
// the primary epoch moves after preview, so neither intent nor cleanup may
// move when confirm loses its guard.
func TestApplyUntrackStalePrimaryPreviewKeepsIntent(t *testing.T) {
	f := newFamilyFixture(t, "untrack-auth-epoch")
	defer f.close()
	ctx := context.Background()

	preview, err := f.lc.PreviewUntrack(ctx, f.main)
	require.NoError(t, err)
	require.Equal(t, UntrackPlanPrimaryClosure, preview.Plan)
	family, found, err := f.catalog.GetRepositoryFamily(ctx, f.familyID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, family.PrimaryEpoch, preview.PrimaryEpoch)

	// Reinstalling the same graph is enough to advance the compare-and-set
	// epoch without changing what should remain live after the refused confirm.
	require.NoError(t, f.catalog.SetPrimaryDedicatedGraph(ctx,
		store_sqlite.SetPrimaryDedicatedGraphRequest{
			FamilyID:             f.familyID,
			GraphID:              f.primaryGraph,
			ExpectedPrimaryEpoch: family.PrimaryEpoch,
			LastSeen:             family.LastSeen + 1,
		}))

	result, err := f.lc.ApplyUntrack(ctx, preview)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store_sqlite.ErrCatalogStaleGuard), "got %v", err)
	assert.Empty(t, result.Revoked)
	assert.NotNil(t, f.mi.GetMetadata(f.mainPrefix), "stale confirm evicted the primary corpus")
	_, found, graphErr := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	require.NoError(t, graphErr)
	assert.True(t, found, "stale confirm removed the primary graph")
	owner := f.checkoutOf(f.mainPrefix)
	intents, err := f.catalog.ListTrackingIntents(ctx, owner.CheckoutID)
	require.NoError(t, err)
	require.Len(t, intents, 1)
	assert.True(t, intents[0].Active, "stale primary confirm revoked explicit intent")
	entries, err := f.catalog.ListCleanupEntries(ctx)
	require.NoError(t, err)
	assert.Empty(t, entries, "stale primary confirm started cleanup")
}

// TestApplyUntrackIntentAddedAfterPreviewBlocksEverything proves the catalog
// re-preflights intent in the authorization transaction. A non-revocable
// project membership arriving after a demote preview keeps both it and the
// previously revocable CLI intent active and starts no transition.
func TestApplyUntrackIntentAddedAfterPreviewBlocksEverything(t *testing.T) {
	f := newFamilyFixture(t, "untrack-auth-blocked")
	defer f.close()
	ctx := context.Background()

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	preview, err := f.lc.PreviewUntrack(ctx, f.worktree)
	require.NoError(t, err)
	require.Equal(t, UntrackPlanDemote, preview.Plan)

	require.NoError(t, f.catalog.UpsertTrackingIntent(ctx, store_sqlite.TrackingIntent{
		IntentID:      "intent-project-after-preview",
		CheckoutID:    tracked.CheckoutID,
		SourceKind:    store_sqlite.IntentSourceProjectMembership,
		SourceLocator: "project:test",
		Active:        true,
		CreatedAt:     200,
	}))

	result, err := f.lc.ApplyUntrack(ctx, preview)
	require.Error(t, err)
	assert.True(t, errors.Is(err, reconcile.ErrIntentNotRevocable), "got %v", err)
	assert.Empty(t, result.Revoked)
	assert.False(t, result.Demoted)
	checkout, found, readErr := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
	require.NoError(t, readErr)
	require.True(t, found)
	assert.Equal(t, store_sqlite.CheckoutModeDedicated, checkout.EffectiveMode)
	assert.Empty(t, checkout.ActiveIntentTransitionID)
	intents, err := f.catalog.ListTrackingIntents(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.Len(t, intents, 2)
	for _, intent := range intents {
		assert.True(t, intent.Active, "blocked authorization revoked %s", intent.IntentID)
	}
	assert.NotNil(t, f.mi.GetMetadata(tracked.Prefix), "blocked authorization evicted its corpus")
	_, found, err = f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	assert.True(t, found, "blocked authorization removed its graph")
	_, pending, err := f.catalog.GetIntentTransition(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	assert.False(t, pending, "blocked authorization started a demotion transition")
	entries, err := f.catalog.ListCleanupEntries(ctx)
	require.NoError(t, err)
	assert.Empty(t, entries, "blocked authorization started cleanup")
}
