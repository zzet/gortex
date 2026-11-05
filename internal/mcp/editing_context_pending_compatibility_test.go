package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

func requirePartialPendingContext(t *testing.T, result *mcplib.CallToolResult) {
	t.Helper()
	require.NotNil(t, result)
	require.False(t, result.IsError, "the prior explicit partial-pending contract is not a refusal: %s", resultText(result))
	fields := extractTextResult(t, result)
	require.Nil(t, fields["file"])
	require.Empty(t, fields["defines"])
	require.NotEqual(t, true, fields["not_modified"])
	fresh, ok := fields["freshness"].(map[string]any)
	require.True(t, ok, "empty fields must never be mistaken for complete metadata")
	require.Equal(t, "pending", fresh["graph_freshness"])
	require.Equal(t, "store_busy", fresh["reason"])
	require.Equal(t, false, fresh["complete"])
	require.NotEqual(t, true, fresh["readback_exact"])
}

// Reserve real readers only AFTER canonical selection, at the existing bundle
// capability. Release them before decoration; this isolates the30ms contract
// from a later unavailable-store/tool deadline without inventing busy rows.
type canonicalContextDeadlineStore struct {
	*store_sqlite.Store
	armed       atomic.Bool
	hold        func(context.Context) error
	release     func()
	join        func()
	observedErr error
	budget      time.Duration
	waits       int64
}

func (s *canonicalContextDeadlineStore) WithFileEditingContextReader(ctx context.Context, fn func(context.Context, graph.FileEditingContextRows) error) error {
	if !s.armed.CompareAndSwap(true, false) {
		return s.Store.WithFileEditingContextReader(ctx, fn)
	}
	if deadline, ok := ctx.Deadline(); ok {
		s.budget = time.Until(deadline)
	}
	if err := s.hold(ctx); err != nil {
		s.release()
		s.join()
		return err
	}
	before := s.Store.ReaderWaitMark()
	s.observedErr = s.Store.WithFileEditingContextReader(ctx, fn)
	after := s.Store.ReaderWaitMark()
	s.waits = after.PoolWaits - before.PoolWaits
	s.release()
	s.join()
	return s.observedErr
}

