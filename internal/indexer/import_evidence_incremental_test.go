package indexer

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
				assert.Empty(t, calls[0].Origin)
				assert.NotContains(t, calls[0].Meta, resolver.NpmImportSpecifierMeta)
				assert.NotContains(t, calls[0].Meta, "resolution")
				assert.False(t, graph.HasRestubProvenance(calls[0]))
				if mode == "replace" || mode == "untrack" {
					assert.Empty(t, g.GetOutEdges("other/b.ts::run"), "non-npm cross-repo require must not be parked on repo eviction")
				}
			})
		}
	}

	t.Run("mixed aliased ES import and local require park with distinct names and clean meta", func(t *testing.T) {
		for _, backend := range []string{"memory", "sqlite"} {
			t.Run(backend, func(t *testing.T) {
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
						From: "app/a.ts", To: "lib/index.ts::f", Kind: graph.EdgeImports,
						FilePath: "app/a.ts", Line: 1, CrossRepo: true, Alias: "aliased",
						Meta: map[string]any{resolver.NpmImportSpecifierMeta: "p::f", "import_alias": "aliased"},
					},
					{
						From: "app/a.ts::viaES", To: "lib/index.ts::f", Kind: graph.EdgeCalls,
						FilePath: "app/a.ts", Line: 2, CrossRepo: true, Origin: graph.OriginASTResolved, Confidence: 0.9,
						Meta: map[string]any{"resolution": "import_binding"},
					},
					{
						From: "app/a.ts::viaReq", To: "lib/index.ts::f", Kind: graph.EdgeCalls,
						FilePath: "app/a.ts", Line: 3, CrossRepo: true, Origin: graph.OriginASTResolved, Confidence: 0.9,
						Meta: map[string]any{
							graph.JSLexicalBindingMeta:      graph.JSLexicalRequire,
							graph.JSLexicalRequireSpecMeta:  "p",
							resolver.NpmImportSpecifierMeta: "p",
							"resolution":                    "import_closure",
						},
					},
				})
				consumers := restubTrackedNpmImportsFromView(g, nil, g.GetInEdgesByNodeIDs([]string{"lib/index.ts", "lib/index.ts::f"}))
				assert.Equal(t, []string{"app/a.ts"}, consumers)

				esCalls := g.GetOutEdges("app/a.ts::viaES")
				require.Len(t, esCalls, 1)
				assert.Equal(t, graph.UnresolvedMarker+"aliased", esCalls[0].To)
				assert.True(t, graph.HasRestubProvenance(esCalls[0]))
				assert.Contains(t, esCalls[0].Meta, "resolution")

				reqCalls := g.GetOutEdges("app/a.ts::viaReq")
				require.Len(t, reqCalls, 1)
				assert.Equal(t, graph.UnresolvedMarker+"f", reqCalls[0].To)
				assert.False(t, graph.HasRestubProvenance(reqCalls[0]))
				assert.NotContains(t, reqCalls[0].Meta, "resolution")
				assert.NotContains(t, reqCalls[0].Meta, resolver.NpmImportSpecifierMeta)
			})
		}
	})

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

	t.Run("TrackRepoCtx for competing or higher-priority provider discovers already-resolved require consumers", func(t *testing.T) {
		for _, ext := range []string{".js", ".ts"} {
			for _, backend := range []string{"memory", "sqlite"} {
				t.Run(ext+"/"+backend, func(t *testing.T) {
					var g graph.Store = graph.New()
					if backend == "sqlite" {
						db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, db.Close()) })
						g = db
					}
					provA, provB, provC, consumer := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
					writePackageJSON(t, provA, `{"name":"p","exports":"./index`+ext+`"}`)
					writeWorkspaceFile(t, provA, "index"+ext, "export function f() { return 1; }\n")
					writePackageJSON(t, provB, `{"name":"p","exports":"./index`+ext+`"}`)
					writeWorkspaceFile(t, provB, "index"+ext, "export function f() { return 2; }\n")
					writePackageJSON(t, provC, `{"name":"p","exports":"./index`+ext+`"}`)
					writeWorkspaceFile(t, provC, "index"+ext, "export function f() { return 3; }\n")
					writePackageJSON(t, consumer, `{"name":"app","dependencies":{"p":"^1"}}`)
					writeWorkspaceFile(t, consumer, ".gortex.yaml", "workspace: wsB\ncross_workspace_deps:\n  - workspace: wsA\n    modules: [p]\n    mode: read-only\n")
					writeWorkspaceFile(t, consumer, "app"+ext, "export function run() { const { f } = require('p'); return f(); }\n")

					reg := newTestRegistry()
					reg.Register(languages.NewJavaScriptExtractor())
					reg.Register(languages.NewTypeScriptExtractor())
					mi := NewMultiIndexer(g, reg, search.NewNull(), newTestConfigManager(t), zap.NewNop())
					t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })

					reqCall := func() *graph.Edge {
						t.Helper()
						var found []*graph.Edge
						for _, edge := range g.GetOutEdges("app/app" + ext + "::run") {
							if edge.Kind == graph.EdgeCalls && edge.Meta[graph.JSLexicalBindingMeta] == graph.JSLexicalRequire {
								found = append(found, edge)
							}
						}
						require.Len(t, found, 1)
						return found[0]
					}

					// 1. Track foreign provA + consumer in wsB -> binds to provA.
					_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "provA", Path: provA, Workspace: "wsA"})
					require.NoError(t, err)
					_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "app", Path: consumer, Workspace: "wsB"})
					require.NoError(t, err)
					require.Equal(t, "provA/index"+ext+"::f", reqCall().To)

					// 2. Track higher-priority same-workspace provB -> npmConsumerFilesForRepo discovers
					// already-resolved app/app.{js,ts} and retargets cleanly to provB.
					_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "provB", Path: provB, Workspace: "wsB"})
					require.NoError(t, err)
					require.Equal(t, "provB/index"+ext+"::f", reqCall().To)
					require.True(t, reqCall().CrossRepo)

					// 3. Track competing provC in wsB -> ambiguity restubs to unresolved::f.
					_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "provC", Path: provC, Workspace: "wsB"})
					require.NoError(t, err)
					c := reqCall()
					require.Equal(t, graph.UnresolvedMarker+"f", c.To)
					require.False(t, c.CrossRepo)
					require.NotContains(t, c.Meta, resolver.NpmImportSpecifierMeta)
					require.NotContains(t, c.Meta, "resolution")
					require.False(t, graph.HasRestubProvenance(c))
				})
			}
		}
	})

	t.Run("TrackRepoCtx for competing or higher-priority provider discovers already-resolved ES import consumers", func(t *testing.T) {
		for _, backend := range []string{"memory", "sqlite"} {
			t.Run(backend, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					g = db
				}
				provA, provB, provC, consumer := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
				writePackageJSON(t, provA, `{"name":"p","exports":"./index.ts"}`)
				writeWorkspaceFile(t, provA, "index.ts", "export function f() { return 1; }\n")
				writePackageJSON(t, provB, `{"name":"p","exports":"./index.ts"}`)
				writeWorkspaceFile(t, provB, "index.ts", "export function f() { return 2; }\n")
				writePackageJSON(t, provC, `{"name":"p","exports":"./index.ts"}`)
				writeWorkspaceFile(t, provC, "index.ts", "export function f() { return 3; }\n")
				writePackageJSON(t, consumer, `{"name":"app","dependencies":{"p":"^1"}}`)
				writeWorkspaceFile(t, consumer, ".gortex.yaml", "workspace: wsB\ncross_workspace_deps:\n  - workspace: wsA\n    modules: [p]\n    mode: read-only\n")
				writeWorkspaceFile(t, consumer, "app.ts", "import { f } from 'p'\nexport function run() { return f(); }\n")

				reg := newTestRegistry()
				reg.Register(languages.NewTypeScriptExtractor())
				mi := NewMultiIndexer(g, reg, search.NewNull(), newTestConfigManager(t), zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })

				esCall := func() *graph.Edge {
					t.Helper()
					var found []*graph.Edge
					for _, edge := range g.GetOutEdges("app/app.ts::run") {
						if edge.Kind == graph.EdgeCalls {
							found = append(found, edge)
						}
					}
					require.Len(t, found, 1)
					return found[0]
				}

				_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "provA", Path: provA, Workspace: "wsA"})
				require.NoError(t, err)
				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "app", Path: consumer, Workspace: "wsB"})
				require.NoError(t, err)
				require.Equal(t, "provA/index.ts::f", esCall().To)

				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "provB", Path: provB, Workspace: "wsB"})
				require.NoError(t, err)
				require.Equal(t, "provB/index.ts::f", esCall().To)
				require.True(t, esCall().CrossRepo)

				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "provC", Path: provC, Workspace: "wsB"})
				require.NoError(t, err)
				c := esCall()
				require.Equal(t, graph.UnresolvedMarker+"f", c.To)
				require.False(t, c.CrossRepo)
			})
		}
	})

	t.Run("npmConsumerFilesForRepo skips cross-repo edge hydration for non-provider repos", func(t *testing.T) {
		for _, backend := range []string{"memory", "sqlite"} {
			t.Run(backend, func(t *testing.T) {
				var base graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					base = db
				}
				counting := &candidateCountingStore{Store: base}
				prov, consumer, goRepo, unnamedJS := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
				writePackageJSON(t, prov, `{"name":"p","exports":"./index.ts"}`)
				writeWorkspaceFile(t, prov, "index.ts", "export function f() { return 1; }\n")
				writePackageJSON(t, consumer, `{"name":"app","dependencies":{"p":"^1"}}`)
				writeWorkspaceFile(t, consumer, "app.ts", "import { f } from 'p'\nexport function viaES() { return f(); }\nexport function viaReq() { const { f } = require('p'); return f(); }\n")
				writeWorkspaceFile(t, goRepo, "go.mod", "module example.com/g\n\ngo 1.23\n")
				writeWorkspaceFile(t, goRepo, "main.go", "package main\nfunc main() {}\n")
				writePackageJSON(t, unnamedJS, `{"private":true}`)
				writeWorkspaceFile(t, unnamedJS, "index.js", "export function noop() { return 0 }\n")

				reg := newTestRegistry()
				reg.Register(languages.NewJavaScriptExtractor())
				reg.Register(languages.NewTypeScriptExtractor())
				reg.Register(languages.NewGoExtractor())
				mi := NewMultiIndexer(counting, reg, search.NewNull(), newTestConfigManager(t), zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })

				_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "prov", Path: prov, Workspace: "shared"})
				require.NoError(t, err)
				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "app", Path: consumer, Workspace: "shared"})
				require.NoError(t, err)
				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "gorepo", Path: goRepo, Workspace: "shared"})
				require.NoError(t, err)
				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "unnamed", Path: unnamedJS, Workspace: "shared"})
				require.NoError(t, err)

				// Neither gorepo nor unnamed declares any npm package; scanning
				// consumers for them must perform zero GetEdgeCandidates hydrations.
				counting.candidateCalls = 0
				assert.Empty(t, mi.npmConsumerFilesForRepo("gorepo"))
				assert.Empty(t, mi.npmConsumerFilesForRepo("unnamed"))
				assert.Zero(t, counting.candidateCalls, "non-provider repos must not hydrate workspace cross-repo JS/TS edges")

				// Provider repo DOES hydrate and find app/app.ts.
				counting.candidateCalls = 0
				assert.Equal(t, []string{"app/app.ts"}, mi.npmConsumerFilesForRepo("prov"))
				assert.Positive(t, counting.candidateCalls)
			})
		}
	})
}

