package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func (s *handlerBudgetStore) GetInEdges(id string) []*graph.Edge {
	s.legacyReads++
	return s.Store.GetInEdges(id)
}

func (s *handlerBudgetStore) GetNode(id string) *graph.Node {
	s.legacyReads++
	return s.Store.GetNode(id)
}

func (s *handlerBudgetStore) GetFileSubGraph(path string) ([]*graph.Node, []*graph.Edge) {
	s.legacyReads++
	return s.Store.(graph.FileSubGraphReader).GetFileSubGraph(path)
}

func (s *handlerBudgetStore) GetFileSubGraphCounts(path string) ([]*graph.Node, int) {
	s.legacyReads++
	return s.Store.(graph.FileSubGraphCountReader).GetFileSubGraphCounts(path)
}

func TestUneditedSummaryStoreDeadlineReturnsPendingWithoutLegacyReads(t *testing.T) {
	for _, format := range []string{"json", "gcx"} {
		t.Run(format, func(t *testing.T) {
			s, _ := setupTestServer(t)
			s.indexer = nil // Unedited selected-store path, no projection to rescue it.
			ctx := withRequestProjectionFacts(t.Context())
			store := &handlerBudgetStore{Store: s.graph}
			s.graph = store
			s.engine = query.NewEngine(store)
			req := mcplib.CallToolRequest{}
			req.Params.Name = "get_file_summary"
			req.Params.Arguments = map[string]any{"path": "main.go", "format": format}
			result, err := s.handleGetFileSummary(ctx, req)
			require.NoError(t, err)
			require.False(t, result.IsError, "%s", resultText(result))
			require.True(t, store.budgetRead, "actual summary symbols/subgraph/counts must use the selected 30ms store scope")
			require.Zero(t, store.legacyReads, "summary timeout cannot fall back to errorless symbols/subgraph/count APIs")
			fields := map[string]any{}
			if format == "json" {
				fields = extractTextResult(t, result)
				require.Empty(t, fields["nodes"], "no stale unedited rows may escape a refused scope")
			}
			s.freshnessFor(ctx, "relations", nil, []string{"main.go"}).apply(fields)
			require.Equal(t, "pending", fields["graph_freshness"])
			require.Equal(t, "store_busy", fields["reason"])
			require.Equal(t, false, fields["complete"])
		})
	}
}

type handlerHealthyBudgetStore struct{ handlerBudgetStore }

func (s *handlerHealthyBudgetStore) WithFileEditingContextReader(ctx context.Context, fn func(context.Context, graph.FileEditingContextRows) error) error {
	deadline, ok := ctx.Deadline()
	s.budgetRead = ok && time.Until(deadline) <= 30*time.Millisecond
	return s.Store.(graph.FileEditingContextReadScope).WithFileEditingContextReader(ctx, fn)
}

func TestUneditedSummaryCheckedScopePreservesFullRowsCountsAndETag(t *testing.T) {
	for _, format := range []string{"json", "gcx"} {
		t.Run(format, func(t *testing.T) {
			s, _ := setupTestServer(t)
			s.indexer = nil
			s.graph.AddNode(&graph.Node{ID: "other.go", Kind: graph.KindFile, FilePath: "other.go", Meta: map[string]any{"full": "importer metadata"}})
			s.graph.AddEdge(&graph.Edge{From: "other.go", To: "main.go", Kind: graph.EdgeImports, FilePath: "other.go", Meta: map[string]any{"full": "importer edge"}})
			want := s.engine.GetFileSymbols("main.go")
			if format == "gcx" {
				want = s.engine.GetFileSymbolsCounts("main.go")
			}
			want = stripNonDefinitionNodes(want)
			wantETag := etagSubGraph(want)
			store := &handlerHealthyBudgetStore{handlerBudgetStore{Store: s.graph}}
			s.graph = store
			s.engine = query.NewEngine(store)
			req := mcplib.CallToolRequest{}
			req.Params.Name = "get_file_summary"
			req.Params.Arguments = map[string]any{"path": "main.go", "format": format}
			result, err := s.handleGetFileSummary(withRequestProjectionFacts(t.Context()), req)
			require.NoError(t, err)
			require.False(t, result.IsError, "%s", resultText(result))
			require.True(t, store.budgetRead)
			require.Zero(t, store.legacyReads)
			if format == "gcx" {
				encoded, err := encodeFileSummary(want, wantETag)
				require.NoError(t, err)
				require.Equal(t, string(encoded), resultText(result))
			} else {
				fields := extractTextResult(t, result)
				require.Equal(t, []any{"other.go"}, fields["dependents"], "importer hydration must stay inside the same checked store scope")
				wantNodes, err := json.Marshal(want.Nodes)
				require.NoError(t, err)
				gotNodes, err := json.Marshal(fields["nodes"])
				require.NoError(t, err)
				require.JSONEq(t, string(wantNodes), string(gotNodes), "full node metadata must survive checked hydration")
				wantEdges, err := json.Marshal(want.Edges)
				require.NoError(t, err)
				gotEdges, err := json.Marshal(fields["edges"])
				require.NoError(t, err)
				require.JSONEq(t, string(wantEdges), string(gotEdges))
				require.Equal(t, wantETag, fields["etag"])
			}
		})
	}
}
