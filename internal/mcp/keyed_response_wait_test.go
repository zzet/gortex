package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

type keyedReplyResult struct {
	result *mcplib.CallToolResult
	err    error
}

// Keep the original graph wait open after the real physical commit. The
// duplicate must release its acquired path before waiting for that response.
func TestKeyedReplayWaitsForOriginalResponseOutsidePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyed.txt")
	require.NoError(t, os.WriteFile(path, []byte("x\n"), 0600))
	s := newMutationServer()
	publication := make(chan indexer.MutationResult, 1)
	var scheduled atomic.Int64
	s.SetWatcher(mutationTestWatcher{done: publication, generation: 1, scheduled: &scheduled})
	s.mutationReindexWait = time.Minute // an owned signal controls completion
	complete := sync.OnceFunc(func() {
		publication <- indexer.MutationResult{Reindexed: true, RequestedGeneration: 1, AppliedGeneration: 1}
		close(publication)
	})
	t.Cleanup(complete)
	args := map[string]any{"path": path, "old_string": "x", "new_string": "x\ny", "mutation_id": "once"}
	original := make(chan keyedReplyResult, 1)
	go func() {
		r, err := s.handleEditFile(t.Context(), editFileRequest(args))
		original <- keyedReplyResult{r, err}
	}()
	require.Eventually(t, func() bool { return scheduled.Load() == 1 }, 3*time.Second, time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, releasePath, err := acquireMutationPath(ctx, path)
	require.NoError(t, err, "the original graph wait must release its physical path")
	defer releasePath()
	record, ok := s.mutationCommits.byMutationID("once")
	require.True(t, ok)
	record.mu.Lock()
	releaseRecord := sync.OnceFunc(record.mu.Unlock)
	defer releaseRecord()
	require.Equal(t, mutationDiskCommitted, record.disk)
	require.Nil(t, record.response, "the original result is deliberately not ready")
	duplicate := make(chan keyedReplyResult, 1)
	go func() { r, err := s.handleEditFile(ctx, editFileRequest(args)); duplicate <- keyedReplyResult{r, err} }()
	releasePath()
	key := pathkey.MutationKey(path)
	require.Eventually(t, func() bool {
		mutationPathLocks.Lock()
		defer mutationPathLocks.Unlock()
		entry := mutationPathLocks.byPath[key]
		return entry != nil && entry.refs == 1 && len(entry.token) == 0
	}, time.Second, time.Millisecond, "the duplicate must own the path and wait at the held record")
	probe := make(chan error, 1)
	go func() {
		_, release, err := acquireMutationPath(ctx, path)
		if err != nil {
			probe <- err
			return
		}
		complete() // only after the duplicate returned or released its path
		release()
		probe <- nil
	}()
	require.Eventually(t, func() bool {
		mutationPathLocks.Lock()
		defer mutationPathLocks.Unlock()
		entry := mutationPathLocks.byPath[key]
		return entry != nil && entry.refs == 2
	}, time.Second, time.Millisecond, "the independent path probe must queue behind the duplicate")
	releaseRecord()
	select {
	case err := <-probe:
		require.NoError(t, err, "a replay wait must not retain the path")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var repeated, first keyedReplyResult
	select {
	case repeated = <-duplicate:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case first = <-original:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, first.err)
	require.NoError(t, repeated.err)
	require.False(t, first.result.IsError, "%s", resultText(first.result))
	require.False(t, repeated.result.IsError, "%s", resultText(repeated.result))
	firstBody, repeatedBody := decodeFileOpsResult(t, first.result), decodeFileOpsResult(t, repeated.result)
	require.Equal(t, true, repeatedBody["replayed"])
	require.Equal(t, firstBody["mutation_receipt"], repeatedBody["mutation_receipt"])
	require.Equal(t, firstBody["new_sha"], repeatedBody["new_sha"])
	require.Equal(t, int64(1), scheduled.Load(), "a replay cannot enqueue another publication")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "x\ny\n", string(raw), "the physical edit occurs once")
}