type candidateCountingStore struct {
	graph.Store
	candidateCalls int
}

func (s *candidateCountingStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	s.candidateCalls++
	return s.Store.GetEdgeCandidates(endpoints, sites)
}

type siteKindCountingStore struct {
	graph.Store
	repoNodeScans int
	importSites   int
	callSites     int
	seenCallSites []graph.EdgeSite
}

func (s *siteKindCountingStore) RepoNodeIdentitiesSeq(repos []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	s.repoNodeScans++
	return graph.RepoNodeIdentitiesSeq(s.Store, repos, kinds...)
}

func (s *siteKindCountingStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	for _, site := range sites {
		switch site.Kind {
		case graph.EdgeImports:
			s.importSites++
		case graph.EdgeCalls:
			s.callSites++
			s.seenCallSites = append(s.seenCallSites, site)
		}
	}
	return s.Store.GetEdgeCandidates(endpoints, sites)
}

func TestNpmConsumerFilesForRepoSkipsCrossRepoCallScanForSoleProvider(t *testing.T) {
	t.Run("sole provider skips unrelated hydrations and finds monorepo competing provider", func(t *testing.T) {
		for _, backend := range []string{"memory", "sqlite"} {
			t.Run(backend, func(t *testing.T) {
				var base graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					base = db
				}
				counting := &siteKindCountingStore{Store: base}
				otherProv, otherApp, prov, app, monoA, provB := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
				writePackageJSON(t, otherProv, `{"name":"other-pkg","exports":"./index.ts"}`)
				writeWorkspaceFile(t, otherProv, "index.ts", "export function g() { return 1 }\n")
				writePackageJSON(t, otherApp, `{"name":"other-app","dependencies":{"other-pkg":"^1"}}`)
				writeWorkspaceFile(t, otherApp, "main.ts", "import { g } from 'other-pkg'\nexport function run() { return g() }\nexport function reqRun() { const { g } = require('other-pkg'); return g() }\n")

				writePackageJSON(t, prov, `{"name":"sole-pkg","exports":"./index.ts"}`)
				writeWorkspaceFile(t, prov, "index.ts", "export function f() { return 2 }\n")
				writePackageJSON(t, app, `{"name":"app","dependencies":{"sole-pkg":"^1"}}`)
				writeWorkspaceFile(t, app, "main.ts", "export function run() { const { f } = require('sole-pkg'); return f() }\n")

				// Monorepo workspace member providing competing package "comp-pkg".
				writePackageJSON(t, monoA, `{"private":true,"workspaces":["packages/*"]}`)
				writePackageJSON(t, filepath.Join(monoA, "packages", "pkg-p"), `{"name":"comp-pkg","exports":"./index.ts"}`)
				writeWorkspaceFile(t, monoA, "packages/pkg-p/index.ts", "export function h() { return 3 }\n")
				writePackageJSON(t, provB, `{"name":"comp-pkg","exports":"./index.ts"}`)
				writeWorkspaceFile(t, provB, "index.ts", "export function h() { return 4 }\n")
				compApp := t.TempDir()
				writePackageJSON(t, compApp, `{"name":"comp-app","dependencies":{"comp-pkg":"^1"}}`)
				writeWorkspaceFile(t, compApp, "main.ts", "export function run() { const { h } = require('comp-pkg'); return h() }\n")

				reg := newTestRegistry()
				reg.Register(languages.NewJavaScriptExtractor())
				reg.Register(languages.NewTypeScriptExtractor())
				mi := NewMultiIndexer(counting, reg, search.NewNull(), newTestConfigManager(t), zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })

				_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "otherProv", Path: otherProv, Workspace: "ws"})
				require.NoError(t, err)
				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "otherApp", Path: otherApp, Workspace: "ws"})
				require.NoError(t, err)

				// Initial track of sole provider prov: otherApp targets otherProv (NOT prov and NOT competing),
				// so npmConsumerFilesForRepo("prov") must do ZERO EdgeImports & EdgeCalls hydrations.
				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "prov", Path: prov, Workspace: "ws"})
				require.NoError(t, err)
				counting.importSites, counting.callSites, counting.seenCallSites = 0, 0, nil
				assert.Empty(t, mi.npmConsumerFilesForRepo("prov"))
				assert.Zero(t, counting.importSites, "sole provider must not hydrate unrelated EdgeImports")
				assert.Zero(t, counting.callSites, "sole provider must not hydrate unrelated EdgeCalls: %+v", counting.seenCallSites)

				// Track app (already-resolved TS require consumer into sole provider prov): must be discovered.
				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "app", Path: app, Workspace: "ws"})
				require.NoError(t, err)
				assert.Equal(t, []string{"app/main.ts"}, mi.npmConsumerFilesForRepo("prov"))

				// Monorepo competing provider discovery: monoA/packages/pkg-p vs provB.
				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "monoA", Path: monoA, Workspace: "ws"})
				require.NoError(t, err)
				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "compApp", Path: compApp, Workspace: "ws"})
				require.NoError(t, err)
				_, err = mi.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "provB", Path: provB, Workspace: "ws"})
				require.NoError(t, err)
				assert.Contains(t, mi.npmConsumerFilesForRepo("provB"), "compApp/main.ts",
					"competing provider must discover consumer resolved to monorepo workspace member")
			})
		}
	})

	t.Run("barrel discovery across all track orders and chained tracked-npm barrels", func(t *testing.T) {
		orders := []struct {
			name  string
			order []string
		}{
			{name: "prov_shared_app", order: []string{"prov", "shared", "app"}},
			{name: "shared_app_prov", order: []string{"shared", "app", "prov"}},
			{name: "app_shared_prov", order: []string{"app", "shared", "prov"}},
			{name: "prov_app_shared", order: []string{"prov", "app", "shared"}},
		}
		for _, ord := range orders {
			for _, backend := range []string{"memory", "sqlite"} {
				t.Run(ord.name+"/"+backend, func(t *testing.T) {
					var g graph.Store = graph.New()
					if backend == "sqlite" {
						db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, db.Close()) })
						g = db
					}
					prov, shared, app := t.TempDir(), t.TempDir(), t.TempDir()
					writePackageJSON(t, prov, `{"name":"p","exports":"./index.js"}`)
					writeWorkspaceFile(t, prov, "index.js", "export function foo() { return 42 }\n")

					writePackageJSON(t, shared, `{"name":"@acme/shared","exports":"./index.js","dependencies":{"p":"^1"}}`)
					writeWorkspaceFile(t, shared, "index.js", "export { foo } from 'p'\n")

					writePackageJSON(t, app, `{"name":"app","dependencies":{"@acme/shared":"^1","p":"^1"}}`)
					writeWorkspaceFile(t, app, "local_barrel.js", "module.exports = require('p')\n")
					writeWorkspaceFile(t, app, "main.js", strings.Join([]string{
						"export function viaChained() { const { foo } = require('@acme/shared'); return foo() }",
						"export function viaLocal() { const { foo } = require('./local_barrel'); return foo() }",
					}, "\n")+"\n")

					entries := map[string]config.RepoEntry{
						"prov":   {Name: "prov", Path: prov, Workspace: "ws"},
						"shared": {Name: "shared", Path: shared, Workspace: "ws"},
						"app":    {Name: "app", Path: app, Workspace: "ws"},
					}

					reg := newTestRegistry()
					reg.Register(languages.NewJavaScriptExtractor())
					reg.Register(languages.NewTypeScriptExtractor())
					mi := NewMultiIndexer(g, reg, search.NewNull(), newTestConfigManager(t), zap.NewNop())
					t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })

					for _, name := range ord.order {
						_, err := mi.TrackRepoCtx(t.Context(), entries[name])
						require.NoError(t, err)
					}

					assertCall := func(fnID, wantTo string) {
						t.Helper()
						var found *graph.Edge
						for _, e := range g.GetOutEdges(fnID) {
							if e != nil && e.Kind == graph.EdgeCalls && e.To == wantTo {
								found = e
								break
							}
						}
						require.NotNil(t, found, "%s must bind to %s (order=%s)", fnID, wantTo, ord.name)
						require.Equal(t, "p", found.Meta[resolver.NpmImportSpecifierMeta], "innermost package spec must be stamped")
					}
					assertCall("app/main.js::viaChained", "prov/index.js::foo")
					assertCall("app/main.js::viaLocal", "prov/index.js::foo")

					// Untrack prov -> calls restub instead of disappearing -> re-track prov -> re-links cleanly!
					_, _ = mi.UntrackRepo("prov")
					for _, fnID := range []string{"app/main.js::viaChained", "app/main.js::viaLocal"} {
						var hasUnresolved bool
						for _, e := range g.GetOutEdges(fnID) {
							if e != nil && e.Kind == graph.EdgeCalls && e.To == graph.UnresolvedMarker+"foo" {
								hasUnresolved = true
							}
						}
						require.True(t, hasUnresolved, "%s must restub on provider untrack", fnID)
					}
					_, err := mi.TrackRepoCtx(t.Context(), entries["prov"])
					require.NoError(t, err)
					assertCall("app/main.js::viaChained", "prov/index.js::foo")
					assertCall("app/main.js::viaLocal", "prov/index.js::foo")
				})
			}
		}
	})
}

