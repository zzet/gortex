package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

func TestChangedSinceMtimesCensusIncludesContractManifests(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "go.mod")
	require.NoError(t, os.WriteFile(manifest, []byte("module example.com/sample\n"), 0o644))

	idx := newTestIndexer(graph.New())
	idx.SetFileMtimes(map[string]int64{"go.mod": 1})

	changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
	require.NoError(t, err)
	assert.Equal(t, []string{"go.mod"}, changed)
	assert.Empty(t, deleted)
	assert.Equal(t, 1, detected)

	require.NoError(t, os.Remove(manifest))
	changed, deleted, detected, err = idx.changedSinceMtimesCensus(root)
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Equal(t, []string{"go.mod"}, deleted)
	assert.Zero(t, detected)
}

func TestChangedSinceMtimesCensusDeletesNewlyExcludedTrackedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ignored.go")
	require.NoError(t, os.WriteFile(path, []byte("package sample\n"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)

	idx := newTestIndexer(graph.New())
	idx.SetFileMtimes(map[string]int64{"ignored.go": info.ModTime().UnixNano()})
	idx.SetExcludePatterns([]string{"ignored.go"})

	changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Equal(t, []string{"ignored.go"}, deleted)
	assert.Zero(t, detected)
}

func TestCleanCensusRestoresDurableVectorsWithoutEmbedding(t *testing.T) {
	root := vectorPersistFixture(t, 1)
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	seedEmbedder := &poolEmbedder{}
	seed := newVectorPersistIndexer(t, store, seedEmbedder)
	_, err = seed.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	require.Greater(t, seedEmbedder.calls, int32(0))

	restartEmbedder := &poolEmbedder{failOnText: "every call would fail"}
	restarted := newVectorPersistIndexer(t, store, restartEmbedder)
	result, err := restarted.cleanCensusResult(context.Background(), 1, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Zero(t, restartEmbedder.calls,
		"warm restoration must publish durable corpus statistics without a paid embedding pass")
	assertDelegatedVectorPublication(t, restarted)
}

func TestCleanCensusRebuildsMigrationClearedVectorCorpus(t *testing.T) {
	root := vectorPersistFixture(t, 1)
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	seed := newVectorPersistIndexer(t, store, &poolEmbedder{})
	_, err = seed.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	_, err = store.ReplaceVectorCorpus(context.Background(), "", 3, nil)
	require.NoError(t, err)

	rebuildEmbedder := &poolEmbedder{}
	restarted := newVectorPersistIndexer(t, store, rebuildEmbedder)
	_, err = restarted.cleanCensusResult(context.Background(), 1, time.Now())
	require.NoError(t, err)
	require.Greater(t, rebuildEmbedder.calls, int32(0),
		"an empty durable sidecar with live graph nodes must be rebuilt once")
	stats, err := store.VectorCorpusStatsForRepo(context.Background(), "", 3)
	require.NoError(t, err)
	require.Greater(t, stats.RepositoryVectorCount, 0)
	assertDelegatedVectorPublication(t, restarted)
}

func TestRestoreDurableVectorBackendRequiresCurrentRepositoryCorpus(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	store.AddBatch([]*graph.Node{
		{ID: "A/alpha", Kind: graph.KindFunction, Name: "alpha", FilePath: "A/a.go", RepoPrefix: "A"},
		{ID: "B/beta", Kind: graph.KindFunction, Name: "beta", FilePath: "B/b.go", RepoPrefix: "B"},
	}, nil)
	_, err = store.ReplaceVectorCorpus(context.Background(), "B", 3, []graph.VectorCorpusItem{
		{NodeID: "B/beta", Vec: []float32{1, 0, 0}},
	})
	require.NoError(t, err)

	emb := &poolEmbedder{}
	idx := newVectorPersistIndexer(t, store, emb)
	idx.SetRepoPrefix("A")
	restored, err := idx.restoreDurableVectorBackend(context.Background(), store)
	require.NoError(t, err)
	require.False(t, restored,
		"another repository's vectors must not suppress this repository's rebuild")
	require.Zero(t, emb.calls)

	_, err = store.ReplaceVectorCorpus(context.Background(), "A", 3, []graph.VectorCorpusItem{
		{NodeID: "A/alpha", Vec: []float32{0, 1, 0}},
	})
	require.NoError(t, err)
	restored, err = idx.restoreDurableVectorBackend(context.Background(), store)
	require.NoError(t, err)
	require.True(t, restored)
	require.Zero(t, emb.calls, "repository-scoped restore must never invoke embedding")
	assertDelegatedVectorPublication(t, idx)
}

func TestReconcileRepoCtxUsesCleanCensusNoOp(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package sample\nfunc Alpha() {}\n")
	entry := config.RepoEntry{Path: root, Name: "repo"}
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{entry}

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	seed := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = seed.IndexAll()
	require.NoError(t, err)
	prior := seed.GetIndexer("repo").FileMtimes()
	before := store.Stats()

	core, logs := observer.New(zap.DebugLevel)
	restarted := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.New(core))
	result, err := restarted.ReconcileRepoCtx(t.Context(), entry, prior)
	require.NoError(t, err)
	require.NotNil(t, result)

	after := store.Stats()
	assert.Zero(t, result.StaleFileCount)
	assert.Zero(t, result.DeletedFileCount)
	assert.False(t, result.FullRetrack)
	assert.Equal(t, before.TotalNodes, after.TotalNodes)
	assert.Equal(t, before.TotalEdges, after.TotalEdges)
	assert.Equal(t, len(prior), result.FileCount)
	assert.Equal(t, result.FileCount, restarted.GetIndexer("repo").TotalDetected())

	entries := logs.FilterMessage("daemon: reconciled repo from snapshot").All()
	require.Len(t, entries, 1)
	assert.Equal(t, "census_noop", entries[0].ContextMap()["route"])
}

// TestReconcileRepoCtxRestagesStaleExtractorVersionsWithoutChurn pins #695:
// a quiet non-Merkle repository whose persisted extractor-version snapshot is
// behind the running binary must not take census_noop, which never consults
// the snapshot. It must take the full-tree incremental route that restages the
// stale languages and re-stamps the snapshot, after which the next restart is
// a census_noop again.
func TestReconcileRepoCtxRestagesStaleExtractorVersionsWithoutChurn(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package sample\nfunc Alpha() {}\n")
	entry := config.RepoEntry{Path: root, Name: "repo"}
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{entry}

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	seed := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = seed.IndexAll()
	require.NoError(t, err)
	seedIdx := seed.GetIndexer("repo")
	require.False(t, seedIdx.merkleEnabled())
	prior := seedIdx.FileMtimes()
	repoPrefix := seedIdx.repoPrefix

	// Simulate a snapshot written by an older binary: no post-extraction
	// policy epoch, so every tracked language is stale against this one.
	state, found, err := store.GetRepoIndexState(repoPrefix)
	require.NoError(t, err)
	require.True(t, found)
	legacy := extractorVersionsSnapshot()
	delete(legacy, postExtractionPolicySnapshotKey)
	encoded, err := json.Marshal(legacy)
	require.NoError(t, err)
	state.ExtractorVersions = string(encoded)
	require.NoError(t, store.SetRepoIndexState(state))
	require.NotEmpty(t, ExtractorVersionStaleLangs(state.ExtractorVersions))

	core, logs := observer.New(zap.DebugLevel)
	restarted := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.New(core))
	result, err := restarted.ReconcileRepoCtx(t.Context(), entry, prior)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.GreaterOrEqual(t, result.StaleFileCount, 1,
		"a stale extractor snapshot must restage unchanged source")

	entries := logs.FilterMessage("daemon: reconciled repo from snapshot").All()
	require.Len(t, entries, 1)
	assert.Equal(t, "incremental", entries[0].ContextMap()["route"])

	state, found, err = store.GetRepoIndexState(repoPrefix)
	require.NoError(t, err)
	require.True(t, found)
	assert.Empty(t, ExtractorVersionStaleLangs(state.ExtractorVersions),
		"the restage must re-stamp the snapshot so it fires once per bump")

	core2, logs2 := observer.New(zap.DebugLevel)
	again := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.New(core2))
	_, err = again.ReconcileRepoCtx(t.Context(), entry, restarted.GetIndexer("repo").FileMtimes())
	require.NoError(t, err)
	entries = logs2.FilterMessage("daemon: reconciled repo from snapshot").All()
	require.Len(t, entries, 1)
	assert.Equal(t, "census_noop", entries[0].ContextMap()["route"])
}

