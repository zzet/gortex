package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

type interactiveReadStoreSpy struct {
	graph.Store
	active  atomic.Int32
	entered chan struct{}
	once    sync.Once
}

func (s *interactiveReadStoreSpy) AnnounceInteractiveRead() func() {
	s.active.Add(1)
	s.once.Do(func() { close(s.entered) })
	return sync.OnceFunc(func() { s.active.Add(-1) })
}

func TestEditAcknowledgementMarksOnlyPostAwaitFormatting(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "published", true: "cancelled"}[cancelled], func(t *testing.T) {
			s := newMutationServer()
			spy := &interactiveReadStoreSpy{Store: graph.New(), entered: make(chan struct{})}
			s.graph = spy
			path := filepath.Join(t.TempDir(), "edited.txt")
			require.NoError(t, os.WriteFile(path, []byte("before"), 0o644))
			done := make(chan indexer.MutationResult, 1)
			admitted := make(chan struct{})
			s.SetWatcher(pathReleaseWatcher{mutationTestWatcher: mutationTestWatcher{done: done}, admitted: admitted})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var formattingActive atomic.Int32
			formattingActive.Store(-1)
			s.physicalEvidenceOverride = func(path string) ([]byte, physicalReadEvidence, error) {
				formattingActive.Store(spy.active.Load())
				return readPhysicalFileEvidence(path)
			}
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				_, _ = s.handleEditFile(ctx, editFileRequest(map[string]any{"path": path, "old_string": "before", "new_string": "after", "physical_evidence": true}))
			}()
			<-admitted
			defer func() { done <- indexer.MutationResult{Reindexed: true}; close(done); <-joined }()
			lockCtx, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
			_, release, err := acquireMutationPath(lockCtx, path)
			stop()
			if release != nil {
				release()
			}
			require.NoError(t, err, "ACK publication wait retained the source path lock")
			require.Zero(t, spy.active.Load(), "an interactive marker across publication can block its own reclaim dependency")
			if cancelled {
				cancel()
			} else {
				done <- indexer.MutationResult{Reindexed: true}
			}
			<-joined
			if cancelled {
				require.Zero(t, formattingActive.Load(), "an already canceled caller must not acquire new interactive priority")
			} else {
				require.EqualValues(t, 1, formattingActive.Load(), "actual post-await ACK evidence/formatting was unmarked")
			}
			require.Zero(t, spy.active.Load(), "interactive marker leaked after return/cancellation")
		})
	}
}

func TestEditedFileReadbackMarkerUsesSessionAndPhysicalFile(t *testing.T) {
	s, root := setupTestServer(t)
	spy := &interactiveReadStoreSpy{Store: s.graph, entered: make(chan struct{})}
	s.graph = spy
	s.sessions = newSessionMap()
	first := WithSessionID(t.Context(), "editor")
	second := WithSessionID(t.Context(), "other")
	edited := filepath.Join(root, "edited.txt")
	unrelated := filepath.Join(root, "other.txt")
	require.NoError(t, os.WriteFile(edited, []byte("before"), 0o644))
	require.NoError(t, os.WriteFile(unrelated, []byte("unrelated"), 0o644))
	result, err := s.handleEditFile(first, editFileRequest(map[string]any{"path": edited, "old_string": "before", "new_string": "after"}))
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(result))
	require.Zero(t, spy.active.Load(), "idle session retained a store marker")
	for _, cell := range []struct {
		name string
		ctx  context.Context
		path string
		want int32
	}{
		{"same edited file", first, edited, 1},
		{"another session", second, edited, 0},
		{"unrelated file", first, unrelated, 0},
	} {
		t.Run(cell.name, func(t *testing.T) {
			s.physicalEvidenceOverride = func(path string) ([]byte, physicalReadEvidence, error) {
				require.Equal(t, cell.want, spy.active.Load(), "marker must cover the actual read, not idle time")
				return readPhysicalFileEvidence(path)
			}
			req := editFileRequest(map[string]any{"path": cell.path, "physical_evidence": true, "format": "json"})
			req.Params.Name = "read_file"
			result, err := s.wrapToolHandler(s.handleReadFile)(cell.ctx, req)
			require.NoError(t, err)
			require.False(t, result.IsError, resultText(result))
			require.Zero(t, spy.active.Load())
		})
	}
}

func TestEditedFileMarkerDoesNotCrossCheckoutIdentity(t *testing.T) {
	s := newMutationServer()
	spy := &interactiveReadStoreSpy{Store: graph.New(), entered: make(chan struct{})}
	s.graph = spy
	path := filepath.Join(t.TempDir(), "edited.go")
	require.NoError(t, os.WriteFile(path, []byte("package p"), 0o644))
	s.rememberInteractiveEdit(t.Context(), path)
	ctx := context.WithValue(t.Context(), checkoutMutationContextKey{}, &checkoutMutationState{checkoutID: "other", incarnation: "new"})
	release := s.beginEditedFileRead(ctx, path)
	require.Zero(t, spy.active.Load(), "another checkout borrowed the canonical edit identity")
	release()
	release = s.beginEditedFileRead(t.Context(), path)
	require.EqualValues(t, 1, spy.active.Load())
	release()
	release = s.beginEditedFileRead(t.Context(), path)
	require.Zero(t, spy.active.Load(), "a completed read-back must not prioritize unrelated later calls")
	release()
}
