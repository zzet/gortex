package indexer

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

func TestPromotionCollisionKeepsOtherCorpus(t *testing.T) {
	for _, replacement := range []string{"registry", "catalog-authority"} {
		t.Run(replacement, func(t *testing.T) {
			f := newFamilyFixture(t, "late-collision")
			t.Cleanup(f.close)
			prefix := f.mainPrefix + "@" + f.automatic.AdminName
			other := f.gitRepo("other-owner")
			var binding store_sqlite.DedicatedGraph
			var configBytes []byte
			var watcherCalls []string
			var nodes, edges int
			f.lc.afterPromotionIndex = func() {
				// Use real teardown and registration, not a forged registry entry.
				// The catalog-only case leaves the indexed registration in place
				// but gives the graph real authority owned by another checkout.
				name := "other"
				if replacement == "registry" {
					_, _, err := f.mi.UntrackRepoChecked(t.Context(), prefix)
					require.NoError(t, err)
					require.NoError(t, f.cm.Global().RemoveRepo(f.automatic.RootPath))
					name = prefix
				}
				registered, err := f.lc.Register(t.Context(), config.RepoEntry{Path: other, Name: name}, TrackSourceCLI)
				require.NoError(t, err)
				require.NoError(t, registered.CatalogErr)
				var found bool
				binding, found, err = f.catalog.GetDedicatedGraph(t.Context(), registered.GraphID)
				require.NoError(t, err)
				require.True(t, found)
				if replacement == "catalog-authority" {
					require.NoError(t, f.catalog.DeleteDedicatedGraph(t.Context(), binding.GraphID))
					binding.GraphID, binding.RepoPrefix = GraphIDFor(prefix), prefix
					require.NoError(t, f.catalog.UpsertDedicatedGraph(t.Context(), binding))
					guardConfiguredOwner(t, f.lifecycleFixture, prefix)
				}
				configBytes, err = os.ReadFile(f.cfgPath)
				require.NoError(t, err)
				f.watcher.mu.Lock()
				watcherCalls = append([]string(nil), f.watcher.calls...)
				f.watcher.mu.Unlock()
				nodes, edges = f.store.NodeCount(), f.store.EdgeCount()
			}
			transition, err := f.lc.beginModeChange(t.Context(), f.automatic,
				store_sqlite.CheckoutModeDedicated, promotionTransitionCause, nil)
			require.NoError(t, err)
			// Run the real durable transition on the test goroutine so a failed
			// fixture assertion inside the scheduling hook cannot strand a waiter.
			outcome := f.lc.executeModeTransition(t.Context(), transition)
			out, err := outcome.promotion, outcome.err
			var collision *PrefixCollisionError
			require.ErrorAs(t, err, &collision)
			require.True(t, out.Retryable)
			require.Equal(t, prefix, collision.Prefix)
			if replacement == "catalog-authority" {
				require.ErrorIs(t, err, store_sqlite.ErrCatalogStaleGuard)
				_, published, err := f.catalog.DedicatedBasePublication(t.Context(), binding.GraphID)
				require.NoError(t, err)
				require.True(t, published, "rollback must retain the replacement authority")
			} else {
				require.Equal(t, other, f.mi.GetMetadata(prefix).RootPath)
			}
			afterBinding, found, err := f.catalog.GetDedicatedGraph(t.Context(), binding.GraphID)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, binding, afterBinding)
			require.Equal(t, nodes, f.store.NodeCount())
			require.Equal(t, edges, f.store.EdgeCount())
			require.NotEmpty(t, f.store.GetRepoNodes(prefix))
			afterConfig, err := os.ReadFile(f.cfgPath)
			require.NoError(t, err)
			require.Equal(t, configBytes, afterConfig)
			f.watcher.mu.Lock()
			afterCalls := append([]string(nil), f.watcher.calls...)
			f.watcher.mu.Unlock()
			require.Equal(t, watcherCalls, afterCalls, "failed promotion must not detach the replacement watcher")
			checkout, found, err := f.catalog.GetCheckout(t.Context(), f.automatic.CheckoutID)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, store_sqlite.CheckoutModeAutomatic, checkout.EffectiveMode)
		})
	}
}

