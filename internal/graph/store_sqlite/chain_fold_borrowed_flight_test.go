package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChainFoldBorrowedFlightSurvivesBeginRetryAndPublication(t *testing.T) {
	store := openCatalogStore(t)
	ctx := t.Context()
	chain := foldChain(t, store, 4)
	to := reservedGeneration(t, store, "borrowed")
	flight, leader, ready, err := store.JoinPayloadBuildFlight(ctx, to, false)
	require.NoError(t, err)
	require.True(t, leader)
	require.False(t, ready)
	t.Cleanup(func() { flight.Complete(nil) })
	req := ChainFoldRequest{Chain: chain, To: to, Owner: "test", BuildFlight: flight}
	oldHook := chainFoldMetadataHook
	t.Cleanup(func() { chainFoldMetadataHook = oldHook })
	chainFoldMetadataHook = func(context.Context, bool) error { return ErrChainFoldYielded }
	_, err = store.BeginChainFold(ctx, req)
	require.ErrorIs(t, err, ErrChainFoldYielded)
	require.True(t, store.PayloadBuildFlightActive(to), "a refused Begin must not complete borrowed ownership")
	_, err = store.RetirePayloadGenerationQuantum(ctx, to, nil)
	require.ErrorIs(t, err, ErrPayloadGenerationInUse)
	chainFoldMetadataHook = oldHook
	fold, err := store.BeginChainFold(ctx, req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fold.Release(context.Background()) })
	for {
		done, err := fold.Step(ctx)
		require.NoError(t, err)
		if done {
			break
		}
	}
	require.NoError(t, store.Catalog().SetViewGenerationState(ctx, to, ViewGenerationReady, ViewGenerationBuilding))
	require.NoError(t, fold.Release(ctx))
	require.True(t, store.PayloadBuildFlightActive(to), "publication Release precedes caller handoff")
	require.False(t, store.chainFoldHolds(chain[0]))
	_, err = store.RetirePayloadGenerationQuantum(ctx, to, nil)
	require.ErrorIs(t, err, ErrPayloadGenerationInUse)
	flight.Complete(nil)
	require.False(t, store.PayloadBuildFlightActive(to))
}

func TestChainFoldBorrowedFlightAbandonAndCancelledBegin(t *testing.T) {
	for _, cancelBegin := range []bool{false, true} {
		t.Run(map[bool]string{false: "abandon", true: "cancel_begin"}[cancelBegin], func(t *testing.T) {
			store := openCatalogStore(t)
			chain := foldChain(t, store, 2)
			to := reservedGeneration(t, store, "abandoned")
			flight, leader, _, err := store.JoinPayloadBuildFlight(t.Context(), to, false)
			require.NoError(t, err)
			require.True(t, leader)
			t.Cleanup(func() { flight.Complete(nil) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelBegin {
				cancel()
			}
			fold, err := store.BeginChainFold(ctx, ChainFoldRequest{Chain: chain, To: to, BuildFlight: flight})
			if cancelBegin {
				require.ErrorIs(t, err, context.Canceled)
				require.NoError(t, store.Catalog().SetViewGenerationState(t.Context(), to, ViewGenerationFailed, ViewGenerationBuilding))
			} else {
				require.NoError(t, err)
				require.NoError(t, fold.Abandon(t.Context()))
			}
			require.True(t, store.PayloadBuildFlightActive(to), "caller owns abandonment handoff")
			_, err = store.RetirePayloadGenerationQuantum(t.Context(), to, nil)
			require.ErrorIs(t, err, ErrPayloadGenerationInUse)
			flight.Complete(context.Canceled)
			require.False(t, store.PayloadBuildFlightActive(to))
			_, err = store.RetirePayloadGenerationQuantum(t.Context(), to, nil)
			require.False(t, errors.Is(err, ErrPayloadGenerationInUse), "released abandonment is collectible")
		})
	}
}

func TestChainFoldRejectsInvalidBorrowedFlightWithoutCompletingIt(t *testing.T) {
	store := openCatalogStore(t)
	other := openCatalogStore(t)
	chain := foldChain(t, store, 2)
	to := reservedGeneration(t, store, "target")
	owner, leader, _, err := store.JoinPayloadBuildFlight(t.Context(), to, false)
	require.NoError(t, err)
	require.True(t, leader)
	t.Cleanup(func() { owner.Complete(nil) })
	follower, leader, _, err := store.JoinPayloadBuildFlight(t.Context(), to, false)
	require.NoError(t, err)
	require.False(t, leader)
	foreignID := int64(0)
	for foreignID < to {
		foreignID = reservedGeneration(t, other, fmt.Sprintf("foreign-%d", foreignID))
	}
	require.Equal(t, to, foreignID, "foreign-core refusal must not rely on different generation IDs")
	foreign, leader, _, err := other.JoinPayloadBuildFlight(t.Context(), foreignID, false)
	require.NoError(t, err)
	require.True(t, leader)
	t.Cleanup(func() { foreign.Complete(nil) })
	wrongID := reservedGeneration(t, store, "different")
	for _, test := range []struct {
		name   string
		flight *PayloadBuildFlight
		to     int64
	}{
		{"follower", follower, to}, {"foreign", foreign, to}, {"wrong_generation", owner, wrongID},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.BeginChainFold(t.Context(), ChainFoldRequest{Chain: chain, To: test.to, BuildFlight: test.flight})
			require.ErrorIs(t, err, ErrCatalogInvalidValue)
			require.True(t, store.PayloadBuildFlightActive(to))
			require.True(t, other.PayloadBuildFlightActive(foreignID))
		})
	}
	owner.Complete(nil)
	_, err = store.BeginChainFold(t.Context(), ChainFoldRequest{Chain: chain, To: to, BuildFlight: owner})
	require.ErrorIs(t, err, ErrCatalogInvalidValue)
	replacement, leader, _, err := store.JoinPayloadBuildFlight(t.Context(), to, false)
	require.NoError(t, err)
	require.True(t, leader)
	t.Cleanup(func() { replacement.Complete(nil) })
	_, err = store.BeginChainFold(t.Context(), ChainFoldRequest{Chain: chain, To: to, BuildFlight: owner})
	require.ErrorIs(t, err, ErrCatalogInvalidValue)
	require.True(t, store.PayloadBuildFlightActive(to), "stale borrowing must not complete the replacement leader")
}

func TestChainFoldUnborrowedPrepareFailureAndReleaseStillComplete(t *testing.T) {
	store := openCatalogStore(t)
	chain := foldChain(t, store, 2)
	to := reservedGeneration(t, store, "default")
	oldHook := chainFoldMetadataHook
	t.Cleanup(func() { chainFoldMetadataHook = oldHook })
	chainFoldMetadataHook = func(context.Context, bool) error { return ErrChainFoldYielded }
	_, err := store.BeginChainFold(t.Context(), ChainFoldRequest{Chain: chain, To: to})
	require.ErrorIs(t, err, ErrChainFoldYielded)
	require.False(t, store.PayloadBuildFlightActive(to), "default refused Begin retains no caller owner")
	chainFoldMetadataHook = oldHook
	fold, err := store.BeginChainFold(t.Context(), ChainFoldRequest{Chain: chain, To: to})
	require.NoError(t, err)
	require.True(t, store.PayloadBuildFlightActive(to))
	require.NoError(t, fold.Release(t.Context()))
	require.False(t, store.PayloadBuildFlightActive(to), "default Release still completes its own leader")
}
