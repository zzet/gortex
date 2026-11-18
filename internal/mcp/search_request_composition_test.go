package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func registeredSymbolCompositionFixture(t *testing.T) (*Server, *store_sqlite.Store, context.Context, string) {
	t.Helper()
	selectedProjectionRegistry(t)
	root := t.TempDir()
	for file, raw := range map[string]string{
		"main.go":  "package fixture\n\nfunc BeforeMain() int { return BeforeOther() }\n",
		"other.go": "package fixture\n\nfunc BeforeOther() int { return 7 }\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, file), []byte(raw), 0o600))
	}
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "symbols.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	idx := newProjectionTestIndexer(t, sql)
	_, err = idx.Index(root)
	require.NoError(t, err)
	n, err := sql.SymbolFTSCount()
	require.NoError(t, err)
	require.Positive(t, n, "actual native FTS must be present")
	engine := query.NewEngine(sql)
	engine.SetSearch(search.NewSymbolSearcherBackend(sql))
	s := NewServer(engine, sql, idx, nil, zap.NewNop(), nil)
	s.editAckMode = config.EditAckLocal
	pending := make(chan indexer.MutationResult)
	s.SetWatcher(mutationTestWatcher{done: pending})
	var receipts []*mutationReceipt
	t.Cleanup(func() {
		close(pending)
		for _, receipt := range receipts {
			select {
			case <-receipt.done:
			case <-time.After(3 * time.Second):
				t.Error("owned pending receipt did not join")
			}
		}
	})
	const session = "symbol-reader-construction"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"symbol-construction","version":"1"}}}`)))
	s.NoteSessionToolPolicy(session, "all", "defer")
	for i, edit := range []struct{ file, old, next string }{{"main.go", "BeforeMain", "CurrentMain"}, {"other.go", "BeforeOther", "CurrentOther"}} {
		ack := preselectionRegisteredCall(t, s, ctx, 10+i, "edit_file", map[string]any{"path": filepath.Join(root, edit.file), "old_string": edit.old, "new_string": edit.next, "format": "json"})
		require.False(t, ack.IsError, resultText(ack))
		payload := extractTextResult(t, ack)
		require.Equal(t, "local", payload["ack_mode"])
		require.Equal(t, true, payload["readback_exact"])
		value, ok := s.mutationReceipts.Load(payload["reindex_receipt"].(string))
		require.True(t, ok)
		receipts = append(receipts, value.(*mutationReceipt))
		require.NotNil(t, liveEntry(pendingFileProjections, filepath.Join(root, edit.file)))
	}
	require.Len(t, pendingFileProjections.snap.Load().entries, 2, "actual two-owner multi-layer route must be exercised")
	require.Nil(t, sql.GetNode("main.go::CurrentMain"), "base publication remains held")
	require.NotNil(t, sql.GetNode("main.go::BeforeMain"))
	return s, sql, ctx, root
}

func TestRegisteredSymbolSearchBuildsOnePendingComposition(t *testing.T) {
	s, sql, ctx, _ := registeredSymbolCompositionFixture(t)
	var builds atomic.Int32
	ctx = context.WithValue(ctx, requestReaderCompositionObserverKey{}, func(*graph.OverlaidView) { builds.Add(1) })
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result := preselectionRegisteredCall(t, s, ctx, 20, "search_symbols", map[string]any{"query": "CurrentMain", "query_class": "symbol", "expand": "off", "limit": 1000, "max_bytes": 0, "format": "json"})
	require.False(t, result.IsError, resultText(result))
	body := extractTextResult(t, result)
	rows, ok := body["results"].([]any)
	require.True(t, ok)
	require.Equal(t, float64(len(rows)), body["total"])
	require.Equal(t, false, body["truncated"])
	found := false
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["id"] == "main.go::CurrentMain" {
			found = row["file_path"] == "main.go" && row["start_line"] == float64(3)
		}
	}
	require.True(t, found, "actual same-connection current name/file/line witness is required")
	require.Nil(t, sql.GetNode("main.go::CurrentMain"), "search must not publish or read-repair base")
	t.Logf("actual_completed_multi_layer_constructions=%d", builds.Load())
	require.Equal(t, int32(1), builds.Load(), "one immutable selected request composition must serve the complete handler")
}

func TestPreparedSymbolCompositionKeepsExactSelection(t *testing.T) {
	s, sql, ctx, _ := registeredSymbolCompositionFixture(t)
	pinned, err := s.bindPendingSearch(ctx)
	require.NoError(t, err)
	preparedCtx := s.prepareSymbolRequestReader(pinned)
	prepared, _ := preparedCtx.Value(preparedSymbolReaderKey{}).(*preparedSymbolReader)
	require.NotNil(t, prepared)
	original := s.requestBaseReader(preparedCtx)
	require.Same(t, original, s.requestBaseReader(preparedCtx))
	layers := requestProjectionLayers(preparedCtx)
	extra := graph.NewOverlayLayer()
	extra.MarkFile("extra.go", false)
	extra.AddNode("extra.go", &graph.Node{ID: "extra.go::Extra", FilePath: "extra.go", Name: "Extra", Kind: graph.KindFunction})
	changed := context.WithValue(preparedCtx, requestProjectionKey{}, append(append([]graph.OverlayLayerReader(nil), layers...), extra))
	require.NotSame(t, original, s.requestBaseReader(changed), "an extended selection must rebuild")
	require.NotNil(t, s.requestBaseReader(changed).GetNode("extra.go::Extra"))
	alternate := graph.New()
	alternate.AddNode(&graph.Node{ID: "uncovered.go::Other", FilePath: "uncovered.go", Kind: graph.KindFunction})
	view := &requestView{reader: alternate}
	routed := withRequestView(preparedCtx, view)
	require.NotSame(t, original, s.requestBaseReader(routed), "a different selected view must not reuse the old base")
	require.NotNil(t, s.requestBaseReader(routed).GetNode("uncovered.go::Other"))
	require.False(t, prepared.matches(s, preparedCtx, alternate, layers), "actual base identity is part of preparation")
	require.True(t, prepared.matches(s, preparedCtx, sql, layers))
	cancelCtx, cancel := context.WithCancel(preparedCtx)
	cancel()
	_, err = s.requestBaseReader(cancelCtx).(*graph.OverlaidView).GetNodesByIDsContext(cancelCtx, []string{"main.go::CurrentMain"})
	require.ErrorIs(t, err, context.Canceled, "prepared readers retain actual contextual cancellation")
}

func TestPreparedSymbolCompositionCannotReviveSupersededRequest(t *testing.T) {
	s, _, ctx, root := registeredSymbolCompositionFixture(t)
	pinned, err := s.bindPendingSearch(ctx)
	require.NoError(t, err)
	pinned = s.prepareSymbolRequestReader(pinned)
	require.NotNil(t, pinned.Value(preparedSymbolReaderKey{}))
	ack := preselectionRegisteredCall(t, s, ctx, 30, "edit_file", map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "CurrentMain", "new_string": "NewPhysicalMain", "format": "json"})
	require.False(t, ack.IsError, resultText(ack))
	result := preselectionRegisteredCall(t, s, pinned, 31, "search_symbols", map[string]any{"query": "CurrentMain", "query_class": "symbol", "expand": "off", "limit": 1000, "max_bytes": 0, "format": "json"})
	require.True(t, result.IsError, "a prepared reader cannot make an old owner current")
	require.Contains(t, resultText(result), "superseded")
}
