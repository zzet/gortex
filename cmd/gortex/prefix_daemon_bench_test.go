package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// This exercises the real warmup dispatch, prelude, workspace passes and
// watcher startup over a persisted SQLite corpus. Setup and shutdown are not
// timed. No SnapshotRepoPaths is injected into the daemon's nil-lookup path.
func BenchmarkPrefixDaemonWarmup(b *testing.B) {
	for _, count := range []int{200, 1000} {
		for _, workspace := range []bool{false, true} {
			b.Run(fmt.Sprintf("repos=%d/workspace=%t", count, workspace), func(b *testing.B) {
				b.StopTimer()
				dir := b.TempDir()
				configPath := filepath.Join(dir, "config.yaml")
				cm, err := config.NewConfigManager(configPath)
				require.NoError(b, err)
				entries := make([]config.RepoEntry, count)
				for i := range entries {
					root := filepath.Join(dir, fmt.Sprintf("repo-%04d", i))
					require.NoError(b, os.MkdirAll(root, 0o755))
					require.NoError(b, os.WriteFile(filepath.Join(root, "main.go"), []byte("package sample\nfunc Alpha() {}\n"), 0o644))
					if workspace {
						require.NoError(b, os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte("workspace: bench\nexclude:\n  - ignored/**\n"), 0o644))
					}
					entries[i] = config.RepoEntry{Path: root}
				}
				cm.Global().Repos = entries
				require.NoError(b, cm.Global().Save())
				store, err := store_sqlite.Open(filepath.Join(dir, "graph.sqlite"))
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, store.Close()) })
				newState := func() *daemonState {
					manager, err := config.NewConfigManager(configPath)
					require.NoError(b, err)
					registry := parser.NewRegistry()
					registry.Register(languages.NewGoExtractor())
					mi := indexer.NewMultiIndexer(store, registry, search.NewNull(), manager, zap.NewNop())
					return &daemonState{graph: store, multiIndexer: mi, configManager: manager}
				}
				// First establish a real cold corpus, including its derived state.
				seed := newState()
				watcher, _ := warmupDaemonState(seed, zap.NewNop(), func() {})
				require.NotNil(b, watcher)
				require.NoError(b, watcher.Stop())
				require.NoError(b, seed.multiIndexer.Close(context.Background()))
				b.ReportAllocs()
				for n := 0; n < b.N; n++ {
					state := newState()
					ready := false
					b.StartTimer()
					watcher, timings := warmupDaemonState(state, zap.NewNop(), func() { ready = true })
					b.StopTimer()
					require.NotNil(b, watcher)
					require.NoError(b, watcher.Stop())
					require.True(b, ready)
					require.Len(b, state.multiIndexer.AllMetadata(), count)
					require.Zero(b, timings.reposChanged)
					require.Zero(b, timings.filesReindexed)
					require.NoError(b, state.multiIndexer.Close(context.Background()))
				}
				b.ReportMetric(float64(count), "repos/op")
			})
		}
	}
}
