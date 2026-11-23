package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

func jsCallTargets(g *graph.Graph, from string) []string {
	var out []string
	for _, e := range g.GetOutEdges(from) {
		if e != nil && e.Kind == graph.EdgeCalls {
			out = append(out, e.To)
		}
	}
	sort.Strings(out)
	return out
}

func TestIncremental_LocalRequireEditsRebind(t *testing.T) {
	for _, ext := range []string{".js", ".ts"} {
		for _, mode := range []string{"IndexFile", "IncrementalReindexPaths"} {
			t.Run(mode+"/"+ext, func(t *testing.T) {
				root := t.TempDir()
				builderWriteTree(t, root, map[string]string{
					"src/v1" + ext:  "export function helper() { return 1 }\n",
					"src/v2" + ext:  "export function helper() { return 2 }\n",
					"src/app" + ext: "export function run() { const { helper } = require('./v1'); return helper() }\n",
				})

				g := graph.New()
				cfg := config.Default().Index
				cfg.Workers = 1
				idx := New(g, builderRegistry(), cfg, zap.NewNop())
				idx.SetRepoPrefix(builderRepoPrefix)
				_, err := idx.Index(root)
				require.NoError(t, err)

				appPath := filepath.Join(root, "src", "app"+ext)
				runID := builderRepoPrefix + "/src/app" + ext + "::run"
				v1Helper := builderRepoPrefix + "/src/v1" + ext + "::helper"
				v2Helper := builderRepoPrefix + "/src/v2" + ext + "::helper"
				require.Contains(t, jsCallTargets(g, runID), v1Helper)

				reparse := func(src string) {
					bumpMtime(t, appPath, src)
					if mode == "IndexFile" {
						require.NoError(t, idx.IndexFile(appPath))
					} else {
						_, err := idx.IncrementalReindexPaths(root, []string{appPath})
						require.NoError(t, err)
					}
				}

				// 1. Retarget require('./v1') -> require('./v2')
				reparse("export function run() { const { helper } = require('./v2'); return helper() }\n")
				require.NotContains(t, jsCallTargets(g, runID), v1Helper)
				require.Contains(t, jsCallTargets(g, runID), v2Helper)

				// 2. Swap local require -> top-level ES import of v1
				reparse("import { helper } from './v1'\nexport function run() { return helper() }\n")
				require.NotContains(t, jsCallTargets(g, runID), v2Helper)
				require.Contains(t, jsCallTargets(g, runID), v1Helper)

				// 3. Shadow with parameter
				reparse("import { helper } from './v1'\nexport function run(helper) { return helper() }\n")
				require.NotContains(t, jsCallTargets(g, runID), v1Helper)
				require.Contains(t, jsCallTargets(g, runID), graph.UnresolvedMarker+"helper")
			})
		}
	}
}

func TestIncremental_AddedLocalRequireResolvesAfterCrossPackageRefusal(t *testing.T) {
	for _, ext := range []string{".js", ".ts"} {
		for _, mode := range []string{"IndexFile", "IncrementalReindexPaths"} {
			t.Run(mode+"/"+ext, func(t *testing.T) {
				root := t.TempDir()
				builderWriteTree(t, root, map[string]string{
					"src/lib/lib" + ext: "export function helper() { return 1 }\n",
					"src/app/app" + ext: "export function run() { return helper() }\n",
				})

				g := graph.New()
				cfg := config.Default().Index
				cfg.Workers = 1
				idx := New(g, builderRegistry(), cfg, zap.NewNop())
				idx.SetRepoPrefix(builderRepoPrefix)
				_, err := idx.Index(root)
				require.NoError(t, err)

				appPath := filepath.Join(root, "src", "app", "app"+ext)
				runID := builderRepoPrefix + "/src/app/app" + ext + "::run"
				libHelper := builderRepoPrefix + "/src/lib/lib" + ext + "::helper"
				require.Contains(t, jsCallTargets(g, runID), graph.UnresolvedMarker+"helper")

				bumpMtime(t, appPath, "export function run() { const { helper } = require('../lib/lib'); return helper() }\n")
				if mode == "IndexFile" {
					require.NoError(t, idx.IndexFile(appPath))
				} else {
					_, err := idx.IncrementalReindexPaths(root, []string{appPath})
					require.NoError(t, err)
				}
				require.Contains(t, jsCallTargets(g, runID), libHelper)
				require.NotContains(t, jsCallTargets(g, runID), graph.UnresolvedMarker+"helper")
			})
		}
	}
}

