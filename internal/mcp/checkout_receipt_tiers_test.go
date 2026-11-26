package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// requireTierStateKeys checks one edit payload's tier_state carries exactly
// the four tiers, each in its typed vocabulary, and that every fresh graph
// tier has the publication stamp a client times it by.
func requireTierStateKeys(t *testing.T, label string, payload map[string]any) map[string]any {
	t.Helper()
	states, ok := payload["tier_state"].(map[string]any)
	require.True(t, ok, "%s: tier_state must be an object: %+v", label, payload)
	require.ElementsMatch(t, []string{"file", "file_graph", "relations", "derived"}, mapKeys(states),
		"%s: a linked checkout must report the same four tiers as the primary: %+v", label, states)
	require.Contains(t, []string{"fresh", "pending"}, states["file"], label)
	ack, ok := payload["ack_at"].(string)
	require.True(t, ok, "%s: graph tiers need the acknowledgement stamp: %+v", label, payload)
	_, err := time.Parse(time.RFC3339Nano, ack)
	require.NoError(t, err, label)
	for tier, field := range map[string]string{"file_graph": "file_graph_at", "relations": "relations_at", "derived": "derived_at"} {
		require.Contains(t, []string{"pending", "fresh", "partial", "failed"}, states[tier], "%s: %s", label, tier)
		if states[tier] == "fresh" {
			value, ok := payload[field].(string)
			require.True(t, ok, "%s: fresh %s must carry %s: %+v", label, tier, field, payload)
			_, err := time.Parse(time.RFC3339Nano, value)
			require.NoError(t, err, label)
		}
	}
	return states
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

// A linked checkout publishes its own working-tree generation, which holds the
// edited file's syntax graph and its relations. Its edit ACK and its receipt
// must carry the same tier evidence a primary edit carries, not the file tier
// alone. The edit changes a function body, so the checkout owes its clone rows
// to a follow-up; as on the primary, that does not hold derived back.
func TestLinkedCheckoutEditReceiptCarriesGraphTierStates(t *testing.T) {
	f := newRealCheckoutMutationFixture(t)
	written := f.edit(t, f.worktree, map[string]any{
		"path": "repo/edit.go", "old_string": "func New() {}",
		"new_string": "func LinkedTiers() {}", "format": "json",
	})
	require.False(t, written.IsError, viewResultText(t, written))
	ack := decodeFileOpsResult(t, written)

	// Prove the write took the linked route: its publication receipt is
	// scoped to the worktree's checkout, not to the primary's watcher.
	publication, ok := ack["reindex_receipt"].(string)
	require.True(t, ok, "committed linked write needs a publication receipt: %+v", ack)
	value, found := f.srv.mutationReceipts.Load(publication)
	require.True(t, found)
	require.Equal(t, f.checkoutID, value.(*mutationReceipt).checkoutID, "the edit must be published by the linked checkout")
	requireTierStateKeys(t, "ack", ack)

	receipt, ok := ack["mutation_receipt"].(string)
	require.True(t, ok, "committed write needs a pollable receipt: %+v", ack)
	var status map[string]any
	require.Eventually(t, func() bool {
		status = f.mutationStatus(t, f.worktree, f.worktree, receipt)
		require.NotEqual(t, "failed", status["graph_status"], "unexpected failed receipt: %+v", status)
		return status["graph_status"] == "fresh"
	}, 20*time.Second, 10*time.Millisecond, "linked edit never published")
	require.Equal(t, f.checkoutID, status["checkout_id"])
	states := requireTierStateKeys(t, "receipt", status)
	require.Equal(t, "fresh", states["file_graph"], "a published checkout generation holds the file graph: %+v", status)
	require.Equal(t, "fresh", states["relations"], "a published checkout generation holds the file's relations: %+v", status)
	require.Equal(t, "fresh", states["derived"], "clone rows owed to a follow-up do not make derived partial: %+v", status)

	// The stamps are the daemon's own publication clock: the acknowledgement
	// precedes the one publication instant all three graph tiers share.
	stamp := func(field string) time.Time {
		value, ok := status[field].(string)
		require.True(t, ok, "%s must be stamped: %+v", field, status)
		at, err := time.Parse(time.RFC3339Nano, value)
		require.NoError(t, err)
		return at
	}
	published := stamp("file_graph_at")
	require.False(t, published.Before(stamp("ack_at")), "publication precedes the acknowledgement: %+v", status)
	require.True(t, stamp("relations_at").Equal(published), "relations_at differs from file_graph_at: %+v", status)
	require.True(t, stamp("derived_at").Equal(published), "derived_at differs from file_graph_at: %+v", status)
	after, ok := status["tier_after_commit_ms"].(map[string]any)
	require.True(t, ok, "a published linked edit reports its daemon-side tier latency: %+v", status)
	for _, tier := range []string{"file_graph", "relations", "derived"} {
		ms, ok := after[tier].(float64)
		require.True(t, ok, "%s latency must be measured once published: %+v", tier, after)
		require.GreaterOrEqual(t, ms, 0.0)
	}
}
