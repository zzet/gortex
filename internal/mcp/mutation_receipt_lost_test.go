package mcp

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestOrphanedCommitRecordTerminal(t *testing.T) {
	for _, state := range []string{"seed", "pending"} {
		for _, checkoutID := range []string{"", "checkout"} {
			t.Run(state+"/"+checkoutID, func(t *testing.T) {
				s := newMutationServer()
				ctx := context.WithValue(context.Background(), checkoutMutationContextKey{}, &checkoutMutationState{
					checkoutID: checkoutID, incarnation: "incarnation",
				})
				record := s.beginMutationCommit(ctx, "edit", "", "", "file.go", "/fixture/file.go")
				record.markCommitted("bytes", 5)
				receipt := pendingFreshnessReceipt(s, "mutation-lost", "fixture", "/fixture/file.go", 7)
				receipt.checkoutID, receipt.checkoutIncarnation = checkoutID, "incarnation"
				if state == "pending" {
					record.recordGraph(mutationReindexOutcome{Pending: true, Receipt: receipt.id})
				} else {
					// Reproduce an outcome not yet recorded by the disk handler.
					record.mu.Lock()
					record.reindexReceipt = receipt.id
					record.mu.Unlock()
				}
				s.mutationReceipts.Delete(receipt.id)
				req := mcp.CallToolRequest{}
				req.Params.Arguments = map[string]any{"receipt": record.id}
				res, err := s.handleMutationStatus(ctx, req)
				require.NoError(t, err)
				payload := resultJSON(t, res)
				require.Equal(t, mutationGraphFailed, payload["graph_status"])
				require.Equal(t, "receipt_lost", payload["reason"])
				require.Equal(t, true, payload["graph_status_terminal"])
				require.Equal(t, mutationDiskCommitted, payload["disk_status"])
				require.Contains(t, payload["graph_note"], "does not establish graph freshness")
				require.Equal(t, "receipt_lost", mutationCommitListingPayload([]*mutationCommitRecord{record})[0]["reason"])
				if checkoutID == "" {
					require.Zero(t, pendingSourceVersionCount(s, ""))
				} else {
					require.Equal(t, 1, pendingSourceVersionCount(s, checkoutID), "a lost receipt is not publication evidence")
				}
			})
		}
	}

	t.Run("no ticket yet is not lost; recorded stale stays historical", func(t *testing.T) {
		s := newMutationServer()
		record := s.beginMutationCommit(context.Background(), "edit", "", "", "file.go", "/fixture/file.go")
		payload := s.mutationStatusPayload(record)
		require.Equal(t, mutationGraphStale, payload["graph_status"])
		require.Equal(t, false, payload["graph_status_terminal"])
		require.NotContains(t, payload, "reason")
		record.recordGraph(mutationReindexOutcome{Receipt: "unconfirmed"})
		payload = s.mutationStatusPayload(record)
		require.Equal(t, mutationGraphStale, payload["graph_status"])
		require.Equal(t, true, payload["graph_status_terminal"])
		require.NotContains(t, payload, "reason")
	})
}