func TestIncremental_DeletedLocalRequireDoesNotCarryPriorBindingOrMeta(t *testing.T) {
	added := resolutionMetaAdded(
		map[string]any{
			graph.JSLexicalBindingMeta:     graph.JSLexicalRequire,
			graph.JSLexicalRequireSpecMeta: "./libB",
			"resolution":                   "import_closure",
		},
		nil,
	)
	require.NotContains(t, added, graph.JSLexicalBindingMeta)
	require.NotContains(t, added, graph.JSLexicalRequireSpecMeta)
	require.Equal(t, "import_closure", added["resolution"])

	for _, ext := range []string{".js", ".ts"} {
		t.Run(ext, func(t *testing.T) {
			root := t.TempDir()
			// Poison stage.reuse[run, calls, "", "helper"] by having two unstamped
			// helper calls in run() with distinct targets (imported helper() ->
			// libA::helper and untyped method call x.helper() -> box::Box.helper).
			// That forces applyResolvedOutEdgesFromView to leave all helper calls
			// unresolved so stagePriorBindings / carry_prior_bindings genuinely
			// runs (carried > 0) and must refuse the deleted local require's prior
			// binding and metadata.
			beforeSrc := "import { helper } from './libA'\nexport function run(x) {\n  const a = helper()\n  const b = x.helper()\n  {\n    const { helper } = require('./libB')\n    return a + b + helper()\n  }\n}\n"
			afterSrc := "import { helper } from './libA'\nexport function run(x) {\n  const a = helper()\n  const b = x.helper()\n  {\n    // deleted local require\n    return a + b + helper()\n  }\n}\n"
			builderWriteTree(t, root, map[string]string{
				"src/libA" + ext: "export function helper() { return 1 }\n",
				"src/libB" + ext: "export function helper() { return 2 }\n",
				"src/box" + ext:  "export class Box { helper() { return 3 } }\n",
				"src/app" + ext:  beforeSrc,
			})

			indexCold := func(logger *zap.Logger) (*graph.Graph, *Indexer) {
				g := graph.New()
				cfg := config.Default().Index
				cfg.Workers = 1
				idx := New(g, builderRegistry(), cfg, zap.NewNop())
				idx.SetRepoPrefix(builderRepoPrefix)
				if logger != nil {
					idx.resolver.SetLogger(logger)
				}
				_, err := idx.Index(root)
				require.NoError(t, err)
				return g, idx
			}

			core, logs := observer.New(zap.InfoLevel)
			g, idx := indexCold(zap.New(core))
			logs.TakeAll()

			appPath := filepath.Join(root, "src", "app"+ext)
			runID := builderRepoPrefix + "/src/app" + ext + "::run"
			libAHelper := builderRepoPrefix + "/src/libA" + ext + "::helper"
			libBHelper := builderRepoPrefix + "/src/libB" + ext + "::helper"
			boxHelper := builderRepoPrefix + "/src/box" + ext + "::Box.helper"
			require.Contains(t, jsCallTargets(g, runID), libAHelper)
			require.Contains(t, jsCallTargets(g, runID), libBHelper)
			require.Contains(t, jsCallTargets(g, runID), boxHelper)

			bumpMtime(t, appPath, afterSrc)
			_, err := idx.IncrementalReindexPaths(root, []string{appPath})
			require.NoError(t, err)

			carried := 0
			for _, entry := range logs.FilterMessage("resolver: incremental phase complete").All() {
				if entry.ContextMap()["phase"] == "carry_prior_bindings" {
					carried += int(entry.ContextMap()["carried"].(int64))
				}
			}
			require.Positive(t, carried, "expected stagePriorBindings / carry_prior_bindings to run on this reparse")

			require.NotContains(t, jsCallTargets(g, runID), libBHelper)
			require.Contains(t, jsCallTargets(g, runID), libAHelper)
			require.Contains(t, jsCallTargets(g, runID), boxHelper)
			for _, e := range g.GetOutEdges(runID) {
				if e.Kind != graph.EdgeCalls {
					continue
				}
				require.NotEqual(t, libBHelper, e.To)
				require.NotContains(t, e.Meta, graph.JSLexicalBindingMeta)
				require.NotContains(t, e.Meta, graph.JSLexicalRequireSpecMeta)
			}

			fresh, _ := indexCold(nil)
			formatEdges := func(store *graph.Graph) []string {
				var rows []string
				for _, e := range store.GetOutEdges(runID) {
					if e.Kind == graph.EdgeCalls {
						rows = append(rows, fmt.Sprintf("%s@%d->%s(%v,%v)", e.Kind, e.Line, e.To, e.Meta[graph.JSLexicalBindingMeta], e.Meta[graph.JSLexicalRequireSpecMeta]))
					}
				}
				sort.Strings(rows)
				return rows
			}
			require.Equal(t, formatEdges(fresh), formatEdges(g))
		})
	}
}

