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

// A published edit's mutation_status says how long after the disk commit each
// tier was published, from the tier stamps it reports: the daemon-side
// publication latency of that edit. A tier not yet published is null, and a
// receipt without a commit time or a tier snapshot carries none.
func TestMutationStatusReportsTierLatencyAfterCommit(t *testing.T) {
	s, root := setupTestServer(t)
	watcher, err := indexer.NewWatcher(s.indexer, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(t, err)
	s.SetWatcher(watcher)
	// Native events stay out of the tool-admitted edit under test.
	require.NoError(t, watcher.Start([]string{t.TempDir()}))
	t.Cleanup(func() { require.NoError(t, watcher.Stop()) })
	s.mutationReindexWait = 60 * time.Second

	result := callTool(t, s, "edit_file", map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "func helper() {}", "new_string": "func helper() { _ = 3 }", "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	status := mutationStatusFor(t, s, extractTextResult(t, result))
	require.Equal(t, true, status["graph_status_terminal"], "the edit was not published: %v", status)
	committed, err := time.Parse(time.RFC3339Nano, status["committed_at"].(string))
	require.NoError(t, err)
	after, ok := status["tier_after_commit_ms"].(map[string]any)
	require.True(t, ok, "mutation_status carries no tier latency: %v", status)
	require.Len(t, after, 3)
	for tier, field := range map[string]string{"file_graph": "file_graph_at", "relations": "relations_at", "derived": "derived_at"} {
		value, stamped := status[field].(string)
		if !stamped {
			require.Nil(t, after[tier], "%s has a latency but no stamp", tier)
			continue
		}
		stamp, err := time.Parse(time.RFC3339Nano, value)
		require.NoError(t, err)
		ms, ok := after[tier].(float64)
		require.True(t, ok, "%s is stamped but has no latency: %v", tier, after)
		require.GreaterOrEqual(t, ms, 0.0, tier)
		require.InDelta(t, float64(stamp.Sub(committed).Microseconds())/1000, ms, 1, tier)
	}
	require.NotNil(t, after["relations"], "a published edit has no relations latency")

	now := time.Now()
	payload := map[string]any{}
	attachTierAfterCommit(payload, now, indexer.MutationTierState{AckAt: now, RelationsAt: now.Add(1500 * time.Millisecond)})
	pending := payload["tier_after_commit_ms"].(map[string]*float64)
	require.Equal(t, 1500.0, *pending["relations"])
	require.Nil(t, pending["file_graph"], "an unpublished tier is null, not zero")
	require.Nil(t, pending["derived"], "an unpublished tier is null, not zero")
	attachTierAfterCommit(payload, time.Time{}, indexer.MutationTierState{AckAt: now})
	require.NotContains(t, payload, "tier_after_commit_ms", "a receipt without a commit time carries a latency")
	attachTierAfterCommit(payload, now, indexer.MutationTierState{})
	require.NotContains(t, payload, "tier_after_commit_ms", "a receipt without a tier snapshot carries a latency")
}
