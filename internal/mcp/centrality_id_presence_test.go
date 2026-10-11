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
	"github.com/zzet/gortex/internal/graphview"
)

type centralityIDPresenceSpy struct {
	*store_sqlite.Store
	presenceReads, kindReads, fullReads int
	fail                                error
	cancel                              context.CancelFunc
}

func (s *centralityIDPresenceSpy) GetNodePresenceByIDsContext(ctx context.Context, ids []string) (map[string]struct{}, error) {
	s.presenceReads++
	rows, err := s.Store.GetNodePresenceByIDsContext(ctx, ids)
	if s.cancel != nil {
		s.cancel()
	}
	if s.fail != nil {
		return rows, s.fail
	}
	return rows, err
}

func (s *centralityIDPresenceSpy) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]graph.NodeKindRow, error) {
	s.kindReads++
	return s.Store.GetNodeKindsByIDsContext(ctx, ids)
}

func (s *centralityIDPresenceSpy) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.fullReads++
	return s.Store.GetNodesByIDs(ids)
}

func TestCentralityIDPresenceCSRAndPPRParity(t *testing.T) {
	g := walkTestGraph(t)
	g.AddNode(&graph.Node{ID: "unknown", Kind: ""})
	g.AddNode(&graph.Node{ID: "future", Kind: "future-kind"})
	resolved := &graph.Edge{From: "a.go::A", To: "unknown", Kind: graph.EdgeReferences, Origin: graph.OriginASTResolved}
	legacyOrigin := &graph.Edge{From: "unknown", To: "future", Kind: graph.EdgeCalls, Meta: map[string]any{"origin": graph.OriginTextMatched}}
	require.NotEqual(t, graph.ProvenanceWeight(resolved), graph.ProvenanceWeight(legacyOrigin))
	g.AddEdge(resolved)
	g.AddEdge(legacyOrigin)
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "presence.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch(g.AllNodes(), g.AllEdges())
	ids := []string{"a.go::A", "a.go::A", "missing", "", "unknown"}
	spy := &centralityIDPresenceSpy{Store: store}
	compact := &centralityPresenceReader{Reader: requestBoundReader(t.Context(), spy), ctx: t.Context(), ids: spy}
	full, fs := analysis.BuildBoundedAdjacencySnapshot(store, ids, 2, 4096, 16384)
	got, gs := analysis.BuildBoundedAdjacencySnapshot(compact, ids, 2, 4096, 16384)
	require.NoError(t, compact.err)
	require.Equal(t, full, got, "CSR order, caps and full provenance weights must remain exact")
	require.Equal(t, fs, gs)
	require.Positive(t, spy.presenceReads)
	require.Zero(t, spy.fullReads)
	require.Zero(t, spy.kindReads)

	legacy := &Server{graph: struct{ graph.Store }{store}}
	selected := &Server{graph: spy}
	installContractCoreKindTestRuntime(t, legacy)
	installContractCoreKindTestRuntime(t, selected)
	ctx := withContractCoreReadErrors(t.Context())
	require.Equal(t, legacy.boundedCentralityForRequest(ctx, ids, ids), selected.boundedCentralityForRequest(ctx, ids, ids))
	require.NoError(t, contractCoreReadError(ctx))
	require.Zero(t, spy.fullReads)
	require.Zero(t, spy.kindReads)
}

