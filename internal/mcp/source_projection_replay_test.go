package mcp

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"go.uber.org/zap"
)

func TestReplayAfterNewerCommitKeepsLiveProjection(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	done := make(chan indexer.MutationResult, 2)
	t.Cleanup(func() { close(done) })
	s.SetWatcher(mutationTestWatcher{done: done})
	s.mutationReindexWait = time.Nanosecond
	path := filepath.Join(root, "main.go")
	firstArgs := map[string]any{"path": path, "old_string": "func helper() {}", "new_string": "func helper() {}\nfunc FirstProjection() {}", "mutation_id": "projection-old", "format": "json"}
	first := callTool(t, s, "edit_file", firstArgs)
	require.False(t, first.IsError)
	originalAck := extractTextResult(t, first)
	requireMutationFileViewState(t, originalAck, "projected")
	firstEntry := liveEntry(pendingFileProjections, path)
	require.NotNil(t, firstEntry)
	initialParses := parser.ParseCount()
	same := callTool(t, s, "edit_file", firstArgs)
	require.False(t, same.IsError)
	requireMutationFileViewState(t, extractTextResult(t, same), "projected")
	require.Equal(t, true, extractTextResult(t, same)["readback_exact"])
	require.Equal(t, initialParses, parser.ParseCount())
	require.Same(t, firstEntry, liveEntry(pendingFileProjections, path))
	second := callTool(t, s, "edit_file", map[string]any{"path": path, "old_string": "func FirstProjection() {}", "new_string": "func SecondProjection() {}", "mutation_id": "projection-new", "format": "json"})
	require.False(t, second.IsError)
	live := liveEntry(pendingFileProjections, path)
	require.NotSame(t, firstEntry, live)
	beforeReplay, err := os.ReadFile(path)
	require.NoError(t, err)
	parses := parser.ParseCount()
	replay := callTool(t, s, "edit_file", firstArgs)
	require.False(t, replay.IsError)
	payload := extractTextResult(t, replay)
	require.Equal(t, true, payload["replayed"])
	require.Equal(t, originalAck["mutation_receipt"], payload["mutation_receipt"], "the physical receipt remains historical")
	require.Equal(t, originalAck["commit_seq"], payload["commit_seq"])
	requireMutationFileViewState(t, payload, "unavailable")
	require.Equal(t, false, payload["readback_exact"])
	require.Equal(t, "projection_not_live", payload["file_reason"])
	require.Equal(t, parses, parser.ParseCount(), "replay must neither parse nor resurrect the historical entry")
	require.Same(t, live, liveEntry(pendingFileProjections, path))
	afterReplay, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeReplay, afterReplay)
	reader := graph.NewOverlaidView(s.graph, live.layer)
	require.NotNil(t, reader.GetNode("main.go::SecondProjection"))
	require.Nil(t, reader.GetNode("main.go::FirstProjection"))
}

func TestReplayAfterNewerCommitRefreshesOriginalTicketTiers(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := ackModeServer(t, "local")
	watcher, err := indexer.NewWatcher(s.indexer, config.WatchConfig{Enabled: true, DebounceMs: 60000}, zap.NewNop())
	require.NoError(t, err)
	s.SetWatcher(watcher)
	// Keep native echoes independent of the real tool-admitted tickets.
	require.NoError(t, watcher.Start([]string{t.TempDir()}))
	t.Cleanup(func() { require.NoError(t, watcher.Stop()) })

	resolve := s.graph.ResolveMutex()
	resolve.Lock()
	releaseFirst := sync.OnceFunc(resolve.Unlock)
	t.Cleanup(releaseFirst)
	path := filepath.Join(root, "main.go")
	firstArgs := map[string]any{"path": path, "old_string": "func helper() {}", "new_string": "func helper() {}\nfunc FirstProjection() {}", "mutation_id": "old-replay-ticket", "format": "json"}
	first := callTool(t, s, "edit_file", firstArgs)
	require.False(t, first.IsError, "%v", first.Content)
	original := extractTextResult(t, first)
	require.Equal(t, true, original["readback_exact"])
	require.Equal(t, "pending", original["tier_state"].(map[string]any)["relations"])
	firstReceipt := original["reindex_receipt"].(string)
	releaseFirst()
	firstTicket := waitCheckoutReceipt(t, s, firstReceipt)
	firstRelationsAt := firstTicket.ticket.TierState().RelationsAt
	require.False(t, firstRelationsAt.IsZero())

	resolve.Lock()
	t.Cleanup(resolve.Unlock)
	second := callTool(t, s, "edit_file", map[string]any{"path": path, "old_string": "func FirstProjection() {}", "new_string": "func SecondProjection() {}", "mutation_id": "new-replay-ticket", "format": "json"})
	require.False(t, second.IsError, "%v", second.Content)
	newer := extractTextResult(t, second)
	require.Equal(t, true, newer["readback_exact"])
	require.Equal(t, "pending", newer["tier_state"].(map[string]any)["relations"])
	newerReceipt := newer["reindex_receipt"].(string)
	require.NotEqual(t, firstReceipt, newerReceipt)
	beforeReplay, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(beforeReplay), "func SecondProjection() {}")
	require.NotContains(t, string(beforeReplay), "func FirstProjection() {}")

	replay := callTool(t, s, "edit_file", firstArgs)
	require.False(t, replay.IsError, "%v", replay.Content)
	current := extractTextResult(t, replay)
	require.Equal(t, true, current["replayed"])
	require.Equal(t, original["mutation_receipt"], current["mutation_receipt"])
	require.Equal(t, original["commit_seq"], current["commit_seq"])
	require.Equal(t, false, current["readback_exact"])
	requireMutationFileViewState(t, current, "unavailable")
	require.Equal(t, true, current["file_view"].(map[string]any)["historical"])
	tiers := current["tier_state"].(map[string]any)
	require.Equal(t, "pending", tiers["file"])
	require.Equal(t, "fresh", tiers["relations"], "replay must refresh the original receipt's live tier")
	require.Equal(t, firstRelationsAt.UTC().Format(time.RFC3339Nano), current["relations_at"])
	pending, found := s.mutationReceiptState(newerReceipt)
	require.True(t, found)
	require.True(t, pending.Pending, "the newer receipt must still be pending after replay")
	require.True(t, pending.tiers.RelationsAt.IsZero())
	afterReplay, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeReplay, afterReplay)
}
