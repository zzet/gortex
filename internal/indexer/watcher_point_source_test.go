package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
	"go.uber.org/zap"
)

func pointSourceProofFixture(t *testing.T) (string, *Indexer, *store_sqlite.Store) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package p\nfunc Stable() {}\n"), 0600))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := newTestIndexer(store)
	t.Cleanup(idx.Close) // registered after SQLite: join extraction workers before store close
	idx.SetRootPath(root)
	_, err = idx.Index(root)
	require.NoError(t, err)
	require.NoError(t, idx.IndexFile(path))
	require.True(t, idx.indexedFileReceiptMatches(path))
	return path, idx, store
}

func TestPointDuplicateSourceRequiresKnownUnchangedProof(t *testing.T) {
	for _, name := range []string{"unchanged", "same_stat_changed_bytes", "missing_receipt", "unknown_backend", "derived_generation", "snapshot_source", "canceled", "outside_root"} {
		t.Run(name, func(t *testing.T) {
			path, idx, store := pointSourceProofFixture(t)
			ctx := t.Context()
			wantDuplicate := true
			switch name {
			case "same_stat_changed_bytes":
				before, err := os.Stat(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, []byte("package p\nfunc Edited() {}\n"), 0600))
				require.NoError(t, os.Chtimes(path, before.ModTime(), before.ModTime()))
				after, err := os.Stat(path)
				require.NoError(t, err)
				require.True(t, sameFileVersion(before, after))
				wantDuplicate = false
			case "missing_receipt":
				require.NoError(t, store.ReplaceFileMetas(idx.repoPrefix, nil))
				wantDuplicate = false
			case "unknown_backend":
				// Preserve real receipt/read behavior but hide the checked SQLite identity.
				idx.graph = struct{ *store_sqlite.Store }{store}
			case "derived_generation":
				idx.graph = store.AtGeneration(1)
				wantDuplicate = false // no matching file receipt in this generation
			case "snapshot_source":
				src, err := source.NewFilesystemSource(filepath.Dir(path))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, src.Close()) })
				idx.SetContentSource(src)
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "outside_root":
				idx.SetRootPath(t.TempDir())
				wantDuplicate = false
			}
			duplicate, content := idx.pointDuplicateSourceContent(ctx, path)
			require.Equal(t, wantDuplicate, duplicate)
			if name == "unchanged" {
				require.NotNil(t, content)
				require.False(t, content.Moved)
				require.Equal(t, idx.rootPath, content.Root)
				require.Equal(t, []string{path}, content.Paths)
			} else {
				require.Nil(t, content, "unknown, changed, or canceled proof must not restore a witness")
			}
		})
	}
}

func TestPointDuplicateSourceRejectsInterleavedIdentityOrRevision(t *testing.T) {
	for _, name := range []string{"owner", "graph", "file_metadata", "file_version", "snapshot", "canceled", "unknown_before", "escaped_symlink"} {
		t.Run(name, func(t *testing.T) {
			path, idx, store := pointSourceProofFixture(t)
			ctx := t.Context()
			before := idx.capturePointDuplicateState(ctx, path)
			require.NotNil(t, before)
			switch name {
			case "owner":
				idx.repoPrefix = "different-owner"
			case "graph":
				rev := store.MutationRevision()
				store.AddNode(&graph.Node{ID: "new-node", Name: "New", Kind: graph.KindFunction, FilePath: "main.go"})
				require.NotEqual(t, rev, store.MutationRevision())
			case "file_metadata":
				rows, err := store.FileMetasByPaths(idx.repoPrefix, []string{idx.prefixPath(idx.relKey(path))})
				require.NoError(t, err)
				row := rows[idx.prefixPath(idx.relKey(path))]
				row.Errors = "changed"
				require.NoError(t, store.SetFileMetas(idx.repoPrefix, []graph.FileMetaRow{row}))
			case "file_version":
				require.NoError(t, os.WriteFile(path, []byte("package p\nfunc LongerContent() {}\n"), 0600))
			case "snapshot":
				src, err := source.NewFilesystemSource(filepath.Dir(path))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, src.Close()) })
				idx.SetContentSource(src)
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "unknown_before":
				before = nil
			case "escaped_symlink":
				outside := filepath.Join(t.TempDir(), "outside.go")
				require.NoError(t, os.WriteFile(outside, []byte("package p"), 0600))
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Symlink(outside, path))
			}
			require.False(t, before.matches(idx.capturePointDuplicateState(ctx, path)))
		})
	}
}

