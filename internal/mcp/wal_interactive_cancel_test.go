package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInteractiveMarkerCancellationReleasesBeforePhysicalReadReturns(t *testing.T) {
	for _, kind := range []string{"ack", "read_back"} {
		t.Run(kind, func(t *testing.T) {
			s, root := setupTestServer(t)
			spy := &interactiveReadStoreSpy{Store: s.graph, entered: make(chan struct{})}
			s.graph = spy
			path := filepath.Join(root, "edited.txt")
			require.NoError(t, os.WriteFile(path, []byte("before"), 0o644))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "read_back" {
				res, err := s.handleEditFile(ctx, editFileRequest(map[string]any{"path": path, "old_string": "before", "new_string": "after"}))
				require.NoError(t, err)
				require.False(t, res.IsError, resultText(res))
				require.Zero(t, spy.active.Load())
			}
			entered, resume, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(resume) })
			s.physicalEvidenceOverride = func(path string) ([]byte, physicalReadEvidence, error) {
				close(entered)
				<-resume // An actual call stays in flight after its caller canceled.
				return readPhysicalFileEvidence(path)
			}
			t.Cleanup(func() { cancel(); unblock(); <-joined })
			go func() {
				defer close(joined)
				if kind == "ack" {
					_, _ = s.handleEditFile(ctx, editFileRequest(map[string]any{"path": path, "old_string": "before", "new_string": "after", "physical_evidence": true}))
				} else {
					req := editFileRequest(map[string]any{"path": path, "physical_evidence": true})
					req.Params.Name = "read_file"
					_, _ = s.handleReadFile(ctx, req)
				}
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("actual physical ACK/read did not reach the block")
			}
			require.EqualValues(t, 1, spy.active.Load())
			cancel()
			require.Eventually(t, func() bool { return spy.active.Load() == 0 }, time.Second, time.Millisecond,
				"canceled interactive priority outlived the caller while physical I/O remained blocked")
			select {
			case <-joined:
				t.Fatal("precondition: physical I/O returned before it was unblocked")
			default:
			}
			unblock()
			<-joined
			require.Zero(t, spy.active.Load(), "the final release underflowed or reactivated a canceled marker")
		})
	}
}
