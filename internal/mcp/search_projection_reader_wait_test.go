package mcp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
	"go.uber.org/zap"
)

type searchMetadataPoolStore struct {
	*store_sqlite.Store
	armed    atomic.Bool
	entered  chan struct{}
	query    chan struct{}
	returned chan error
}

func (s *searchMetadataPoolStore) FileMetasByPaths(prefix string, paths []string) (map[string]graph.FileMetaRow, error) {
	observed := s.armed.CompareAndSwap(true, false)
	if observed {
		close(s.entered)
		<-s.query // Test-only barrier before the real SQL call, never fake rows.
	}
	rows, err := s.Store.FileMetasByPaths(prefix, paths)
	if observed {
		s.returned <- err
	}
	return rows, err
}

func searchReaderWaitFixture(t *testing.T) (*Server, *searchMetadataPoolStore, context.Context, string, *projectionEntry) {
	t.Helper()
	selectedProjectionRegistry(t)
	require.True(t, tokens.EncoderReady())
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package probe\n\nfunc TargetBefore() int { return 1 }\n\nfunc NeighborBefore() int { return 2 }\n"), 0o600))
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "search-reader.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	backend := &searchMetadataPoolStore{Store: sql, entered: make(chan struct{}), query: make(chan struct{}), returned: make(chan error, 1)}
	idx := newProjectionTestIndexer(t, backend)
	_, err = idx.Index(root)
	require.NoError(t, err)
	s := NewServer(query.NewEngine(backend), backend, idx, nil, zap.NewNop(), nil)
	s.editAckMode = config.EditAckLocal
	pending := make(chan indexer.MutationResult)
	s.SetWatcher(mutationTestWatcher{done: pending})
	t.Cleanup(func() { close(pending) })
	const session = "search-reader-cancellation"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"search-reader-component","version":"1"}}}`)))
	s.NoteSessionToolPolicy(session, "all", "defer")
	ack := preselectionRegisteredCall(t, s, ctx, 10, "edit_file", map[string]any{"path": path, "old_string": "TargetBefore", "new_string": "Target", "format": "json"})
	require.False(t, ack.IsError, resultText(ack))
	fields := extractTextResult(t, ack)
	require.Equal(t, "local", fields["ack_mode"])
	require.Equal(t, true, fields["readback_exact"])
	entry := liveEntry(pendingFileProjections, path)
	require.NotNil(t, entry)
	require.Positive(t, entry.commitSeq)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(raw)), fields["content_sha256"])
	require.Nil(t, sql.GetNode("main.go::Target"), "canonical publication must remain held")
	return s, backend, ctx, path, entry
}

func TestRegisteredPendingSearchMetadataWaitHonorsCallerCancellation(t *testing.T) {
	s, backend, ctx, path, original := searchReaderWaitFixture(t)
	quiet := preselectionRegisteredCall(t, s, ctx, 11, "search_symbols", map[string]any{"query": "Target", "query_class": "symbol", "expand": "off", "limit": 10000, "max_per_file": 0, "max_bytes": 0, "format": "json"})
	require.False(t, quiet.IsError, resultText(quiet))
	require.Contains(t, resultText(quiet), "main.go::Target", "the same real registered fixture is exact before contention")
	captured, ok := s.facades.legacy("search_symbols")
	require.True(t, ok)
	leafDone := make(chan struct{})
	s.addTool(captured.tool, func(call context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		defer close(leafDone)
		return captured.handler(call, req)
	})
	// This is the exact shared source-event callback, not an injected error.
	// It does not assert which native event caused the retained Linux failure.
	pendingFileProjections.markSuspect(path)
	require.True(t, original.suspect.Load())
	caller, cancel := context.WithCancel(ctx)
	defer cancel()
	startQuery := sync.OnceFunc(func() { close(backend.query) })
	t.Cleanup(startQuery)
	backend.armed.Store(true)
	type answer struct {
		result *mcplib.CallToolResult
		err    error
	}
	answered := make(chan answer, 1)
	var request sync.WaitGroup
	request.Add(1)
	t.Cleanup(func() { startQuery(); request.Wait() })
	go func() {
		defer request.Done()
		result, err := externalVersionRegisteredCall(s, caller, 20, "search_symbols", map[string]any{"query": "Target", "query_class": "symbol", "expand": "off", "limit": 10000, "max_per_file": 0, "max_bytes": 0, "format": "json"})
		answered <- answer{result: result, err: err}
	}()
	select {
	case <-backend.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("registered source refresh did not enter actual file metadata reader")
	}

	// Reserve the actual read-pool connections only after every earlier
	// registered/view/selection stage has reached the real metadata seam.
	count := min(runtime.NumCPU(), 4) // Existing configureConnectionPool bound.
	poolRelease := make(chan struct{})
	releasePool := sync.OnceFunc(func() { close(poolRelease) })
	t.Cleanup(releasePool)
	entered := make(chan struct{}, count)
	holderErrors := make(chan error, count)
	fixtureRelease := errors.New("fixture reader scope released")
	var holders sync.WaitGroup
	t.Cleanup(func() { releasePool(); holders.Wait() })
	for range count {
		holders.Add(1)
		go func() {
			defer holders.Done()
			holderErrors <- backend.Store.WithFileEditingContextReader(t.Context(), func(context.Context, graph.FileEditingContextRows) error {
				entered <- struct{}{}
				<-poolRelease
				return fixtureRelease // Roll back without another pool acquisition.
			})
		}()
	}
	for range count {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("real SQLite scope did not reserve its reader connection")
		}
	}
	before := backend.Store.ReaderWaitMark()
	startQuery()
	admissionDeadline := time.Now().Add(time.Second)
	for backend.Store.ReaderWaitMark().PoolWaits == before.PoolWaits && time.Now().Before(admissionDeadline) {
		runtime.Gosched() // Wait for an actual SQL pool-admission witness only.
	}
	blocked := backend.Store.ReaderWaitMark()
	poolWaitProven := blocked.PoolWaits > before.PoolWaits
	cancelled := time.Now()
	cancel()
	var metadataErr error
	metadataReturnedBeforeRelease := false
	select {
	case metadataErr = <-backend.returned:
		metadataReturnedBeforeRelease = true
	case <-time.After(projectedReadBudget):
	}
	elapsed := time.Since(cancelled)
	releasePool()
	holders.Wait()
	if !metadataReturnedBeforeRelease {
		select {
		case metadataErr = <-backend.returned:
		case <-time.After(3 * time.Second):
			t.Fatal("owned metadata call did not join after releasing actual pool")
		}
	}
	request.Wait()
	select {
	case <-leafDone:
	case <-time.After(3 * time.Second):
		t.Fatal("actual registered leaf remained live after its SQL read joined")
	}
	response := <-answered
	for range count {
		require.ErrorIs(t, <-holderErrors, fixtureRelease)
	}
	// All work is joined before assertions. SDK cancellation may reply before
	// a detached leaf exits; the separate metadata witness catches that escape.
	t.Logf("pool_wait_proven=%v metadata_returned_before_pool_release=%v after_cancel=%s metadata_error=%v caller_error=%v result=%v protocol_error=%v", poolWaitProven, metadataReturnedBeforeRelease, elapsed, metadataErr, caller.Err(), response.result, response.err)
	require.True(t, poolWaitProven, "prove the real SQL pool wait, not a sleeping fake reader")
	require.ErrorIs(t, caller.Err(), context.Canceled)
	if response.result != nil {
		require.True(t, response.result.IsError, "cancellation cannot earn current data: %s", resultText(response.result))
		require.NotContains(t, resultText(response.result), `"readback_exact":true`)
	} else {
		require.Error(t, response.err, "cancellation must remain a transport/tool refusal")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := parser.ParseFile(token.NewFileSet(), path, raw, 0)
	require.NoError(t, err)
	targets := 0
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "Target" {
			targets++
		}
	}
	require.Equal(t, 1, targets, "physical target remains independently present")
	require.True(t, original.suspect.Load(), "an old owner's event cannot be consumed")
	require.True(t, metadataReturnedBeforeRelease, "actual metadata SQL must honor caller cancellation while the reader pool remains occupied")
	require.ErrorIs(t, metadataErr, context.Canceled)
	require.Same(t, original, liveEntry(pendingFileProjections, path), "a cancelled refresh must not publish a checked replacement")
}
