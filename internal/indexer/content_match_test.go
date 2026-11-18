package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
)

func TestIndexedContentMatches(t *testing.T) {
	dir := t.TempDir()
	plain := []byte("package main\n\nfunc A() {}\n")
	// The recorded identity is over transformed bytes, so a raw read that
	// still carries the BOM the index stripped must match too.
	bom := append([]byte{0xef, 0xbb, 0xbf}, []byte("package main\n\nfunc B() {}\n")...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), plain, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.go"), bom, 0o644))
	idx := newExtractSourceTestIndexer(t, config.Default().Index)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	require.True(t, idx.IndexedContentMatches("a.go", plain))
	require.True(t, idx.IndexedContentMatches("b.go", bom))
	require.False(t, idx.IndexedContentMatches("a.go", []byte("package main\n\n\nfunc A() {}\n")),
		"bytes the index has not seen must not vouch for stored spans")
	require.False(t, idx.IndexedContentMatches("missing.go", plain), "an untracked file has no recorded identity")
}
