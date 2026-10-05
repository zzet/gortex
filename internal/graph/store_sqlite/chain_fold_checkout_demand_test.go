package store_sqlite

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// A checkout ticket remains foreground demand throughout its CPU/build-lane
// waits, but must permit its own inline fold and background fold progress.
func TestCheckoutDemandPermitsFoldWithoutReleasingGlobalDemand(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	store, _ := openWALReclaimStore(t)
	t.Cleanup(func() { _ = store.Close() })
	store.stopCheckpointLoop()
	chain := foldChain(t, store, 400)
	to := reservedGeneration(t, store, "checkout-demand-fold")
	reference := reservedGeneration(t, store, "checkout-demand-reference")
	_, err := store.FlattenGenerationChain(t.Context(), chain, reference)
	require.NoError(t, err)

	// Announcements share the core even when made through different handles.
	first := store.AnnounceCheckoutRefresh()
	second := store.AtGeneration(chain[0]).AnnounceCheckoutRefresh()
	defer first()
	defer second()
	require.True(t, store.WriteWanted())
	require.True(t, store.editIntentActive(), "WAL cooperation must retain ticket demand")
	require.True(t, store.buildLaneBusy(), "cold/background cooperation must retain ticket demand")
	fold, err := store.BeginChainFold(t.Context(), ChainFoldRequest{Chain: chain, To: to, Owner: "checkout/inline"})
	require.NoError(t, err)
	defer func() { require.NoError(t, fold.Release(t.Context())) }()

	// Standard announcements, even upstream of the writer gate, still refuse
	// steps and admission without changing the cursor or any committed rows.
	ordinary := store.AnnounceWrite()
	defer ordinary()
	cursor := fold.cursor()
	_, err = fold.Step(t.Context())
	require.ErrorIs(t, err, ErrChainFoldYielded)
	require.Equal(t, cursor, fold.cursor())
	ordinary()
	ordinary()
	require.True(t, store.WriteWanted(), "ordinary release must not release checkout demand")
	runFold(t, fold, nil)
	require.True(t, store.WriteWanted(), "committed fold must not consume ticket announcements")
	require.Equal(t, renderGenerationNodes(t, store, reference), renderGenerationNodes(t, store, to))
	require.Equal(t, renderGenerationEdges(t, store, reference), renderGenerationEdges(t, store, to))
	require.Equal(t, renderFoldMasks(t, store, reference), renderFoldMasks(t, store, to))
	require.Equal(t, renderFoldFTS(t, store, reference), renderFoldFTS(t, store, to))
	require.NoError(t, fold.Release(t.Context()))

	blockedTarget := reservedGeneration(t, store, "ordinary-demand-begin")
	ordinary = store.AnnounceWrite()
	defer ordinary()
	_, err = store.BeginChainFold(t.Context(), ChainFoldRequest{Chain: chain, To: blockedTarget, Owner: "blocked"})
	require.ErrorIs(t, err, ErrChainFoldYielded)
	require.False(t, store.chainFold.active.Load(), "refused admission must release its lease")
	ordinary()
	first()
	first()
	require.True(t, store.WriteWanted(), "one checkout ticket remains")
	second()
	second()
	require.False(t, store.WriteWanted())
	require.False(t, store.editIntentActive())
	require.False(t, store.foldWriteWanted())
}

// A queued mutation does not turn already-canceled fold admission into a
// retryable yield. Neither admission nor the payload callback may proceed.
func TestFoldDemandDoesNotMaskCanceledAdmission(t *testing.T) {
	store := openCatalogStore(t)
	to := reservedGeneration(t, store, "canceled-fold-demand")
	release := store.AnnounceWrite()
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	err := store.withFoldTx(ctx, to, func(context.Context, *sql.Tx) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, called)
	require.True(t, store.WriteWanted(), "canceled fold must not consume ordinary demand")
}
