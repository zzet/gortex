package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

func requirePublishedVectorStats(
	t *testing.T,
	sw *search.Swappable,
	wantVectorCount int,
	wantChunks bool,
) {
	t.Helper()
	backend, release := sw.AcquireBackend()
	defer release()
	hybrid, ok := backend.(*search.HybridBackend)
	require.True(t, ok, "shared search backend must publish one hybrid layer")
	require.NotNil(t, hybrid.VectorIndex())
	require.Equal(t, wantVectorCount, hybrid.VectorIndex().Count())
	require.Equal(t, wantChunks, hybrid.VectorIndex().HasChunks())
	require.Zero(t, hybrid.VectorSizeBytes(), "multi-repo publication must never retain a repo-local heap HNSW")
}

func TestIndexMultiRepoPublishesCombinedDurableVectorCorpus(t *testing.T) {
	repos := coldOrchestrationRepos(t, 2)
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "multi-vectors.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sw := search.NewSwappable(initialSearchBackend(store))
	mi := NewMultiIndexer(store, coldOrchestrationRegistry(), sw, nil, zap.NewNop())
	mi.SetEmbedder(&poolEmbedder{})

	results, err := mi.indexFixtureRepos(repos)
	require.NoError(t, err)
	require.Len(t, results, len(repos))
	stats, err := store.VectorCorpusStats(context.Background(), 3)
	require.NoError(t, err)
	require.Greater(t, stats.VectorCount, 0)
	for _, entry := range repos {
		prefix := config.ResolvePrefix(entry)
		repoStats, statsErr := store.VectorCorpusStatsForRepo(context.Background(), prefix, 3)
		require.NoError(t, statsErr)
		require.Greater(t, repoStats.RepositoryVectorCount, 0, "%s corpus missing", prefix)
	}
	requirePublishedVectorStats(t, sw, stats.VectorCount, stats.ChunkCount > 0)
}

func TestUntrackRepoRemovesVectorsAndRepublishesAggregateStats(t *testing.T) {
	repos := coldOrchestrationRepos(t, 2)
	cm := newTestConfigManager(t)
	cm.Global().Repos = append([]config.RepoEntry(nil), repos...)
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "untrack-vectors.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sw := search.NewSwappable(initialSearchBackend(store))
	mi := NewMultiIndexer(store, coldOrchestrationRegistry(), sw, cm, zap.NewNop())
	mi.SetEmbedder(&poolEmbedder{})
	_, err = mi.indexFixtureRepos(repos)
	require.NoError(t, err)

	prefixA := config.ResolvePrefix(repos[0])
	prefixB := config.ResolvePrefix(repos[1])
	mi.UntrackRepo(prefixA)
	statsA, err := store.VectorCorpusStatsForRepo(context.Background(), prefixA, 3)
	require.NoError(t, err)
	require.Zero(t, statsA.RepositoryVectorCount)
	statsB, err := store.VectorCorpusStatsForRepo(context.Background(), prefixB, 3)
	require.NoError(t, err)
	require.Greater(t, statsB.RepositoryVectorCount, 0)
	requirePublishedVectorStats(t, sw, statsB.VectorCount, statsB.ChunkCount > 0)

	mi.UntrackRepo(prefixB)
	empty, err := store.VectorCorpusStats(context.Background(), 3)
	require.NoError(t, err)
	require.Zero(t, empty.VectorCount)
	requirePublishedVectorStats(t, sw, 0, false)
	backend, release := sw.AcquireBackend()
	hybrid := backend.(*search.HybridBackend)
	require.Empty(t, hybrid.VectorIndex().Search([]float32{1, 0, 0}, 10))
	release()
}
