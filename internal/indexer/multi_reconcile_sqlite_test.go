package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// TestReconcileRepoCtx_Sqlite_FullRetrackFlag exercises ReconcileRepoCtx
// against a disk-backed (sqlite) store, which is the only backend that
// takes the whole-repo IndexCtx branch. It asserts the honest-reporting
// contract: a repo that changed while the daemon was down is reported via
// FullRetrack, not by inflating StaleFileCount to FileCount, and an
// unchanged repo reports neither.
func TestReconcileRepoCtx_Sqlite_FullRetrackFlag(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	writeFile(t, filepath.Join(repoPath, "a.go"), "package main\nfunc Alpha() {}\n")

	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "repo"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// First "daemon run": index the repo on the disk-backed store and
	// capture mtimes as if we were writing a warm-restart snapshot.
	mi := NewMultiIndexer(graph.Store(s), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	meta := mi.GetMetadata("repo")
	require.NotNil(t, meta)
	priorMtimes := mi.FileMtimes("repo")

	// While the daemon is "down", a new file appears on disk. This is
	// enough to trip HasChangesSinceMtimes and route ReconcileRepoCtx
	// down the whole-repo re-track branch.
	writeFile(t, filepath.Join(repoPath, "b.go"), "package main\nfunc Beta() {}\n")

	mi2 := NewMultiIndexer(graph.Store(s), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	result, err := mi2.ReconcileRepoCtx(context.Background(), config.RepoEntry{Path: repoPath, Name: "repo"}, priorMtimes)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.True(t, result.FullRetrack,
		"a changed disk-backed repo must be reported as a full re-track")
	assert.Equal(t, 0, result.StaleFileCount,
		"StaleFileCount must keep its honest incremental-work meaning (0) on a full re-track, not be stamped to FileCount")

	// Third "daemon run": nothing changed since the previous reconcile,
	// so ReconcileRepoCtx must report neither flag.
	meta2 := mi2.GetMetadata("repo")
	require.NotNil(t, meta2)
	require.Nil(t, mi2.GetIndexer("repo").warmupWriteBoundary, "published edit indexer retained a startup wait")
	unchangedMtimes := mi2.FileMtimes("repo")

	mi3 := NewMultiIndexer(graph.Store(s), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	result2, err := mi3.ReconcileRepoCtx(context.Background(), config.RepoEntry{Path: repoPath, Name: "repo"}, unchangedMtimes)
	require.NoError(t, err)
	require.NotNil(t, result2)
	require.Nil(t, mi3.GetIndexer("repo").warmupWriteBoundary, "clean-census publication retained a startup wait")

	assert.False(t, result2.FullRetrack,
		"an unchanged disk-backed repo must not be reported as a full re-track")
	assert.Equal(t, 0, result2.StaleFileCount,
		"an unchanged disk-backed repo must report zero stale files")
}

func TestTSConfigPathAlias_WarmRestartChecksInheritedConfig(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	parent := filepath.Join(root, ".nuxt", "aliases.json")
	writeFileMk(t, configPath, `{"extends":"./.nuxt/aliases.json"}`)
	writeFileMk(t, parent, `{"compilerOptions":{"paths":{"~/*":["../old/*"]}}}`)

	entry := config.RepoEntry{Path: root, Name: "repo"}
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{entry}}
	gc.SetConfigPath(configFile)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(configFile)
	require.NoError(t, err)
	idx, store := newSQLiteIndexer(t)
	start := func() *MultiIndexer {
		invalidateTSAliasCollection(root)
		return NewMultiIndexer(store, idx.registry, search.NewNull(), cm, zap.NewNop())
	}
	first := start()
	_, err = first.TrackRepoCtx(t.Context(), entry)
	require.NoError(t, err)
	require.Equal(t, "repo/old/auth.ts::useRedirect", callTargetForName(t, store, "repo/pages/index.ts", "run"))
	mtimes := first.FileMtimes("repo")
	stamp, err := os.Stat(parent)
	require.NoError(t, err)
	writeFileMk(t, parent, `{"compilerOptions":{"paths":{"~/*":["../new/*"]}}}`)
	require.NoError(t, os.Chtimes(parent, stamp.ModTime(), stamp.ModTime()))

	second := start()
	_, err = second.ReconcileRepoCtx(t.Context(), entry, mtimes)
	require.NoError(t, err)
	require.Equal(t, "repo/new/auth.ts::useRedirect", callTargetForName(t, store, "repo/pages/index.ts", "run"))
	unchanged, err := start().ReconcileRepoCtx(t.Context(), entry, second.FileMtimes("repo"))
	require.NoError(t, err)
	require.Zero(t, unchanged.StaleFileCount, "a cold lookup cache alone must not reparse unchanged callers")
}
