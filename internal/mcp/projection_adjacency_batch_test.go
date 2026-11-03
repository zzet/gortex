package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type pendingAdjacencyCountStore struct {
	graph.Store
	inReads, outReads int
}

func (s *pendingAdjacencyCountStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.outReads++
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *pendingAdjacencyCountStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.inReads++
	return s.Store.GetInEdgesByNodeIDs(ids)
}

// A dense canonical adjacency outside the pending files must not be copied
// once for every unrelated immutable file layer. Count allocations rather than
// assert a machine-specific elapsed time; every query still asks the actual
// base and preserves the complete incoming/outgoing answer.
func TestPendingAdjacencyDoesNotCopyCanonicalRowsAtEveryLayer(t *testing.T) {
	base := graph.New()
	for i := range 128 {
		base.AddEdge(&graph.Edge{From: "base.go::Source", To: fmt.Sprintf("callee.go::F%d", i), Kind: graph.EdgeCalls, FilePath: "base.go"})
		base.AddEdge(&graph.Edge{From: fmt.Sprintf("caller.go::F%d", i), To: "base.go::Target", Kind: graph.EdgeCalls, FilePath: "caller.go"})
	}
	store := &pendingAdjacencyCountStore{Store: base}
	srv := &Server{graph: store}
	var layers []graph.OverlayLayerReader
	for i := range 256 {
		path := fmt.Sprintf("pending/%03d.go", i)
		layer := graph.NewOverlayLayer()
		layer.MarkFile(path, false)
		layer.AddNode(path, &graph.Node{ID: path + "::Pending", FilePath: path, Kind: graph.KindFunction})
		layers = append(layers, layer)
	}
	ctx := context.WithValue(t.Context(), requestProjectionKey{}, layers)
	reader := srv.requestBaseReader(ctx)
	allocations := testing.AllocsPerRun(1, func() {
		store.inReads, store.outReads = 0, 0
		out := reader.GetOutEdgesByNodeIDs([]string{"base.go::Source"})
		in := reader.GetInEdgesByNodeIDs([]string{"base.go::Target"})
		require.Len(t, out["base.go::Source"], 128)
		require.Len(t, in["base.go::Target"], 128)
		require.Equal(t, 1, store.inReads)
		require.Equal(t, 1, store.outReads)
	})
	require.Less(t, allocations, float64(128), "canonical adjacency is repeatedly copied through the pending-layer chain")
}

func TestPendingAdjacencyBatchCannotReviveSupersededRequestFacts(t *testing.T) {
	selectedProjectionRegistry(t)
	server, root := ackModeServer(t, "local")
	searchProjectedAck(t, server, root, "BatchBorrowed")
	other := callToolByName(t, server, t.Context(), "write_file", map[string]any{
		"path": filepath.Join(root, "other.go"), "content": "package main\n\nfunc BatchOther() {}\n", "format": "json",
	})
	require.False(t, other.IsError, "%v", other.Content)
	ctx, err := server.bindPendingSearch(t.Context())
	require.NoError(t, err)
	require.Len(t, requestProjectionLayers(ctx), 2, "the public fixture must actually select the batch path")
	path := filepath.Join(root, "main.go")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	replaced := callToolByName(t, server, t.Context(), "write_file", map[string]any{"path": path, "content": string(raw), "format": "json"})
	require.False(t, replaced.IsError, "%v", replaced.Content)
	result := callToolByName(t, server, ctx, "search_symbols", map[string]any{"query": "BatchBorrowed", "query_class": "symbol", "expand": "off", "format": "json"})
	require.True(t, result.IsError, "a new same-byte physical owner must invalidate the whole selected request")
	require.Contains(t, toolResultText(result), "superseded")
}
