package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

func TestIndexFileRejectsMissingAndDirectoryBeforeAdmission(t *testing.T) {
	root := t.TempDir()
	coordinator := newRepositoryMutationCoordinator(func([]string) (*IndexResult, error) {
		t.Fatal("invalid IndexFile request reached the repository executor")
		return nil, nil
	})
	idx := &Indexer{rootPath: root}
	idx.attachRepositoryMutationCoordinator(coordinator)

	require.Error(t, idx.IndexFile("missing.go"))
	require.ErrorContains(t, idx.IndexFile("."), "repository root")
	directory := filepath.Join(root, "pkg")
	require.NoError(t, os.Mkdir(directory, 0o755))
	require.ErrorContains(t, idx.IndexFile(directory), "not a regular file")
	require.Zero(t, coordinator.stats().RequestedGeneration)
}

func TestIndexFileRevalidatesAfterWaitingForRepositoryLane(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "queued.go")
	require.NoError(t, os.WriteFile(filePath, []byte("package queued\n"), 0o644))

	coordinator := newRepositoryMutationCoordinator(nil)
	var batchGate sync.RWMutex
	coordinator.batchMutationGate = &batchGate
	idx := &Indexer{rootPath: root}
	idx.attachRepositoryMutationCoordinator(coordinator)

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- coordinator.runExclusive(context.Background(), func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	require.Eventually(t, func() bool {
		select {
		case <-firstEntered:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond, "first mutation never acquired the repository lane")

	admitted := make(chan struct{})
	coordinator.batchAdmissionHook = sync.OnceFunc(func() { close(admitted) })
	done := make(chan error, 1)
	go func() { done <- idx.IndexFile(filePath) }()
	require.Eventually(t, func() bool {
		select {
		case <-admitted:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond, "IndexFile never acquired batch admission")
	require.NoError(t, os.Remove(filePath))
	close(releaseFirst)

	require.NoError(t, <-firstDone)
	require.ErrorContains(t, <-done, "queued.go")
}

func TestDirectMutationAPIsUseStableLaneAndCurrentIndexer(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(filePath, []byte("package main\nfunc Original() {}\n"), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1
	mi := NewMultiIndexer(g, reg, search.NewNull(), nil, zap.NewNop())
	current := mi.newPerRepoIndexer(cfg.Index)
	current.SetRepoPrefix("repo")
	// Fixture construction mirrors TrackRepo/IndexRepo: the per-repository
	// Indexer is not live until it is published in mi.indexers, so initialize
	// its graph through the already-coordinated raw full-tree primitive.
	_, err := current.indexCtxRaw(context.Background(), root)
	require.NoError(t, err)
	coordinator := mi.repositoryMutationCoordinator("repo")
	current.attachRepositoryMutationCoordinator(coordinator)
	mi.mu.Lock()
	mi.indexers["repo"] = current
	mi.repos["repo"] = &RepoMetadata{
		RepoPrefix: "repo",
		RootPath:   root,
		FileCount:  1,
		NodeCount:  len(g.GetFileNodes("repo/main.go")),
		FileMtimes: current.FileMtimes(),
	}
	mi.mu.Unlock()

	// The stale handle intentionally has an empty parser registry and a separate
	// graph. Success therefore proves raw work selected the current registered
	// Indexer after admission on the prefix-owned lane.
	stale := New(graph.New(), parser.NewRegistry(), cfg.Index, zap.NewNop())
	stale.SetRepoPrefix("repo")
	stale.storeRootPath(root)
	stale.repositoryMutationOwner = mi
	stale.attachRepositoryMutationCoordinator(coordinator)

	entered := make(chan struct{})
	release := make(chan struct{})
	blockDone := make(chan error, 1)
	go func() {
		blockDone <- stale.coordinateRepositoryMutation(context.Background(), OutputEntryIndexFile, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	require.NoError(t, os.WriteFile(filePath, []byte("package main\nfunc Modern() {}\n"), 0o644))
	indexDone := make(chan error, 1)
	go func() { indexDone <- stale.IndexFile(filePath) }()
	select {
	case err := <-indexDone:
		t.Fatalf("IndexFile crossed an occupied repository lane: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-blockDone)
	require.NoError(t, <-indexDone)
	require.NotEmpty(t, g.FindNodesByName("Modern"))
	require.Empty(t, g.FindNodesByName("Original"))

	require.NoError(t, os.WriteFile(filePath, []byte("package main\nfunc Compatible() {}\n"), 0o644))
	require.NoError(t, stale.IndexFileNoResolve("main.go"))
	require.NotEmpty(t, g.FindNodesByName("Compatible"), "deprecated alias must run the complete modern point tail")
	require.NoError(t, stale.ReresolveFileScoped("main.go"))

	nodesRemoved, _ := stale.EvictFile("main.go")
	require.Positive(t, nodesRemoved)
	_, statErr := os.Stat(filePath)
	require.NoError(t, statErr, "forced eviction must not delete or require deletion of the disk file")
	require.Empty(t, g.GetFileNodes("repo/main.go"))
	require.False(t, current.incrementalPathOwned(filePath), "file metadata sidecar survived forced eviction")
	current.mtimeMu.RLock()
	_, mtimePresent := current.fileMtimes["main.go"]
	current.mtimeMu.RUnlock()
	require.False(t, mtimePresent, "file mtime survived forced eviction")
}

func TestEvictFileClearsOrphanSidecarsWithoutGraphOwnership(t *testing.T) {
	root := t.TempDir()
	g := graph.New()
	cfg := config.Default()
	idx := New(g, parser.NewRegistry(), cfg.Index, zap.NewNop())
	idx.SetRepoPrefix("repo")
	idx.storeRootPath(root)
	idx.SetFileMtimes(map[string]int64{"orphan.go": 42})
	require.NoError(t, g.SetFileMetas("repo", []graph.FileMetaRow{{
		FilePath:    "repo/orphan.go",
		ContentHash: "orphaned-sidecar",
	}}))
	require.Empty(t, g.GetFileNodes("repo/orphan.go"), "fixture must not have graph ownership")

	nodesRemoved, edgesRemoved := idx.EvictFile("orphan.go")
	require.Zero(t, nodesRemoved)
	require.Zero(t, edgesRemoved)
	rows, err := g.FileMetasForRepo("repo")
	require.NoError(t, err)
	require.Empty(t, rows, "forced eviction left an orphan file metadata sidecar")
	require.NotContains(t, idx.FileMtimes(), "orphan.go", "forced eviction left an orphan mtime")
}