func TestRestubTrackedNpmRepoIncomingSkipsCallsForMarkedTargets(t *testing.T) {
	t.Run("ES-import-only provider untrack does 1 scan and 0 EdgeCalls hydrations", func(t *testing.T) {
		g := graph.New()
		g.AddBatch([]*graph.Node{
			{ID: "prov/index.ts", Kind: graph.KindFile, Name: "index.ts", FilePath: "prov/index.ts", Language: "typescript", RepoPrefix: "prov"},
			{ID: "prov/index.ts::foo", Kind: graph.KindFunction, Name: "foo", FilePath: "prov/index.ts", Language: "typescript", RepoPrefix: "prov"},
			{ID: "prov/util.go", Kind: graph.KindFile, Name: "util.go", FilePath: "prov/util.go", Language: "go", RepoPrefix: "prov"},
			{ID: "prov/util.go::GoFn", Kind: graph.KindFunction, Name: "GoFn", FilePath: "prov/util.go", Language: "go", RepoPrefix: "prov"},
			{ID: "app/main.ts", Kind: graph.KindFile, Name: "main.ts", FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app"},
			{ID: "app/main.ts::viaES", Kind: graph.KindFunction, Name: "viaES", FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app"},
			{ID: "app/main.ts::viaReq", Kind: graph.KindFunction, Name: "viaReq", FilePath: "app/main.ts", Language: "typescript", RepoPrefix: "app"},
			{ID: "goapp/main.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "goapp/main.go", Language: "go", RepoPrefix: "goapp"},
		}, []*graph.Edge{
			{From: "app/main.ts", To: "prov/index.ts::foo", Kind: graph.EdgeImports, FilePath: "app/main.ts", Line: 1, CrossRepo: true, Meta: map[string]any{resolver.NpmImportSpecifierMeta: "p::foo"}},
			{From: "app/main.ts::viaES", To: "prov/index.ts::foo", Kind: graph.EdgeCalls, FilePath: "app/main.ts", Line: 2, CrossRepo: true, Origin: graph.OriginASTResolved},
			{From: "app/main.ts::viaReq", To: "prov/index.ts::foo", Kind: graph.EdgeCalls, FilePath: "app/main.ts", Line: 3, CrossRepo: true, Origin: graph.OriginASTResolved, Meta: map[string]any{graph.JSLexicalBindingMeta: graph.JSLexicalRequire, graph.JSLexicalRequireSpecMeta: "p", resolver.NpmImportSpecifierMeta: "p"}},
			{From: "goapp/main.go::Run", To: "prov/util.go::GoFn", Kind: graph.EdgeCalls, FilePath: "goapp/main.go", Line: 4, CrossRepo: true},
		})
		counting := &siteKindCountingStore{Store: g}
		restubTrackedNpmRepoIncoming(counting, "prov")
		assert.Equal(t, 1, counting.repoNodeScans, "must perform exactly one RepoNodeIdentitiesSeq pass")
		assert.Zero(t, counting.callSites, "Stage 2 must issue 0 EdgeCalls hydrations when target is marked by Stage 1 or non-JSTS")

		// Both ES-import and require calls into prov/index.ts::foo were restubbed; Go call untouched.
		require.Equal(t, graph.UnresolvedMarker+"foo", g.GetOutEdges("app/main.ts::viaES")[0].To)
		require.Equal(t, graph.UnresolvedMarker+"foo", g.GetOutEdges("app/main.ts::viaReq")[0].To)
		require.Equal(t, "prov/util.go::GoFn", g.GetOutEdges("goapp/main.go::Run")[0].To)
	})

	t.Run("Vue SFC script require call without EdgeImports is restubbed via Stage 2", func(t *testing.T) {
		g := graph.New()
		g.AddBatch([]*graph.Node{
			{ID: "prov/index.ts", Kind: graph.KindFile, Name: "index.ts", FilePath: "prov/index.ts", Language: "typescript", RepoPrefix: "prov"},
			{ID: "prov/index.ts::foo", Kind: graph.KindFunction, Name: "foo", FilePath: "prov/index.ts", Language: "typescript", RepoPrefix: "prov"},
			{ID: "app/App.vue#script:5::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/App.vue", Language: "typescript", RepoPrefix: "app"},
		}, []*graph.Edge{
			{
				From: "app/App.vue#script:5::run", To: "prov/index.ts::foo", Kind: graph.EdgeCalls,
				FilePath: "app/App.vue", Line: 6, CrossRepo: true, Origin: graph.OriginASTResolved,
				Meta: map[string]any{
					graph.JSLexicalBindingMeta:      graph.JSLexicalRequire,
					graph.JSLexicalRequireSpecMeta:  "p",
					graph.JSLexicalRequireLocalMeta: "myFoo",
					resolver.NpmImportSpecifierMeta: "p",
				},
			},
		})
		restubTrackedNpmRepoIncoming(g, "prov")
		out := g.GetOutEdges("app/App.vue#script:5::run")
		require.Len(t, out, 1)
		require.Equal(t, graph.UnresolvedMarker+"myFoo", out[0].To)
		require.False(t, out[0].CrossRepo)
	})
}

func TestImportEvidence_LocalRequireRenamedDestructuringAndMember(t *testing.T) {
	findCalls := func(g graph.Store, from string) []string {
		var out []string
		for _, e := range g.GetOutEdges(from) {
			if e != nil && e.Kind == graph.EdgeCalls {
				out = append(out, e.To)
			}
		}
		sort.Strings(out)
		return out
	}

	countCallsTo := func(g graph.Store, from, target string) int {
		n := 0
		for _, to := range findCalls(g, from) {
			if to == target {
				n++
			}
		}
		return n
	}

	t.Run("same-repo target edit, skip, barrel edit, multi-symbol carry-merge, deletion and ref_facts parity", func(t *testing.T) {
		for _, backend := range []string{"memory", "sqlite"} {
			for _, mode := range []string{"IndexFile", "IncrementalReindexPaths"} {
				t.Run(backend+"/"+mode, func(t *testing.T) {
					var g graph.Store = graph.New()
					if backend == "sqlite" {
						db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, db.Close()) })
						g = db
					}
					root := t.TempDir()
					builderWriteTree(t, root, map[string]string{
						"src/lib/impl.js":     "export function foo(x) { return x + 1 }\nexport function bar(y) { return y + 2 }\n",
						"src/lib/mid.js":      "export { foo, bar } from './impl'\n",
						"src/lib/dupalias.js": "export { foo as a, foo as b } from './impl'; export { bar } from './impl'\n",
						"src/lib/nsmix.js":    "export * as ns from './impl'; export { foo } from './impl'\n",
						"src/lib/index.js":    "exports.aliased = require('./mid').foo\nexports.bar = require('./mid').bar\n",
						"src/app/caller.js": strings.Join([]string{
							"export function viaRename(v) { const { foo: myFoo } = require('../lib/impl'); return myFoo(v) }",
							"export function viaMember(v) { const mFoo = require('../lib/impl').foo; return mFoo(v) }",
							"export function viaBarrel(v) { const { aliased } = require('../lib'); return aliased(v) }",
							"export function viaBoth() { const { foo, bar } = require('../lib/mid'); foo(1); bar(2) }",
							"export function viaNsMix() { const { foo } = require('../lib/nsmix'); return foo(3) }",
							"export function viaDupAlias() {",
							"  const { a, b } = require('../lib/dupalias')",
							"  const x = a(1)",
							"  return x + b(2)",
							"}",
							"export function viaDupPhantom() { const { foo } = require('../lib/dupalias'); return foo(3) }",
						}, "\n") + "\n",
					})

					cfg := config.Default().Index
					cfg.Workers = 1
					idx := New(g, builderRegistry(), cfg, zap.NewNop())
					idx.SetRepoPrefix(builderRepoPrefix)
					_, err := idx.Index(root)
					require.NoError(t, err)

					p := func(rel string) string { return builderRepoPrefix + "/" + rel }
					implPath := filepath.Join(root, "src", "lib", "impl.js")
					midPath := filepath.Join(root, "src", "lib", "mid.js")

					reparse := func(filePath, src string) {
						bumpMtime(t, filePath, src)
						if mode == "IndexFile" {
							require.NoError(t, idx.IndexFile(filePath))
						} else {
							_, err := idx.IncrementalReindexPaths(root, []string{filePath})
							require.NoError(t, err)
						}
					}

					require.Contains(t, findCalls(g, p("src/app/caller.js::viaRename")), p("src/lib/impl.js::foo"))
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaMember")), p("src/lib/impl.js::foo"))
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaBarrel")), p("src/lib/impl.js::foo"))
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaNsMix")), p("src/lib/impl.js::foo"))

					// 1. Signature edit on impl.js while barrel.js & mid.js are untouched -> all callers rebind cleanly.
					reparse(implPath, "export function foo(x, extra) { return x + extra }\nexport function bar(y) { return y + 2 }\n")
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaRename")), p("src/lib/impl.js::foo"))
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaMember")), p("src/lib/impl.js::foo"))
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaBarrel")), p("src/lib/impl.js::foo"))

					// 2. Edit pure barrel mid.js (zero non-reexport symbols) -> index.js re-export survives.
					reparse(midPath, "// edited comment\nexport { foo, bar } from './impl'\n")
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaBarrel")), p("src/lib/impl.js::foo"))

					// 3. Delete BOTH foo and bar in one edit (tests identity-keyed carry-merge on mid.js & nsmix.js) -> restub to local names.
					reparse(implPath, "export function unrelated() { return 0 }\n")
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaRename")), graph.UnresolvedMarker+"myFoo")
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaMember")), graph.UnresolvedMarker+"mFoo")
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaBarrel")), graph.UnresolvedMarker+"aliased")
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaNsMix")), graph.UnresolvedMarker+"foo")

					// 4. Re-add foo first, then bar (tests two-edit staggered carry-merge recovery on mid.js and nsmix.js).
					reparse(implPath, "export function foo(x) { return x + 10 }\n")
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaNsMix")), p("src/lib/impl.js::foo"))
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaBoth")), p("src/lib/impl.js::foo"))

					reparse(implPath, "export function foo(x) { return x + 10 }\nexport function bar(y) { return y + 20 }\n")
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaBoth")), p("src/lib/impl.js::foo"))
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaBoth")), p("src/lib/impl.js::bar"))

					// Re-resolve renamed callers (which were parked at unresolved::myFoo while foo was absent in step 3)
					// so step 4b & step 5 test from a resolved state.
					idx.ResolveAll()
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaRename")), p("src/lib/impl.js::foo"))
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaMember")), p("src/lib/impl.js::foo"))
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaBarrel")), p("src/lib/impl.js::foo"))
					require.Equal(t, 2, countCallsTo(g, p("src/app/caller.js::viaDupAlias"), p("src/lib/impl.js::foo")))
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaDupPhantom")), graph.UnresolvedMarker+"foo")

					// 4b. Two-edit staggered deletion carry-merge: delete ONLY foo in edit 1, then delete bar in edit 2,
					// then re-add both. Verifies mergeCarriedReExports merges across two edits AND repointReExportToFile
					// never injects a phantom `reexport_original: "foo"` onto grouped `reexport_bindings` (`dupalias.js`).
					reparse(implPath, "export function bar(y) { return y + 20 }\n")
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaBoth")), p("src/lib/impl.js::bar"))
					reparse(implPath, "export function unrelated() { return 0 }\n")
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaBoth")), graph.UnresolvedMarker+"foo")
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaBoth")), graph.UnresolvedMarker+"bar")
					reparse(implPath, "export function foo(x) { return x + 30 }\nexport function bar(y) { return y + 40 }\n")
					idx.ResolveAll()
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaBoth")), p("src/lib/impl.js::foo"))
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaBoth")), p("src/lib/impl.js::bar"))
					require.Equal(t, 2, countCallsTo(g, p("src/app/caller.js::viaDupAlias"), p("src/lib/impl.js::foo")))
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaDupPhantom")), graph.UnresolvedMarker+"foo",
						"carry-merge on grouped reexport_bindings must never mint a phantom unexported 'foo' binding")

					// 5. Delete impl.js completely on disk -> renamed callers park on local unresolved stub.
					require.NoError(t, os.Remove(implPath))
					if mode == "IndexFile" {
						_, _, err := idx.incrementalEvictWatcherPath("src/lib/impl.js")
						require.NoError(t, err)
					} else {
						_, err := idx.IncrementalReindexPaths(root, []string{implPath})
						require.NoError(t, err)
					}
					require.Contains(t, findCalls(g, p("src/app/caller.js::viaRename")), graph.UnresolvedMarker+"myFoo")
					for _, e := range g.GetOutEdges(p("src/app/caller.js::viaRename")) {
						if e.Kind == graph.EdgeCalls {
							require.False(t, e.CrossRepo)
						}
					}

					// Verify SQLite ref_facts dropped stale rows for caller.js when impl.js disappeared.
					if sq, ok := g.(*store_sqlite.Store); ok {
						facts, err := sq.LoadRefFactsByFiles(builderRepoPrefix, []string{p("src/app/caller.js")})
						require.NoError(t, err)
						for _, f := range facts {
							require.NotEqual(t, p("src/lib/impl.js::foo"), f.ToID, "stale ref_fact must be removed")
						}
					}
				})
			}
		}
	})

	t.Run("IndexFile size-cap skip flushes renamed require callers", func(t *testing.T) {
		root := t.TempDir()
		builderWriteTree(t, root, map[string]string{
			"src/lib/foo.js":    "export function foo() { return 1 }\n",
			"src/app/caller.js": "export function run() { const { foo: bar } = require('../lib/foo'); return bar() }\n",
		})
		g := graph.New()
		cfg := config.Default().Index
		cfg.Workers = 1
		cfg.MaxFileSize = 256
		idx := New(g, builderRegistry(), cfg, zap.NewNop())
		_, err := idx.Index(root)
		require.NoError(t, err)
		require.Contains(t, findCalls(g, "src/app/caller.js::run"), "src/lib/foo.js::foo")

		fooPath := filepath.Join(root, "src", "lib", "foo.js")
		big := "export function foo() { return 1 }\n// " + strings.Repeat("x", 300) + "\n"
		bumpMtime(t, fooPath, big)
		require.NoError(t, idx.IndexFile(fooPath))
		require.Contains(t, findCalls(g, "src/app/caller.js::run"), graph.UnresolvedMarker+"bar")
	})

	t.Run("delete+add unmasking and dataflow EdgeArgOf parity", func(t *testing.T) {
		root := t.TempDir()
		builderWriteTree(t, root, map[string]string{
			"src/lib/foo.js":    "export function foo(x) { return x + 1 }\n",
			"src/app/caller.js": "export function run(v) { const { foo: bar } = require('../lib/foo'); return bar(v) }\n",
		})
		g := graph.New()
		cfg := config.Default().Index
		cfg.Workers = 1
		idx := New(g, builderRegistry(), cfg, zap.NewNop())
		_, err := idx.Index(root)
		require.NoError(t, err)

		require.Contains(t, findCalls(g, "src/app/caller.js::run"), "src/lib/foo.js::foo")

		// In one non-deferred batch: delete src/lib/foo.js and add src/lib/foo/index.js
		// so require('../lib/foo') unmasks to src/lib/foo/index.js cleanly.
		fooFile := filepath.Join(root, "src", "lib", "foo.js")
		fooIndex := filepath.Join(root, "src", "lib", "foo", "index.js")
		require.NoError(t, os.Remove(fooFile))
		builderWriteTree(t, root, map[string]string{
			"src/lib/foo/index.js": "export function foo(x) { return x + 2 }\n",
		})
		_, err = idx.IncrementalReindexPaths(root, []string{fooFile, fooIndex})
		require.NoError(t, err)

		require.Contains(t, findCalls(g, "src/app/caller.js::run"), "src/lib/foo/index.js::foo")
		require.NotContains(t, findCalls(g, "src/app/caller.js::run"), "src/lib/foo.js::foo")

		// Verify stateless dataflow materialization rewrites EdgeArgOf against the unmasked callee's param node.
		const (
			paramID = "src/lib/foo/index.js::foo#param:0"
			argFrom = "src/app/caller.js::run#param:0"
		)
		g.AddBatch(
			[]*graph.Node{
				{ID: paramID, Kind: graph.KindParam, Name: "x", FilePath: "src/lib/foo/index.js", Meta: map[string]any{"position": 0}},
				{ID: argFrom, Kind: graph.KindParam, Name: "v", FilePath: "src/app/caller.js", Meta: map[string]any{"position": 0}},
			},
			[]*graph.Edge{
				{From: paramID, To: "src/lib/foo/index.js::foo", Kind: graph.EdgeParamOf, FilePath: "src/lib/foo/index.js"},
				{From: argFrom, To: "src/lib/foo/index.js::foo", Kind: graph.EdgeArgOf, FilePath: "src/app/caller.js", Line: 1, Meta: map[string]any{"arg_position": 0}},
			},
		)
		materializeDataflowParamsForFilesInGraph(g, []string{"src/app/caller.js"})
		var gotArgOf []string
		for _, e := range g.GetOutEdges(argFrom) {
			if e != nil && e.Kind == graph.EdgeArgOf {
				gotArgOf = append(gotArgOf, e.To)
			}
		}
		require.Contains(t, gotArgOf, paramID)
		require.NotContains(t, gotArgOf, "src/lib/foo/index.js::foo")
	})

	t.Run("cross-repo bare-dir renamed require rebind and stateless SQLite ref_facts + concurrent lanes", func(t *testing.T) {
		db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "multi.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })

		libDir, app1Dir, app2Dir := t.TempDir(), t.TempDir(), t.TempDir()
		writeWorkspaceFile(t, libDir, "shared_mod/index.ts", "export function foo(a: number): number { return a + 1 }\n")
		writeWorkspaceFile(t, app1Dir, "main.ts", "export function run(x: number) { const { foo: bar } = require('lib/shared_mod'); return bar(x) }\n")
		writeWorkspaceFile(t, app2Dir, "main.ts", "export function run(x: number) { const { foo: baz } = require('lib/shared_mod'); return baz(x) }\n")

		reg := newTestRegistry()
		reg.Register(languages.NewJavaScriptExtractor())
		reg.Register(languages.NewTypeScriptExtractor())
		mi := NewMultiIndexer(db, reg, search.NewNull(), newTestConfigManager(t), zap.NewNop())
		t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })

		for _, entry := range []config.RepoEntry{
			{Name: "lib", Path: libDir, Workspace: "ws"},
			{Name: "app1", Path: app1Dir, Workspace: "ws"},
			{Name: "app2", Path: app2Dir, Workspace: "ws"},
		} {
			_, err := mi.TrackRepoCtx(t.Context(), entry)
			require.NoError(t, err)
		}
		mi.runCrossRepoResolve(false)
		require.Contains(t, findCalls(db, "app1/main.ts::run"), "lib/shared_mod/index.ts::foo")
		require.Contains(t, findCalls(db, "app2/main.ts::run"), "lib/shared_mod/index.ts::foo")

		// Seed unmaterialized EdgeReturnsTo placeholders in app1/main.ts and app2/main.ts
		// so IncrementalReindexRepo("lib", ...) proves the post-cross-repo foreignRepoRenamed
		// stateless materializeDataflowParamsForFilesInGraph pass rewrites them live.
		for _, repo := range []string{"app1", "app2"} {
			db.AddBatch(
				[]*graph.Node{
					{ID: repo + "/main.ts::run#local:res", Kind: graph.KindLocal, Name: "res", FilePath: repo + "/main.ts", RepoPrefix: repo},
				},
				[]*graph.Edge{
					{
						From: repo + "/main.ts::run", To: repo + "/main.ts::run#local:res",
						Kind: graph.EdgeReturnsTo, FilePath: repo + "/main.ts", Line: 1,
						Meta: map[string]any{"returns_to_call": true, "call_line": 1, "callee_target": "foo"},
					},
				},
			)
		}

		// Concurrent multi-repo edits + signature change on lib/shared_mod/index.ts must be race-free
		// and refresh foreign consumers' SQLite ref_facts and dataflow under each consumer's own repo_prefix.
		libFile := filepath.Join(libDir, "shared_mod", "index.ts")
		app2Extra := filepath.Join(app2Dir, "extra.ts")
		writeWorkspaceFile(t, app2Dir, "extra.ts", "export function extra() { return 1 }\n")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			bumpMtime(t, libFile, "export function foo(a: number, b: string): string { return b + a }\n")
			_, err := mi.IncrementalReindexRepo("lib", []string{libFile})
			assert.NoError(t, err)
		}()
		go func() {
			defer wg.Done()
			_, err := mi.IncrementalReindexRepo("app2", []string{app2Extra})
			assert.NoError(t, err)
		}()
		wg.Wait()

		require.Contains(t, findCalls(db, "app1/main.ts::run"), "lib/shared_mod/index.ts::foo")
		require.Contains(t, findCalls(db, "app2/main.ts::run"), "lib/shared_mod/index.ts::foo")
		for _, repo := range []string{"app1", "app2"} {
			facts, err := db.LoadRefFactsByFiles(repo, []string{repo + "/main.ts"})
			require.NoError(t, err)
			foundFact := false
			for _, f := range facts {
				if f.FromID == repo+"/main.ts::run" && f.ToID == "lib/shared_mod/index.ts::foo" {
					foundFact = true
				}
			}
			require.True(t, foundFact, "stateless ref_facts refresh must keep %s's cross-repo fact under repo_prefix=%s", repo, repo)
			foundReturnsTo := false
			for _, e := range db.GetInEdges(repo + "/main.ts::run#local:res") {
				if e != nil && e.Kind == graph.EdgeReturnsTo && e.From == "lib/shared_mod/index.ts::foo" {
					foundReturnsTo = true
				}
			}
			require.True(t, foundReturnsTo, "stateless dataflow refresh must rewrite %s's EdgeReturnsTo from lib/shared_mod/index.ts::foo", repo)
		}

		// Verify nil-logger fallback on persistRefFactsForFilesInGraph never panics.
		persistRefFactsForFilesInGraph(db, "app1", []string{"app1/main.ts"}, nil)
	})
}
