package indexer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func watcherBoundaryTestIndexer(t *testing.T, store graph.Store, root, prefix string) *Indexer {
	t.Helper()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(store, registry, cfg.Index, zap.NewNop())
	idx.SetRepoPrefix(prefix)
	_, err := idx.Index(root)
	require.NoError(t, err)
	return idx
}

func TestWatcher_SymbolChangeCallbackRunsAfterRepositoryLaneRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Original() {}\n")

	idx := watcherBoundaryTestIndexer(t, graph.New(), dir, "")
	watcher, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)

	var callbackOld, callbackNew []*graph.Node
	callbackResult := make(chan error, 1)
	watcher.OnSymbolChange(func(_ string, oldSymbols, newSymbols []*graph.Node) {
		callbackOld = oldSymbols
		callbackNew = newSymbols
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		callbackResult <- idx.coordinateRepositoryMutation(ctx, OutputEntryWatcherPatchGraph, func() error { return nil })
	})

	writeTestFile(t, path, "package main\n\nfunc Modified() {}\n")
	require.NoError(t, watcher.patchGraph(path, ChangeModified))
	select {
	case err := <-callbackResult:
		require.NoError(t, err, "callback must be able to re-enter the repository lane")
	case <-time.After(2 * time.Second):
		t.Fatal("symbol callback did not complete")
	}

	oldNames := make([]string, 0, len(callbackOld))
	for _, node := range callbackOld {
		oldNames = append(oldNames, node.Name)
	}
	newNames := make([]string, 0, len(callbackNew))
	for _, node := range callbackNew {
		if node.Kind != graph.KindFile && node.Kind != graph.KindImport {
			newNames = append(newNames, node.Name)
		}
	}
	assert.Contains(t, oldNames, "Original")
	assert.Contains(t, newNames, "Modified")

	liveByID := make(map[string]*graph.Node)
	for _, node := range idx.graph.GetFileNodes("main.go") {
		liveByID[node.ID] = node
	}
	for _, callbackNode := range callbackNew {
		if live := liveByID[callbackNode.ID]; live != nil {
			assert.NotSame(t, live, callbackNode, "callback payload must not alias graph-owned nodes")
		}
	}
}

func TestMultiWatcher_PointPatchUsesCurrentIndexerAfterReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Original() {}\n")

	const prefix = "repo"
	oldGraph := graph.New()
	oldIndexer := watcherBoundaryTestIndexer(t, oldGraph, dir, prefix)

	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	multi := NewMultiIndexer(oldGraph, registry, nil, nil, zap.NewNop())
	multi.repos[prefix] = &RepoMetadata{RepoPrefix: prefix, RootPath: dir}
	multi.indexers[prefix] = oldIndexer

	multiWatcher, err := NewMultiWatcher(multi, map[string]config.WatchConfig{
		prefix: {DebounceMs: 10},
	}, zap.NewNop())
	require.NoError(t, err)
	watcher := multiWatcher.watchers[prefix]
	require.NotNil(t, watcher)

	newGraph := graph.New()
	newIndexer := watcherBoundaryTestIndexer(t, newGraph, dir, prefix)
	newIndexer.attachRepositoryMutationCoordinator(multi.repositoryMutationCoordinator(prefix))
	multi.mu.Lock()
	multi.indexers[prefix] = newIndexer
	multi.mu.Unlock()

	writeTestFile(t, path, "package main\n\nfunc Modified() {}\n")
	require.NoError(t, watcher.patchGraph(path, ChangeModified))

	assert.NotEmpty(t, oldGraph.FindNodesByName("Original"), "retired Indexer graph must remain untouched")
	assert.Empty(t, oldGraph.FindNodesByName("Modified"), "retired Indexer must not receive the point patch")
	assert.Empty(t, newGraph.FindNodesByName("Original"))
	assert.NotEmpty(t, newGraph.FindNodesByName("Modified"), "current registered Indexer must receive the point patch")
}
