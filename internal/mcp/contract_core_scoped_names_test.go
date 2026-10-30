package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

type scopedExactNameSpy struct {
	graph.Reader
	backend                        graph.ScopedOutlineNodeReader
	rows, scopedCalls, legacyCalls int
}

func (r *scopedExactNameSpy) OutlineNodesByName(scope graph.NodeScope, name string) ([]*graph.Node, error) {
	r.scopedCalls++
	rows, err := r.backend.OutlineNodesByName(scope, name)
	r.rows += len(rows)
	return rows, err
}
func (r *scopedExactNameSpy) OutlineNodeIDsInScope(scope graph.NodeScope, ids []string) ([]string, error) {
	return r.backend.OutlineNodeIDsInScope(scope, ids)
}
func (r *scopedExactNameSpy) FindNodesByName(name string) []*graph.Node {
	r.legacyCalls++
	return r.Reader.FindNodesByName(name)
}

func TestContractCoreScopedExactNameRescueAvoidsUnrelatedRows(t *testing.T) {
	s := openContractCoreCapStore(t)
	s.AddBatch([]*graph.Node{
		{ID: "a/focus.go::Needle", Name: "Needle", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "project", FilePath: "a/focus.go", Meta: map[string]any{"signature": "func Needle()"}},
		{ID: "external::Needle", Name: "Needle", Kind: graph.KindFunction, FilePath: "external::dependency"},
	}, nil)
	for pass := range 2 {
		spy := &scopedExactNameSpy{Reader: s, backend: s}
		wrapped := newContractCoreEdges(spy, t.Context(), nil)
		scope := graph.NodeScope{WorkspaceID: "ws", ProjectID: "project", RepoAllow: map[string]bool{"a": true}}
		got, err := graph.FindNodesByNameScopedContext(t.Context(), wrapped, "Needle", scope)
		require.NoError(t, err)
		require.Len(t, got, 2, "synthetic global ownership retains its existing carve-out")
		require.Equal(t, 2, spy.rows)
		require.Zero(t, spy.legacyCalls)
		want, err := graph.FindNodesByNameScopedContext(t.Context(), legacyOutlineReader{wrapped}, "Needle", scope)
		require.NoError(t, err)
		require.Equal(t, want, got)

		spy.rows, spy.scopedCalls, spy.legacyCalls = 0, 0, 0
		eng := query.NewEngine(s).WithReader(wrapped)
		// An authoritative empty BM25 answer leaves the exact-name rescue as
		// the only ranked text lane. It must carry the same scope to SQL.
		eng.SetSearch(&searchSymbolsContextBackend{bundles: []search.SymbolBundle{}})
		eng.SetRerank(nil)
		candidates := eng.GatherSymbolCandidatesContext(t.Context(), "Needle", 1, query.QueryOptions{WorkspaceID: "ws", ProjectID: "project", RepoAllow: scope.RepoAllow, SkipVectorChannel: true}, nil)
		require.Len(t, candidates, 2)
		require.Equal(t, 1, spy.scopedCalls)
		require.Equal(t, 2, spy.rows)
		require.Zero(t, spy.legacyCalls)
		if pass == 0 {
			for i := range 5 {
				prefix := fmt.Sprintf("unrelated-%d", i)
				s.AddNode(&graph.Node{ID: prefix + "/needle", Name: "Needle", Kind: graph.KindFunction, RepoPrefix: prefix, WorkspaceID: prefix, FilePath: prefix + "/focus.go"})
			}
		}
	}
}

func TestContractCoreScopedExactNamesPreserveOverlayAndCancellation(t *testing.T) {
	s := openContractCoreCapStore(t)
	s.AddNode(&graph.Node{ID: "a/focus.go::Needle", Name: "Needle", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/focus.go"})
	layer := graph.NewOverlayLayer()
	layer.MarkFile("a/focus.go", true)
	wrapped := newContractCoreEdges(graph.NewOverlaidView(s, layer), t.Context(), nil)
	scope := graph.NodeScope{RepoAllow: map[string]bool{"a": true}}
	rows, err := graph.FindNodesByNameScopedContext(t.Context(), wrapped, "Needle", scope)
	require.NoError(t, err)
	require.Empty(t, rows, "scoped physical lookup must not resurrect an overlay tombstone")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = graph.FindNodesByNameScopedContext(ctx, wrapped, "Needle", scope)
	require.ErrorIs(t, err, context.Canceled)
}
