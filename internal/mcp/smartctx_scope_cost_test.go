package mcp

import (
	"context"
	"fmt"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

type contextScopeReader struct {
	*graph.Graph
	allNodes, allEdges int
}

func (r *contextScopeReader) AllNodes() []*graph.Node {
	r.allNodes++
	return r.Graph.AllNodes()
}

func (r *contextScopeReader) AllEdges() []*graph.Edge {
	r.allEdges++
	return r.Graph.AllEdges()
}

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

func TestSmartContextRetrievalScopeCost(t *testing.T) {
	for _, path := range []string{"", "pkg"} {
		t.Run("path="+path, func(t *testing.T) {
			var baselineRows int
			for _, repositories := range []int{1, 6} {
				t.Run(fmt.Sprintf("repos=%d", repositories), func(t *testing.T) {
					r := &contextScopeReader{Graph: graph.New()}
					b := &contextScopeBackend{}
					for repo := 0; repo < repositories; repo++ {
						prefix := fmt.Sprintf("repo-%d", repo)
						for i := 0; i < 4; i++ {
							n := &graph.Node{ID: fmt.Sprintf("%s/pkg/p%d.go::parser%d", prefix, i, i), Name: fmt.Sprintf("parser%d", i), FilePath: fmt.Sprintf("%s/pkg/p%d.go", prefix, i), RepoPrefix: prefix, Kind: graph.KindFunction}
							r.AddNode(n)
							b.nodes = append(b.nodes, n)
						}
					}
					eng := query.NewEngine(r)
					eng.SetSearch(b)
					eng.SetRerank(nil)
					srv := NewServer(eng, r, nil, nil, zap.NewNop(), nil)
					req := mcplib.CallToolRequest{}
					req.Params.Name = "smart_context"
					// The whole-task confidence query must use the same scope as
					// each keyword search. More unrelated repos change neither.
					req.Params.Arguments = map[string]any{"task": "how does parser work", "repo": "repo-0", "path": path}
					res, err := srv.handleSmartContext(context.Background(), req)
					require.NoError(t, err)
					require.False(t, res.IsError)
					require.Zero(t, b.contextCalls.Load(), "unscoped bundle query")
					require.Zero(t, b.legacyBundleCalls.Load())
					require.Zero(t, b.legacySearchCalls.Load())
					require.Equal(t, 3, b.scopedCalls+b.pathCalls, "two keywords plus one confidence query")
					if path != "" {
						require.Equal(t, 3, b.pathCalls)
					}
					require.Positive(t, b.rows)
					if repositories == 1 {
						baselineRows = b.rows
					} else {
						require.Equal(t, baselineRows, b.rows, "adding unrelated repositories increased returned bundle rows")
					}
					require.Zero(t, r.allNodes)
					require.Zero(t, r.allEdges)
				})
			}
		})
	}
}
