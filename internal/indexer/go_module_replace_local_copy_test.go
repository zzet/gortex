package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

func goModuleColdCall(t *testing.T, backend string, repos []config.RepoEntry) *graph.Edge {
	var g graph.Store = graph.New()
	if backend == "sqlite" {
		db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		g = db
	}
	cold := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
	t.Cleanup(func() { _ = cold.Close(context.Background()) })
	for _, entry := range repos {
		if _, err := cold.TrackRepoCtx(t.Context(), entry); err != nil {
			t.Fatal(err)
		}
	}
	cold.RunGlobalResolve()
	return goModuleLifecycleCall(t, g)
}

// A same-repository copy of a foreign module binds by path suffix. Adding an
// in-checkout replace later must recheck that call, as a cold index would.
func TestGoModuleReplaceAddedOverSameRepoCopyRechecks(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, repos := goModuleLifecycleFixture(t, backend)
			caller := repos[0].Path
			manifest := filepath.Join(caller, "go.mod")
			for _, d := range []string{"third_party/example.org/library", "forks/library"} {
				if err := os.MkdirAll(filepath.Join(caller, d), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(caller, d, "lib.go"), "package library\nfunc Hello() {}\n")
			}
			order := []config.RepoEntry{repos[1], repos[0]}
			for _, entry := range order {
				if _, err := mi.TrackRepoCtx(t.Context(), entry); err != nil {
					t.Fatal(err)
				}
			}
			mi.RunGlobalResolve()
			writeFile(t, manifest, "module example.org/caller\nreplace example.org/library => ./forks/library\n")
			if _, err := mi.IncrementalReindexRepo("a", []string{manifest}); err != nil {
				t.Fatal(err)
			}
			mi.RunGlobalResolve()
			e := goModuleLifecycleCall(t, g)
			c := goModuleColdCall(t, backend, order)
			if c.To != e.To {
				t.Errorf("after the replace was added the call stays on %s; a cold index binds %s", e.To, c.To)
			}
		})
	}
}