func TestApplyResolvedOutEdges_SkipsJSLexicalBindingEdges(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "app.ts", Kind: graph.KindFile, FilePath: "app.ts", Language: "typescript"},
		{ID: "app.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "app.ts", Language: "typescript"},
		{ID: "v1.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "v1.ts", Language: "typescript"},
		{ID: "v1.ts::keep", Kind: graph.KindFunction, Name: "keep", FilePath: "v1.ts", Language: "typescript"},
	}, []*graph.Edge{
		{
			From: "app.ts::run", To: "v1.ts::helper", Kind: graph.EdgeCalls,
			FilePath: "app.ts", Line: 2, Origin: graph.OriginASTResolved, Confidence: 0.9,
			Meta: map[string]any{
				graph.JSLexicalBindingMeta:     graph.JSLexicalRequire,
				graph.JSLexicalRequireSpecMeta: "./v1",
			},
		},
		{
			From: "app.ts::run", To: "v1.ts::keep", Kind: graph.EdgeCalls,
			FilePath: "app.ts", Line: 3, Origin: graph.OriginASTResolved, Confidence: 0.9,
		},
	})

	reuse, _, _ := captureIncrementalState(g, "app.ts")
	require.NotContains(t, reuse, reuseKey{from: "app.ts::run", kind: graph.EdgeCalls, name: "helper"})
	require.Contains(t, reuse, reuseKey{from: "app.ts::run", kind: graph.EdgeCalls, name: "keep"})

	freshUnstamped := &graph.Edge{From: "app.ts::run", To: graph.UnresolvedMarker + "keep", Kind: graph.EdgeCalls, FilePath: "app.ts", Line: 3}
	freshRequire := &graph.Edge{
		From: "app.ts::run", To: graph.UnresolvedMarker + "keep", Kind: graph.EdgeCalls, FilePath: "app.ts", Line: 4,
		Meta: map[string]any{graph.JSLexicalBindingMeta: graph.JSLexicalRequire, graph.JSLexicalRequireSpecMeta: "./v2"},
	}
	freshParam := &graph.Edge{
		From: "app.ts::run", To: graph.UnresolvedMarker + "keep", Kind: graph.EdgeCalls, FilePath: "app.ts", Line: 5,
		Meta: map[string]any{graph.JSLexicalBindingMeta: graph.JSPendingParameter},
	}
	reused := applyResolvedOutEdges(g, []*graph.Edge{freshUnstamped, freshRequire, freshParam}, reuse, nil)
	require.Equal(t, 1, reused)
	require.Equal(t, "v1.ts::keep", freshUnstamped.To)
	require.Equal(t, graph.UnresolvedMarker+"keep", freshRequire.To)
	require.Equal(t, graph.UnresolvedMarker+"keep", freshParam.To)
}

