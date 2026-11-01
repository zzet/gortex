package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDiscoveryAttemptsRetainSessionCWDAndRefusalTarget(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "pending", "subdir")
	root := filepath.Dir(cwd)
	details := map[string]any{"error_code": "view_building", "path": root,
		"family": "family-a", "reason": "family_capacity", "state": "discovery_pending", "retryable": true}
	socket := startStub(t, func(_ stubFrame) (any, *rpcError) {
		return nil, &rpcError{Code: -32000, Message: "checkout discovery unavailable", Data: details}
	})
	client, err := openMCP(t.Context(), socket, cwd)
	require.NoError(t, err)
	defer client.Close()
	_, attempts, err := callTool(t.Context(), client, request{Tool: "search", Operation: "symbols"},
		map[string]any{"operation": "symbols", "query": "Target"}, "workload", 1, 0, 0, time.Second,
		func(context.Context, time.Duration) error { return nil })
	require.Error(t, err)
	require.Len(t, attempts, 1)
	encoded, err := json.Marshal(attempts[0])
	require.NoError(t, err)
	var observation map[string]any
	require.NoError(t, json.Unmarshal(encoded, &observation))
	require.Equal(t, cwd, observation["cwd"], "request cwd must remain distinct from its pending checkout root")
	require.Equal(t, details, observation["error_data"], "refusal scope must survive report serialization")
}
