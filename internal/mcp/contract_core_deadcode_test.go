package mcp

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

type deadCodeFilteredSpy struct {
	graph.Reader
	backend  graph.FilteredScopedDeadCodeReader
	queries  *int
	allNodes *int
}

func (r *deadCodeFilteredSpy) AllNodes() []*graph.Node {
	(*r.allNodes)++
	return r.Reader.AllNodes()
}

func (r *deadCodeFilteredSpy) DeadCodeCandidatesInReposFiltered(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, repos []string, filter graph.OutlineEdgeFilter) ([]*graph.Node, error) {
	(*r.queries)++
	return r.backend.DeadCodeCandidatesInReposFiltered(kinds, incoming, repos, filter)
}
func (r *deadCodeFilteredSpy) IfaceImplementsRowsInReposFiltered(repos []string, filter graph.OutlineEdgeFilter) ([]graph.IfaceImplementsRow, error) {
	(*r.queries)++
	return r.backend.IfaceImplementsRowsInReposFiltered(repos, filter)
}
func (r *deadCodeFilteredSpy) BindReadContext(ctx context.Context) graph.Reader {
	bound := graph.BindReadContext(r.Reader, ctx)
	return &deadCodeFilteredSpy{Reader: bound, backend: bound.(graph.FilteredScopedDeadCodeReader), queries: r.queries, allNodes: r.allNodes}
}