func TestRegisteredCanonicalContextDeadlineKeepsPartialPendingWire(t *testing.T) {
	s, backend, base, path, _ := snapshotFacadeFixture(t)
	// Canonicalize the exact owned bytes, with no read-time mutation.
	retirement := s.canonicalProjectionRetirement(path)
	require.NotNil(t, retirement)
	require.NoError(t, s.indexer.IndexFile(path))
	retirement.run()
	require.Nil(t, liveEntry(pendingFileProjections, path))
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	wrapped := &canonicalContextDeadlineStore{Store: backend.Store}
	var holders sync.WaitGroup
	release := make(chan struct{})
	wrapped.release = sync.OnceFunc(func() { close(release) })
	wrapped.join = holders.Wait
	var holderErrors []error
	var errorsMu sync.Mutex
	wrapped.hold = func(ctx context.Context) error {
		count := min(runtime.NumCPU(), 4)
		entered := make(chan struct{}, count)
		for range count {
			holders.Add(1)
			go func() {
				defer holders.Done()
				err := backend.Store.WithFileEditingContextReader(t.Context(), func(context.Context, graph.FileEditingContextRows) error {
					entered <- struct{}{}
					<-release
					return errors.New("owned reader released")
				})
				errorsMu.Lock()
				holderErrors = append(holderErrors, err)
				errorsMu.Unlock()
			}()
		}
		for range count {
			select {
			case <-entered:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	t.Cleanup(func() { wrapped.release(); wrapped.join() })
	s.graph = wrapped
	s.engine = query.NewEngine(wrapped)
	leaf, ok := s.facades.legacy("get_editing_context")
	require.True(t, ok)
	var selected context.Context
	s.facades.capture(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		r, e := leaf.handler(ctx, req)
		selected = ctx
		return r, e
	})
	wrapped.armed.Store(true)
	result, err := externalVersionRegisteredCall(s, base, 20, "read", map[string]any{"operation": "editing_context", "target": map[string]any{"file": path}, "output": map[string]any{"format": "json", "max_bytes": 0}})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.ErrorIs(t, wrapped.observedErr, context.DeadlineExceeded)
	require.Positive(t, wrapped.budget)
	require.LessOrEqual(t, wrapped.budget, 30*time.Millisecond)
	require.Positive(t, wrapped.waits, "the selected request actually waited for the physical SQL pool")
	require.Len(t, holderErrors, min(runtime.NumCPU(), 4))
	for _, held := range holderErrors {
		require.ErrorContains(t, held, "owned reader released")
	}
	require.NotNil(t, selected)
	require.Empty(t, requestDeferredProjectionFiles(selected), "this is canonical exhaustion, not a projection deferral")
	require.Empty(t, requestProjectionLayers(selected))
	require.Contains(t, requestStoreBusyFiles(selected), "main.go")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	t.Logf("CANONICAL_PARTIAL_PENDING_WIRE actual_SQL_deadline=true pool_waits=%d no_file_deferral=true same_bytes=true original_store_budget=%s is_error=%t", wrapped.waits, wrapped.budget, result.IsError)
	requirePartialPendingContext(t, result)
	fresh := snapshotRegisteredCall(t, s, base, 21, "read", map[string]any{"operation": "editing_context", "target": map[string]any{"file": path}, "output": map[string]any{"format": "json", "max_bytes": 0}})
	require.False(t, fresh.IsError, resultText(fresh))
	require.Contains(t, resultText(fresh), "main.go::Target")
}

// A different existing branch was already an error: no canonical symbols
// after a real private-budget deferral. Releasing actual SQL readers before
// the bundle keeps storeBusy=false and proves this is the old notfound site.
func TestRegisteredDeferredEmptyRefusalKeepsTypedReason(t *testing.T) {
	s, backend, base, path, _ := snapshotFacadeFixture(t)
	path = filepath.Join(filepath.Dir(path), "untracked.go")
	raw := []byte("package probe\n\nfunc NewPending() int { return 7 }\n")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	require.Empty(t, backend.GetFileNodes("untracked.go"))
	backend.entered = make(chan struct{})
	backend.query = make(chan struct{})
	backend.returned = make(chan error, 1)
	backend.armed.Store(true)
	startSQL := sync.OnceFunc(func() { close(backend.query) })
	release := make(chan struct{})
	releasePool := sync.OnceFunc(func() { close(release) })
	var request, holders sync.WaitGroup
	t.Cleanup(func() { startSQL(); releasePool(); request.Wait(); holders.Wait() })
	leaf, ok := s.facades.legacy("get_editing_context")
	require.True(t, ok)
	var selected context.Context
	s.facades.capture(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		r, e := leaf.handler(ctx, req)
		selected = ctx
		return r, e
	})
	caller, cancel := context.WithTimeout(base, 3*time.Second)
	defer cancel()
	type answer struct {
		result *mcplib.CallToolResult
		err    error
	}
	answered := make(chan answer, 1)
	request.Add(1)
	go func() {
		defer request.Done()
		r, e := externalVersionRegisteredCall(s, caller, 20, "read", map[string]any{"operation": "editing_context", "target": map[string]any{"file": path}, "output": map[string]any{"format": "json", "max_bytes": 0}})
		answered <- answer{r, e}
	}()
	select {
	case <-backend.entered:
	case <-caller.Done():
		t.Fatal("new-file projection did not reach the real metadata query")
	}
	entered := make(chan struct{}, min(runtime.NumCPU(), 4))
	holderErrors := make(chan error, cap(entered))
	for range cap(entered) {
		holders.Add(1)
		go func() {
			defer holders.Done()
			holderErrors <- backend.Store.WithFileEditingContextReader(t.Context(), func(context.Context, graph.FileEditingContextRows) error {
				entered <- struct{}{}
				<-release
				return nil
			})
		}()
	}
	for range cap(entered) {
		select {
		case <-entered:
		case <-caller.Done():
			t.Fatal("actual SQL reader reservation failed")
		}
	}
	privateDeadline, ok := backend.observed.Deadline()
	require.True(t, ok)
	require.Positive(t, time.Until(privateDeadline), "the original private budget must remain live before actual SQL wait")
	beforeWait := backend.ReaderWaitMark()
	startSQL()
	queryErr := <-backend.returned
	require.ErrorIs(t, queryErr, context.DeadlineExceeded)
	releasePool()
	holders.Wait()
	for range cap(entered) {
		require.NoError(t, <-holderErrors)
	}
	got := <-answered
	request.Wait()
	require.NoError(t, got.err)
	require.NoError(t, caller.Err())
	require.Greater(t, backend.ReaderWaitMark().PoolWaits, beforeWait.PoolWaits)
	require.NotNil(t, selected)
	require.Contains(t, requestDeferredProjectionFiles(selected), "untracked.go")
	require.Empty(t, requestStoreBusyFiles(selected), "the canonical empty bundle must finish without a second store deadline")
	physical, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, raw, physical)
	t.Logf("ACTUAL_DEFERRED_NOTFOUND_REFUSAL private_deadline=true pool_released=true canonical_empty=true store_busy=false caller_live=true is_error=%t", got.result.IsError)
	requireReadFreshnessContract(t, got.result, "file_view_pending")
}
