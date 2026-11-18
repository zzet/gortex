package indexer

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

// Given a consumer linked to a tracked npm package, when the provider is
// untracked or the package is renamed, then the parked imports carry no
// npm_import marker, as in a cold index without that package, and
// retracking links them again.
func TestNpmParkedImportsDropMarker(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			prov, app := t.TempDir(), t.TempDir()
			writePackageJSON(t, prov, `{"name":"p","exports":"./src/index.ts"}`)
			writeWorkspaceFile(t, prov, "src/index.ts", "export function f(): number { return 42; }\nexport function g(): number { return 1; }\n")
			writePackageJSON(t, app, `{"name":"app","dependencies":{"p":"^1"}}`)
			writeWorkspaceFile(t, app, "src/app.ts", "import { f, g as gg } from 'p';\nexport function run(): number { gg(); return f(); }\n")
			var g graph.Store = graph.New()
			if backend == "sqlite" {
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "g.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				g = db
			}
			reg := newTestRegistry()
			reg.Register(languages.NewTypeScriptExtractor())
			mi := NewMultiIndexer(g, reg, search.NewNull(), newTestConfigManager(t), zap.NewNop())
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			provider := config.RepoEntry{Path: prov, Name: "prov", Workspace: "shared"}
			for _, entry := range []config.RepoEntry{provider, {Path: app, Name: "app", Workspace: "shared"}} {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			imports := func() []string {
				var out []string
				for _, e := range g.GetOutEdges("app/src/app.ts") {
					if e.Kind == graph.EdgeImports {
						marker, _ := e.Meta[resolver.NpmImportSpecifierMeta].(string)
						out = append(out, e.To+" marker="+marker)
					}
				}
				sort.Strings(out)
				return out
			}
			linked := []string{"prov/src/index.ts marker=p", "prov/src/index.ts::f marker=p::f", "prov/src/index.ts::g marker=p::g"}
			parked := []string{"external::p marker=", "external::p::f marker=", "external::p::g marker="}
			require.Equal(t, linked, imports())
			_, _, err := mi.UntrackRepoChecked(t.Context(), "prov")
			require.NoError(t, err)
			assert.Equal(t, parked, imports(), "provider untracked")
			_, err = mi.TrackRepoCtx(t.Context(), provider)
			require.NoError(t, err)
			assert.Equal(t, linked, imports(), "provider retracked")
			writePackageJSON(t, prov, `{"name":"p-renamed","exports":"./src/index.ts"}`)
			mi.RunGlobalResolve()
			assert.Equal(t, parked, imports(), "package renamed")
		})
	}
}
