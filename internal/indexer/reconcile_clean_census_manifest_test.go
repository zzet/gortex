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

func TestReconcileRepoCtxRoutesManifestOnlyChurnScopedAndConverges(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package sample\nfunc Alpha() {}\n")
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/sample\n")
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
	_, manifestTrackedByFullIndex := prior["go.mod"]
	require.True(t, manifestTrackedByFullIndex)

	// Create real manifest-only churn. A completed cold index now records
	// its successful manifest receipt, so an unchanged restart is a no-op.
	manifestPath := filepath.Join(root, "go.mod")
	writeFile(t, manifestPath, "module example.com/sample\nrequire example.com/dependency v1.0.0\n")
	changedAt := time.Unix(0, prior["go.mod"]).Add(time.Second)
	require.NoError(t, os.Chtimes(manifestPath, changedAt, changedAt))

	core, logs := observer.New(zap.DebugLevel)
	firstRestart := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.New(core))
	result, err := firstRestart.ReconcileRepoCtx(t.Context(), entry, prior)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.FullRetrack)
	assert.Equal(t, 1, result.StaleFileCount)
	entries := logs.FilterMessage("daemon: reconciled repo from snapshot").All()
	require.Len(t, entries, 1)
	assert.Equal(t, "scoped", entries[0].ContextMap()["route"])

	convergedMtimes := firstRestart.GetIndexer("repo").FileMtimes()
	_, manifestTracked := convergedMtimes["go.mod"]
	assert.True(t, manifestTracked)

	core, logs = observer.New(zap.DebugLevel)
	secondRestart := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.New(core))
	result, err = secondRestart.ReconcileRepoCtx(t.Context(), entry, convergedMtimes)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Zero(t, result.StaleFileCount)
	entries = logs.FilterMessage("daemon: reconciled repo from snapshot").All()
	require.Len(t, entries, 1)
	assert.Equal(t, "census_noop", entries[0].ContextMap()["route"])
}
