package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

type contextScopeBackend struct {
	searchSymbolsContextBackend
	nodes                        []*graph.Node
	scopedCalls, pathCalls, rows int
}

func (b *contextScopeBackend) scoped(repos, paths []string, limit int) []search.SymbolBundle {
	allow := make(map[string]bool, len(repos))
	for _, repo := range repos {
		allow[repo] = true
	}
	out := []search.SymbolBundle{}
	for _, n := range b.nodes {
		if !repoNarrowAdmits(allow, n.RepoPrefix) || len(paths) > 0 && !pathMatchesAnyPrefix(repoRelativePath(n), paths) {
			continue
		}
		out = append(out, search.SymbolBundle{Node: n, Score: 1})
		if len(out) >= limit {
			break
		}
	}
	b.rows += len(out)
	return out
}

func (b *contextScopeBackend) SearchSymbolBundlesScopedContext(_ context.Context, _ string, repos []string, limit int) []search.SymbolBundle {
	b.scopedCalls++
	return b.scoped(repos, nil, limit)
}

func (b *contextScopeBackend) SearchSymbolBundlesPathScopedContext(_ context.Context, _ string, repos, paths []string, limit int) ([]search.SymbolBundle, bool) {
	b.pathCalls++
	return b.scoped(repos, paths, limit), true
}

// A repo-scoped smart_context retrieves, ranks and expands only the scoped
// repository: every keyword search and the whole-task confidence query use
// the same scope, and the name lookups, caller walks and blast radius that
// follow read the same graph rows however many repositories share the
// symbol names and paths.
func TestSmartContextScopedNeverScansAndRowsStayFlat(t *testing.T) {
	for _, path := range []string{"", "pkg"} {
		t.Run("path="+path, func(t *testing.T) {
			var baseline string
			var baselineBundles int
			var baselineRows map[string]int
			for _, repositories := range []int{1, 6} {
				t.Run(fmt.Sprintf("repos=%d", repositories), func(t *testing.T) {
					store := openContractCoreCapStore(t)
					b := &contextScopeBackend{}
					for repo := 0; repo < repositories; repo++ {
						prefix := fmt.Sprintf("repo-%d", repo)
						var nodes []*graph.Node
						var edges []*graph.Edge
						for i := 0; i < 4; i++ {
							nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%s/pkg/p%d.go::parser%d", prefix, i, i), Name: fmt.Sprintf("parser%d", i), FilePath: fmt.Sprintf("%s/pkg/p%d.go", prefix, i), RepoPrefix: prefix, Kind: graph.KindFunction, Language: "go", StartLine: 3, EndLine: 9})
							if i > 0 {
								edges = append(edges, &graph.Edge{From: nodes[i-1].ID, To: nodes[i].ID, Kind: graph.EdgeCalls, FilePath: nodes[i-1].FilePath, Line: 5, Origin: graph.OriginASTResolved, Confidence: .9})
							}
						}
						edges = append(edges, &graph.Edge{From: nodes[3].ID, To: graph.UnresolvedMarker + "parser0", Kind: graph.EdgeCalls, FilePath: nodes[3].FilePath, Line: 7})
						require.NoError(t, store.AddBatchChecked(nodes, edges))
						b.nodes = append(b.nodes, nodes...)
					}
					counter := newScopedReadCounter(store)
					eng := query.NewEngine(counter)
					eng.SetSearch(b)
					eng.SetRerank(nil)
					srv := NewServer(eng, counter, nil, nil, zap.NewNop(), nil)
					installContractCoreKindTestRuntime(t, srv)
					// The whole-task confidence query must use the same scope as
					// each keyword search. More unrelated repos change neither.
					payload := scopedRowCall(t, srv, srv.handleSmartContext, t.Context(), "smart_context", map[string]any{"task": "how does parser work", "repo": "repo-0", "path": path})
					require.Zero(t, b.contextCalls.Load(), "unscoped bundle query")
					require.Zero(t, b.legacyBundleCalls.Load())
					require.Zero(t, b.legacySearchCalls.Load())
					require.Equal(t, 3, b.scopedCalls+b.pathCalls, "two keywords plus one confidence query")
					if path != "" {
						require.Equal(t, 3, b.pathCalls)
					}
					require.Positive(t, b.rows)
					require.Empty(t, counter.scans(), "a scoped request read the whole store")
					rows := counter.rows()
					require.NotEmpty(t, rows)
					if repositories == 1 {
						baseline, baselineBundles, baselineRows = payload, b.rows, rows
						return
					}
					require.JSONEq(t, baseline, payload, "unrelated repositories changed the scoped context")
					require.Equal(t, baselineBundles, b.rows, "unrelated repositories increased returned bundle rows")
					require.Equal(t, baselineRows, rows, "unrelated repositories changed the graph rows a scoped request read")
				})
			}
		})
	}
}
