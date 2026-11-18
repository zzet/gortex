package indexer

import (
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

func censusCountSource(t *testing.T, logs *observer.ObservedLogs) string {
	t.Helper()
	entries := logs.FilterMessage("indexer: clean census counts").All()
	require.Len(t, entries, 1)
	source, _ := entries[0].ContextMap()["source"].(string)
	return source
}

// A restart over an unchanged tree reports the counts the last index stored
// and counts no row.
func TestCleanCensusReusesTheStoredCounts(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package sample\nfunc Alpha() {}\nfunc Beta() { Alpha() }\n")
	entry := config.RepoEntry{Path: root, Name: "repo"}
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{entry}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	seed := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = seed.IndexAll()
	require.NoError(t, err)
	stored, found, err := store.GetRepoIndexState("repo")
	require.NoError(t, err)
	require.True(t, found)
	require.Positive(t, stored.NodeCount)

	counted := cleanCensusCounts.Load()
	core, logs := observer.New(zap.InfoLevel)
	restarted := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.New(core))
	result, err := restarted.ReconcileRepoCtx(t.Context(), entry, seed.GetIndexer("repo").FileMtimes())
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "stored", censusCountSource(t, logs))
	assert.Equal(t, counted, cleanCensusCounts.Load(), "a clean census counted rows")
	assert.Equal(t, stored.NodeCount, result.NodeCount)
	assert.Equal(t, stored.EdgeCount, result.EdgeCount)
}

// A clean census with no stored state counts once and stores the count; the
// next census reuses it.
func TestCleanCensusWithNothingStoredCountsOnceAndStores(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	store.AddBatch([]*graph.Node{
		{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "repo/a.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "repo/a.go", RepoPrefix: "repo"},
	}, []*graph.Edge{{From: "repo/a.go::B", To: "repo/a.go::A", Kind: graph.EdgeCalls, FilePath: "repo/a.go"}})
	core, logs := observer.New(zap.InfoLevel)
	idx := New(store, newTestRegistry(), config.Default().Index, zap.New(core))
	defer idx.Close()
	idx.SetRepoPrefix("repo")
	idx.SetRootPath(t.TempDir())

	counted := cleanCensusCounts.Load()
	first, err := idx.cleanCensusResult(t.Context(), 1, time.Now())
	require.NoError(t, err)
	assert.Equal(t, "counted", censusCountSource(t, logs))
	assert.Equal(t, counted+1, cleanCensusCounts.Load())
	assert.Equal(t, 2, first.NodeCount)
	stored, found, err := store.GetRepoIndexState("repo")
	require.NoError(t, err)
	require.True(t, found, "the counted census stored nothing")
	assert.Equal(t, 2, stored.NodeCount)
	assert.Equal(t, 1, stored.EdgeCount)

	core2, logs2 := observer.New(zap.InfoLevel)
	idx.logger = zap.New(core2)
	second, err := idx.cleanCensusResult(t.Context(), 1, time.Now())
	require.NoError(t, err)
	assert.Equal(t, "stored", censusCountSource(t, logs2))
	assert.Equal(t, counted+1, cleanCensusCounts.Load(), "the second census counted again")
	assert.Equal(t, first.NodeCount, second.NodeCount)
	_ = logs
}

// A changed tree does not take the clean path: the scoped reindex counts, and
// no clean census reports counts.
func TestChangedTreeDoesNotTakeTheStoredCensusCounts(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	writeFile(t, path, "package sample\nfunc Alpha() {}\n")
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

	writeFile(t, path, "package sample\nfunc Alpha() {}\nfunc Gamma() {}\n")
	changedAt := time.Unix(0, prior["main.go"]).Add(time.Second)
	require.NoError(t, os.Chtimes(path, changedAt, changedAt))
	core, logs := observer.New(zap.DebugLevel)
	restarted := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.New(core))
	result, err := restarted.ReconcileRepoCtx(t.Context(), entry, prior)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Empty(t, logs.FilterMessage("indexer: clean census counts").All())
	entries := logs.FilterMessage("daemon: reconciled repo from snapshot").All()
	require.Len(t, entries, 1)
	assert.NotEqual(t, "census_noop", entries[0].ContextMap()["route"])
}
