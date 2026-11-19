package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestFullIndexPersistsUnreadableFilesAndClearsRecovery(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"denied.go", "blocked.go"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("package p\nfunc Keep() {}\n"), 0o600))
	}
	fsSource, err := source.NewFilesystemSource(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsSource.Close() })
	failing := &failingIndexSource{ContentSource: fsSource, opens: map[string]int{}, errs: map[string][]error{
		"denied.go": {syscall.EACCES}, "blocked.go": {syscall.EPERM},
	}}
	g := &failureCountingStore{Graph: graph.New()}
	idx := newContentSourceIndexer(t, g)
	idx.SetRepoPrefix("repo")
	idx.SetContentSource(failing)
	result, err := idx.Index(root)
	require.NoError(t, err)
	require.Len(t, result.FailedFiles, 2)
	require.Len(t, result.Errors, 2)
	require.Len(t, LoadFileIndexFailures(g, "repo"), 2)
	require.Empty(t, LoadFileIndexFailures(g, "other"))
	require.Equal(t, 1, g.writes, "full workers publish one failure batch")
	metas, err := g.FileMetasForRepo("repo")
	require.NoError(t, err)
	require.Empty(t, metas, "failed reads must not mint successful FileMeta receipts")

	idx.SetContentSource(fsSource)
	result, err = idx.Index(root)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.Empty(t, LoadFileIndexFailures(g, "repo"))
	metas, err = g.FileMetasForRepo("repo")
	require.NoError(t, err)
	require.Len(t, metas, 2)
}

type failedWalkIndexSource struct{ source.ContentSource }

func (s failedWalkIndexSource) Walk(context.Context, func(source.FileMeta) error) error {
	return &os.PathError{Op: "readdir", Path: ".", Err: syscall.EACCES}
}

func TestFullIndexPersistsWalkFailure(t *testing.T) {
	root := t.TempDir()
	fsSource, err := source.NewFilesystemSource(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsSource.Close() })
	g := graph.New()
	idx := newContentSourceIndexer(t, g)
	idx.SetContentSource(failedWalkIndexSource{ContentSource: fsSource})
	_, err = idx.Index(root)
	require.ErrorIs(t, err, syscall.EACCES)
	rows := LoadFileIndexFailures(g, "")
	require.Len(t, rows, 1)
	require.True(t, rows[0].PermissionDenied)
	require.Contains(t, rows[0].Error, "readdir")
}

func TestIncrementalDiscoveryDeletesNeverIndexedFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "new.go")
	g := graph.New()
	idx := newContentSourceIndexer(t, g)
	idx.SetRootPath(root)
	idx.noteFileIndexFailure(path, &os.PathError{Op: "open", Path: path, Err: syscall.EACCES})
	idx.flushFileIndexFailures()
	result, err := idx.IncrementalReindexPaths(root, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.DeletedFileCount)
	require.Empty(t, LoadFileIndexFailures(g, ""))
}

func TestDirectIndexFilePreservesPermissionError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "new.go")
	require.NoError(t, os.WriteFile(path, []byte("package p\n"), 0o600))
	fsSource, err := source.NewFilesystemSource(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsSource.Close() })
	idx := newContentSourceIndexer(t, graph.New())
	idx.SetRootPath(root)
	idx.SetContentSource(&failingIndexSource{ContentSource: fsSource, opens: map[string]int{}, errs: map[string][]error{"new.go": {syscall.EPERM}}})
	err = idx.IndexFile(path)
	require.ErrorIs(t, err, syscall.EPERM)
	require.False(t, errors.Is(err, errFileVersionChanged))
}

func TestFullIndexFailureLedgerSurvivesShadowDrainAndReopen(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "denied.go"), []byte("package p\n"), 0o600))
	fsSource, err := source.NewFilesystemSource(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsSource.Close() })
	failing := &failingIndexSource{ContentSource: fsSource, opens: map[string]int{}, errs: map[string][]error{"denied.go": {syscall.EPERM}}}
	dbPath := filepath.Join(t.TempDir(), "graph.db")
	for pass := 0; pass < 2; pass++ {
		g, err := store_sqlite.Open(dbPath)
		require.NoError(t, err)
		idx := newContentSourceIndexer(t, g)
		idx.SetRepoPrefix("repo")
		idx.SetContentSource(failing)
		result, err := idx.Index(root)
		require.NoError(t, err)
		require.Len(t, result.FailedFiles, 1)
		rows, err := LoadFileIndexFailuresWithError(g, "repo")
		require.NoError(t, err)
		require.Len(t, rows, 1, "unchanged failures survive generation eviction on repeated full indexes")
		require.True(t, rows[0].PermissionDenied)
		require.NoError(t, g.Close())
	}
	g, err := store_sqlite.Open(dbPath)
	require.NoError(t, err)
	defer g.Close()
	rows, err := LoadFileIndexFailuresWithError(g, "repo")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	idx := newContentSourceIndexer(t, g)
	idx.SetRepoPrefix("repo")
	idx.SetContentSource(fsSource)
	result, err := idx.Index(root)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.Empty(t, LoadFileIndexFailures(g, "repo"))
}

