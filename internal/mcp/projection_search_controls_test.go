package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

func TestProjectionSearchChargesImmutableNameCacheToExistingCap(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	server, root := setupTestServer(t)
	searchProjectedAck(t, server, root, "ChargedNames")
	entry := liveEntry(pendingFileProjections, filepath.Join(root, "main.go"))
	require.NotNil(t, entry)
	require.NotNil(t, entry.names)
	require.Positive(t, entry.names.Count())
	base := int64(len(entry.layer.FileNodes(entry.graphPath))) * projectionNodeBytes
	for _, edges := range entry.layer.OutEdgesByFromAll() {
		base += int64(len(edges)) * projectionEdgeBytes
	}
	var cacheBytes int64
	for _, name := range entry.names.names {
		cacheBytes += 48 + int64(len(name.name)+len(name.qualified))
	}
	require.GreaterOrEqual(t, entry.size, base+cacheBytes, "the new immutable name cache must not escape the existing byte allowance")
}

func TestProjectionSearchUsesNativeBackendRankRatherThanSubstringFallback(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc helper() {}\n"), 0o600))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, testRegistry(), config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	backend := search.NewSymbolSearcherBackend(store)
	require.NotEmpty(t, backend.Search("helper", 10), "fixture must exercise actual SQLite native symbol search")
	engine := query.NewEngine(store)
	engine.SetSearch(backend)
	server := NewServer(engine, store, idx, nil, zap.NewNop(), nil)
	searchProjectedAck(t, server, root, "NativeACKOnly")
	result := callTool(t, server, "search_symbols", map[string]any{"query": "NativeACKOnly", "query_class": "symbol", "expand": "off", "limit": 1, "max_bytes": 0})
	require.False(t, result.IsError)
	rows, _ := extractTextResult(t, result)["results"].([]any)
	require.Len(t, rows, 1)
	require.Equal(t, "main.go::NativeACKOnly", rows[0].(map[string]any)["id"])
	bound, err := server.bindPendingSearch(t.Context())
	require.NoError(t, err)
	got := server.engineFor(bound).GatherSymbolCandidates("NativeACKOnly", 10, query.QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true, SkipExactNameSplice: true}, nil)
	found := false
	for _, candidate := range got {
		if candidate.Node.ID == "main.go::NativeACKOnly" {
			found = true
			require.GreaterOrEqual(t, candidate.TextRank, 0, "substring fallback has TextRank=-1 and cannot satisfy the native candidate proof")
		}
	}
	require.True(t, found)
}

func TestProjectionSearchCapturedVersionsKeepCandidatesAndRowsTogether(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	server, root := setupTestServer(t)
	searchProjectedAck(t, server, root, "PinnedACKOne")
	first, err := server.bindPendingSearch(t.Context())
	require.NoError(t, err)
	result := callTool(t, server, "edit_file", map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "PinnedACKOne", "new_string": "PinnedACKTwo", "format": "json"})
	require.False(t, result.IsError)
	require.Equal(t, true, extractTextResult(t, result)["readback_exact"])
	second, err := server.bindPendingSearch(t.Context())
	require.NoError(t, err)
	for _, sample := range []struct {
		ctx             context.Context
		present, absent string
	}{{first, "PinnedACKOne", "PinnedACKTwo"}, {second, "PinnedACKTwo", "PinnedACKOne"}, {first, "PinnedACKOne", "PinnedACKTwo"}} {
		engine := server.engineFor(sample.ctx)
		got := engine.GatherSymbolCandidates(sample.present, 10, query.QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}, nil)
		found := false
		for _, row := range got {
			found = found || row.Node.ID == "main.go::"+sample.present
		}
		require.True(t, found, "candidate enumeration and payload hydration used different versions")
		require.Nil(t, server.readerFor(sample.ctx).GetNode("main.go::"+sample.absent))
	}
	require.Nil(t, server.graph.GetNode("main.go::PinnedACKOne"))
	require.Nil(t, server.graph.GetNode("main.go::PinnedACKTwo"))
}

