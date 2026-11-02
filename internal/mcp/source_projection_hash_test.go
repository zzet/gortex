package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
)

func TestMutationContentSHA256UsesActualCommittedBytesAndSurvivesReplay(t *testing.T) {
	s, root := setupTestServer(t)
	published := make(chan indexer.MutationResult, 1)
	defer close(published)
	s.SetWatcher(mutationTestWatcher{done: published})
	s.mutationReindexWait = time.Nanosecond
	path := filepath.Join(root, "main.go")
	args := map[string]any{"path": path, "old_string": "func helper() {}", "new_string": "func helper() { println(\"Δ\") }", "mutation_id": "raw-sha-proof", "format": "json"}
	before := parser.ParseCount()
	result := callTool(t, s, "edit_file", args)
	require.False(t, result.IsError, "%v", result.Content)
	require.EqualValues(t, 1, parser.ParseCount()-before, "raw byte identity must reuse the one candidate parse")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	expected := hex.EncodeToString(sum[:])
	payload := extractTextResult(t, result)
	require.Equal(t, expected, payload["content_sha256"])
	require.NotEqual(t, gitBlobSHA(raw), expected)
	record, found := s.mutationCommits.byReceipt(payload["mutation_receipt"].(string))
	require.True(t, found)
	require.NotNil(t, record)
	require.Equal(t, expected, s.mutationStatusPayload(record)["content_sha256"])
	replayParses := parser.ParseCount()
	replay := callTool(t, s, "edit_file", args)
	require.False(t, replay.IsError, "%v", replay.Content)
	require.Equal(t, expected, extractTextResult(t, replay)["content_sha256"])
	require.Equal(t, replayParses, parser.ParseCount(), "replay must not parse again to reconstruct a hash")
}

func TestMutationContentSHA256DoesNotInventHashForModeledLegacyRecord(t *testing.T) {
	s := newMutationServer()
	path := filepath.Join(t.TempDir(), "legacy.go")
	record := s.beginMutationCommit(t.Context(), "write_file", "", "", "legacy.go", path)
	record.markCommitted("legacy-git-blob", 99)
	payload := map[string]any{}
	attachMutationCommit(payload, record)
	require.NotContains(t, payload, "content_sha256", "a blob label is not a verified raw-byte SHA256")
	require.NotContains(t, s.mutationStatusPayload(record), "content_sha256")
}
