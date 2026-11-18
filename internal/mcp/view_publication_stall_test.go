package mcp

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// A require_fresh wait that did not go fresh while the checkout's coordinator
// was stalled says so beside fresh_reason: publication_stalled carries the
// stall's reason, its start and how many cycles published nothing. A fresh
// answer, or one with no stall, carries no such field.
func TestFreshnessRiderReportsAPublicationStall(t *testing.T) {
	since := time.Unix(1_800_000_000, 0)
	stall := &indexer.CheckoutPublicationStall{CheckoutID: viewTestWorktree, ConsecutiveNonpublishingCycles: 4,
		Since: since.Unix(), LastPublicationAgeSeconds: 42, StallReason: "failed:catalog_guard", ChangeSetSize: 20}
	rider := func(outcome *requestFreshnessOutcome) map[string]any {
		view := &requestView{kind: requestViewKindBase,
			rider: graphview.NewViewRider(graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: viewTestWorktree})}
		return viewRiderFields(annotateRequestFreshness(view, outcome, time.Now()))
	}

	fields := rider(&requestFreshnessOutcome{reason: freshReasonDeadlineExceeded, stalled: stall})
	require.Equal(t, freshReasonDeadlineExceeded, fields["fresh_reason"], "fields = %v", fields)
	require.Equal(t, map[string]any{
		"reason": "failed:catalog_guard",
		"since":  since.UTC().Format(time.RFC3339),
		"cycles": 4,
	}, fields["publication_stalled"], "fields = %v", fields)

	fields = rider(&requestFreshnessOutcome{reason: freshReasonDeadlineExceeded})
	require.NotContains(t, fields, "publication_stalled", "a wait with no stall claims one: %v", fields)
	fields = rider(&requestFreshnessOutcome{fresh: true, stalled: stall})
	require.NotContains(t, fields, "publication_stalled", "a fresh answer carries a stall: %v", fields)
}

// stallingFreshnessWaiter is a waiter whose coordinator has a publication
// stall, the lookup the checkout lifecycle answers in production.
type stallingFreshnessWaiter struct {
	*fakeFreshnessWaiter
	stall   indexer.CheckoutPublicationStall
	stalled bool

	mu     sync.Mutex
	lookup []string
}

func (w *stallingFreshnessWaiter) CheckoutPublicationStalled(checkoutID string) (indexer.CheckoutPublicationStall, bool) {
	w.mu.Lock()
	w.lookup = append(w.lookup, checkoutID)
	w.mu.Unlock()
	return w.stall, w.stalled
}

// A require_fresh wait that runs out while the coordinator it waited on has
// published nothing for three cycles answers with the stall in its rider,
// read from that waiter for the checkout the wait named. With no stall, the
// rider carries no such field.
func TestRequireFreshExpiryReportsThePublicationStall(t *testing.T) {
	since := time.Unix(1_800_000_000, 0)
	for _, stalled := range []bool{true, false} {
		stack := newViewStack(t)
		waiter := &stallingFreshnessWaiter{
			fakeFreshnessWaiter: &fakeFreshnessWaiter{
				answer: func(_ int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
					return pendingTicket(checkoutID, root), nil
				},
			},
			stall: indexer.CheckoutPublicationStall{CheckoutID: viewTestWorktree, ConsecutiveNonpublishingCycles: 3,
				Since: since.Unix(), StallReason: "torn_by_motion", ChangeSetSize: 2},
			stalled: stalled,
		}
		stack.srv.freshnessWaiter = waiter

		var reader graph.Reader
		res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", freshArgs(nil, 200*time.Millisecond), captureReader(stack.srv, &reader))
		require.NoError(t, err)
		require.False(t, res.IsError, "an expired wait without require_exact must still answer: %s", viewResultText(t, res))
		rider := resultFreshness(t, res)
		require.Equal(t, false, rider["fresh"], "rider = %v", rider)
		require.Equal(t, freshReasonDeadlineExceeded, rider["fresh_reason"], "rider = %v", rider)
		waiter.mu.Lock()
		lookup := append([]string(nil), waiter.lookup...)
		waiter.mu.Unlock()
		require.Contains(t, lookup, viewTestWorktree, "the wait never asked its waiter for a stall")
		if !stalled {
			require.NotContains(t, rider, "publication_stalled", "a coordinator with no stall reported one: %v", rider)
			continue
		}
		require.Equal(t, map[string]any{
			"reason": "torn_by_motion",
			"since":  since.UTC().Format(time.RFC3339),
			"cycles": float64(3),
		}, rider["publication_stalled"], "rider = %v", rider)
	}
}
