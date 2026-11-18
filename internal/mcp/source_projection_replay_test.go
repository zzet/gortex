package mcp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
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