type unavailableFailureStore struct {
	*graph.Graph
	readErr       error
	reads, writes int
}

func (g *unavailableFailureStore) FileIndexFailuresForRepo(repo string) ([]graph.FileIndexFailure, error) {
	g.reads++
	if g.readErr != nil {
		return nil, g.readErr
	}
	return g.Graph.FileIndexFailuresForRepo(repo)
}

func (g *unavailableFailureStore) ReplaceFileIndexFailures(repo string, rows []graph.FileIndexFailure) error {
	g.writes++
	return g.Graph.ReplaceFileIndexFailures(repo, rows)
}

func TestFileFailureStoreReadErrorRetainsPendingUpdates(t *testing.T) {
	root := t.TempDir()
	readErr := errors.New("failure ledger unavailable")
	g := &unavailableFailureStore{Graph: graph.New(), readErr: readErr}
	require.NoError(t, g.Graph.ReplaceFileIndexFailures("", []graph.FileIndexFailure{
		{Path: "old.go", Error: "old failure"}, {Path: "recovered.go", Error: "recovered failure"},
	}))
	_, err := LoadFileIndexFailuresWithError(g, "")
	require.ErrorIs(t, err, readErr)
	idx := newContentSourceIndexer(t, g)
	idx.SetRootPath(root)
	idx.loadFileIndexFailures()
	idx.noteFileIndexFailure(filepath.Join(root, "new.go"), syscall.EACCES)
	idx.noteFileIndexFailure(filepath.Join(root, "second.go"), syscall.EPERM)
	idx.noteFileIndexFailure(filepath.Join(root, "recovered.go"), nil)
	idx.flushFileIndexFailures()
	require.Equal(t, 2, g.reads, "per-file observations do not retry or reset the failed initial read")
	require.Zero(t, g.writes, "an unknown prior snapshot must not be overwritten")
	g.readErr = nil
	idx.loadFileIndexFailures()
	idx.flushFileIndexFailures()
	rows, err := g.Graph.FileIndexFailuresForRepo("")
	require.NoError(t, err)
	var paths []string
	for _, row := range rows {
		paths = append(paths, row.Path)
	}
	require.ElementsMatch(t, []string{"old.go", "new.go", "second.go"}, paths)
	require.Equal(t, 1, g.writes)
}

type failingIndexSource struct {
	source.ContentSource
	mu    sync.Mutex
	errs  map[string][]error
	opens map[string]int
}

func (s *failingIndexSource) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	s.mu.Lock()
	s.opens[path]++
	errs := s.errs[path]
	if len(errs) > 0 {
		err := errs[0]
		if len(errs) > 1 {
			s.errs[path] = errs[1:]
		}
		s.mu.Unlock()
		return nil, source.FileMeta{}, fmt.Errorf("source open failed: %w", &os.PathError{Op: "open", Path: path, Err: err})
	}
	s.mu.Unlock()
	return s.ContentSource.Open(path)
}

type failureCountingStore struct {
	*graph.Graph
	writes int
}

func (g *failureCountingStore) ReplaceFileIndexFailures(repo string, rows []graph.FileIndexFailure) error {
	g.writes++
	return g.Graph.ReplaceFileIndexFailures(repo, rows)
}

