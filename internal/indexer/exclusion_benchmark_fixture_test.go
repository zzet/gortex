package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWalkNegationBenchmarkPolicy(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "node_modules/pkg/schema.proto")
	writeExcludeFixture(t, target, "syntax = \"proto3\";\n")
	writeExcludeFixture(t, filepath.Join(root, "node_modules/pkg/drop.js"), "module.exports = {}\n")
	idx := newAdmissionTestIndexer(t, root, walkNegationPatterns...)
	reached := false
	require.NoError(t, idx.walkDisk(root, root, func(path string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		if path == target {
			reached = true
			require.False(t, idx.shouldExcludeRules(path, root, false))
		}
		return nil
	}))
	require.True(t, reached, "wildcard include must be reachable inside dependency tree")
	require.True(t, idx.shouldExcludeRules(filepath.Join(root, "node_modules/pkg/drop.js"), root, false))
}

func TestWatcherExclusionBenchmarkDeepFileExists(t *testing.T) {
	root := t.TempDir()
	writeWatcherExclusionFixture(t, root)
	info, err := os.Lstat(filepath.Join(root, "node_modules/pkg/a/b/c/d/e/f/index.js"))
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular())
}
