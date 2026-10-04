package mcp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

// arrivalRecordingWaiter is the lifecycle's proof and ticket-after-proof
// admission, recording the request arrival each was handed on its context.
type arrivalRecordingWaiter struct {
	*fakeFreshnessWaiter
	mu           sync.Mutex
	proofArrival []time.Time
	proofCalled  []time.Time
	admitArrival []time.Time
	refuse       indexer.CheckoutFreshProof
}

func (w *arrivalRecordingWaiter) ProveCheckoutFresh(ctx context.Context, _, _ string) (indexer.CheckoutFreshProof, error) {
	arrived, _ := indexer.FreshRequestArrival(ctx)
	w.mu.Lock()
	w.proofArrival = append(w.proofArrival, arrived)
	w.proofCalled = append(w.proofCalled, time.Now())
	w.mu.Unlock()
	return w.refuse, nil
}

func (w *arrivalRecordingWaiter) RequestCheckoutRefreshAfterProof(
	ctx context.Context, checkoutID, root string, _ indexer.CheckoutFreshProof,
) (*indexer.CheckoutRefreshTicket, error) {
	arrived, _ := indexer.FreshRequestArrival(ctx)
	w.mu.Lock()
	w.admitArrival = append(w.admitArrival, arrived)
	w.mu.Unlock()
	return w.RequestCheckoutRefresh(ctx, checkoutID, root)
}

// The proof and the ticket admitted right after it are both bounded by the
// instant the request entered the server — not by when the proof ran — so a
// sample another concurrent request of the checkout began after that arrival
// can answer them (indexer.WithFreshRequestArrival), and no sample begun
// before it can.
func TestRequireFreshHandsTheRequestArrivalToTheProofAndItsTicket(t *testing.T) {
	stack := newViewStack(t)
	waiter := &arrivalRecordingWaiter{
		fakeFreshnessWaiter: &fakeFreshnessWaiter{
			answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
				return settledTicket(checkoutID, root, uint64(stack.dirty)), nil
			},
		},
		refuse: indexer.CheckoutFreshProof{Reason: indexer.FreshProofRouteNotActive},
	}
	stack.srv.freshnessWaiter = waiter

	called := time.Now()
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", freshArgs(nil, time.Minute), captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))

	waiter.mu.Lock()
	defer waiter.mu.Unlock()
	require.Len(t, waiter.proofArrival, 1)
	require.Len(t, waiter.admitArrival, 1, "the refused proof did not fall back through the ticket-after-proof admission")
	arrived := waiter.proofArrival[0]
	require.False(t, arrived.IsZero(), "the proof was handed no request arrival")
	require.False(t, arrived.Before(called), "the recorded arrival %v predates the call %v", arrived, called)
	require.False(t, arrived.After(waiter.proofCalled[0]), "the recorded arrival is later than the proof itself")
	require.True(t, waiter.admitArrival[0].Equal(arrived), "the ticket was bounded by %v, the proof by %v", waiter.admitArrival[0], arrived)
}