func TestIncrementalPermissionFailuresRetainCauseWithoutRetry(t *testing.T) {
	root := t.TempDir()
	paths := []string{filepath.Join(root, "denied.go"), filepath.Join(root, "blocked.go")}
	for _, path := range paths {
		require.NoError(t, os.WriteFile(path, []byte("package p\nfunc Keep() {}\n"), 0o600))
	}
	g := &failureCountingStore{Graph: graph.New()}
	idx := newContentSourceIndexer(t, g)
	idx.SetRepoPrefix("repo")
	idx.SetWorkspaceID("workspace")
	idx.SetProjectID("project")
	_, err := idx.Index(root)
	require.NoError(t, err)
	beforeNodes, beforeEdges := graphShape(g)
	beforeMtimes := idx.FileMtimes()
	fsSource, err := source.NewFilesystemSource(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsSource.Close() })
	failing := &failingIndexSource{ContentSource: fsSource, opens: map[string]int{}, errs: map[string][]error{
		"denied.go": {syscall.EACCES}, "blocked.go": {syscall.EPERM},
	}}
	idx.SetContentSource(failing)
	core, logs := observer.New(zap.DebugLevel)
	idx.logger = zap.New(core)
	_, _, failed, raced := idx.reindexIncrementalFilesBatched(paths, nil, nil, false)
	require.ElementsMatch(t, paths, failed)
	require.Empty(t, raced)
	require.Equal(t, map[string]int{"denied.go": 1, "blocked.go": 1}, failing.opens)
	require.ErrorIs(t, idx.fileIndexFailureError(paths[0]), syscall.EACCES)
	require.ErrorIs(t, idx.fileIndexFailureError(paths[1]), syscall.EPERM)
	rows := LoadFileIndexFailures(g, "repo")
	require.Len(t, rows, 2)
	require.Equal(t, 1, g.writes, "the batch persists the ledger once, not once per file")
	for _, row := range rows {
		require.True(t, row.PermissionDenied)
		require.Contains(t, row.Error, "source open failed: open")
		require.Equal(t, "repo", row.RepoPrefix)
		require.Equal(t, "workspace", row.WorkspaceID)
		require.Equal(t, "project", row.ProjectID)
	}
	afterNodes, afterEdges := graphShape(g)
	require.Equal(t, beforeNodes, afterNodes)
	require.Equal(t, beforeEdges, afterEdges)
	require.Equal(t, beforeMtimes, idx.FileMtimes())
	warnings := logs.FilterMessage("indexer: repository indexing incomplete due to filesystem permissions")
	require.Equal(t, 1, warnings.Len())
	require.Equal(t, int64(2), warnings.All()[0].ContextMap()["unreadable_files"])
	require.Contains(t, warnings.All()[0].ContextMap()["error"], "operation not permitted")
	_, _, _, _ = idx.reindexIncrementalFilesBatched(paths, nil, nil, false)
	require.Equal(t, 1, logs.FilterMessage("indexer: repository indexing incomplete due to filesystem permissions").Len())
	require.Equal(t, 1, g.writes, "unchanged failures do not rewrite the ledger")

	idx.SetContentSource(fsSource)
	_, _, failed, raced = idx.reindexIncrementalFilesBatched(paths, nil, nil, false)
	require.Empty(t, failed)
	require.Empty(t, raced)
	require.Empty(t, LoadFileIndexFailures(g, "repo"), "successful inert receipts clear failures too")
}

func TestIncrementalRetryLogsTerminalReadError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.go")
	require.NoError(t, os.WriteFile(path, []byte("package p\n"), 0o600))
	g := graph.New()
	idx := newContentSourceIndexer(t, g)
	idx.SetRootPath(root)
	fsSource, err := source.NewFilesystemSource(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fsSource.Close() })
	failing := &failingIndexSource{ContentSource: fsSource, opens: map[string]int{}, errs: map[string][]error{
		"source.go": {syscall.EIO, syscall.ENOSPC},
	}}
	idx.SetContentSource(failing)
	core, logs := observer.New(zap.DebugLevel)
	idx.logger = zap.New(core)
	_, _, failed, raced := idx.reindexIncrementalFilesBatched([]string{path}, nil, nil, false)
	require.Equal(t, []string{path}, failed)
	require.Empty(t, raced)
	require.Equal(t, 2, failing.opens["source.go"])
	require.ErrorIs(t, idx.fileIndexFailureError(path), syscall.ENOSPC)
	warnings := logs.FilterMessage("incremental reindex: file failed after retry").All()
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0].ContextMap()["error"], "no space left on device")
	var plan DerivedInvalidationPlan
	idx.evictDeletedFilesBatched([]string{"source.go"}, &plan, nil)
	require.Empty(t, LoadFileIndexFailures(g, ""), "explicit deletion clears even a never-indexed failure")
}

func TestFileReadReceiptKeepsStatErrorDistinctFromVersionRace(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.go")
	require.NoError(t, os.WriteFile(path, []byte("package p\n"), 0o600))
	_, version, err := readOSFileWithVersion(path)
	require.NoError(t, err)
	idx := newContentSourceIndexer(t, graph.New())
	idx.SetRootPath(root)
	badPath := filepath.Join(path, "child.go")
	_, _, readErr := readOSFileWithVersion(badPath)
	require.ErrorIs(t, readErr, syscall.ENOTDIR)
	_, statErr := os.Stat(badPath)
	require.Error(t, statErr)
	require.Equal(t, statErr, readErr, "the original platform-specific stat error is preserved before attempting a read")
	fresh, stale := idx.recordFileReadVersionsBatched([]fileReadReceipt{{absPath: badPath, mtimeKey: "source.go/child.go", readVersion: version}})
	require.Empty(t, fresh)
	require.Equal(t, []string{badPath}, stale)
	require.ErrorIs(t, idx.fileIndexFailureError(badPath), syscall.ENOTDIR)
	require.False(t, errors.Is(idx.fileIndexFailureError(badPath), errFileVersionChanged))
	require.False(t, idx.recordFileReadVersion("source.go/child.go", badPath, version))
	require.ErrorIs(t, idx.fileIndexFailureError(badPath), syscall.ENOTDIR)
}
