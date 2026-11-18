package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

func TestCleanLinkedFixtureObservationRetriesOnlyTypedPending(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	path := "same-owned-checkout"
	expected := store_sqlite.Checkout{CheckoutID: "checkout", Incarnation: "incarnation-7"}
	pending := errors.Join(&indexer.CheckoutDiscoveryPendingError{Root: path, Family: "family", Reason: "in_progress"}, context.DeadlineExceeded)
	calls := 0
	var recorded []error
	got, found, err := observeCleanLinkedSourceCheckout(ctx, path, func(actual context.Context, root string) (store_sqlite.Checkout, bool, error) {
		require.Same(t, ctx, actual)
		require.Equal(t, path, root)
		calls++
		if calls == 1 {
			return store_sqlite.Checkout{}, false, pending
		}
		return expected, true, nil
	}, func(err error) { recorded = append(recorded, err) })
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, expected, got)
	require.Equal(t, 2, calls)
	require.Equal(t, []error{pending}, recorded)
}

func TestCleanLinkedFixtureObservationPreservesNonPendingResults(t *testing.T) {
	fatal := errors.New("metadata failed")
	for _, tc := range []struct {
		name  string
		found bool
		err   error
	}{
		{name: "fatal", err: fatal},
		{name: "ordinary_deadline", err: context.DeadlineExceeded},
		{name: "busy_without_typed_pending", err: indexer.ErrCheckoutMutationBusy},
		{name: "stale", err: indexer.ErrCheckoutMutationStale},
		{name: "absent"},
		{name: "present", found: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			expected := store_sqlite.Checkout{CheckoutID: "unchanged", Incarnation: "incarnation-9"}
			got, found, err := observeCleanLinkedSourceCheckout(t.Context(), "root", func(context.Context, string) (store_sqlite.Checkout, bool, error) {
				calls++
				return expected, tc.found, tc.err
			}, func(error) { t.Fatal("non-pending result was recorded as pending") })
			require.Equal(t, expected, got)
			require.Equal(t, tc.found, found)
			require.Equal(t, tc.err, err)
			require.Equal(t, 1, calls)
		})
	}
}

func TestCleanLinkedFixtureObservationCancellationPreservesPending(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pending := &indexer.CheckoutDiscoveryPendingError{Root: "root", Reason: "in_progress"}
	calls, recorded := 0, 0
	_, found, err := observeCleanLinkedSourceCheckout(ctx, "root", func(context.Context, string) (store_sqlite.Checkout, bool, error) {
		calls++
		return store_sqlite.Checkout{}, false, pending
	}, func(err error) {
		require.Same(t, pending, err)
		recorded++
		cancel()
	})
	require.False(t, found)
	require.ErrorIs(t, err, context.Canceled)
	var actual *indexer.CheckoutDiscoveryPendingError
	require.ErrorAs(t, err, &actual)
	require.Same(t, pending, actual)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, recorded)
}

func TestCleanLinkedFixtureObservationCanceledContextDoesNotCall(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, found, err := observeCleanLinkedSourceCheckout(ctx, "root", func(context.Context, string) (store_sqlite.Checkout, bool, error) {
		t.Fatal("canceled setup admitted observation")
		return store_sqlite.Checkout{}, false, nil
	}, func(error) { t.Fatal("no observation can produce pending") })
	require.False(t, found)
	require.ErrorIs(t, err, context.Canceled)
}
