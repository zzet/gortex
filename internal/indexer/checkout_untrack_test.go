package indexer

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
)

// TestPreviewUntrackRejectsUnknownRelativeSelectorFromTrackedRoot reproduces
// the daemon-CWD hazard: the generic resolver considers every relative token a
// descendant of the daemon's current repository. Destructive resolution must
// fail closed instead of turning a typo into that repository's prefix.
func TestPreviewUntrackRejectsUnknownRelativeSelectorFromTrackedRoot(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	root := f.gitRepo("selector-main")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "selector-main"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	oldCWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(root))
	defer func() { require.NoError(t, os.Chdir(oldCWD)) }()

	preview, err := f.lc.PreviewUntrack(ctx, "definitely-not-a-repository")
	require.ErrorIs(t, err, ErrCheckoutNotTracked)
	assert.Empty(t, preview.Prefix)
	assert.NotNil(t, f.mi.GetMetadata(tracked.Prefix), "a rejected selector must not move live state")
	_, found, graphErr := f.catalog.GetDedicatedGraph(ctx, GraphIDFor(tracked.Prefix))
	require.NoError(t, graphErr)
	assert.True(t, found, "a rejected selector must not move catalog state")

	valid, err := f.lc.PreviewUntrack(ctx, tracked.Prefix)
	require.NoError(t, err)
	assert.Equal(t, tracked.Prefix, valid.Prefix, "an exact prefix remains a valid destructive selector")
}

// TestApplyUntrackCancellationBeforeValidationWritesNothing pins the other
// side of the cancellation boundary: detachment starts only after the catalog
// guards have passed, so a request already cancelled at confirm time cannot
// revoke intents or start cleanup.
func TestApplyUntrackCancellationBeforeValidationWritesNothing(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	root := f.gitRepo("cancelled-untrack")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "cancelled-untrack"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	preview, err := f.lc.PreviewUntrack(ctx, tracked.Prefix)
	require.NoError(t, err)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = f.lc.ApplyUntrack(cancelled, preview)
	require.Error(t, err)
	assert.NotNil(t, f.mi.GetMetadata(tracked.Prefix))
	intents, listErr := f.catalog.ListTrackingIntents(ctx, tracked.CheckoutID)
	require.NoError(t, listErr)
	require.Len(t, intents, 1)
	assert.True(t, intents[0].Active, "validation cancellation must precede intent revocation")
}

// TestPreviewUntrackResolvesDurableIdentityWithoutLiveMetadata protects cleanup
// of a stale daemon state: an exact catalog prefix and an absolute checkout
// path still address the intended checkout even after its in-memory corpus
// entry is gone.
func TestPreviewUntrackResolvesDurableIdentityWithoutLiveMetadata(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	root := f.gitRepo("selector-stale")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "selector-stale"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	checkout := f.checkoutOf(tracked.Prefix)

	f.mi.UntrackRepo(tracked.Prefix)
	require.Nil(t, f.mi.GetMetadata(tracked.Prefix))

	byPrefix, err := f.lc.PreviewUntrack(ctx, tracked.Prefix)
	require.NoError(t, err)
	assert.Equal(t, checkout.CheckoutID, byPrefix.CheckoutID)
	assert.Equal(t, tracked.Prefix, byPrefix.Prefix)

	byPath, err := f.lc.PreviewUntrack(ctx, root)
	require.NoError(t, err)
	assert.Equal(t, checkout.CheckoutID, byPath.CheckoutID)
	assert.Equal(t, tracked.Prefix, byPath.Prefix)
}
