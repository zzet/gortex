package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

type centralityPresenceSpy struct {
	graph.Store
	kinds       graph.NodeKindsByIDsReader
	reads, full int
	fail        error
	cancel      context.CancelFunc
}

func (s *centralityPresenceSpy) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.full++
	return s.Store.GetNodesByIDs(ids)
}
func (s *centralityPresenceSpy) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]graph.NodeKindRow, error) {
	s.reads++
	rows, err := s.kinds.GetNodeKindsByIDsContext(ctx, ids)
	if s.cancel != nil {
		s.cancel()
	}
	if s.fail != nil {
		return rows, s.fail
	}
	return rows, err
}
func TestCentralityPresenceCSRAndPPRParity(t *testing.T) {
	g := walkTestGraph(t)
	g.AddNode(&graph.Node{ID: "unknown", Kind: ""})
	g.AddNode(&graph.Node{ID: "future", Kind: "future-kind"})
	g.AddEdge(&graph.Edge{From: "a.go::A", To: "unknown", Kind: graph.EdgeReferences, Meta: map[string]any{"confidence": 0.25}})
	g.AddEdge(&graph.Edge{From: "unknown", To: "future", Kind: graph.EdgeCalls})
	ids := []string{"a.go::A", "a.go::A", "missing", "", "unknown"}
	spy := &centralityPresenceSpy{Store: g, kinds: g}
	compact := &centralityPresenceReader{Reader: requestBoundReader(t.Context(), spy), ctx: t.Context(), checked: spy}
	full, fs := analysis.BuildBoundedAdjacencySnapshot(g, ids, 2, 4096, 16384)
	got, gs := analysis.BuildBoundedAdjacencySnapshot(compact, ids, 2, 4096, 16384)
	require.NoError(t, compact.err)
	require.Equal(t, full, got, "CSR and provenance weights must remain exact")
	require.Equal(t, fs, gs)
	require.Zero(t, spy.full)
	require.Positive(t, spy.reads)
	legacy := &Server{graph: struct{ graph.Store }{g}}
	selected := &Server{graph: spy}
	require.Equal(t, legacy.boundedCentralityForRequest(t.Context(), ids, ids), selected.boundedCentralityForRequest(t.Context(), ids, ids))
}
func TestCentralityPresenceSelectedMasksAndBaseOwnership(t *testing.T) {
	g := walkTestGraph(t)
	g.AddNode(&graph.Node{ID: "repo/keep", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/file.go"})
	g.AddNode(&graph.Node{ID: "foreign", Kind: graph.KindFunction, RepoPrefix: "other", FilePath: "other/file.go"})
	layer := graph.NewOverlayLayer()
	layer.MarkFile("repo/file.go", true)
	layer.AddNode("repo/new.go", &graph.Node{ID: "repo/new", Kind: "", RepoPrefix: "repo", FilePath: "repo/new.go"})
	selected := graph.NewOverlaidView(g, layer)
	r := &centralityPresenceReader{Reader: selected, ctx: t.Context(), checked: selected}
	nodes := r.GetNodesByIDs([]string{"repo/keep", "repo/new", "missing"})
	require.NotContains(t, nodes, "repo/keep")
	require.NotNil(t, nodes["repo/new"])
	require.NotContains(t, nodes, "missing")
	base := &baseGraphReader{base: selected, repoPrefix: "repo"}
	narrowed := &centralityPresenceReader{Reader: base, ctx: t.Context(), checked: base}
	require.Len(t, narrowed.GetNodesByIDs([]string{"repo/new", "foreign"}), 1)
}
func TestCentralityPresenceCheckedFailureNeverCachesPartialCSR(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-error", true: "cancel"}[cancel], func(t *testing.T) {
			g := walkTestGraph(t)
			ctx, end := context.WithCancel(withContractCoreReadErrors(context.Background()))
			defer end()
			spy := &centralityPresenceSpy{Store: g, kinds: g}
			if cancel {
				spy.cancel = end
			} else {
				spy.fail = errors.New("checked presence failed")
			}
			srv := &Server{graph: spy, pprCache: newPPRWalkCache()}
			got := srv.boundedCentralityForRequest(ctx, []string{"a.go::A"}, []string{"a.go::A"})
			require.Empty(t, got.Scores)
			_, _, size, _, _ := srv.pprCache.stats()
			require.Zero(t, size)
			require.Zero(t, spy.full)
			if !cancel {
				require.ErrorIs(t, contractCoreReadError(ctx), spy.fail)
			}
		})
	}
}

func TestCentralityPresenceRequiresActualSelectedKindCapability(t *testing.T) {
	g := walkTestGraph(t)
	checked, ok := centralityCheckedPresence(newContractCoreEdges(g, t.Context(), nil))
	require.True(t, ok)
	require.NotNil(t, checked)
	_, ok = centralityCheckedPresence(newContractCoreEdges(struct{ graph.Reader }{g}, t.Context(), nil))
	require.False(t, ok, "legacy full-node fallback is not the compact capability")
}