func TestTrackedNpmLocalRequireSurvivesEvictionAndRebinds(t *testing.T) {
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
					{ID: "lib/index.ts", Kind: graph.KindFile, FilePath: "lib/index.ts", Language: "typescript", RepoPrefix: "lib"},
					{ID: "lib/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "lib/index.ts", Language: "typescript", RepoPrefix: "lib"},
				}
				g.AddBatch(nodes, []*graph.Edge{
					{
						From: "app/a.ts::run", To: "lib/index.ts::f", Kind: graph.EdgeCalls,
						FilePath: "app/a.ts", Line: 2, CrossRepo: true,
						Meta: map[string]any{
							graph.JSLexicalBindingMeta:      graph.JSLexicalRequire,
							graph.JSLexicalRequireSpecMeta:  "p",
							resolver.NpmImportSpecifierMeta: "p",
						},
					},
					{
						From: "app/a.ts::run", To: "lib/index.ts::f", Kind: graph.EdgeCrossRepoCalls,
						FilePath: "app/a.ts", Line: 2, CrossRepo: true,
					},
					{
						From: "other/b.ts::run", To: "lib/index.ts::f", Kind: graph.EdgeCalls,
						FilePath: "other/b.ts", Line: 2, CrossRepo: true,
						Meta: map[string]any{
							graph.JSLexicalBindingMeta:     graph.JSLexicalRequire,
							graph.JSLexicalRequireSpecMeta: "lib",
						},
					},
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
				var calls []*graph.Edge
				for _, e := range g.GetOutEdges("app/a.ts::run") {
					if e.Kind == graph.EdgeCrossRepoCalls {
						t.Fatalf("stale cross_repo_calls edge survived eviction: %+v", e)
					}
					if e.Kind == graph.EdgeCalls {
						calls = append(calls, e)
					}
				}
				require.Len(t, calls, 1)
				assert.Equal(t, graph.UnresolvedMarker+"f", calls[0].To)
				assert.False(t, calls[0].CrossRepo)
				assert.NotContains(t, calls[0].Meta, resolver.NpmImportSpecifierMeta)
				if mode == "replace" || mode == "untrack" {
					assert.Empty(t, g.GetOutEdges("other/b.ts::run"), "non-npm cross-repo require must not be parked on repo eviction")
				}
			})
		}
	}

	t.Run("TS local require consumer discovered without EdgeImports on provider track", func(t *testing.T) {
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
				writeWorkspaceFile(t, consumer, "app.ts", "export function run() { const { f } = require('p'); return f(); }\n")
				reg := newTestRegistry()
				reg.Register(languages.NewTypeScriptExtractor())
				mi := NewMultiIndexer(g, reg, search.NewNull(), newTestConfigManager(t), zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })

				// Track consumer FIRST so require('p') parks unresolved with zero EdgeImports,
				// then track provider so npmConsumerFilesForRepo must discover app/app.ts.
				_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "app", Path: consumer, Workspace: "shared"})
				require.NoError(t, err)
				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "lib", Path: provider, Workspace: "shared"})
				require.NoError(t, err)

				var fCalls []*graph.Edge
				for _, edge := range g.GetOutEdges("app/app.ts::run") {
					if edge.Kind == graph.EdgeCalls && edge.Meta[graph.JSLexicalBindingMeta] == graph.JSLexicalRequire {
						fCalls = append(fCalls, edge)
					}
				}
				require.Len(t, fCalls, 1)
				assert.Equal(t, "lib/index.ts::f", fCalls[0].To)
				assert.True(t, fCalls[0].CrossRepo)
				assert.Equal(t, "p", fCalls[0].Meta[resolver.NpmImportSpecifierMeta])
			})
		}
	})
}