func TestProjectionSearchEmptyAndForeignOnlyRegistriesPreserveBaseEngine(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	selected, _ := setupTestServer(t)
	ctx := t.Context()
	bound, err := selected.bindPendingSearch(ctx)
	require.NoError(t, err)
	require.Same(t, selected.engine, selected.engineFor(bound))
	sibling, root := setupTestServer(t)
	searchProjectedAck(t, sibling, root, "ForeignOnly")
	bound, err = selected.bindPendingSearch(ctx)
	require.NoError(t, err)
	require.Same(t, selected.engine, selected.engineFor(bound), "same prefix from another physical root must not activate a layer")
}

func TestProjectionSearchEditorRemainsAbovePendingDisk(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	server, root := setupTestServer(t)
	searchProjectedAck(t, server, root, "DiskOnly")
	buffer := graph.NewOverlayLayer()
	buffer.MarkFile("main.go", false)
	buffer.AddNode("main.go", &graph.Node{ID: "main.go::BufferOnly", Name: "BufferOnly", Kind: graph.KindFunction, FilePath: "main.go", StartLine: 50})
	ctx := WithOverlayView(t.Context(), graph.NewOverlaidView(server.graph, buffer))
	bound, err := server.bindPendingSearch(ctx)
	require.NoError(t, err)
	require.Nil(t, server.readerFor(bound).GetNode("main.go::DiskOnly"))
	require.NotNil(t, server.readerFor(bound).GetNode("main.go::BufferOnly"))
	for _, candidate := range server.engineFor(bound).GatherSymbolCandidates("DiskOnly", 10, query.QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}, nil) {
		require.False(t, strings.HasSuffix(candidate.Node.ID, "::DiskOnly"), "disk candidate resurrected a name the pinned editor removed")
	}
}

func TestProjectionSearchExactAndPrefixSurviveHighBaseScoreAndLimitOne(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	server, root := setupTestServer(t)
	searchProjectedAck(t, server, root, "ACKSearchRanked")
	base := &graph.Node{ID: "aaa.go::Unrelated", Name: "Unrelated", Kind: graph.KindFunction, FilePath: "aaa.go", StartLine: 1}
	server.graph.AddNode(base)
	server.engine.SetSearch(&searchSymbolsContextBackend{bundles: []search.SymbolBundle{{Node: base, Score: 100000}}})
	for _, q := range []string{"ACKSearchRanked", "ACKSearchRa"} {
		result := callTool(t, server, "search_symbols", map[string]any{"query": q, "query_class": "symbol", "expand": "off", "limit": 1, "max_bytes": 0})
		require.False(t, result.IsError)
		rows, _ := extractTextResult(t, result)["results"].([]any)
		require.Len(t, rows, 1)
		require.Equal(t, "main.go::ACKSearchRanked", rows[0].(map[string]any)["id"], "high base score displaced the pending exact/prefix match: %s", q)
	}
}

func TestProjectionSearchMasksOldDeclarationWhileBaseRemainsPending(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	server, root := setupTestServer(t)
	searchProjectedAck(t, server, root, "BeforeRename")
	result := callTool(t, server, "edit_file", map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "func helper() {}", "new_string": "func AfterRename() {}", "format": "json"})
	require.False(t, result.IsError)
	require.Equal(t, true, extractTextResult(t, result)["readback_exact"])
	require.NotNil(t, server.graph.GetNode("main.go::helper"), "base must still carry the old declaration")
	result = callTool(t, server, "search_symbols", map[string]any{"query": "helper", "query_class": "symbol", "expand": "off", "limit": 1000, "max_bytes": 0})
	require.False(t, result.IsError)
	rows, _ := extractTextResult(t, result)["results"].([]any)
	for _, raw := range rows {
		require.NotEqual(t, "main.go::helper", raw.(map[string]any)["id"], "pending rename leaked its old base declaration")
	}
}
