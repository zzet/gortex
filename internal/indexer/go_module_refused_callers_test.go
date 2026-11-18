package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// Given callers in a workspace without a dependency rule for the provider's,
// when a restart seeds the module retry and the provider then changes its
// definitions, then neither pass re-reads those callers: none of their calls
// can bind, so source recovery has nothing to restore.
func TestGoModuleRefusedCallersSkipSourceRecovery(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			provider, consumer := t.TempDir(), t.TempDir()
			writeFile(t, filepath.Join(provider, "go.mod"), "module example.org/library\n")
			writeFile(t, filepath.Join(provider, "lib.go"), "package library\nfunc Hello() int { return 1 }\n")
			writeFile(t, filepath.Join(consumer, "go.mod"), "module example.org/app\n")
			writeFile(t, filepath.Join(consumer, "main.go"), "package app\nimport library \"example.org/library\"\nfunc Caller() int { return library.Hello() + library.Hello() }\n")
			var g graph.Store = graph.New()
			if backend == "sqlite" {
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "g.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				g = db
			}
			cm := newTestConfigManager(t)
			mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			entries := []config.RepoEntry{{Path: provider, Name: "b", Workspace: "pw"}, {Path: consumer, Name: "a", Workspace: "cw"}}
			for _, entry := range entries {
				_, err := mi.TrackRepoCtx(t.Context(), entry)
				require.NoError(t, err)
			}
			restored := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, restored.Close(context.Background())) })
			for _, entry := range entries {
				_, err := restored.ReconcileRepoCtx(t.Context(), entry, mi.FileMtimes(entry.Name))
				require.NoError(t, err)
			}
			original := readGoModuleCallerFile
			reads := 0
			readGoModuleCallerFile = func(path string) ([]byte, error) {
				if strings.HasPrefix(path, consumer) {
					reads++
				}
				return original(path)
			}
			t.Cleanup(func() { readGoModuleCallerFile = original })
			require.NoError(t, restored.SeedGoModuleDeps(t.Context()))
			assert.Zero(t, reads, "the warmup seed re-read a caller that cannot bind")
			lib := filepath.Join(provider, "lib.go")
			writeFile(t, lib, "package library\nfunc Hello() int { return 2 }\nfunc Added() {}\n")
			_, err := restored.IncrementalReindexRepo("b", []string{lib})
			require.NoError(t, err)
			assert.Zero(t, reads, "a provider save re-read a caller that cannot bind")
			assert.Equal(t, []string{"external-call::dep::example.org/library"}, revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCalls))
		})
	}
}
