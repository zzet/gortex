package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestMoveFileObservationActualDirectReconcileIsBoundPerFile(t *testing.T) {
	s, _ := setupContractMultiRepoServer(t, "fixture")
	t.Cleanup(func() {
		s.DrainBackground()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, s.multiIndexer.Close(ctx))
	})
	root, ok := s.multiIndexer.RepoRoot("fixture")
	require.True(t, ok)
	sourcePath, targetPath := filepath.Join(root, "source.go"), filepath.Join(root, "target.go")
	require.NoError(t, os.WriteFile(sourcePath, []byte("package main\nfunc Relocated() int { return 7 }\nfunc StableCaller() int { return Relocated() }\n"), 0600))
	require.NoError(t, os.WriteFile(targetPath, []byte("package main\nfunc DestinationAnchor() int { return 2 }\n"), 0600))
	_, err := s.multiIndexer.IncrementalReindexRepo("fixture", []string{sourcePath, targetPath})
	require.NoError(t, err)
	oldID, newID := "fixture/source.go::Relocated", "fixture/target.go::Relocated"
	s.facades.capture(mcpgo.NewTool("move_symbol"), s.handleMoveSymbol)
	req := editOperationRequest("refactor", "move")
	req.Params.Arguments = map[string]any{"operation": "move", "target": map[string]any{"symbol": oldID}, "destination": "fixture/target.go", "output": map[string]any{"format": "json"}}
	result, err := s.boundToolHandler(func(ctx context.Context, r mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return s.handleFacade(ctx, "refactor", r)
	})(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError)
	body := unmarshalResult(t, result)
	require.Equal(t, oldID, body["source_id"])
	require.Equal(t, newID, body["target_id"])
	operation, ok := body["move_observation_id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, operation)
	refs, ok := body["file_observations"].([]any)
	require.True(t, ok)
	require.Len(t, refs, 2)
	sharedRPC := ""
	for _, raw := range refs {
		ref := raw.(map[string]any)
		id := ref["receipt"].(string)
		var status map[string]any
		require.Eventually(t, func() bool {
			status, _ = s.moveFileObservationPayload(t.Context(), id)
			return status["direct_reconcile_state"] == "observed"
		}, time.Second, time.Millisecond, "actual direct owner evidence missing: %v", status)
		require.Equal(t, "move_file_observation", status["receipt_kind"])
		require.Equal(t, operation, status["operation_id"])
		require.Equal(t, oldID, status["source_id"])
		require.Equal(t, newID, status["target_id"])
		require.Equal(t, ref["path"], status["path"])
		require.Equal(t, ref["role"], status["role"])
		require.Equal(t, "all_writes_completed", status["move_write_verdict"])
		require.Equal(t, mutationDiskCommitted, status["file_write_status"])
		require.Equal(t, true, status["writer_return_observed"])
		require.Equal(t, "not_applicable_direct_reconcile", status["requested_background_state"])
		require.NotContains(t, status, "mutation_id")
		require.NotContains(t, status, "transaction_id")
		require.NotContains(t, status, "reindex_receipt")
		ack := status["acknowledgement"].(acknowledgementPayload)
		require.NotNil(t, ack.RPCID)
		if sharedRPC == "" {
			sharedRPC = *ack.RPCID
		} else {
			require.Equal(t, sharedRPC, *ack.RPCID, "shared A must be deduplicated by its actual RPC owner")
		}
		direct := status["direct_reconcile"].(map[string]any)
		require.Equal(t, "move_direct_reconcile", direct["scope"])
		require.Equal(t, "monotonic", direct["clock"])
		require.Equal(t, status["new_sha"], direct["accepted_blob_sha"])
		require.Positive(t, direct["owner_generation"])
		require.Positive(t, direct["window_ns"])
		require.Equal(t, false, direct["additive_with_acknowledgement"])
		phases := direct["phase_accounting"].(map[string]any)
		require.Equal(t, "move_direct_reconcile", phases["scope"])
		require.Equal(t, true, phases["observed_partition_complete"])
		require.Equal(t, direct["window_ns"], phases["window_ns"])
		require.Equal(t, phases["window_ns"], phases["observed_subtotal_ns"])
		states := phases["phase_states"].(map[string]string)
		require.Len(t, states, 9)
		for name, state := range states {
			require.Equal(t, "measured", state, "missing actual move executor phase %s", name)
		}
		direct["accepted_blob_sha"] = "foreign"
		again, _ := s.moveFileObservationPayload(t.Context(), id)
		require.Equal(t, status["new_sha"], again["direct_reconcile"].(map[string]any)["accepted_blob_sha"], "caller mutation changed retained observation")
	}
}
