package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func TestConfiguredPrefixCheckDoesNotCopyAllRepos(t *testing.T) {
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	root := t.TempDir()
	entries := make([]config.RepoEntry, 1000)
	for i := range entries {
		entries[i] = config.RepoEntry{Path: filepath.Join(root, fmt.Sprintf("repo-%04d", i))}
	}
	cm.Global().Repos = entries
	require.NoError(t, cm.Global().Save())
	cm.RepoPathSnapshot()
	entry := entries[0]
	// TotalAlloc is process-wide, so background goroutines can add a little.
	// Many checks and a bound far below one copy of the list (about 115 KB)
	// keep the test stable.
	const checks = 100
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range checks {
		require.NoError(t, mi.configuredPrefixCollision(config.ResolvePrefix(entry), entry.Path))
	}
	runtime.ReadMemStats(&after)
	bytes := (after.TotalAlloc - before.TotalAlloc) / checks
	require.Lessf(t, bytes, uint64(16384), "a unique configured owner must not copy the whole repository list: %d bytes/check", bytes)
}
