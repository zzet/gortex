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

// Given an unavailable binding, when its export appears or returns,
// then its distinct import row survives and relinks exactly as a cold index.
func TestNpmBindingIdentityColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"barrel", "added", "restored"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				provider, caller := t.TempDir(), t.TempDir()
				writePackageJSON(t, provider, `{"name":"p","main":"index.ts"}`)
				writePackageJSON(t, caller, `{"name":"app","dependencies":{"p":"^1"}}`)
				initial := "export function g() {}\n"
				if mode == "barrel" {
					initial += "export { h } from './impl';\n"
					writeWorkspaceFile(t, provider, "impl.ts", "export function h() {}\n")
				}
				if mode == "restored" {
					initial += "export function h() {}\n"
				}
				writeWorkspaceFile(t, provider, "index.ts", initial)
				writeWorkspaceFile(t, caller, "app.ts", "import { g, h } from 'p';\nexport function run() { g(); h(); }\n")
				repos := []config.RepoEntry{{Name: "provider", Path: provider, Workspace: "shared"}, {Name: "caller", Path: caller, Workspace: "shared"}}
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					g = db
				}
				reg := newTestRegistry()
				reg.Register(languages.NewTypeScriptExtractor())
				mi := NewMultiIndexer(g, reg, search.NewNull(), newTestConfigManager(t), zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
				for _, entry := range repos {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				if mode == "restored" {
					writeWorkspaceFile(t, provider, "index.ts", "export function g() {}\nexport function gone() {}\n")
					require.NoError(t, mi.GetIndexer("provider").IndexFile(filepath.Join(provider, "index.ts")))
				}
				if mode != "barrel" {
					imports := revisionCallTargets(g, "caller/app.ts", graph.EdgeImports)
					assert.ElementsMatch(t, []string{"provider/index.ts", "provider/index.ts::g", "external::p::h"}, imports)
					for _, edge := range g.GetOutEdges("caller/app.ts") {
						if edge.To == "external::p::h" {
							assert.Equal(t, "p::h", edge.Meta["npm_import"])
						}
					}
					writeWorkspaceFile(t, provider, "index.ts", "export function g() {}\nexport function h() {}\n")
					require.NoError(t, mi.GetIndexer("provider").IndexFile(filepath.Join(provider, "index.ts")))
					mi.RunGlobalResolve()
				}
				cold := revisionColdGraph(t, backend, repos)
				expected := []string{"provider/index.ts", "provider/index.ts::g", "provider/index.ts::h"}
				if mode == "barrel" {
					expected[2] = "external::p::h"
				}
				assert.ElementsMatch(t, expected, revisionCallTargets(g, "caller/app.ts", graph.EdgeImports))
				assert.ElementsMatch(t, revisionCallTargets(cold, "caller/app.ts", graph.EdgeImports), revisionCallTargets(g, "caller/app.ts", graph.EdgeImports))
				assert.ElementsMatch(t, revisionCallTargets(cold, "caller/app.ts::run", graph.EdgeCalls), revisionCallTargets(g, "caller/app.ts::run", graph.EdgeCalls))
			})
		}
	}
}
