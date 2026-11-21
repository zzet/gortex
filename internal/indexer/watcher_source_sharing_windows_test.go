//go:build windows

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
	"golang.org/x/sys/windows"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/platform"
)

func TestPointWatcherReportsSourceSharingFailureOnOwnTicket(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	const base = "package main\nfunc Target() {}\n"
	const earlier = base + "func Earlier() { Target() }\n"
	const later = earlier + "func Later() { Target() }\n"
	require.NoError(t, os.WriteFile(path, []byte(base), 0600))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "sharing.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	idx := New(store, registry, config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	require.NoError(t, idx.IndexFile(path))
	watcher, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(t, err)
	// The deliberate tool tickets own the generations in this test. Watching
	// an unrelated directory keeps native echoes out of the controlled order.
	require.NoError(t, watcher.Start([]string{t.TempDir()}))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(func() {
		cancel()
		require.NoError(t, watcher.Stop())
	})

	type readFailure struct {
		result *IndexResult
		err    error
		cause  error
	}
	readFailureAtLane := make(chan readFailure, 1)
	enteredRead, releaseRead := make(chan struct{}), make(chan struct{})
	generationForRead := make(chan uint64, 1)
	var firstGeneration uint64
	watcher.pointReindexRaw = func(file string, stamps *PublicationStamps) (*IndexResult, error) {
		var generation uint64
		select {
		case generation = <-generationForRead:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		first := generation == firstGeneration
		if first {
			close(enteredRead)
			select {
			case <-releaseRead:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		result, err := idx.incrementalWatcherPaths(root, []string{file}, watcher.pointRelationsMode(file, generation, stamps))
		if first {
			readFailureAtLane <- readFailure{result, err, idx.fileIndexFailureError(file)}
		}
		return result, err
	}

	require.NoError(t, os.WriteFile(path, []byte(earlier), 0600))
	first, err := watcher.EnqueueToolMutation(ctx, path)
	require.NoError(t, err)
	require.NotNil(t, first)
	firstGeneration = first.Generation
	generationForRead <- first.Generation
	select {
	case <-enteredRead:
	case <-ctx.Done():
		t.Fatal("the first point callback did not reach the source-read barrier")
	}

	nextPath := filepath.Join(root, "successor.tmp")
	require.NoError(t, os.WriteFile(nextPath, []byte(later), 0600))
	require.NoError(t, platform.ReplaceFile(nextPath, path))
	nativePath, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	handle, err := windows.CreateFile(nativePath, windows.GENERIC_WRITE,
		windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	releaseHandle := sync.OnceFunc(func() { require.NoError(t, windows.CloseHandle(handle)) })
	t.Cleanup(releaseHandle)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.EqualValues(t, len(later), info.Size(), "the successor is visible before ticket admission")
	_, denied := os.ReadFile(path)
	require.ErrorIs(t, denied, windows.ERROR_SHARING_VIOLATION)
	close(releaseRead)

	failed := awaitMutationSetTicket(t, first)
	var native readFailure
	select {
	case native = <-readFailureAtLane:
	case <-ctx.Done():
		t.Fatal("the point executor did not retain its per-file result")
	}
	require.NoError(t, native.err)
	require.NotNil(t, native.result)
	require.Equal(t, []string{path}, native.result.FailedFiles)
	require.Equal(t, []IndexError{{FilePath: path, Error: denied.Error()}}, native.result.Errors)
	require.ErrorIs(t, native.cause, windows.ERROR_SHARING_VIOLATION)
	require.Equal(t, first.Generation, failed.RequestedGeneration)
	require.Equal(t, first.Generation, failed.AppliedGeneration)
	require.ErrorContains(t, failed.Err, "watcher: exact-path reindex failed")
	require.False(t, failed.Reindexed)
	require.True(t, first.TierState().RelationsAt.IsZero())

	releaseHandle()
	second, err := watcher.EnqueueToolMutation(ctx, path)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, first.Generation+1, second.Generation)
	generationForRead <- second.Generation
	published := awaitMutationSetTicket(t, second)
	require.NoError(t, published.Err)
	require.True(t, published.Reindexed)
	require.Equal(t, second.Generation, published.AppliedGeneration)
	require.False(t, second.TierState().RelationsAt.IsZero())
	t.Logf("source sharing witness: read errno=%d, exact-path failure generation=%d, successor published generation=%d",
		windows.ERROR_SHARING_VIOLATION, first.Generation, second.Generation)
}
