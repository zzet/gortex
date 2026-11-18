package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

type toolAdmissionWatcher struct {
	mutationTestWatcher
	nativeCalls int
	toolCalls   int
}

func (w *toolAdmissionWatcher) EnqueueFileMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	w.nativeCalls++
	return w.mutationTestWatcher.EnqueueToolMutation(ctx, path)
}

func (w *toolAdmissionWatcher) EnqueueToolMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	w.toolCalls++
	return w.mutationTestWatcher.EnqueueToolMutation(ctx, path)
}

func TestServedEditSelectsZeroDebounceAdmission(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single_edit", true: "single_file_batch"}[batch], func(t *testing.T) {
			s := newMutationServer()
			watcher := &toolAdmissionWatcher{}
			s.watcher = watcher
			t.Cleanup(func() {
				s.mutationReceipts.Range(func(_, value any) bool {
					if receipt, ok := value.(*mutationReceipt); ok {
						select {
						case <-receipt.done:
						case <-time.After(5 * time.Second):
							t.Error("owned ticket consumer did not finish")
						}
					}
					return true
				})
			})
			path := filepath.Join(t.TempDir(), "edited.go")
			var outcome mutationReindexOutcome
			if batch {
				outcome = s.beginBatchGraphRefresh(t.Context(), path, batchTransactionReceipt{})
			} else {
				outcome = s.mutationReindexState(t.Context(), path)
			}
			require.NoError(t, outcome.Err)
			require.Equal(t, 1, watcher.toolCalls, "committed tool edits must select the zero-debounce entry")
			require.Zero(t, watcher.nativeCalls, "native/drift admission must remain a separate debounced entry")
		})
	}
}
