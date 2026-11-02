package mcp

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
)

func TestMutationAckIncludesExistingTierProducerEvidence(t *testing.T) {
	s, root := setupTestServer(t)
	watcher, err := indexer.NewWatcher(s.indexer, config.WatchConfig{Enabled: true, DebounceMs: 60000}, zap.NewNop())
	require.NoError(t, err)
	s.SetWatcher(watcher)
	require.NoError(t, watcher.Start([]string{root}))
	t.Cleanup(func() { require.NoError(t, watcher.Stop()) })
	s.mutationReindexWait = time.Nanosecond
	result := callTool(t, s, "edit_file", map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "func helper() {}", "new_string": "func helper() { _ = 3 }", "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	response := extractTextResult(t, result)
	require.NotZero(t, response["reindex_generation"], "prove real watcher admission before checking evidence")
	states, ok := response["tier_state"].(map[string]any)
	require.True(t, ok, "the acknowledgement must consume the existing ticket-owned tier producer")
	require.Len(t, states, 4)
	require.Equal(t, "fresh", states["file"], "only the exact live physical projection supplies this additional tier")
	ack, ok := response["ack_at"].(string)
	require.True(t, ok)
	at, err := time.Parse(time.RFC3339Nano, ack)
	require.NoError(t, err)
	require.False(t, at.IsZero())
	for tier, field := range map[string]string{"file_graph": "file_graph_at", "relations": "relations_at", "derived": "derived_at"} {
		require.Contains(t, []string{"pending", "fresh", "partial", "failed"}, states[tier])
		if states[tier] == "fresh" {
			value, ok := response[field].(string)
			require.True(t, ok, "fresh %s must have actual publication time", tier)
			stamp, err := time.Parse(time.RFC3339Nano, value)
			require.NoError(t, err)
			require.False(t, stamp.IsZero())
		}
	}
}