func TestWatcherPointSourceDoesNotInferUnchangedFromResult(t *testing.T) {
	for _, name := range []string{"nil_result", "zero_counts", "failed_files", "executor_error"} {
		t.Run(name, func(t *testing.T) {
			root, idx, watcher := inertTestWatcher(t, "main.go", "package p\nfunc Stable() {}\n")
			watcher.degradedNoFsnotify = true
			t.Cleanup(idx.Close)
			t.Cleanup(func() { require.NoError(t, watcher.Stop()) })
			entered := false
			stop := errors.New("executor failed")
			watcher.pointReindexRaw = func(string, *PublicationStamps) (*IndexResult, error) {
				entered = true
				switch name {
				case "nil_result":
					return nil, nil
				case "failed_files":
					return &IndexResult{FailedFiles: []string{"main.go"}}, nil
				case "executor_error":
					return nil, stop
				default:
					return &IndexResult{}, nil
				}
			}
			observed := false
			err := watcher.patchGraphWithReceiptStateRawSource(t.Context(), idx, filepath.Join(root, "main.go"), ChangeModified, 0, true, nil, nil,
				func(*OutputSourceContent) { observed = true }, nil)
			require.True(t, entered)
			require.False(t, observed, "absence of reported work is not a positive unchanged-source proof")
			if name == "zero_counts" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestWatcherPointWriteInvalidatesBeforeExecutor(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	authority := NewOutputGenerationAuthority(f.lc.ViewLeases())
	f.mi.SetOutputGenerationAuthority(authority)
	root := f.gitRepo("before-write")
	tracked, err := f.lc.Register(t.Context(), config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	results, err := f.mi.IndexAll()
	require.NoError(t, err)
	require.Empty(t, results[tracked.Prefix].FailedFiles)
	idx := f.mi.GetIndexer(tracked.Prefix)
	path := filepath.Join(root, "before-write.go")
	require.NoError(t, idx.IndexFile(path))
	pin := f.lc.ViewLeases().AcquireBaseCorpus(tracked.Prefix)
	defer pin.Release()
	require.True(t, pin.OwnerPinned())
	require.True(t, pin.Witnessed())
	require.NoError(t, pin.ValidateAcceptedCurrent())
	require.NoError(t, os.WriteFile(path, []byte("package a\nfunc ChangedBeforeExecutor() {}\n"), 0600))
	require.False(t, idx.indexedFileReceiptMatches(path))
	watcher, err := NewWatcher(idx, config.Default().Watch, zap.NewNop())
	require.NoError(t, err)
	watcher.degradedNoFsnotify = true
	defer func() { require.NoError(t, watcher.Stop()) }()
	entered := false
	var during error
	stop := errors.New("stop at original raw entry before any graph write")
	watcher.pointReindexRaw = func(string, *PublicationStamps) (*IndexResult, error) {
		entered = true
		during = pin.ValidateAcceptedCurrent()
		return nil, stop
	}
	err = watcher.patchGraphWithReceiptState(path, ChangeModified, 0, false, nil)
	require.ErrorIs(t, err, stop)
	require.True(t, entered)
	require.ErrorIs(t, during, graphview.ErrBaseCorpusChanged, "raw source gate must still withdraw acceptance before executor entry")
	require.ErrorIs(t, pin.ValidateAcceptedCurrent(), graphview.ErrBaseCorpusChanged)
}
