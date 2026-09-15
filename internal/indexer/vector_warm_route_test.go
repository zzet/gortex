package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// A warm restart whose reconcile takes a non-census route must still publish
// the durable vector corpus onto the daemon-level search backend. Regression
// for https://github.com/zzet/gortex/issues/790: restore was wired only into
// the census_noop route, so every other warm route (incremental, scoped,
// merkle-clean, manifest-only) served text-only despite a durable corpus.
func TestWarmScopedReconcileRestoresDurableVectorCorpus(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "main.go")
	writeFile(t, mainPath, "package sample\nfunc Alpha() {}\n")
	writeFile(t, filepath.Join(root, "extra.go"), "package sample\nfunc Gamma() {}\n")
	writeFile(t, filepath.Join(root, "more.go"), "package sample\nfunc Delta() {}\n")
	entry := config.RepoEntry{Path: root, Name: "repo"}
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{entry}

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	seedSw := search.NewSwappable(initialSearchBackend(store))
	seed := NewMultiIndexer(graph.Store(store), newTestRegistry(), seedSw, cm, zap.NewNop())
	seedEmb := &poolEmbedder{}
	seed.SetEmbedder(seedEmb)
	results, err := seed.indexMultiRepo([]config.RepoEntry{entry})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Greater(t, seedEmb.calls, int32(0))

	prior := seed.GetIndexer("repo").FileMtimes()
	require.NotEmpty(t, prior)
	stats, err := store.VectorCorpusStats(context.Background(), 3)
	require.NoError(t, err)
	require.Greater(t, stats.VectorCount, 0)

	// Mutate one of three files: 33% churn keeps the reconcile off both the
	// census_noop route (no churn) and full_retrack (>40% churn), landing on
	// scoped — a route that previously published nothing to the vector channel.
	writeFile(t, mainPath, "package sample\nfunc Alpha() {}\nfunc Beta() {}\n")

	restartSw := search.NewSwappable(initialSearchBackend(store))
	core, logs := observer.New(zap.DebugLevel)
	restarted := NewMultiIndexer(graph.Store(store), newTestRegistry(), restartSw, cm, zap.New(core))
	restartEmb := &poolEmbedder{}
	restarted.SetEmbedder(restartEmb)
	result, err := restarted.ReconcileRepoCtx(context.Background(), entry, prior)
	require.NoError(t, err)
	require.NotNil(t, result)

	entries := logs.FilterMessage("daemon: reconciled repo from snapshot").All()
	require.Len(t, entries, 1, "expected one reconcile snapshot log line")
	require.Equal(t, "scoped", entries[0].ContextMap()["route"],
		"fixture must exercise a non-census warm route")

	require.Zero(t, restartEmb.calls,
		"warm restore must publish the durable corpus without embedding")
	requirePublishedVectorStats(t, restartSw, int(stats.VectorCount), stats.ChunkCount > 0)
}