func TestCentralityIDPresenceAutoPrimaryAndSelectedFallbacks(t *testing.T) {
	v := newViewStack(t)
	spy := &centralityIDPresenceSpy{Store: v.store}
	v.srv.graph = spy
	installContractCoreKindTestRuntime(t, v.srv)
	ctx := WithSessionCWD(WithSessionID(t.Context(), viewTestSession), v.repoRoot)
	selected, err := v.srv.selectRequestView(ctx, graphview.Selector{Kind: graphview.SelectorAuto}, requestViewPolicy{})
	require.NoError(t, err)
	require.Nil(t, selected, "actual primary Auto selects the indexed Store")
	ctx, overlay, err := v.srv.prepareOverlayRequest(ctx)
	require.NoError(t, err)
	require.Nil(t, overlay)
	reader := v.srv.readerFor(ctx)
	require.Same(t, spy, centralityCheckedIDPresence(reader))
	require.NotImplements(t, (*graph.NodePresenceByIDsReader)(nil), reader, "the physical capability must stay private to CSR")
	result := v.srv.boundedCentralityForRequest(ctx, []string{"repo/edit.go::Old"}, []string{"repo/edit.go::Old"})
	require.NotEmpty(t, result.Scores)
	require.Positive(t, spy.presenceReads)
	require.Zero(t, spy.kindReads)
	require.Zero(t, spy.fullReads)

	v.store.AddBatch([]*graph.Node{
		{ID: "repo/deleted.go::Deleted", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/deleted.go"},
		{ID: "repo/replaced.go::Same", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/replaced.go"},
		{ID: "other/foreign.go::Foreign", Kind: graph.KindFunction, RepoPrefix: "other", FilePath: "other/foreign.go"},
	}, nil)
	layer := graph.NewOverlayLayer()
	layer.MarkFile("repo/deleted.go", true)
	layer.MarkFile("repo/replaced.go", false)
	layer.AddNode("repo/replaced.go", &graph.Node{ID: "repo/replaced.go::Same", Kind: "", RepoPrefix: "repo", FilePath: "repo/replaced.go"})
	layer.AddNode("repo/new.go", &graph.Node{ID: "repo/new.go::New", Kind: "future-kind", RepoPrefix: "repo", FilePath: "repo/new.go"})
	composed := graph.NewOverlaidView(spy, layer)
	ids := []string{"repo/deleted.go::Deleted", "repo/replaced.go::Same", "repo/new.go::New", "other/foreign.go::Foreign", "missing"}
	before := spy.presenceReads
	for _, scope := range []struct {
		reader  graph.Reader
		foreign bool
	}{{composed, true}, {newBaseGraphReader(composed, "repo"), false}} {
		core := newContractCoreEdges(scope.reader, ctx, nil)
		require.Nil(t, centralityCheckedIDPresence(core), "never bypass selected masks or repo ownership with underlying Store presence")
		checked, ok := centralityCheckedPresence(core)
		require.True(t, ok)
		fallback := &centralityPresenceReader{Reader: core, ctx: ctx, checked: checked}
		rows := fallback.GetNodesByIDs(ids)
		require.NoError(t, fallback.err)
		require.NotContains(t, rows, ids[0])
		require.Contains(t, rows, ids[1], "same-ID empty-kind replacement remains present")
		require.Contains(t, rows, ids[2])
		require.NotContains(t, rows, "missing")
		if scope.foreign {
			require.Contains(t, rows, ids[3])
		} else {
			require.NotContains(t, rows, ids[3])
		}
	}
	require.Equal(t, before, spy.presenceReads, "fallbacks must not invoke the underlying physical ID getter")
}

func TestCentralityIDPresenceCheckedFailureNeverCachesPartialCSR(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-error", true: "cancel"}[canceled], func(t *testing.T) {
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "presence.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			g := walkTestGraph(t)
			store.AddBatch(g.AllNodes(), g.AllEdges())
			ctx, end := context.WithCancel(withContractCoreReadErrors(t.Context()))
			defer end()
			spy := &centralityIDPresenceSpy{Store: store}
			if canceled {
				spy.cancel = end
			} else {
				spy.fail = errors.New("checked ID presence failed")
			}
			server := &Server{graph: spy, pprCache: newPPRWalkCache()}
			installContractCoreKindTestRuntime(t, server)
			got := server.boundedCentralityForRequest(ctx, []string{"a.go::A"}, []string{"a.go::A"})
			require.Empty(t, got.Scores)
			_, _, size, _, _ := server.pprCache.stats()
			require.Zero(t, size)
			require.Equal(t, 1, spy.presenceReads)
			require.Zero(t, spy.fullReads)
			require.Zero(t, spy.kindReads)
			if !canceled {
				require.ErrorIs(t, contractCoreReadError(ctx), spy.fail)
			}
		})
	}
}
