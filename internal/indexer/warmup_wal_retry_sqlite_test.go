package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// A warm reconcile refused by the warmup WAL boundary after some chunks have
// committed fails without registering the repository, and does not wait at
// the boundary again for a retry pass. Reconciling again from the mtimes the
// store then holds converges: every changed file carries its new symbols
// exactly once and its old ones not at all.
func TestReconcileRefusedByWarmupWALConvergesOnRetry(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	const files, changedFiles = 100, 35 // two chunks; under the full-retrack churn share
	for i := range files {
		writeFile(t, filepath.Join(repoPath, fmt.Sprintf("f%03d.go", i)), fmt.Sprintf("package main\nfunc F%d() {}\n", i))
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "repo"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	mi := NewMultiIndexer(graph.Store(s), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })
	_, err = mi.IndexAll()
	require.NoError(t, err)
	priorMtimes := mi.FileMtimes("repo")
	require.Len(t, priorMtimes, files)

	later := time.Now().Add(time.Hour)
	for i := range changedFiles {
		path := filepath.Join(repoPath, fmt.Sprintf("f%03d.go", i))
		writeFile(t, path, fmt.Sprintf("package main\nfunc F%dV2() {}\n", i))
		require.NoError(t, os.Chtimes(path, later, later))
	}

	real := awaitWarmupWAL
	t.Cleanup(func() { awaitWarmupWAL = real })
	boundaryCalls := 0
	awaitWarmupWAL = func(store *store_sqlite.Store, ctx context.Context) error {
		boundaryCalls++
		if boundaryCalls == 2 {
			return fmt.Errorf("%w: %w", store_sqlite.ErrWarmupWALPressure, context.DeadlineExceeded)
		}
		return real(store, ctx)
	}
	// As in the warmup: the refused attempt and its retry run on one
	// MultiIndexer, inside one topology batch, the retry reading back the
	// mtimes the refused attempt's committed chunks persisted.
	entry := config.RepoEntry{Path: repoPath, Name: "repo"}
	warm := NewMultiIndexer(graph.Store(s), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { _ = warm.Close(context.Background()) })
	require.NoError(t, warm.RunRepositoryTopologyBatch(context.Background(), func(batchCtx context.Context) error {
		_, err := warm.ReconcileRepoCtx(batchCtx, entry, priorMtimes)
		require.ErrorIs(t, err, store_sqlite.ErrWarmupWALPressure)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, 2, boundaryCalls, "a refused reconcile must not wait at the boundary again for a retry pass")
		assert.Nil(t, warm.GetIndexer("repo"), "a refused reconcile must not register the repository")

		retryMtimes := s.LoadFileMtimes("repo")
		require.NotEmpty(t, retryMtimes)
		_, err = warm.ReconcileRepoCtx(batchCtx, entry, retryMtimes)
		require.NoError(t, err)
		require.NotNil(t, warm.GetIndexer("repo"))
		return nil
	}))
	// The first attempt's committed chunk persisted its mtimes, so the retry
	// re-indexes only the rest: one chunk, one more boundary call.
	assert.Equal(t, 3, boundaryCalls, "the retry reconciles only the files the refusal left")

	for i := range files {
		if i < changedFiles {
			assert.Len(t, s.FindNodesByNameInRepo(fmt.Sprintf("F%dV2", i), "repo"), 1, "file %d new symbol", i)
			assert.Empty(t, s.FindNodesByNameInRepo(fmt.Sprintf("F%d", i), "repo"), "file %d old symbol", i)
		} else {
			assert.Len(t, s.FindNodesByNameInRepo(fmt.Sprintf("F%d", i), "repo"), 1, "file %d unchanged symbol", i)
		}
	}
}
