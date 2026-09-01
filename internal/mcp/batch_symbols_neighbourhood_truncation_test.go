package mcp

import (
	"encoding/json"
	"fmt"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// batch_symbols walks 1-hop callers/callees with a hard-coded Limit:10.
// BFS counts the seed, so the id lists cap at nine. Truncated is
// computed and must ride on the JSON entry; a total must not, because
// TotalNodes is the capped subgraph (seed+9), not the neighbourhood.

func TestBatchSymbols_CallersTruncatedFlag(t *testing.T) {
	t.Parallel()

	t.Run("15_callers_flag_true_len_9", func(t *testing.T) {
		t.Parallel()
		entry := batchSymbolsEntry(t, graphWithCallers(t, 15), "lib.go::Hub")
		callers := mustStringSlice(t, entry["callers"])
		assert.Equal(t, 9, len(callers), "Limit 10 includes the seed")
		assert.Equal(t, true, entry["callers_truncated"], "capped list must carry callers_truncated")
		assertNoInventedTotal(t, entry)
	})

	t.Run("8_callers_flag_absent_len_8", func(t *testing.T) {
		t.Parallel()
		entry := batchSymbolsEntry(t, graphWithCallers(t, 8), "lib.go::Hub")
		callers := mustStringSlice(t, entry["callers"])
		assert.Equal(t, 8, len(callers))
		_, flagged := entry["callers_truncated"]
		assert.False(t, flagged, "complete under-cap page must not grow a truncated key")
		assertNoInventedTotal(t, entry)
	})

	t.Run("15_callees_flag_true_len_9", func(t *testing.T) {
		t.Parallel()
		entry := batchSymbolsEntry(t, graphWithCallees(t, 15), "lib.go::Hub")
		callees := mustStringSlice(t, entry["callees"])
		assert.Equal(t, 9, len(callees), "Limit 10 includes the seed")
		assert.Equal(t, true, entry["callees_truncated"], "capped list must carry callees_truncated")
		assertNoInventedTotal(t, entry)
	})

	t.Run("8_callees_flag_absent_len_8", func(t *testing.T) {
		t.Parallel()
		entry := batchSymbolsEntry(t, graphWithCallees(t, 8), "lib.go::Hub")
		callees := mustStringSlice(t, entry["callees"])
		assert.Equal(t, 8, len(callees))
		_, flagged := entry["callees_truncated"]
		assert.False(t, flagged, "complete under-cap page must not grow a truncated key")
		assertNoInventedTotal(t, entry)
	})
}

func TestBatchSymbols_GCXOmitsNeighbourhood(t *testing.T) {
	t.Parallel()
	g := graphWithCallers(t, 15)
	srv := newBatchSymbolsServer(t, g)
	res := callTool(t, srv, "batch_symbols", map[string]any{
		"ids":    []any{"lib.go::Hub"},
		"format": "gcx",
	})
	require.False(t, res.IsError, "%s", toolResultText(res))
	text := toolResultText(res)
	assert.NotContains(t, text, "callers", "GCX batch_symbols encoder drops the neighbourhood lists")
	assert.NotContains(t, text, "callers_truncated")
}

func graphWithCallers(t *testing.T, n int) *graph.Graph {
	t.Helper()
	g := graph.New()
	target := hubNode()
	g.AddNode(target)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c%d.go::Caller%d", i, i)
		g.AddNode(&graph.Node{
			ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("Caller%d", i),
			FilePath: fmt.Sprintf("c%d.go", i), Language: "go",
			StartLine: 1, EndLine: 5,
		})
		g.AddEdge(&graph.Edge{
			From: id, To: target.ID, Kind: graph.EdgeCalls,
			FilePath: fmt.Sprintf("c%d.go", i), Line: 2,
			Origin: graph.OriginLSPResolved,
		})
	}
	return g
}

func graphWithCallees(t *testing.T, n int) *graph.Graph {
	t.Helper()
	g := graph.New()
	target := hubNode()
	g.AddNode(target)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("d%d.go::Callee%d", i, i)
		g.AddNode(&graph.Node{
			ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("Callee%d", i),
			FilePath: fmt.Sprintf("d%d.go", i), Language: "go",
			StartLine: 1, EndLine: 5,
		})
		g.AddEdge(&graph.Edge{
			From: target.ID, To: id, Kind: graph.EdgeCalls,
			FilePath: "lib.go", Line: 2,
			Origin: graph.OriginLSPResolved,
		})
	}
	return g
}

func hubNode() *graph.Node {
	return &graph.Node{
		ID: "lib.go::Hub", Kind: graph.KindFunction, Name: "Hub",
		FilePath: "lib.go", Language: "go", StartLine: 1, EndLine: 3,
	}
}

func newBatchSymbolsServer(t *testing.T, g *graph.Graph) *Server {
	t.Helper()
	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil)
}

func batchSymbolsEntry(t *testing.T, g *graph.Graph, id string) map[string]any {
	t.Helper()
	srv := newBatchSymbolsServer(t, g)
	res := callTool(t, srv, "batch_symbols", map[string]any{"ids": []any{id}})
	require.False(t, res.IsError, "%s", toolResultText(res))
	var batchResp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &batchResp))
	syms, ok := batchResp["symbols"].([]any)
	require.True(t, ok)
	require.Len(t, syms, 1)
	entry, ok := syms[0].(map[string]any)
	require.True(t, ok)
	return entry
}

func mustStringSlice(t *testing.T, raw any) []any {
	t.Helper()
	require.NotNil(t, raw)
	out, ok := raw.([]any)
	require.True(t, ok, "expected id list, got %T", raw)
	return out
}

func assertNoInventedTotal(t *testing.T, entry map[string]any) {
	t.Helper()
	for _, k := range []string{"callers_total", "callees_total", "caller_total", "callee_total", "total_nodes"} {
		_, present := entry[k]
		assert.False(t, present, "must not emit %s — TotalNodes is the page size, not the neighbourhood", k)
	}
}
