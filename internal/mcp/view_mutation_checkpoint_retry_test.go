package mcp

import (
	"context"
	"errors"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"testing"
	"time"
)

func completedRecompositionTicket(err error) *indexer.CheckoutRefreshTicket {
	done := make(chan indexer.MutationResult, 1)
	done <- indexer.MutationResult{Err: err}
	return &indexer.CheckoutRefreshTicket{Ticket: &indexer.MutationTicket{Done: done}}
}
func TestMutationRecompositionRetriesOnlyPhysicalCheckpointBusy(t *testing.T) {
	calls := 0
	var deadline time.Time
	ok := awaitRouteRecompositionTickets(t.Context(), func(ctx context.Context) (*indexer.CheckoutRefreshTicket, error) {
		calls++
		d, has := ctx.Deadline()
		require.True(t, has)
		if calls == 1 {
			deadline = d
			return completedRecompositionTicket(fmt.Errorf("physical generation attempt: %w", store_sqlite.ErrGenerationBulkCheckpointBusy)), nil
		}
		require.Equal(t, deadline, d, "retry must share the original budget")
		return completedRecompositionTicket(nil), nil
	})
	require.True(t, ok)
	require.Equal(t, 2, calls)
}
func TestMutationRecompositionDoesNotRetrySemanticOrAdmissionRefusal(t *testing.T) {
	for _, err := range []error{indexer.ErrCheckoutMutationRouteMoved, context.Canceled, context.DeadlineExceeded, errors.New("unrelated build failure")} {
		t.Run(err.Error(), func(t *testing.T) {
			calls := 0
			require.False(t, awaitRouteRecompositionTickets(t.Context(), func(context.Context) (*indexer.CheckoutRefreshTicket, error) {
				calls++
				return completedRecompositionTicket(err), nil
			}))
			require.Equal(t, 1, calls)
		})
	}
	calls := 0
	require.False(t, awaitRouteRecompositionTickets(t.Context(), func(context.Context) (*indexer.CheckoutRefreshTicket, error) {
		calls++
		return nil, indexer.ErrCheckoutRefreshQueueFull
	}))
	require.Equal(t, 1, calls)
}
func TestMutationRecompositionCanceledRetryCannotAdmitAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	require.False(t, awaitRouteRecompositionTickets(ctx, func(context.Context) (*indexer.CheckoutRefreshTicket, error) {
		calls++
		cancel()
		return completedRecompositionTicket(store_sqlite.ErrGenerationBulkCheckpointBusy), nil
	}))
	require.Equal(t, 1, calls)
}
func TestMutationRecompositionIncompleteTicketNeverClaimsPublication(t *testing.T) {
	done := make(chan indexer.MutationResult)
	close(done)
	require.False(t, awaitRouteRecompositionTickets(t.Context(), func(context.Context) (*indexer.CheckoutRefreshTicket, error) {
		return &indexer.CheckoutRefreshTicket{Ticket: &indexer.MutationTicket{Done: done}}, nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	require.False(t, awaitRouteRecompositionTickets(ctx, func(context.Context) (*indexer.CheckoutRefreshTicket, error) {
		calls++
		return completedRecompositionTicket(nil), nil
	}))
	require.Zero(t, calls)
}
