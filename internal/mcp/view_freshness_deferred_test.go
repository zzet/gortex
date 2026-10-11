package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

// A require_fresh request composes its checkout's generation stack once,
// after the wait. The selection before the wait only names the checkout: it
// makes every scope and route refusal, and opens no generation. The route
// here names a working-tree generation that can no longer be opened, which a
// pre-wait composition would refuse on; the coordinator then publishes a
// servable one, and the request must wait for it and answer from it.
func TestRequireFreshComposesTheStackOnlyAfterTheWait(t *testing.T) {
	stack := newViewStack(t)
	caughtUp := writeCaughtUpDirtyGeneration(t, stack)
	require.NoError(t, stack.store.Catalog().SetViewGenerationState(
		context.Background(), stack.dirty, store_sqlite.ViewGenerationFailed))
	heldDuringWait := -1
	waiter := &fakeFreshnessWaiter{
		answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			heldDuringWait = stack.leases.Held()
			routeViewCheckout(t, stack.store, stack.graphID, stack.commit, caughtUp, store_sqlite.RouteActive)
			return settledTicket(checkoutID, root, uint64(caughtUp)), nil
		},
	}
	stack.srv.freshnessWaiter = waiter

	var reader graph.Reader
	args := freshArgs(map[string]any{
		"view": map[string]any{"kind": "worktree", "path": stack.worktreeRoot},
	}, time.Minute)
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", args, captureReader(stack.srv, &reader))
	require.NoError(t, err)
	require.False(t, res.IsError, "the pre-wait selection composed the stale stack and refused on it: %s", viewResultText(t, res))
	require.Len(t, waiter.observed(), 1, "the request must reach the settle signal")
	require.Equal(t, 0, heldDuringWait, "the pre-wait selection leased generations")
	require.True(t, hasNode(reader, "repo/keep.go::AfterTheWait"), "the request did not answer from the route it waited for")
	rider := resultFreshness(t, res)
	require.Equal(t, true, rider["fresh"], "rider = %v", rider)
	require.Equal(t, true, rider["exact"], "rider = %v", rider)
}

// Without require_fresh nothing is deferred: the same unopenable route is
// refused exactly as before.
func TestAnOrdinaryRequestStillComposesTheStackItAnswersFrom(t *testing.T) {
	stack := newViewStack(t)
	require.NoError(t, stack.store.Catalog().SetViewGenerationState(
		context.Background(), stack.dirty, store_sqlite.ViewGenerationFailed))
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", map[string]any{
		"view": map[string]any{"kind": "worktree", "path": stack.worktreeRoot},
	}, captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	require.True(t, res.IsError, "an explicit worktree selector over an unopenable generation must refuse: %s", viewResultText(t, res))
}