func TestPromotionPurgeRejectsAReplacedRegistration(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	first, other := f.gitRepo("first"), f.gitRepo("other")
	_, err := f.lc.Register(t.Context(), config.RepoEntry{Path: first, Name: "src"}, TrackSourceCLI)
	require.NoError(t, err)
	owned, err := f.mi.prefixOwnerForRoot(t.Context(), "src", first)
	require.NoError(t, err)
	// Retire through the lifecycle so the exact old reader/publisher admission
	// is finalized before installing a replacement. Deleting only the registry
	// and catalog row must not bypass that owner's admission tombstone.
	retired, err := f.lc.Untrack(t.Context(), first)
	require.NoError(t, err)
	if retired.Pending {
		finishRepositoryCleanup(t, f.lc, "src")
	}
	_, found, err := f.catalog.GetDedicatedGraph(t.Context(), GraphIDFor("src"))
	require.NoError(t, err)
	require.False(t, found)
	registered, err := f.lc.Register(t.Context(), config.RepoEntry{Path: other, Name: "src"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)
	nodes, edges := f.store.NodeCount(), f.store.EdgeCount()
	stopped := false
	_, _, err = f.mi.purgeRepoOwnedChecked(t.Context(), owned, func() { stopped = true })
	var collision *PrefixCollisionError
	require.ErrorAs(t, err, &collision)
	require.False(t, stopped)
	require.Equal(t, other, f.mi.GetMetadata("src").RootPath)
	require.True(t, f.watcher.isAttached("src"))
	require.Equal(t, nodes, f.store.NodeCount())
	require.Equal(t, edges, f.store.EdgeCount())
	require.NotEmpty(t, f.store.GetRepoNodes("src"))
}

func TestPromotionRollbackRemovesOnlyItsUnpublishedCorpus(t *testing.T) {
	f := newFamilyFixture(t, "owned-rollback")
	t.Cleanup(f.close)
	prefix := f.mainPrefix + "@" + f.automatic.AdminName
	_, err := f.mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: f.automatic.RootPath, Name: prefix})
	require.NoError(t, err)
	owned, err := f.mi.prefixOwnerForRoot(t.Context(), prefix, f.automatic.RootPath)
	require.NoError(t, err)
	binding := store_sqlite.DedicatedGraph{
		GraphID: GraphIDFor(prefix), RepoPrefix: prefix, OwnerCheckoutID: f.automatic.CheckoutID,
		FamilyID: f.automatic.FamilyID, State: reconcile.GraphStateReady,
	}
	require.NoError(t, f.catalog.UpsertDedicatedGraph(t.Context(), binding))
	primary := contentIdentities(f.store, f.mainPrefix)
	require.NotEmpty(t, primary)
	require.NotEmpty(t, f.store.GetRepoNodes(prefix))
	require.NoError(t, f.lc.rollbackPromotion(t.Context(), prefix, binding.GraphID, f.automatic, owned))
	require.Nil(t, f.mi.GetMetadata(prefix))
	require.Empty(t, f.store.GetRepoNodes(prefix))
	_, found, err := f.catalog.GetDedicatedGraph(t.Context(), binding.GraphID)
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, primary, contentIdentities(f.store, f.mainPrefix))
	require.NotNil(t, f.mi.GetMetadata(f.mainPrefix))
}

func TestPromotionDoesNotBorrowAnExactRootUnderAnotherName(t *testing.T) {
	f := newFamilyFixture(t, "named-root")
	t.Cleanup(f.close)
	var registrationErr error
	var before []string
	f.lc.indexBarrier = func() {
		// Let promotion decide its requested prefix first. A pre-existing
		// registration is intentionally adopted by dedicatedPrefixFor; the
		// race under test is a different name appearing after that decision.
		_, registrationErr = f.mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: f.automatic.RootPath, Name: "existing-name"})
		before = contentIdentities(f.store, "existing-name")
	}
	out, err := f.lc.PromoteCheckout(t.Context(), f.automatic.CheckoutID, TrackSourceCLI)
	require.NoError(t, registrationErr)
	require.NotEmpty(t, before)
	var collision *PrefixCollisionError
	require.ErrorAs(t, err, &collision, "a nil Track result under another name does not authorize the requested corpus")
	require.True(t, out.Retryable)
	require.Nil(t, f.mi.GetMetadata(out.Prefix))
	require.NotNil(t, f.mi.GetMetadata("existing-name"))
	require.Equal(t, before, contentIdentities(f.store, "existing-name"))
	_, found, err := f.catalog.GetDedicatedGraph(t.Context(), GraphIDFor(out.Prefix))
	require.NoError(t, err)
	require.False(t, found)
}
