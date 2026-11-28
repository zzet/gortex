package mcp

import (
	"context"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// A linked checkout publishes an edit in one coordinator refresh cycle, not in
// a watcher's debounced point attempt. Its receipt must still close its A/P/B
// accounting the way the primary's does, so a client polling it stops on the
// first poll that reads the publication:
//
//   - B is a typed disposition. The cycle arms no requested-background timer,
//     so no measured window exists and the receipt carries the frozen refusal,
//     in the ticket's generation domain.
//   - P names the cycle that served the ticket, without relabelling its
//     builder phases as patch laps.
//   - A counts the acknowledgement's publication wait as not applicable: the
//     linked ACK returns before publication and never waits.
//   - applied_generation is in the ticket's domain, beside reindex_generation;
//     the routed dirty generation stays on the receipt as dirty_generation_id.
func TestLinkedCheckoutEditReceiptClosesItsAccounting(t *testing.T) {
	f := newRealCheckoutMutationFixture(t)
	written := f.edit(t, f.worktree, map[string]any{
		"path": "repo/edit.go", "old_string": "func New() {}",
		"new_string": "func LinkedAccounting() {}", "format": "json",
	})
	require.False(t, written.IsError, viewResultText(t, written))
	ack := decodeFileOpsResult(t, written)
	publication, ok := ack["reindex_receipt"].(string)
	require.True(t, ok, "committed linked write needs a publication receipt: %+v", ack)
	value, found := f.srv.mutationReceipts.Load(publication)
	require.True(t, found)
	require.Equal(t, f.checkoutID, value.(*mutationReceipt).checkoutID, "the edit must be published by the linked checkout")
	require.Equal(t, "pending", ack["graph_status"], "a linked ACK returns before publication: %+v", ack)

	receipt, ok := ack["mutation_receipt"].(string)
	require.True(t, ok, "committed write needs a pollable receipt: %+v", ack)
	// The first poll that reads the publication must already carry the
	// terminal accounting: no later poll is needed to close it.
	var status map[string]any
	require.Eventually(t, func() bool {
		status = f.mutationStatus(t, f.worktree, f.worktree, receipt)
		require.NotEqual(t, "failed", status["graph_status"], "unexpected failed receipt: %+v", status)
		return status["graph_status"] == "fresh"
	}, 20*time.Second, 10*time.Millisecond, "linked edit never published")
	require.Equal(t, f.checkoutID, status["checkout_id"])
	require.Equal(t, true, status["graph_status_terminal"])

	requested, ok := status["reindex_generation"].(float64)
	require.True(t, ok && requested > 0, "a linked receipt names its ticket: %+v", status)
	require.Equal(t, requested, status["applied_generation"],
		"applied_generation must be in the ticket's domain, not the dirty generation: %+v", status)
	phases, ok := status["publication_phases"].(map[string]any)
	require.True(t, ok, "a published linked receipt carries its publication timeline: %+v", status)
	require.Equal(t, "mcp_edit", phases["source"])
	require.Equal(t, requested, phases["ticket"])
	require.Equal(t, true, phases["terminal"])
	dirty, ok := phases["dirty_generation_id"].(float64)
	require.True(t, ok && dirty > 0, "the publication names its routed dirty generation: %+v", phases)
	require.Equal(t, dirty, status["dirty_generation_id"], "the receipt keeps the routed dirty generation: %+v", status)

	// B: no measured window, and the typed refusal for this publication.
	require.NotContains(t, status, "requested_background", "a checkout cycle has no measured requested-background window: %+v", status)
	require.NotContains(t, status, "coalesced_publication", "a linked ticket is applied in its own domain: %+v", status)
	background, ok := status["requested_background_unavailable"].(map[string]any)
	require.True(t, ok, "a linked receipt needs a typed B disposition: %+v", status)
	require.Equal(t, map[string]any{
		"scope":                    "requested_background_observation",
		"reason":                   "serving_attempt_binding_mismatch",
		"binding_predicate":        "checkout_refresh_cycle",
		"background_stage":         "returned",
		"accepted_attempt_stage":   "physical_window_retained",
		"publication_key":          phases["key"],
		"requested_generation":     requested,
		"last_observed_generation": requested,
		"observed_native_handoffs": float64(0),
	}, background)

	// P: the cycle that served the ticket, with its patch laps unobserved.
	timing, ok := status["phase_timing"].(map[string]any)
	require.True(t, ok, "a receipt carries its timing window: %+v", status)
	wall, ok := timing["patch_wall_ms"].(float64)
	require.True(t, ok && wall >= 0, "the serving cycle's wall is measured: %+v", timing)
	patch, ok := timing["patch_accounting"].(map[string]any)
	require.True(t, ok, "a linked receipt needs a P block: %+v", timing)
	require.Equal(t, "checkout_refresh_cycle", patch["execution_disposition"])
	require.Equal(t, "accepted_winning_physical_attempt", patch["scope"])
	require.Equal(t, requested, patch["attempt_ticket"])
	require.Equal(t, float64(0), patch["attempt_retry"])
	require.Equal(t, "not_observed", patch["sum_state"])
	require.Equal(t, false, patch["observed_partition_complete"])
	require.Equal(t, "not_established", patch["background_coverage"])
	laps, ok := patch["phases_ms"].(map[string]any)
	require.True(t, ok)
	lapStates, ok := patch["phase_states"].(map[string]any)
	require.True(t, ok)
	require.NotEmpty(t, laps)
	for name, ms := range laps {
		require.Nil(t, ms, "checkout builder work is not relabelled as patch lap %s: %+v", name, patch)
		require.Equal(t, "not_observed", lapStates[name], name)
	}

	// A: the publication wait is counted, and not applicable.
	acknowledgement, ok := status["acknowledgement"].(map[string]any)
	require.True(t, ok, "a receipt carries its acknowledgement: %+v", status)
	require.Equal(t, "observed", acknowledgement["state"])
	ackStates := acknowledgement["phase_states"].(map[string]any)
	require.Equal(t, "not_applicable", ackStates["reindex_wait"], "a linked ACK never waits on its publication: %+v", acknowledgement)
	ackPhases := acknowledgement["phases_ms"].(map[string]any)
	require.Contains(t, ackPhases, "reindex_wait")
	require.Nil(t, ackPhases["reindex_wait"], "a wait that never happened has no duration")
	observed := acknowledgement["observed_execution_ms"].(map[string]any)
	require.Contains(t, observed, "reindex_wait")
	require.Nil(t, observed["reindex_wait"])
	require.Equal(t, map[string]any{"started": float64(1), "completed": float64(1), "not_applicable": float64(1)},
		acknowledgement["counts"].(map[string]any)["reindex_wait"])
	require.Equal(t, true, acknowledgement["disjoint_observed_intervals"])
}

// A source operation that commits several files is acknowledged once, at its
// frontier (awaitSourceMutationFrontier), not file by file. On a linked
// checkout each committed file's publication is queued on the coordinator.
//
// A routed linked view admits no refactor (refuseRoutedViewMutation), so the
// move runs under a checkout lease acquired the way an approved edit's is,
// through the production middleware and handler. One lease admits one
// publication ticket: the move's first file publishes and its second is
// refused admission after its disk commit, which the test records rather
// than hides.
func TestLinkedCheckoutMoveUnderOneLeasePublishesOneFile(t *testing.T) {
	f := newRealCheckoutMutationFixture(t)
	route, found, err := f.store.Catalog().GetCheckoutRoute(t.Context(), f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	lease, err := f.srv.lifecycle.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, route.RouteEpoch)
	require.NoError(t, err)
	ctx := withCheckoutMutation(WithSessionCWD(WithSessionID(context.Background(), "linked-move-accounting"), f.worktree), lease, f.worktree)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "move_symbol"
	req.Params.Arguments = map[string]any{"id": "repo/edit.go::New", "target_file": "repo/moved.go", "format": "json"}
	moved, err := f.srv.wrapToolHandler(f.srv.handleMoveSymbol)(ctx, req)
	lease.Close()
	require.NoError(t, err)
	require.False(t, moved.IsError, resultText(moved))
	observations, ok := decodeFileOpsResult(t, moved)["file_observations"].([]any)
	require.True(t, ok && len(observations) == 2, "a move commits its source and its target: %s", resultText(moved))

	outcomes := map[string]int{}
	for _, raw := range observations {
		receipt := raw.(map[string]any)["receipt"].(string)
		var status, commit map[string]any
		require.Eventually(t, func() bool {
			status = f.mutationStatus(t, f.worktree, f.worktree, receipt)
			commit, _ = status["physical_commit"].(map[string]any)
			return commit != nil && commit["graph_status_terminal"] == true
		}, 20*time.Second, 10*time.Millisecond, "move file %s never settled", receipt)
		require.Equal(t, f.checkoutID, commit["checkout_id"], "the move must be published by the linked checkout")
		outcomes[commit["graph_status"].(string)]++
		if commit["graph_status"] == "failed" {
			require.Equal(t, "admission_failed", status["requested_background_state"])
			require.Contains(t, commit["error"], "no unqueued committed checkout mutation")
		} else {
			require.Equal(t, "admitted", status["requested_background_state"], "a queued checkout admission")
		}
	}
	require.Equal(t, map[string]int{"fresh": 1, "failed": 1}, outcomes, "one lease publishes one of the move's files")
}
