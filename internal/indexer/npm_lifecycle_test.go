package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestTrackedNpmImportsSurviveEviction(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"file", "batch", "replace", "untrack"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					g = db
				}
				nodes := []*graph.Node{
					{ID: "lib/index.ts", Kind: graph.KindFile, FilePath: "lib/index.ts", RepoPrefix: "lib"},
					{ID: "lib/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "lib/index.ts", RepoPrefix: "lib"},
				}
				g.AddBatch(nodes, []*graph.Edge{
					{From: "app/a.ts", To: "lib/index.ts", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 1, CrossRepo: true, Meta: map[string]any{"npm_import": "p"}},
					{From: "app/a.ts", To: "lib/index.ts::f", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 1, CrossRepo: true, Meta: map[string]any{"npm_import": "p::f"}},
					{From: "app/a.ts", To: "lib/index.ts", Kind: graph.EdgeImports, FilePath: "app/a.ts", Line: 2},
				})
				switch mode {
				case "file":
					idx := New(g, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
					assert.Equal(t, []string{"app/a.ts"}, idx.restubIncomingRefs("lib/index.ts"))
					g.EvictFile("lib/index.ts")
				case "batch":
					stages := []*incrementalBatchStage{{graphPath: "lib/index.ts", priorNodes: nodes}}
					_, consumers := restubIncomingRefsFromView(g, stages, loadIncrementalPriorView(g, stages))
					assert.Equal(t, []string{"app/a.ts"}, consumers)
					g.EvictFile("lib/index.ts")
				case "replace":
					evictRepoCurrentGeneration(g, "lib")
				case "untrack":
					mi := &MultiIndexer{graph: g}
					_, _, err := mi.purgeRepositoryPayload("lib", nil)
					require.NoError(t, err)
				}
				var targets []string
				for _, e := range g.GetOutEdges("app/a.ts") {
					require.Equal(t, 1, e.Line, "unmarked relative import must still be removed")
					targets = append(targets, e.To)
					assert.False(t, e.CrossRepo)
				}
				assert.ElementsMatch(t, []string{"external::p", "external::p::f"}, targets)
			})
		}
	}
}

func TestTrackedNpmRepoParkingIsScoped(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(map[bool]string{false: "not npm provider", true: "npm provider"}[marked], func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "lib/index.ts::f", Kind: graph.KindFunction, Name: "f", RepoPrefix: "lib"})
			meta := map[string]any{}
			if marked {
				meta["npm_import"] = "p::f"
			}
			g.AddEdge(&graph.Edge{From: "app/a.ts", To: "lib/index.ts::f", Kind: graph.EdgeImports, FilePath: "app/a.ts", Meta: meta})
			g.AddEdge(&graph.Edge{From: "app/a.ts::run", To: "lib/index.ts::f", Kind: graph.EdgeCalls, FilePath: "app/a.ts", CrossRepo: true})
			g.AddEdge(&graph.Edge{From: "other/a.ts::run", To: "lib/index.ts::f", Kind: graph.EdgeCalls, FilePath: "other/a.ts", CrossRepo: true})
			g.AddEdge(&graph.Edge{From: "app/a.ts::run", To: "lib/index.ts::f", Kind: graph.EdgeReferences, FilePath: "app/a.ts", CrossRepo: true})
			evictRepoCurrentGeneration(g, "lib")
			assert.Empty(t, g.GetOutEdges("other/a.ts::run"))
			calls := g.GetOutEdges("app/a.ts::run")
			if marked {
				require.Len(t, calls, 1)
				assert.Equal(t, graph.EdgeCalls, calls[0].Kind)
				assert.Equal(t, "unresolved::f", calls[0].To)
				assert.False(t, calls[0].CrossRepo)
			} else {
				assert.Empty(t, calls)
			}
		})
	}
}

type npmRestubCountingStore struct {
	*graph.Graph
	incomingReads int
}

func (g *npmRestubCountingStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	g.incomingReads++
	return g.Graph.GetInEdgesByNodeIDs(ids)
}

