package indexer

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
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
			beforeSrc := "import { helper } from './libA'\nexport function run() {\n  const a = helper()\n  {\n    const { helper } = require('./libB')\n    return a + helper()\n  }\n}\n"
			afterSrc := "import { helper } from './libA'\nexport function run() {\n  const a = helper()\n  {\n    // deleted local require\n    return a + helper()\n  }\n}\n"
			builderWriteTree(t, root, map[string]string{
				"src/libA" + ext: "export function helper() { return 1 }\n",
				"src/libB" + ext: "export function helper() { return 2 }\n",
				"src/app" + ext:  beforeSrc,
			})

			indexCold := func() (*graph.Graph, *Indexer) {
				g := graph.New()
				cfg := config.Default().Index
				cfg.Workers = 1
				idx := New(g, builderRegistry(), cfg, zap.NewNop())
				idx.SetRepoPrefix(builderRepoPrefix)
				_, err := idx.Index(root)
				require.NoError(t, err)
				return g, idx
			}

			g, idx := indexCold()
			appPath := filepath.Join(root, "src", "app"+ext)
			runID := builderRepoPrefix + "/src/app" + ext + "::run"
			libAHelper := builderRepoPrefix + "/src/libA" + ext + "::helper"
			libBHelper := builderRepoPrefix + "/src/libB" + ext + "::helper"
			require.Contains(t, jsCallTargets(g, runID), libAHelper)
			require.Contains(t, jsCallTargets(g, runID), libBHelper)

			bumpMtime(t, appPath, afterSrc)
			_, err := idx.IncrementalReindexPaths(root, []string{appPath})
			require.NoError(t, err)

			require.NotContains(t, jsCallTargets(g, runID), libBHelper)
			for _, e := range g.GetOutEdges(runID) {
				if e.Kind != graph.EdgeCalls {
					continue
				}
				require.Equal(t, libAHelper, e.To)
				require.NotContains(t, e.Meta, graph.JSLexicalBindingMeta)
				require.NotContains(t, e.Meta, graph.JSLexicalRequireSpecMeta)
			}

			fresh, _ := indexCold()
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
