package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

func committedProjectionAckFixture(t *testing.T) (string, *projectionEntry, *mutationCommitRecord) {
	t.Helper()
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	done := make(chan indexer.MutationResult, 1)
	t.Cleanup(func() { close(done) })
	s.SetWatcher(mutationTestWatcher{done: done})
	s.mutationReindexWait = time.Nanosecond
	path := filepath.Join(root, "main.go")
	result := callTool(t, s, "edit_file", map[string]any{"path": path, "old_string": "func helper() {}", "new_string": "func helper() {}\nfunc SameOne() {}", "format": "json"})
	require.False(t, result.IsError)
	response := extractTextResult(t, result)
	requireMutationFileViewState(t, response, "projected")
	entry := liveEntry(pendingFileProjections, path)
	require.NotNil(t, entry)
	record, ok := s.mutationCommits.byReceipt(response["mutation_receipt"].(string))
	require.True(t, ok)
	return path, entry, record
}

func TestMutationAckSuspectCheckPreservesNextLookupInvalidation(t *testing.T) {
	path, entry, record := committedProjectionAckFixture(t)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	changed := strings.Replace(string(before), "SameOne", "SameTwo", 1)
	require.NotEqual(t, string(before), changed)
	require.Len(t, changed, len(before))
	require.NoError(t, os.WriteFile(path, []byte(changed), 0o644))
	require.NoError(t, os.Chtimes(path, entry.disk.info.ModTime(), entry.disk.info.ModTime()))
	stamp, err := statDiskStamp(path)
	require.NoError(t, err)
	require.True(t, entry.disk.matches(stamp), "the external change must defeat the stat fast path")
	pendingFileProjections.markSuspect(path)
	response := make(map[string]any)
	attachMutationCommit(response, record)
	requireMutationFileViewState(t, response, "unavailable")
	suspectAfterAck := entry.suspect.Load()
	next, lookupErr := pendingFileProjections.lookup(path)
	assert.ErrorIs(t, lookupErr, errProjectionExternalWrite, "an ACK check must not consume the only invalidation witness")
	assert.Nil(t, next, "the next read must never receive the stale SameOne graph")
	assert.True(t, suspectAfterAck, "receipt rendering must leave hash revalidation to the real lookup")
}

func TestMutationAckRejectsSuspectReplacementWithoutFollowingIt(t *testing.T) {
	for _, kind := range []string{"large", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			path, entry, record := committedProjectionAckFixture(t)
			switch kind {
			case "large":
				require.NoError(t, os.WriteFile(path, make([]byte, (1<<20)+1), 0o644))
			case "directory":
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0o755))
			case "symlink":
				other := filepath.Join(filepath.Dir(path), "other.go")
				require.NoError(t, os.WriteFile(other, []byte("private replacement must not be read"), 0o644))
				require.NoError(t, os.Remove(path))
				if err := os.Symlink(other, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			pendingFileProjections.markSuspect(path)
			response := make(map[string]any)
			attachMutationCommit(response, record)
			requireMutationFileViewState(t, response, "unavailable")
			require.Equal(t, false, response["readback_exact"])
			require.True(t, entry.suspect.Load(), "an ACK may stat/refuse, but it must not read or clear a suspect file")
			require.Same(t, entry, liveEntry(pendingFileProjections, path), "receipt rendering must not alter projection ownership")
		})
	}
}

func TestMutationAckCannotCreditReplacedEntryWithMatchingDiskStamp(t *testing.T) {
	_, entry, record := committedProjectionAckFixture(t)
	// A modeled COW replacement shares immutable content and disk metadata;
	// pointer ownership, rather than equal bytes or sequence arithmetic, fences
	// the old receipt. This is not a second physical write or timing sample.
	replacement := entry.committedCopy(entry.commitSeq+1, entry.disk)
	require.NoError(t, pendingFileProjections.publish(replacement))
	response := make(map[string]any)
	attachMutationCommit(response, record)
	requireMutationFileViewState(t, response, "unavailable")
	require.Equal(t, false, response["readback_exact"])
	require.Same(t, replacement, liveEntry(pendingFileProjections, entry.absPath))
	require.False(t, replacement.suspect.Load())
}
