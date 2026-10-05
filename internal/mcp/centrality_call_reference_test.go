package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type centralityCallReferenceSpy struct {
	graph.Store
	checked graph.CallReferenceOutgoingReader
	reads   int
	fail    error
	cancel  context.CancelFunc
}

func (s *centralityCallReferenceSpy) GetCallReferenceOutEdgesContext(ctx context.Context, ids []string) (map[string][]*graph.Edge, error) {
	s.reads++
	rows, err := s.checked.GetCallReferenceOutEdgesContext(ctx, ids)
	if s.cancel != nil {
		s.cancel()
	}
	if s.fail != nil {
		return rows, s.fail
	}
	return rows, err
}

func TestCentralityCallReferenceActualStoreCSRAndPPRParity(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "csr.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	nodes := []*graph.Node{{ID: "a", Kind: graph.KindFunction}, {ID: "b", Kind: graph.KindFunction}, {ID: "c", Kind: graph.KindFunction}, {ID: "contract", Kind: graph.KindContract}, {ID: "table", Kind: graph.KindTable}}
	edges := []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeReferences, Line: 1, Meta: map[string]any{"origin": graph.OriginTextMatched}},
		{From: "a", To: "c", Kind: graph.EdgeReturnsTo, Line: 2},
		{From: "a", To: "c", Kind: graph.EdgeCalls, Line: 3, Origin: graph.OriginASTResolved, Confidence: .8123},
		{From: "a", To: "b", Kind: graph.EdgeCalls, Line: 4, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "a", To: "contract", Kind: graph.EdgeCalls, Line: 5},
		{From: "a", To: "table", Kind: graph.EdgeProvides, Line: 6},
		{From: "b", To: "c", Kind: graph.EdgeCalls, Line: 7},
	}
	require.NoError(t, s.AddBatchChecked(nodes, edges))
	positive := s.AtGeneration(7)
	require.NoError(t, positive.AddBatchChecked(nodes, []*graph.Edge{edges[0], edges[6]}))
	for _, selected := range []*store_sqlite.Store{s, positive} {
		ctx := withContractCoreReadErrors(t.Context())
		spy := &centralityCallReferenceSpy{Store: selected, checked: selected}
		core := newContractCoreEdges(spy, ctx, nil)
		read := centralityCheckedCallReferences(ctx, core)
		require.NotNil(t, read)
		adapter := &centralityCallReferenceReader{Reader: core, ctx: ctx, read: read}
		ids := []string{"a", "b", "a", "", "missing"}
		for _, caps := range [][2]int{{4096, 16384}, {2, 1}} {
			full, fullStats := analysis.BuildBoundedAdjacencySnapshot(core, ids, 2, caps[0], caps[1])
			got, stats := analysis.BuildBoundedAdjacencySnapshot(adapter, ids, 2, caps[0], caps[1])
			require.NoError(t, adapter.err)
			require.Equal(t, full, got)
			require.Equal(t, fullStats, stats)
		}
		// The optional path must not change public SQL dataflow adjacency.
		if selected == s {
			keptSQL := false
			for _, edge := range core.GetOutEdges("a") {
				if edge.Kind == graph.EdgeProvides && edge.To == "table" {
					keptSQL = true
				}
			}
			require.True(t, keptSQL, "ordinary SQL provides remains on the public adjacency path")
		}
		rows := adapter.GetOutEdgesByNodeIDs([]string{"a"})
		for _, edge := range rows["a"] {
			require.NotEqual(t, "spring.Bean", edge.Meta["via"])
			require.NotEqual(t, "contract", edge.To)
		}
		fast := &Server{graph: spy}
		legacy := &Server{graph: struct{ graph.Store }{selected}}
		installContractCoreKindTestRuntime(t, fast)
		installContractCoreKindTestRuntime(t, legacy)
		require.Equal(t, legacy.boundedCentralityForRequest(ctx, ids, ids), fast.boundedCentralityForRequest(ctx, ids, ids))
		require.Positive(t, spy.reads)
	}
}

func TestCentralityCallReferenceSelectedAndLegacyFallback(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "selected.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.AddBatchChecked([]*graph.Node{
		{ID: "repo/f.go::A", Kind: graph.KindFunction, FilePath: "repo/f.go", RepoPrefix: "repo"},
		{ID: "repo/g.go::B", Kind: graph.KindFunction, FilePath: "repo/g.go", RepoPrefix: "repo"},
	}, []*graph.Edge{{From: "repo/f.go::A", To: "repo/g.go::B", Kind: graph.EdgeCalls, FilePath: "repo/f.go"}}))
	layer := graph.NewOverlayLayer()
	layer.MarkFile("repo/f.go", true)
	selected := graph.NewOverlaidViewWithLayer(s, layer)
	ctx := withContractCoreReadErrors(t.Context())
	for _, reader := range []graph.Reader{selected, newBaseGraphReader(s, "repo"), struct{ graph.Reader }{s}} {
		core := newContractCoreEdges(reader, ctx, nil)
		require.Nil(t, centralityCheckedCallReferences(ctx, core), "unsupported selected wrappers must not be unwrapped")
	}
	core := newContractCoreEdges(selected, ctx, nil)
	require.Empty(t, core.GetOutEdgesByNodeIDs([]string{"repo/f.go::A"}), "selected tombstone remains authoritative")
	layer = graph.NewOverlayLayer()
	layer.AddNode("repo/f.go", &graph.Node{ID: "repo/f.go::A", Kind: graph.KindFunction, FilePath: "repo/f.go", RepoPrefix: "repo"})
	layer.AddEdge(&graph.Edge{From: "repo/f.go::A", To: "repo/g.go::B", Kind: graph.EdgeReferences, FilePath: "repo/f.go", Line: 99})
	selected = graph.NewOverlaidViewWithLayer(s, layer)
	core = newContractCoreEdges(selected, ctx, nil)
	require.Nil(t, centralityCheckedCallReferences(ctx, core))
	require.Equal(t, 99, core.GetOutEdges("repo/f.go::A")[0].Line)
	require.Nil(t, centralityCheckedCallReferences(t.Context(), newContractCoreEdges(s, t.Context(), nil)), "potential writers retain legacy error and reader behavior")
}

func TestCentralityCallReferenceCheckedErrorAndCancellationNeverCache(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "failure.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "a", Kind: graph.KindFunction}, {ID: "b", Kind: graph.KindFunction}}, []*graph.Edge{{From: "a", To: "b", Kind: graph.EdgeCalls}}))
	for _, cancel := range []bool{false, true} {
		ctx, end := context.WithCancel(withContractCoreReadErrors(t.Context()))
		spy := &centralityCallReferenceSpy{Store: s, checked: s, fail: errors.New("projected batch failed")}
		if cancel {
			spy.fail = nil
			spy.cancel = end
		}
		srv := &Server{graph: spy, pprCache: newPPRWalkCache()}
		installContractCoreKindTestRuntime(t, srv)
		got := srv.boundedCentralityForRequest(ctx, []string{"a"}, []string{"a"})
		require.Empty(t, got.Scores)
		_, _, size, _, _ := srv.pprCache.stats()
		require.Zero(t, size)
		require.Positive(t, spy.reads)
		if !cancel {
			require.ErrorIs(t, contractCoreReadError(ctx), spy.fail)
		}
		end()
	}
}