func BenchmarkCleanReconcileDiscovery(b *testing.B) {
	root := b.TempDir()
	const files = 512
	mtimes := make(map[string]int64, files)
	for i := 0; i < files; i++ {
		rel := fmt.Sprintf("pkg/file-%04d.go", i)
		path := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(b, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(b, os.WriteFile(path, []byte("package sample\n"), 0o644))
		info, err := os.Stat(path)
		require.NoError(b, err)
		mtimes[rel] = info.ModTime().UnixNano()
	}
	idx := newTestIndexer(graph.New())
	idx.SetFileMtimes(mtimes)

	b.Run("authoritative_census", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
			if err != nil || len(changed) != 0 || len(deleted) != 0 {
				b.Fatalf("clean census: changed=%d deleted=%d err=%v", len(changed), len(deleted), err)
			}
			if _, err := idx.cleanCensusResult(context.Background(), detected, time.Now()); err != nil {
				b.Fatalf("clean result: %v", err)
			}
		}
	})

	b.Run("legacy_double_walk", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			changed, deleted, _, err := idx.changedSinceMtimesCensus(root)
			if err != nil || len(changed) != 0 || len(deleted) != 0 {
				b.Fatalf("clean census: changed=%d deleted=%d err=%v", len(changed), len(deleted), err)
			}
			result, err := idx.incrementalReindexPathsMode(root, nil, incrementalPathMode{detectDeletions: true})
			if err != nil || result.StaleFileCount != 0 || result.DeletedFileCount != 0 {
				b.Fatalf("legacy no-op: result=%+v err=%v", result, err)
			}
		}
	})
}
