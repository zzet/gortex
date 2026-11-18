package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// prefixLatencyFixture uses persisted SQLite and ordinary daemon entries, not
// IndexAll's caller-supplied path snapshot. Every repo has workspace overrides.
func prefixLatencyFixture(b *testing.B, count int) (*store_sqlite.Store, *config.ConfigManager, *MultiIndexer) {
	b.Helper()
	dir := b.TempDir()
	cm, err := config.NewConfigManager(filepath.Join(dir, "config.yaml"))
	require.NoError(b, err)
	entries := make([]config.RepoEntry, count)
	for i := range entries {
		root := filepath.Join(dir, fmt.Sprintf("repo-%04d", i))
		require.NoError(b, os.MkdirAll(root, 0o755))
		require.NoError(b, os.WriteFile(filepath.Join(root, "main.go"), []byte("package sample\nfunc Alpha() {}\nfunc Beta() {}\nfunc Caller() { Alpha() }\n"), 0o644))
		require.NoError(b, os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte("workspace: bench\nexclude:\n  - ignored/**\n"), 0o644))
		entries[i] = config.RepoEntry{Path: root}
	}
	cm.Global().Repos = entries
	require.NoError(b, cm.Global().Save())
	store, err := store_sqlite.Open(filepath.Join(dir, "graph.sqlite"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	b.Cleanup(func() { require.NoError(b, mi.Close(context.Background())) })
	mi.BeginBatch()
	for _, entry := range entries {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(b, err)
	}
	mi.EndBatch()
	mi.RunGlobalResolve()
	return store, cm, mi
}

// One operation reconciles the whole configured workspace after a restart.
func BenchmarkPrefixReconcileRepo(b *testing.B) {
	for _, count := range []int{200, 1000} {
		b.Run(fmt.Sprintf("repos=%d", count), func(b *testing.B) {
			b.StopTimer()
			store, seedCM, seed := prefixLatencyFixture(b, count)
			entries := seedCM.Global().ReposSnapshot()
			require.NoError(b, seed.Close(context.Background()))
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				cm, err := config.NewConfigManager(seedCM.Global().ConfigPath())
				require.NoError(b, err)
				mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
				mi.BeginBatch()
				b.StartTimer()
				for _, entry := range entries {
					result, err := mi.ReconcileRepoCtx(context.Background(), entry, store.LoadFileMtimes(config.ResolvePrefix(entry)))
					require.NoError(b, err)
					require.NotNil(b, result)
					require.Zero(b, result.StaleFileCount)
					require.False(b, result.FullRetrack)
				}
				b.StopTimer()
				mi.ResetBatch()
				require.NoError(b, mi.Close(context.Background()))
			}
			b.ReportMetric(float64(count), "repos/op")
		})
	}
}

// One operation changes a call target through the watcher's coordinated save
// API. Only the filesystem edit is excluded from the measured operation.
func BenchmarkPrefixCallChangingSave(b *testing.B) {
	for _, count := range []int{200, 1000} {
		b.Run(fmt.Sprintf("repos=%d", count), func(b *testing.B) {
			b.StopTimer()
			_, cm, mi := prefixLatencyFixture(b, count)
			entry := cm.Global().ReposSnapshot()[0]
			path := filepath.Join(entry.Path, "main.go")
			stamp := time.Now()
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				callee := "Beta"
				if n%2 == 1 {
					callee = "Alpha"
				}
				source := fmt.Sprintf("package sample\nfunc Alpha() {}\nfunc Beta() {}\nfunc Caller() { %s() }\n", callee)
				require.NoError(b, os.WriteFile(path, []byte(source), 0o644))
				mtime := stamp.Add(time.Duration(n+1) * time.Second)
				require.NoError(b, os.Chtimes(path, mtime, mtime))
				b.StartTimer()
				result, err := mi.IncrementalReindexRepo(config.ResolvePrefix(entry), []string{path})
				b.StopTimer()
				require.NoError(b, err)
				require.NotNil(b, result)
				require.Equal(b, 1, result.StaleFileCount)
			}
		})
	}
}
