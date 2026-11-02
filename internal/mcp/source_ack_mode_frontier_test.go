package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
)

func TestEditAckLocalDurableBatchDoesNotWaitForBackground(t *testing.T) {
	s, root := ackModeServer(t, "local")
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	paths := []string{writeAtomicBatchFixture(t, root, "a.txt", "before\n"), writeAtomicBatchFixture(t, root, "b.txt", "before\n")}
	published := make(chan indexer.MutationResult, 2)
	watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: published}, announced: make(chan []string, 1)}
	s.SetWatcher(watcher)
	s.mutationReindexWait = time.Minute
	complete := sync.OnceFunc(func() {
		for range 2 {
			published <- indexer.MutationResult{Reindexed: true}
		}
		close(published)
	})
	joined := make(chan struct{})
	var receipt batchTransactionReceipt
	var runErr error
	go func() {
		defer close(joined)
		receipt, runErr = s.runBatchTransaction(t.Context(), []batchEditItem{atomicFileEdit(paths[0], "before", "after"), atomicFileEdit(paths[1], "before", "after")}, "local-batch")
	}()
	defer func() {
		complete()
		select {
		case <-joined:
		case <-time.After(3 * time.Second):
			t.Error("owned local durable batch did not join")
		}
	}()
	select {
	case admitted := <-watcher.announced:
		require.ElementsMatch(t, paths, admitted)
	case <-time.After(3 * time.Second):
		t.Fatal("local durable batch did not admit")
	}
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("edit.ack local durable batch waited for background publication")
	}
	require.NoError(t, runErr)
	require.Equal(t, "committed", receipt.Status)
	require.Equal(t, "pending", receipt.GraphStatus)
	raw, err := json.Marshal(receipt)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"ack_mode":"local"`)
	require.EqualValues(t, 1, watcher.sets.Load())
	require.Zero(t, watcher.points.Load())
}

type ackModeCancelWatcher struct {
	mutationTestWatcher
	cancel context.CancelFunc
}

func (w ackModeCancelWatcher) EnqueueToolMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	ticket, err := w.mutationTestWatcher.EnqueueToolMutation(ctx, path)
	w.cancel()
	return ticket, err
}

func TestEditAckLocalCancellationAfterCommitRetainsAdmission(t *testing.T) {
	s, root := ackModeServer(t, "local")
	published := make(chan indexer.MutationResult, 1)
	defer close(published)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.SetWatcher(ackModeCancelWatcher{mutationTestWatcher: mutationTestWatcher{done: published}, cancel: cancel})
	path := filepath.Join(root, "main.go")
	request := editFileRequest(map[string]any{"path": path, "old_string": "func helper() {}", "new_string": "func helper() { println(1) }", "format": "json"})
	result, err := s.handleEditFile(ctx, request)
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), "println(1)")
	payload := extractTextResult(t, result)
	require.Equal(t, "local", payload["ack_mode"])
	require.Equal(t, true, payload["reindex_pending"])
	require.NotEmpty(t, payload["reindex_receipt"])
}

type ackModeTierWatcher struct {
	watchedTierFixture
	scope    string
	admitted chan struct{}
}

func (w *ackModeTierWatcher) EnqueueToolMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	ticket, err := w.mutationTestWatcher.EnqueueToolMutation(ctx, path)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	state := indexer.MutationTierState{Generation: ticket.Generation, AckAt: now, FileGraphAt: now}
	if w.scope == "derived" {
		state.RelationsAt = now
	}
	w.mu.Lock()
	w.paths[path] = state
	w.mu.Unlock()
	close(w.admitted)
	return ticket, nil
}

func TestEditAckRequireFreshUsesRequestedTierBeforeWholePatch(t *testing.T) {
	for _, scope := range []string{"file", "relations", "derived"} {
		t.Run(scope, func(t *testing.T) {
			s, root := ackModeServer(t, "local")
			path := filepath.Join(root, "main.go")
			published := make(chan indexer.MutationResult, 1)
			watcher := &ackModeTierWatcher{watchedTierFixture: watchedTierFixture{mutationTestWatcher: mutationTestWatcher{done: published}, paths: make(map[string]indexer.MutationTierState)}, scope: scope, admitted: make(chan struct{})}
			s.SetWatcher(watcher)
			complete := sync.OnceFunc(func() { published <- indexer.MutationResult{Reindexed: true}; close(published) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s.mutationReindexWait = time.Minute
			waiting := make(chan struct{}, 1)
			s.releaseDispatchSlot = func(context.Context, <-chan struct{}) bool { waiting <- struct{}{}; return true }
			request := editFileRequest(map[string]any{"path": path, "old_string": "func helper() {}", "new_string": "func helper() { println(1) }", "format": "json", "require_fresh": true, "freshness_scope": scope, "wait_deadline": time.Now().Add(5 * time.Second).Format(time.RFC3339Nano)})
			joined := make(chan struct{})
			var result *mcplib.CallToolResult
			var runErr error
			go func() { defer close(joined); result, runErr = s.runAcknowledgedEdit(ctx, request, s.handleEditFile) }()
			defer func() {
				cancel()
				complete()
				select {
				case <-joined:
				case <-time.After(3 * time.Second):
					t.Error("owned tier ACK did not join")
				}
			}()
			select {
			case <-watcher.admitted:
			case <-time.After(3 * time.Second):
				t.Fatal("tier ACK never admitted")
			}
			if scope != "file" {
				select {
				case <-waiting:
				case <-time.After(time.Second):
					t.Fatal("pending requested tier did not enter unlocked wait")
				}
				select {
				case <-joined:
					t.Fatal("file-graph stage incorrectly certified pending later tier")
				default:
				}
				watcher.mu.Lock()
				state := watcher.paths[path]
				if scope == "relations" {
					state.RelationsAt = time.Now()
				} else {
					state.DerivedAt = time.Now()
				}
				watcher.paths[path] = state
				watcher.mu.Unlock()
			}
			select {
			case <-joined:
			case <-time.After(time.Second):
				t.Fatal("requested published tier remained coupled to whole-patch completion")
			}
			require.NoError(t, runErr)
			require.False(t, result.IsError, "%v", result.Content)
			require.Contains(t, toolResultText(result), `"reindex_pending":true`)
			require.NotContains(t, toolResultText(result), `"graph_status":"fresh"`)
		})
	}
}

func TestEditAckLocalMoveDoesNotClaimBackgroundWait(t *testing.T) {
	s, _ := setupMoveInlineRepo(t, map[string]string{"pkg/a.go": "package pkg\nfunc Foo() int { return 42 }\n", "pkg/b.go": "package pkg\nfunc Bar() int { return 1 }\n"})
	s.editAckMode = config.EditAckLocal
	published := make(chan indexer.MutationResult, 2)
	defer close(published)
	s.SetWatcher(&renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: published}, announced: make(chan []string, 1)})
	s.mutationReindexWait = time.Minute
	request := mcplib.CallToolRequest{}
	request.Params.Name = "move_symbol"
	request.Params.Arguments = map[string]any{"id": "pkg/a.go::Foo", "target_file": "pkg/b.go", "format": "json"}
	result, err := s.runAcknowledgedEdit(t.Context(), request, s.handleMoveSymbol)
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	response := decodeFileOpsResult(t, result)
	for _, raw := range response["file_observations"].([]any) {
		id := raw.(map[string]any)["receipt"].(string)
		status, ok := s.moveFileObservationPayload(t.Context(), id)
		require.True(t, ok)
		require.Equal(t, "admitted", status["requested_background_state"])
		require.Equal(t, map[string]string{"publish": "applicable", "reindex_wait": "not_applicable_fast_ack"}, status["acknowledgement_phase_applicability"], "a queued ticket is not evidence that local mode waited")
		require.Nil(t, status["direct_reconcile"])
		require.Nil(t, status["read_assisted_reconcile"])
	}
}
