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
	for _, mode := range []string{"published", "cancelled waiter", "evicted receipt"} {
		t.Run(mode, func(t *testing.T) { keyedReplayResponseHandoff(t, mode) })
	}
}

func keyedReplayResponseHandoff(t *testing.T, mode string) {
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
	replayCtx, cancelReplay := context.WithCancel(ctx)
	defer cancelReplay()
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
	go func() {
		r, err := s.handleEditFile(replayCtx, editFileRequest(args))
		duplicate <- keyedReplyResult{r, err}
	}()
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
		// The independent probe can act only after the duplicate returned or
		// released its path. Cancellation never targets the original caller.
		switch mode {
		case "cancelled waiter":
			cancelReplay()
		case "evicted receipt":
			s.mutationCommits.mu.Lock()
			s.mutationCommits.evictLocked(time.Now().Add(mutationCommitRetention + time.Second))
			s.mutationCommits.mu.Unlock()
		}
		complete()
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
	firstBody := decodeFileOpsResult(t, first.result)
	require.Equal(t, true, firstBody["reindexed"], "a duplicate's cancellation/eviction cannot cancel original publication")
	if mode == "published" {
		require.False(t, repeated.result.IsError, "%s", resultText(repeated.result))
		repeatedBody := decodeFileOpsResult(t, repeated.result)
		require.Equal(t, true, repeatedBody["replayed"])
		require.Equal(t, firstBody["mutation_receipt"], repeatedBody["mutation_receipt"])
		require.Equal(t, firstBody["new_sha"], repeatedBody["new_sha"])
	} else {
		require.True(t, repeated.result.IsError)
		require.Contains(t, resultText(repeated.result), "already committed")
		require.Contains(t, resultText(repeated.result), record.id)
	}
	require.Equal(t, int64(1), scheduled.Load(), "a replay cannot enqueue another publication")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "x\ny\n", string(raw), "the physical edit occurs once")
}

type unavailableKeyedReplyWatcher struct {
	mutationTestWatcher
	calls atomic.Int64
}

func (w *unavailableKeyedReplyWatcher) EnqueueToolMutation(context.Context, string) (*indexer.MutationTicket, error) {
	w.calls.Add(1)
	panic("owned publication failure after physical commit")
}

func TestKeyedReplayTerminalUnavailableNeverWritesAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "terminal.txt")
	require.NoError(t, os.WriteFile(path, []byte("x\n"), 0600))
	s := newMutationServer()
	w := &unavailableKeyedReplyWatcher{}
	s.SetWatcher(w)
	args := map[string]any{"path": path, "old_string": "x", "new_string": "x\ny", "mutation_id": "terminal"}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = s.handleEditFile(t.Context(), editFileRequest(args))
	}()
	require.Equal(t, "owned publication failure after physical commit", recovered)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := s.handleEditFile(ctx, editFileRequest(args))
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, resultText(result), "already committed")
	require.Contains(t, resultText(result), "original handler finished")
	require.NotContains(t, resultText(result), "deadline")
	require.EqualValues(t, 1, w.calls.Load())
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "x\ny\n", string(raw))
}
