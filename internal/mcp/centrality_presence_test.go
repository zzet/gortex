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
	resolved := &graph.Edge{From: "a.go::A", To: "unknown", Kind: graph.EdgeReferences, Origin: graph.OriginASTResolved}
	legacyOrigin := &graph.Edge{From: "unknown", To: "future", Kind: graph.EdgeCalls, Meta: map[string]any{"origin": graph.OriginTextMatched}}
	require.NotEqual(t, graph.ProvenanceWeight(resolved), graph.ProvenanceWeight(legacyOrigin), "fixture must carry heterogeneous CSR weights, including Meta origin fallback")
	g.AddEdge(resolved)
	g.AddEdge(legacyOrigin)
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
	g.AddNode(&graph.Node{ID: "repo/file.go::Keep", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/file.go"})
	g.AddNode(&graph.Node{ID: "other/file.go::Foreign", Kind: graph.KindFunction, RepoPrefix: "other", FilePath: "other/file.go"})
	g.AddNode(&graph.Node{ID: "repo/replaced.go::Replace", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/replaced.go"})
	layer := graph.NewOverlayLayer()
	layer.MarkFile("repo/file.go", true)
	layer.AddNode("repo/new.go", &graph.Node{ID: "repo/new.go::New", Kind: "", RepoPrefix: "repo", FilePath: "repo/new.go"})
	layer.MarkFile("repo/replaced.go", false)
	layer.AddNode("repo/replaced.go", &graph.Node{ID: "repo/replaced.go::Replace", Kind: "future-kind", RepoPrefix: "repo", FilePath: "repo/replaced.go"})
	selected := graph.NewOverlaidView(g, layer)
	r := &centralityPresenceReader{Reader: selected, ctx: t.Context(), checked: selected}
	nodes := r.GetNodesByIDs([]string{"repo/file.go::Keep", "repo/new.go::New", "repo/replaced.go::Replace", "missing"})
	require.NotContains(t, nodes, "repo/file.go::Keep")
	require.NotNil(t, nodes["repo/new.go::New"])
	require.NotNil(t, nodes["repo/replaced.go::Replace"])
	rows, err := selected.GetNodeKindsByIDsContext(t.Context(), []string{"repo/replaced.go::Replace"})
	require.NoError(t, err)
	require.Equal(t, graph.NodeKind("future-kind"), rows["repo/replaced.go::Replace"].Kind, "same identity must resolve to selected replacement")
	require.NotContains(t, nodes, "missing")
	base := &baseGraphReader{base: selected, repoPrefix: "repo"}
	narrowed := &centralityPresenceReader{Reader: base, ctx: t.Context(), checked: base}
	require.Len(t, narrowed.GetNodesByIDs([]string{"repo/new.go::New", "other/file.go::Foreign"}), 1)
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
