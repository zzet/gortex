package indexer

import (
	"context"
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
)

// TestWarmRestart_PrunesDeletedFileMtimes_FastPath is the end-to-end
// regression for the "warm restart re-indexes everything even though nothing
// changed on disk" bug.
//
// Root cause: the full-index mtime persist used to be an upsert
// (BulkSetFileMtimes), so a file deleted since the last index left its row in
// the store forever. On every warm restart HasChangesSinceMtimes hit that
// phantom-deletion row, flagged the repo as changed, and forced a full
// re-track + all global passes — which never converged, because the re-track
// re-persisted with the same upsert.
//
// The fix makes the full-index persist authoritative (ReplaceFileMtimes). This
// test proves: (1) a deleted file's row is pruned on the next full index, and
// (2) the subsequent unchanged warm restart takes the fast path
// (HasChangesSinceMtimes == false).
func TestWarmRestart_PrunesDeletedFileMtimes_FastPath(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	writeFile(t, filepath.Join(repoPath, "a.go"), "package main\nfunc Alpha() {}\n")
	writeFile(t, filepath.Join(repoPath, "b.go"), "package main\nfunc Beta() {}\n")

	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// sqlite must advertise the replace capability or the fix can't engage.
	_, isReplacer := graph.Store(s).(graph.FileMtimeReplacer)
	require.True(t, isReplacer, "sqlite store must implement FileMtimeReplacer")

	newIdx := func() *Indexer {
		idx := New(graph.Store(s), newTestRegistry(), config.Default().Index, zap.NewNop())
		idx.SetRepoPrefix("repo")
		idx.SetRootPath(repoPath)
		return idx
	}

	// First index: both files persisted.
	_, err = newIdx().IndexCtx(context.Background(), repoPath)
	require.NoError(t, err)

	got := s.LoadFileMtimes("repo")
	require.Contains(t, got, "a.go", "first index must persist a.go")
	require.Contains(t, got, "b.go", "first index must persist b.go")

	// Delete b.go on disk — the analogue of the deleted store_ladybug files.
	require.NoError(t, os.Remove(filepath.Join(repoPath, "b.go")))

	// Warm restart #1: a fresh indexer seeded from the persisted snapshot
	// must DETECT the deletion — this is correct behaviour the first time.
	idxR1 := newIdx()
	idxR1.SetFileMtimes(s.LoadFileMtimes("repo"))
	require.True(t, idxR1.HasChangesSinceMtimes(repoPath),
		"the first warm restart after a deletion must detect the change")

	// Re-track (full index). The authoritative persist must prune b.go.
	_, err = idxR1.IndexCtx(context.Background(), repoPath)
	require.NoError(t, err)

	got = s.LoadFileMtimes("repo")
	assert.Contains(t, got, "a.go", "surviving file must stay persisted")
	assert.NotContains(t, got, "b.go",
		"deleted file's mtime row must be pruned by the authoritative full-index persist")

	// Warm restart #2: nothing changed and the persisted set now matches
	// disk, so the reconcile must take the FAST PATH — no phantom deletion,
	// no full re-track, no global passes.
	idxR2 := newIdx()
	idxR2.SetFileMtimes(s.LoadFileMtimes("repo"))
	assert.False(t, idxR2.HasChangesSinceMtimes(repoPath),
		"after pruning, an unchanged warm restart must take the fast path")
}

// The warm-restart probe counts the root go.mod, as the census does: an
// unchanged one is no change, and an edited one is.
func TestWarmRestart_ProbeSeesGoModEdits(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "repo")
	writeExcludeFixture(t, filepath.Join(repoPath, "go.mod"), "module example.com/repo\n")
	writeExcludeFixture(t, filepath.Join(repoPath, "a.go"), "package main\nfunc Alpha() {}\n")

	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	newIdx := func() *Indexer {
		idx := New(graph.Store(s), newTestRegistry(), config.Default().Index, zap.NewNop())
		idx.SetRepoPrefix("repo")
		idx.SetRootPath(repoPath)
		idx.SetFileMtimes(s.LoadFileMtimes("repo"))
		return idx
	}
	_, err = newIdx().IndexCtx(context.Background(), repoPath)
	require.NoError(t, err)
	require.Contains(t, s.LoadFileMtimes("repo"), "go.mod")

	assert.False(t, newIdx().HasChangesSinceMtimes(repoPath), "nothing changed since the index")

	later := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(repoPath, "go.mod"), later, later))
	assert.True(t, newIdx().HasChangesSinceMtimes(repoPath), "an edited go.mod is a change")
}

// TestIncrementalReindex_PrunesDeletedFileMtimes covers the watcher /
// incremental path: a file deleted between scans must have its persisted
// mtime row removed by IncrementalReindexPaths (via DeleteFileMtimes), not just
// its in-memory entry — otherwise the next warm restart re-discovers it as a
// phantom deletion.
func TestIncrementalReindex_PrunesDeletedFileMtimes(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	writeFile(t, filepath.Join(repoPath, "keep.go"), "package main\nfunc Keep() {}\n")
	writeFile(t, filepath.Join(repoPath, "drop.go"), "package main\nfunc Drop() {}\n")

	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	idx := New(graph.Store(s), newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("repo")
	idx.SetRootPath(repoPath)

	_, err = idx.IndexCtx(context.Background(), repoPath)
	require.NoError(t, err)
	require.Contains(t, s.LoadFileMtimes("repo"), "drop.go")

	// Delete drop.go and run the incremental path (the janitor / watcher
	// route), not a full re-track.
	require.NoError(t, os.Remove(filepath.Join(repoPath, "drop.go")))
	res, err := idx.IncrementalReindexPaths(repoPath, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.DeletedFileCount, "incremental reindex must report the deletion")

	got := s.LoadFileMtimes("repo")
	assert.Contains(t, got, "keep.go")
	assert.NotContains(t, got, "drop.go",
		"incremental reindex must prune the deleted file's persisted mtime row")
}