func TestNonNpmEvictionDoesNotHydrateIncoming(t *testing.T) {
	for _, withImport := range []bool{false, true} {
		t.Run(map[bool]string{false: "calls only", true: "unmarked import"}[withImport], func(t *testing.T) {
			g := &npmRestubCountingStore{Graph: graph.New()}
			g.AddBatch([]*graph.Node{
				{ID: "lib/a.go", Kind: graph.KindFile, RepoPrefix: "lib"},
				{ID: "lib/a.go::f", Kind: graph.KindFunction, Name: "f", RepoPrefix: "lib"},
			}, []*graph.Edge{
				{From: "lib/a.go", To: "lib/a.go::f", Kind: graph.EdgeCalls},
				{From: "other/a.go::run", To: "lib/a.go::f", Kind: graph.EdgeCalls, CrossRepo: true},
			})
			if withImport {
				g.AddEdge(&graph.Edge{From: "other/a.go", To: "lib/a.go", Kind: graph.EdgeImports, CrossRepo: true})
			}
			evictRepoCurrentGeneration(g, "lib")
			assert.Zero(t, g.incomingReads, "non-npm eviction must not hydrate incoming adjacency")
			assert.Empty(t, g.GetOutEdges("other/a.go::run"))
		})
	}
}

func TestTrackedNpmConsumersResolveWithoutRelevantReceipt(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var g graph.Store = graph.New()
			if backend == "sqlite" {
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				g = db
			}
			provider, consumer := t.TempDir(), t.TempDir()
			writePackageJSON(t, provider, `{"name":"p","exports":"./index.ts"}`)
			writeWorkspaceFile(t, provider, "index.ts", "export function f() { return 42; }\n")
			writePackageJSON(t, consumer, `{"name":"app","dependencies":{"p":"^1"}}`)
			writeWorkspaceFile(t, consumer, "app.ts", "import { f } from 'p'; export function run() { return f(); }\n")
			reg := newTestRegistry()
			reg.Register(languages.NewTypeScriptExtractor())
			mi := NewMultiIndexer(g, reg, search.NewNull(), newTestConfigManager(t), zap.NewNop())
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			for _, entry := range []config.RepoEntry{
				{Name: "lib", Path: provider, Workspace: "shared"},
				{Name: "app", Path: consumer, Workspace: "shared"},
			} {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			call := func() *graph.Edge {
				t.Helper()
				var calls []*graph.Edge
				for _, edge := range g.GetOutEdges("app/app.ts::run") {
					if edge.Kind == graph.EdgeCalls {
						calls = append(calls, edge)
					}
				}
				require.Len(t, calls, 1)
				return calls[0]
			}
			require.Equal(t, "lib/index.ts::f", call().To)
			consumers := restubTrackedNpmImportsFromView(g, nil,
				g.GetInEdgesByNodeIDs([]string{"lib/index.ts", "lib/index.ts::f"}))
			require.Equal(t, []string{"app/app.ts"}, consumers)
			require.Equal(t, "unresolved::f", call().To)

			// The receipt describes provider-local work. Parked consumers still
			// need their own cross-repo pass when that receipt needs no resolve.
			result := &IndexResult{}
			receipt := &graph.MutationReceipt{Complete: true}
			_, needed, exact := incrementalResolutionFrontier(result, receipt)
			require.False(t, needed)
			require.True(t, exact)
			mi.resolveIncrementalRepoMutation("lib", result, receipt,
				&reparsePendingEnrichmentBatch{npmConsumerFiles: consumers})

			assert.Equal(t, "lib/index.ts::f", call().To)
			assert.True(t, call().CrossRepo)
			var targets []string
			for _, edge := range g.GetOutEdges("app/app.ts") {
				if edge.Kind == graph.EdgeImports {
					targets = append(targets, edge.To)
					assert.True(t, edge.CrossRepo)
				}
			}
			assert.ElementsMatch(t, []string{"lib/index.ts", "lib/index.ts::f"}, targets)
		})
	}
}
