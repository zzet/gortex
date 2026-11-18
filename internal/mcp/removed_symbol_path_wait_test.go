package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Keep both the old line range and repeated fragment usable by a stale
// writer: a refusal must establish declaration identity, not merely fail to
// find old_source after the peer's edit.
const removedSymbolWaitSource = `package sample

func first() int {
	return 1
}

func second() int {
	return 1
}

func tail() int {
	return 2
}
`

func TestRemovedSymbolWhileWaitingForPathRefusesEditRenameAndBatch(t *testing.T) {
	for _, tool := range []string{"edit_symbol", "rename_symbol", "batch_edit"} {
		t.Run(tool, func(t *testing.T) {
			t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
			s, root := setupMoveInlineRepo(t, map[string]string{"sample.go": removedSymbolWaitSource})
			var scheduled atomic.Int64
			s.SetWatcher(mutationTestWatcher{scheduled: &scheduled})
			path := filepath.Join(root, "sample.go")
			require.NotNil(t, s.engine.GetSymbol("sample.go::second"))
			writerCtx, rawUnlock, err := acquireMutationPath(t.Context(), path)
			require.NoError(t, err)
			unlock := sync.OnceFunc(rawUnlock)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			ctx, note := withMutationCommitNote(ctx)
			finished := make(chan struct{})
			var result *mcpgo.CallToolResult
			var callErr error
			t.Cleanup(func() {
				cancel()
				unlock()
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					t.Error("owned queued frontend did not join")
				}
			})
			go func() {
				defer close(finished)
				request := mcpgo.CallToolRequest{}
				request.Params.Name = tool
				switch tool {
				case "edit_symbol":
					request.Params.Arguments = map[string]any{"id": "sample.go::second", "old_source": "return 1", "new_source": "return 99", "format": "json"}
					result, callErr = s.handleEditSymbol(ctx, request)
				case "rename_symbol":
					request.Params.Arguments = map[string]any{"id": "sample.go::second", "new_name": "Renamed", "dry_run": false, "format": "json"}
					result, callErr = s.handleRenameSymbol(ctx, request)
				case "batch_edit":
					request.Params.Arguments = map[string]any{"transaction_id": "removed-target-batch", "edits": []any{map[string]any{"op": "edit_symbol", "id": "sample.go::second", "old_source": "return 1", "new_source": "return 99"}}, "format": "json"}
					result, callErr = s.handleBatchEdit(ctx, request)
				}
			}()

			// This is the real queued path admission, after node lookup/planning.
			// No sleep or fabricated request-start callback chooses the race.
			waitForMutationRefs(t, path, 2)
			select {
			case <-finished:
				t.Fatal("frontend returned before the held path was released")
			default:
			}
			removed := strings.Replace(removedSymbolWaitSource, "func second()", "func replacement()", 1)
			require.NotEqual(t, removedSymbolWaitSource, removed)
			peer, err := s.commitFileMutation(writerCtx, "write_file", "", "", "sample.go", path, []byte(removed), 0o644)
			require.NoError(t, err)
			require.NotNil(t, peer)
			peerInfo, err := os.Stat(path)
			require.NoError(t, err)
			peerBytes, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, removed, string(peerBytes), "the peer's actual committed removal is the disk oracle")
			unlock()
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal("queued frontend did not return inside its original context")
			}
			require.NoError(t, callErr)
			require.NotNil(t, result)

			if tool == "batch_edit" {
				// Batch refusals are structured aborted receipts, not transport
				// errors. Preserve their actual target and refusal cause.
				require.False(t, result.IsError, "%v", result.Content)
				payload := lifecycleResultPayload(t, result)
				t.Logf("actual batch refusal/receipt: status=%v disk=%v graph=%v error=%v results=%v", payload["status"], payload["disk_status"], payload["graph_status"], payload["error"], payload["results"])
				assert.Equal(t, "aborted", payload["status"])
				assert.Equal(t, "unchanged", payload["disk_status"])
				assert.Equal(t, "not_started", payload["graph_status"])
				assert.Contains(t, payload["error"], errSymbolRemoved.Error())
				rows, ok := payload["results"].([]any)
				if assert.True(t, ok) && assert.Len(t, rows, 1) {
					row, ok := rows[0].(map[string]any)
					if assert.True(t, ok) {
						assert.Equal(t, "sample.go::second", row["id"])
						assert.Equal(t, "failed", row["status"])
						assert.Contains(t, row["error"], errSymbolRemoved.Error())
					}
				}
			} else {
				assert.True(t, result.IsError, "removed target must be refused: %v", result.Content)
				text := freshnessToolResultText(result)
				if tool == "edit_symbol" {
					assert.Contains(t, text, errSymbolRemoved.Error())
				} else {
					assert.Contains(t, text, "sample.go:")
					assert.Contains(t, text, "changed on disk since it was planned")
				}
				t.Logf("actual %s refusal: %s", tool, text)
			}

			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, peerBytes, after, "a stale range must not edit the replacement declaration or the repeated fragment in first")
			afterInfo, err := os.Stat(path)
			require.NoError(t, err)
			assert.True(t, os.SameFile(peerInfo, afterInfo), "a refusal must not perform an identical-byte replacement")
			assert.Equal(t, peerInfo.ModTime(), afterInfo.ModTime())
			assert.Equal(t, peerInfo.Mode(), afterInfo.Mode())
			latest, ok := s.mutationCommits.recentForPath(path)
			assert.True(t, ok)
			assert.Same(t, peer, latest, "queued refusal must not obtain newer source authority")
			assert.Empty(t, note.verdict().Snapshots, "refused frontend must not commit source")
			assert.Zero(t, scheduled.Load(), "refused frontend must not enqueue graph publication")
		})
	}
}
