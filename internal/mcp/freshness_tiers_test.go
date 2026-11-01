package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestRestartLostReceiptsReturnWithoutWaiting(t *testing.T) {
	for _, key := range []string{"receipt", "mutation_id"} {
		t.Run(key, func(t *testing.T) {
			s := newMutationServer()
			req := mcp.CallToolRequest{}
			req.Params.Name = "mutation_status"
			req.Params.Arguments = map[string]any{key: "mutation-100", "require_fresh": true,
				"wait_deadline": time.Now().Add(time.Minute).Format(time.RFC3339Nano)}
			res, err := s.handleMutationStatus(context.Background(), req)
			require.NoError(t, err)
			payload := resultJSON(t, res)
			require.Equal(t, "lost", payload["state"])
			require.Equal(t, false, payload["found"])
			require.Equal(t, true, payload["graph_status_terminal"])
			require.NotContains(t, payload, "graph_freshness")
		})
	}
}

func TestFreshnessRiderLabelsEveryOutputMode(t *testing.T) {
	rider := map[string]any{"freshness_scope": "relations", "graph_freshness": "pending",
		"pending_file_count": 2, "pending_files": []string{"caller.go", "target.go"}}
	for _, body := range []string{"clean: checked 2 callers", "callers[1]{name}:\n  Caller", "[1,2]"} {
		res := decorateResultWithFreshness(mcp.NewToolResultText(body), rider)
		require.Equal(t, "relations: pending (2 files)\n"+body, resultText(res))
	}
	jsonResult := decorateResultWithFreshness(mcp.NewToolResultText(`{"ok":true}`), rider)
	require.Equal(t, "pending", resultJSON(t, jsonResult)["freshness"].(map[string]any)["graph_freshness"])
}

func TestMutationIDsAreEpochSeeded(t *testing.T) {
	// Counters beginning at one can reuse a previous process's receipt after a
	// restart, turning an explicitly lost write into an unrelated live write.
	const plausibleEpoch = uint64(1_000_000_000_000_000_000)
	require.Greater(t, mutationReceiptSequence.Load(), plausibleEpoch)
	require.Greater(t, mutationCommitSequence.Load(), plausibleEpoch)
}
