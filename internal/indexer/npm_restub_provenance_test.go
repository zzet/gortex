package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// A provider save parks its tracked-npm consumers without restub provenance,
// so the consumer file goes through the forward pass again. An unrelated
// function of the same name in the consumer repository must not capture the
// call: it binds what a cold index binds. In another directory that is the
// provider. In the caller's own directory main's locality tier already wins
// on a cold index (the per-repo pass sees only an external import); the save
// must not change that either way.
func TestTrackedNpmCallReturnsToProviderPastSameNamedConsumerFunction(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, layout := range []string{"same directory", "other directory"} {
			t.Run(backend+"/"+layout, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					g = db
				}
				provider, consumer := t.TempDir(), t.TempDir()
				writePackageJSON(t, provider, `{"name":"p","exports":"./index.ts"}`)
				providerFile := filepath.Join(provider, "index.ts")
				writeWorkspaceFile(t, provider, "index.ts", "export function f() { return 42; }\n")
				writePackageJSON(t, consumer, `{"name":"app","dependencies":{"p":"^1"}}`)
				writeWorkspaceFile(t, consumer, "app.ts", "import { f } from 'p'; export function run() { return f(); }\n")
				local := "local.ts"
				if layout == "other directory" {
					local = "util/local.ts"
				}
				writeWorkspaceFile(t, consumer, local, "export function f() { return 1; }\n")

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
				target := func() string {
					t.Helper()
					var to []string
					for _, edge := range g.GetOutEdges("app/app.ts::run") {
						if edge.Kind == graph.EdgeCalls {
							to = append(to, edge.To)
						}
					}
					require.Len(t, to, 1)
					return to[0]
				}
				cold := target()
				if layout == "other directory" {
					require.Equal(t, "lib/index.ts::f", cold)
				}

				bumpMtime(t, providerFile, "export function f() { return 43; }\n")
				_, err := mi.IncrementalReindexRepo("lib", []string{providerFile})
				require.NoError(t, err)
				require.Equal(t, cold, target(), "a provider save rebinds what a cold index binds")
			})
		}
	}
}
