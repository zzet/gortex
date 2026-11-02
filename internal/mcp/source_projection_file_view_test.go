package mcp

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"go.uber.org/zap"
)

func TestMutationFileViewContainsCompleteDeltaAndCheckedSyntax(t *testing.T) {
	s, root := ackModeServer(t, "local")
	published := make(chan indexer.MutationResult, 1)
	defer close(published)
	s.SetWatcher(mutationTestWatcher{done: published})
	result := callTool(t, s, "edit_file", map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "func helper() {}", "new_string": "func helper() { println(1) }\nfunc FileViewAdded() {}", "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	payload := extractTextResult(t, result)
	view, ok := payload["file_view"].(map[string]any)
	require.True(t, ok, "ACK file_view must carry the structured delta, not an availability-only scalar")
	require.Equal(t, "projected", view["state"])
	require.Equal(t, "available", view["delta_state"])
	counts, ok := view["counts"].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, 4, counts["before"], "fixture includes Config, Port, main and helper")
	require.EqualValues(t, 5, counts["after"])
	require.EqualValues(t, 1, counts["added"])
	require.EqualValues(t, 1, counts["changed"])
	require.EqualValues(t, 0, counts["removed"])
	require.EqualValues(t, 2, counts["total_changes"])
	require.Len(t, view["symbols"], 2)
	syntax, ok := view["syntax_health"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, syntax["checked"])
	require.EqualValues(t, 0, syntax["errors"])
	require.Equal(t, "go", syntax["language"])
	require.Contains(t, payload["graph_status_hint"], "file current")
	require.Contains(t, payload["graph_status_hint"], "pending")
	require.Equal(t, "fresh", payload["tier_state"].(map[string]any)["file"])
}

func TestMutationReplayRefreshesActualTicketTiersWithoutAnotherParse(t *testing.T) {
	s, root := ackModeServer(t, "local")
	watcher, err := indexer.NewWatcher(s.indexer, config.WatchConfig{Enabled: true, DebounceMs: 60000}, zap.NewNop())
	require.NoError(t, err)
	s.SetWatcher(watcher)
	require.NoError(t, watcher.Start([]string{root}))
	s.graph.ResolveMutex().Lock()
	unlock := sync.OnceFunc(s.graph.ResolveMutex().Unlock)
	stop := sync.OnceFunc(func() { require.NoError(t, watcher.Stop()) })
	t.Cleanup(func() { unlock(); stop() })
	args := map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "func helper() {}", "new_string": "func helper() { println(2) }", "mutation_id": "tier-replay-real-watcher", "format": "json"}
	first := callTool(t, s, "edit_file", args)
	require.False(t, first.IsError, "%v", first.Content)
	payload := extractTextResult(t, first)
	require.Equal(t, true, payload["reindex_pending"])
	tiers, ok := payload["tier_state"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "pending", tiers["relations"])
	receipt, ok := payload["reindex_receipt"].(string)
	require.True(t, ok)
	unlock()
	waitCheckoutReceipt(t, s, receipt)
	stop() // join native echoes before the replay parse-count observation
	before := parser.ParseCount()
	replay := callTool(t, s, "edit_file", args)
	require.False(t, replay.IsError, "%v", replay.Content)
	current := extractTextResult(t, replay)
	require.Equal(t, "fresh", current["graph_status"])
	require.NotContains(t, current, "reindex_pending", "a pending ACK cache cannot override the actual terminal receipt")
	liveTiers, ok := current["tier_state"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "fresh", liveTiers["relations"])
	require.Equal(t, before, parser.ParseCount(), "live receipt replay cannot parse or rewrite source")
	require.Equal(t, "local", current["ack_mode"])
	require.NotEmpty(t, current["content_sha256"])
}

func requireMutationFileViewState(t *testing.T, payload map[string]any, expected string) {
	t.Helper()
	switch view := payload["file_view"].(type) {
	case mutationFileView:
		require.Equal(t, expected, view.State)
		require.NotEmpty(t, view.DeltaState)
		require.NotEmpty(t, view.SyntaxHealth.State)
	case map[string]any:
		require.Equal(t, expected, view["state"])
		require.NotEmpty(t, view["delta_state"])
		require.Contains(t, view, "syntax_health")
	default:
		t.Fatalf("file_view must be a structured object, got %T", view)
	}
}

func TestMutationFileViewCacheRetainsOnlyBoundedRegistryEvidence(t *testing.T) {
	s, root := ackModeServer(t, "local")
	published := make(chan indexer.MutationResult, 1)
	defer close(published)
	s.SetWatcher(mutationTestWatcher{done: published})
	result := callTool(t, s, "edit_file", map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "func helper() {}", "new_string": "func helper() { println(3) }", "mutation_id": "bounded-delta-replay", "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	payload := extractTextResult(t, result)
	record, found := s.mutationCommits.byReceipt(payload["mutation_receipt"].(string))
	require.True(t, found)
	cached, found := record.replayResponse()
	require.True(t, found)
	require.NotContains(t, cached, "file_view", "only the capped projection registry may retain symbol delta strings")
	require.Equal(t, "fresh", payload["tier_state"].(map[string]any)["file"])
	pendingFileProjections.markSuspect(filepath.Join(root, "main.go"))
	unavailable := make(map[string]any)
	attachMutationCommit(unavailable, record)
	requireMutationFileViewState(t, unavailable, "unavailable")
	require.Equal(t, "pending", unavailable["tier_state"].(map[string]string)["file"])
	require.Nil(t, unavailable["file_view"].(mutationFileView).Counts)
}
