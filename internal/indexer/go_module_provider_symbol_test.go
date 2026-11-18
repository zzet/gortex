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
	"github.com/zzet/gortex/internal/search"
)

func TestGoModuleDotlessProviderAddsSymbol(t *testing.T) {
	for _, mod := range []string{"example.org/library", "corp/library"} {
		for _, mode := range []string{"watcher", "indexrepo"} {
			t.Run(mod+"/"+mode, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				caller, library := t.TempDir(), t.TempDir()
				writeFile(t, filepath.Join(caller, "go.mod"), "module example.org/caller\n")
				writeFile(t, filepath.Join(caller, "main.go"), "package caller\nimport \""+mod+"\"\nfunc Caller() { library.NewFn() }\n")
				writeFile(t, filepath.Join(library, "go.mod"), "module "+mod+"\n")
				writeFile(t, filepath.Join(library, "lib.go"), "package library\nfunc Hello() {}\n")
				s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				mi := NewMultiIndexer(s, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
				for _, e := range []config.RepoEntry{{Path: library, Name: "b", Workspace: "shared"}, {Path: caller, Name: "a", Workspace: "shared"}} {
					_, err = mi.TrackRepoCtx(context.Background(), e)
					require.NoError(t, err)
				}
				mi.RunGlobalResolve()
				call := func() *graph.Edge {
					var calls []*graph.Edge
					for _, e := range s.GetOutEdges("a/main.go::Caller") {
						if e.Kind == graph.EdgeCalls {
							calls = append(calls, e)
						}
					}
					require.Len(t, calls, 1)
					return calls[0]
				}
				writeFile(t, filepath.Join(library, "lib.go"), "package library\nfunc Hello() {}\nfunc NewFn() {}\n")
				if mode == "watcher" {
					_, err = mi.incrementalPointRepoRaw("b", filepath.Join(library, "lib.go"))
				} else {
					_, err = mi.IndexRepo("b")
				}
				require.NoError(t, err)
				if call().To != "b/lib.go::NewFn" {
					t.Errorf("provider-added symbol not bound: %s", call().To)
				}
				mi.RunGlobalResolve()
				require.Equal(t, "b/lib.go::NewFn", call().To)
			})
		}
	}
}
