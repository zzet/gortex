package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// Exercise the real public facade/transaction serializer for the explicit
// whole-file edit_file fixture. A request-shape fixture alone cannot prove
// that its durable file, shared RPC and graph references join as advertised.
func TestBatchOneFileWholeImagePublicAcknowledgementContract(t *testing.T) {
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s, root := setupTestServer(t)
	s.facades.capture(mcpgo.NewTool("batch_edit"), s.handleBatchEdit)
	var schedules, writes atomic.Int64
	s.watcher = mutationTestWatcher{generation: 41, scheduled: &schedules}
	s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
		writes.Add(1)
		return durableAtomicWriteFile(path, content, mode)
	}
	source := filepath.Join(root, "batch-full-image.go")
	before := []byte("package fixture\nfunc Callee() int { return 1 }\n// anchor\n")
	after := []byte("package fixture\nfunc Callee() int { return 1 }\n// anchor\nfunc Added() int { return Callee() }\n")
	require.NoError(t, os.WriteFile(source, before, 0600))
	abs, relative, err := s.resolveFilePath(t.Context(), source)
	require.NoError(t, err)
	req := editOperationRequest("edit", "batch")
	req.Params.Arguments = map[string]any{"operation": "batch", "changes": []any{map[string]any{"op": "edit_file", "path": source, "old_string": string(before), "new_string": string(after)}}, "options": map[string]any{"transaction_id": "whole-file-transaction"}, "output": map[string]any{"format": "json", "max_bytes": 0}}
	h := s.boundToolHandler(func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return s.handleFacade(ctx, "edit", req)
	})
	call := func() map[string]any {
		result, err := h(t.Context(), req)
		require.NoError(t, err)
		require.False(t, result.IsError, "%v", result.Content)
		return unmarshalResult(t, result)
	}
	first := call()
	require.Equal(t, "whole-file-transaction", first["transaction_id"])
	require.Equal(t, "committed", first["status"])
	require.Equal(t, "committed", first["disk_status"])
	ack, ok := first["acknowledgement_receipt"].(string)
	require.True(t, ok)
	require.NotEmpty(t, ack)
	require.Equal(t, "reference_only_query_mutation_status", first["acknowledgement_state"])
	observations := first["file_observations"].([]any)
	require.Len(t, observations, 1)
	observation := observations[0].(map[string]any)
	require.Equal(t, relative, observation["path"])
	observationID := observation["receipt"].(string)
	require.NotEqual(t, ack, observationID)
	status, handled := s.batchFileObservationPayload(t.Context(), observationID)
	require.True(t, handled)
	require.Equal(t, "whole-file-transaction", status["transaction_id"])
	require.Equal(t, "committed", status["batch_disk_verdict"])
	files := first["files"].([]any)
	results := first["results"].([]any)
	require.Len(t, files, 1)
	require.Len(t, results, 1)
	file := files[0].(map[string]any)
	result := results[0].(map[string]any)
	require.Equal(t, abs, file["path"])
	require.Equal(t, relative, file["relative_path"])
	hash := sha256.Sum256(after)
	require.Equal(t, hex.EncodeToString(hash[:]), file["after_sha256"])
	require.Equal(t, "edit_file", result["op"])
	require.Equal(t, "applied", result["status"])
	require.Equal(t, relative, result["path"])
	graph, ok := file["reindex_receipt"].(string)
	require.True(t, ok)
	require.NotEmpty(t, graph)
	require.NotEqual(t, ack, graph)
	require.NotEqual(t, observationID, graph)
	require.Equal(t, graph, status["publication_receipt"])
	require.NotEqual(t, first["transaction_id"], graph)
	require.NotEqual(t, first["transaction_id"], ack)
	require.Equal(t, file["reindex_receipt"], result["reindex_receipt"])
	require.EqualValues(t, 41, file["reindex_generation"])
	require.Equal(t, file["reindex_generation"], result["reindex_generation"])
	actual, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, after, actual)
	second := call()
	require.Equal(t, first["acknowledgement_receipt"], second["acknowledgement_receipt"])
	require.Equal(t, first["files"], second["files"])
	require.EqualValues(t, 1, writes.Load(), "replay does not regenerate whole-file intent")
	require.EqualValues(t, 1, schedules.Load(), "replay does not replace the original graph ticket")
}
