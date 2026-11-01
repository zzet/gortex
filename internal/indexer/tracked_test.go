package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
)

func TestIsTracked(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main\n\nfunc A() {}\n"), 0o644))
	idx := newExtractSourceTestIndexer(t, config.Default().Index)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.go"), []byte("package main\n\nfunc B() {}\n"), 0o644))
	require.NoError(t, os.Remove(filepath.Join(dir, "a.go")))
	require.True(t, idx.IsTracked("a.go"), "a deleted file stays tracked until it is evicted")
	require.False(t, idx.IsTracked("b.go"), "a file created after indexing is not tracked")
}
