package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser"
)

// Count the entire registered edit call, including lowering, syntax gate,
// projection and ACK assembly. The controlled watcher does no parser work.
func TestEditACKWholeRegisteredEditParseCount(t *testing.T) {
	selectedProjectionRegistry(t)
	t.Setenv("GORTEX_QUERY_LOG_DISABLE", "1")
	for _, facade := range []bool{false, true} {
		t.Run(fmt.Sprintf("facade_%t", facade), func(t *testing.T) {
			s, root := setupTestServer(t)
			t.Cleanup(func() { s.DrainBackground(); s.indexer.Close() })
			s.SetWatcher(mutationTestWatcher{})
			s.editAckMode = config.EditAckLocal
			const session = "whole-edit-parse"
			preset := "full"
			if facade {
				preset = "facade-v1"
			}
			s.NoteSessionToolPolicy(session, preset, "hide")
			ctx := WithSessionID(t.Context(), session)
			path := filepath.Join(root, "main.go")
			for _, broken := range []bool{false, true} {
				old, replacement := "func helper() {}", "func helper() { _ = 1 }"
				want := uint64(1)
				if broken {
					old, replacement, want = replacement, "func helper() { _ = }", 2
				}
				name := "edit_file"
				args := map[string]any{"path": path, "old_string": old, "new_string": replacement, "format": "json"}
				if facade {
					name = "edit"
					args = map[string]any{"operation": "file", "target": map[string]any{"file": path}, "match": old, "replacement": replacement, "output": map[string]any{"format": "json", "max_bytes": 0}}
				}
				before := parser.ParseCount()
				result := callEditOperationFrame(t, s, ctx, name, args)
				require.Equal(t, want, parser.ParseCount()-before, "whole request must reuse the candidate parse, adding only the old parse for errors")
				require.Equal(t, broken, result.IsError, resultText(result))
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				if broken {
					require.Contains(t, string(raw), old, "refused syntax regression cannot change the accepted bytes")
				} else {
					require.Contains(t, string(raw), replacement)
				}
			}
		})
	}
}

// Hold the move at its real precommit boundary while another session edits
// the same physical source. Both operations must land without losing either
// update. Every round uses fresh indexed Go files, not a replacement writer.
func TestEditACKMoveAndFileConcurrentStress(t *testing.T) {
	selectedProjectionRegistry(t)
	t.Setenv("GORTEX_QUERY_LOG_DISABLE", "1")
	for iteration := range 32 {
		t.Run(fmt.Sprintf("round_%02d", iteration), func(t *testing.T) {
			s, root := setupMoveInlineRepo(t, map[string]string{
				"pkg/source.go": "package pkg\nfunc MoveMe() int { return 7 }\nfunc KeepMe() int { return 1 }\n",
				"pkg/target.go": "package pkg\nfunc Existing() {}\n",
			})
			s.editAckMode = config.EditAckLocal
			s.SetWatcher(mutationTestWatcher{})
			s.ToolCallTimeout = -1
			t.Cleanup(func() { s.DrainBackground(); s.indexer.Close() })
			moveHeld, releaseMove := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(releaseMove) })
			t.Cleanup(unblock)
			var held sync.Once
			s.mutationPreCommitHook = func(record *mutationCommitRecord) {
				if record.tool == "move_symbol" {
					held.Do(func() { close(moveHeld); <-releaseMove })
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var joined sync.WaitGroup
			var move, edit *mcplib.CallToolResult
			var moveErr, editErr error
			t.Cleanup(func() { cancel(); unblock(); joined.Wait() })
			joined.Go(func() {
				request := editFileRequest(map[string]any{"id": "pkg/source.go::MoveMe", "target_file": "pkg/target.go", "format": "json"})
				request.Params.Name = "move_symbol"
				move, moveErr = s.runAcknowledgedEdit(WithSessionID(ctx, "move-client"), request, s.handleMoveSymbol)
			})
			select {
			case <-moveHeld:
			case <-ctx.Done():
				t.Fatal("move did not reach its real write boundary")
			}
			editDone := make(chan struct{})
			joined.Go(func() {
				defer close(editDone)
				request := editFileRequest(map[string]any{"path": filepath.Join(root, "pkg/source.go"), "old_string": "return 1", "new_string": "return 2", "format": "json"})
				request.Params.Name = "edit_file"
				edit, editErr = s.runAcknowledgedEdit(WithSessionID(ctx, "edit-client"), request, s.handleEditFile)
			})
			waitForMutationRefs(t, filepath.Join(root, "pkg/source.go"), 2)
			select {
			case <-editDone:
				t.Error("edit bypassed the move's still-held path lock")
			default:
			}
			unblock()
			joined.Wait()
			require.NoError(t, moveErr)
			require.NoError(t, editErr)
			require.NotNil(t, move)
			require.NotNil(t, edit)
			require.False(t, edit.IsError, resultText(edit))
			source, err := os.ReadFile(filepath.Join(root, "pkg/source.go"))
			require.NoError(t, err)
			target, err := os.ReadFile(filepath.Join(root, "pkg/target.go"))
			require.NoError(t, err)
			require.Contains(t, string(source), "func KeepMe() int { return 2 }", "move lost the other session's successful edit")
			require.Contains(t, string(target), "func Existing()")
			require.False(t, move.IsError, resultText(move))
			require.NotContains(t, string(source), "func MoveMe()")
			require.Equal(t, 1, strings.Count(string(target), "func MoveMe()"))
		})
	}
}
