package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func TestMutationStatusFallsThroughFreshnessReceiptID(t *testing.T) {
	request := func(id string) mcp.CallToolRequest {
		req := mcp.CallToolRequest{}
		req.Params.Name = "mutation_status"
		req.Params.Arguments = map[string]any{"mutation_id": id}
		return req
	}
	newID := func() string { return fmt.Sprintf("mutation-%d", mutationReceiptSequence.Add(1)) }

	t.Run("live pending and failed freshness receipts", func(t *testing.T) {
		for _, failed := range []bool{false, true} {
			s := newMutationServer()
			id := newID()
			receipt := pendingFreshnessReceipt(s, id, "fixture", "/fixture/edit.go", 7)
			if failed {
				completeFreshnessReceipt(receipt, indexer.MutationResult{Err: errors.New("patch failed")})
			}
			res, err := s.handleMutationStatus(context.Background(), request(id))
			require.NoError(t, err)
			payload := resultJSON(t, res)
			require.Equal(t, id, payload["receipt"])
			require.Equal(t, "graph_refresh", payload["receipt_kind"])
			require.Equal(t, "unrecorded", payload["disk_status"])
			require.Equal(t, "this is a graph freshness receipt id, not a caller mutation_id", payload["note"])
			require.Equal(t, failed, payload["graph_status_terminal"])
		}
	})

	t.Run("missing receipt reports lost", func(t *testing.T) {
		s := newMutationServer()
		id := newID()
		res, err := s.handleMutationStatus(context.Background(), request(id))
		require.NoError(t, err)
		require.Equal(t, "lost", resultJSON(t, res)["state"])
		for _, callerKey := range []string{"mutation-", "mutation-abc", "mutation-1suffix", "mutation-１２", "prefix-mutation-1"} {
			res, err = s.handleMutationStatus(context.Background(), request(callerKey))
			require.NoError(t, err)
			require.Equal(t, "lost", resultJSON(t, res)["state"])
		}
	})

	t.Run("caller key takes precedence", func(t *testing.T) {
		s := newMutationServer()
		id := newID()
		pendingFreshnessReceipt(s, id, "fixture", "/fixture/edit.go", 7)
		record := s.beginMutationCommit(context.Background(), "edit_file", id, "", "edit.go", "/fixture/edit.go")
		res, err := s.handleMutationStatus(context.Background(), request(id))
		require.NoError(t, err)
		payload := resultJSON(t, res)
		require.Equal(t, record.id, payload["receipt"])
		require.Equal(t, id, payload["mutation_id"])
		require.NotEqual(t, "graph_refresh", payload["receipt_kind"])
		require.NotContains(t, payload, "note")
	})

	t.Run("fallback preserves checkout incarnation scope", func(t *testing.T) {
		s := newMutationServer()
		id := newID()
		receipt := pendingFreshnessReceipt(s, id, "fixture", "/fixture/edit.go", 7)
		receipt.checkoutID = "selected"
		receipt.checkoutIncarnation = "current"
		for _, scope := range []struct{ checkoutID, incarnation string }{
			{"", ""}, {"sibling", "current"}, {"selected", "retired"}, {"selected", "current"},
		} {
			ctx := context.WithValue(context.Background(), checkoutMutationContextKey{}, &checkoutMutationState{
				checkoutID: scope.checkoutID, incarnation: scope.incarnation,
			})
			res, err := s.handleMutationStatus(ctx, request(id))
			require.NoError(t, err)
			if scope.checkoutID == "selected" && scope.incarnation == "current" {
				require.Equal(t, id, resultJSON(t, res)["receipt"])
			} else {
				require.Equal(t, "lost", resultJSON(t, res)["state"])
				require.NotContains(t, resultText(res), receipt.path)
			}
		}
	})
}