func TestContractCoreScopedDeadCodePreservesMaskAndSelectedView(t *testing.T) {
	store := openContractCoreCapStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "a/dead", Name: "dead", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f.go", StartLine: 1, Language: "go"},
		{ID: "a/bean", Name: "bean", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f.go", StartLine: 2, Language: "go"},
		{ID: "a/contract", Name: "contractTarget", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f.go", StartLine: 3, Language: "go"},
		{ID: "a/live", Name: "live", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f.go", StartLine: 4, Language: "go"},
		{ID: "b/caller", Name: "caller", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/f.go", StartLine: 1, Language: "go"},
		{ID: "contract", Kind: graph.KindContract, RepoPrefix: "b"},
		{ID: "a/type", Name: "privateType", Kind: graph.KindType, RepoPrefix: "a", FilePath: "a/f.go", StartLine: 5, Language: "go"},
		{ID: "b/interface", Name: "foreignInterface", Kind: graph.KindInterface, RepoPrefix: "b", Meta: map[string]any{"methods": []string{"Work"}}},
	}, []*graph.Edge{
		{From: "b/caller", To: "a/live", Kind: graph.EdgeCalls},
		{From: "b/caller", To: "a/bean", Kind: graph.EdgeCalls, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "contract", To: "a/contract", Kind: graph.EdgeCalls},
		{From: "a/type", To: "b/interface", Kind: graph.EdgeImplements},
	})
	for _, ids := range []map[string]bool{nil, {"contract": true, "b/interface": true}, {}} {
		for _, repos := range [][]string{nil, {"a"}, {"b"}, {}} {
			ctx := withContractCoreReadErrors(t.Context())
			queries := 0
			allNodes := 0
			spy := &deadCodeFilteredSpy{Reader: store, backend: store, queries: &queries, allNodes: &allNodes}
			fast := graph.BindReadContext(newContractCoreEdges(spy, ctx, ids), ctx)
			// The old consumer sees no optional shortcuts and must walk the same
			// selected reader's filtered adjacency. It is independent of SQL predicates.
			slow := legacyOutlineReader{newContractCoreEdges(legacyOutlineReader{store}, ctx, ids)}
			opts := analysis.FindDeadCodeOptions{RepoPrefixes: repos}
			got := analysis.FindDeadCode(fast, nil, nil, opts)
			want := analysis.FindDeadCode(slow, nil, nil, opts)
			require.Equal(t, want, got, "scope=%v contract IDs=%v", repos, ids)
			if repos == nil || len(repos) > 0 {
				require.Positive(t, queries, "binding must keep indexed filtered queries")
			} else {
				require.Zero(t, queries, "explicit empty ownership does no query work")
			}
			require.Zero(t, allNodes, "nil scope is an indexed unscoped query, not an AllNodes fallback")
			if repos == nil || slices.Contains(repos, "a") {
				deadIDs := make([]string, 0, len(got))
				for _, entry := range got {
					deadIDs = append(deadIDs, entry.ID)
				}
				require.Contains(t, deadIDs, "a/dead")
				require.Contains(t, deadIDs, "a/bean", "masked incoming edge cannot keep a symbol alive")
				require.NotContains(t, deadIDs, "a/live", "a real foreign caller still proves use")
				if ids == nil || ids["contract"] {
					require.Contains(t, deadIDs, "a/contract", "masked contract endpoint cannot prove use")
				} else {
					require.NotContains(t, deadIDs, "a/contract", "an explicitly visible caller must remain authoritative")
				}
			}
			require.NoError(t, contractCoreReadError(ctx))
			// Also exercise a composed view that hides the backend capability. It
			// must use its own nodes rather than tunnelling to the physical store.
			fallback := newContractCoreEdges(legacyOutlineReader{store}, ctx, ids)
			require.Equal(t, want, analysis.FindDeadCode(fallback, nil, nil, opts))
		}
	}
}

func TestContractCoreScopedDeadCodeRetainsCancellationEvidence(t *testing.T) {
	store := openContractCoreCapStore(t)
	ctx, cancel := context.WithCancel(withContractCoreReadErrors(t.Context()))
	cancel()
	reader := graph.BindReadContext(newContractCoreEdges(store, ctx, nil), ctx).(graph.ScopedDeadCodeReader)
	require.Empty(t, reader.DeadCodeCandidatesInRepos([]graph.NodeKind{graph.KindFunction}, nil, []string{"a"}))
	require.True(t, errors.Is(contractCoreReadError(ctx), context.Canceled))
	require.Empty(t, reader.IfaceImplementsRowsInRepos([]string{"a"}))
	require.Empty(t, slices.Collect(reader.NodesByKindInRepos(graph.KindFile, []string{"a"})))
}

func TestContractCoreExactWorkspaceDeadCodeKeepsMasksAndFallback(t *testing.T) {
	store := openContractCoreCapStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "own/dead", Name: "privateDead", Kind: graph.KindFunction, WorkspaceID: "ws", RepoPrefix: "stale-owner", FilePath: "own.go", Language: "go"},
		{ID: "own/live", Name: "privateLive", Kind: graph.KindFunction, WorkspaceID: "ws", RepoPrefix: "a", FilePath: "own.go", Language: "go"},
		{ID: "own/masked", Name: "privateMasked", Kind: graph.KindFunction, WorkspaceID: "ws", RepoPrefix: "a", FilePath: "own.go", Language: "go"},
		{ID: "foreign/caller", Name: "caller", Kind: graph.KindFunction, WorkspaceID: "foreign", RepoPrefix: "b", FilePath: "foreign.go", Language: "go"},
	}, []*graph.Edge{{From: "foreign/caller", To: "own/live", Kind: graph.EdgeCalls}, {From: "foreign/caller", To: "own/masked", Kind: graph.EdgeMatches}})
	scope := graph.NodeScope{WorkspaceID: "ws"}
	for _, prefixes := range [][]string{nil, {"a"}, {}} {
		ctx := withContractCoreReadErrors(t.Context())
		fast := graph.BindReadContext(newContractCoreEdges(store, ctx, nil), ctx)
		fallback := graph.BindReadContext(newContractCoreEdges(legacyOutlineReader{store}, ctx, nil), ctx)
		opts := analysis.FindDeadCodeOptions{NodeScope: &scope, RepoPrefixes: prefixes}
		got := analysis.FindDeadCode(fast, nil, nil, opts)
		require.Equal(t, analysis.FindDeadCode(fallback, nil, nil, opts), got)
		ids := make([]string, 0, len(got))
		for _, e := range got {
			ids = append(ids, e.ID)
		}
		if prefixes == nil {
			require.ElementsMatch(t, []string{"own/dead", "own/masked"}, ids)
		}
		if len(prefixes) == 1 {
			require.Equal(t, []string{"own/masked"}, ids)
		}
		if prefixes != nil && len(prefixes) == 0 {
			require.Empty(t, got)
		}
		require.NoError(t, contractCoreReadError(ctx))
	}
}
