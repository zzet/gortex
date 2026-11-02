package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

type pathReleaseWatcher struct {
	mutationTestWatcher
	admitted chan struct{}
}

func (w pathReleaseWatcher) EnqueueToolMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	ticket, err := w.mutationTestWatcher.EnqueueToolMutation(ctx, path)
	close(w.admitted)
	return ticket, err
}

func TestEditFileReleasesPathLockBeforeGraphWait(t *testing.T) {
	for _, tool := range []string{"edit_file", "write_file"} {
		t.Run(tool, func(t *testing.T) {
			s := newMutationServer()
			path := filepath.Join(t.TempDir(), "held.txt")
			require.NoError(t, os.WriteFile(path, []byte("before"), 0o644))
			done := make(chan indexer.MutationResult, 1)
			admitted := make(chan struct{})
			s.SetWatcher(pathReleaseWatcher{mutationTestWatcher: mutationTestWatcher{done: done}, admitted: admitted})
			s.mutationReindexWait = 5 * time.Second
			handlerDone := make(chan error, 1)
			go func() {
				args := map[string]any{"path": path, "content": "after", "old_string": "before", "new_string": "after"}
				var result *mcp.CallToolResult
				var err error
				if tool == "edit_file" {
					result, err = s.handleEditFile(t.Context(), editFileRequest(args))
				} else {
					result, err = s.handleWriteFile(t.Context(), writeFileRequest(args))
				}
				if err == nil && result.IsError {
					err = fmt.Errorf("handler refused: %s", resultText(result))
				}
				handlerDone <- err
			}()
			select {
			case <-admitted:
			case <-time.After(2 * time.Second):
				t.Fatal("mutation never reached graph admission")
			}
			// The graph is deliberately still blocked. Another writer must
			// acquire this path before we release the admitted ticket.
			waitCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			_, release, err := acquireMutationPath(waitCtx, path)
			cancel()
			if release != nil {
				release()
			}
			done <- indexer.MutationResult{RequestedGeneration: 1, AppliedGeneration: 1, Reindexed: true}
			close(done)
			require.NoError(t, <-handlerDone)
			require.NoError(t, err, "a graph wait must not retain the source path lock")
		})
	}
}
