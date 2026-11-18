package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

func TestDeltaWriterContextContentMatchCannotReuseBaseLedger(t *testing.T) {
	root := t.TempDir()
	src := []byte("package p\n\nfunc Target() {}\n")
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), src, 0o600))
	idx := newExtractSourceTestIndexer(t, config.Default().Index)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.True(t, idx.IndexedContentMatches("a.go", src), "the actual base metadata matches these bytes")
	base := idx.graph
	// Even a sidecar containing matching base metadata does not prove the
	// delta's complete covered/tombstoned file identity. It must not be read
	// through as a canonical-match shortcut for explicit delta processing.
	delta := graph.NewDeltaWriter(base, base)
	_, supported := any(delta).(fileMetaPathContextReader)
	require.False(t, supported)
	idx.graph = delta
	matched, err := idx.IndexedContentMatchesContext(t.Context(), "a.go", src)
	require.NoError(t, err)
	require.False(t, matched, "no base/sidecar match credit through a composed delta")
	caller, cancel := context.WithCancel(t.Context())
	cancel()
	matched, err = idx.IndexedContentMatchesContext(caller, "a.go", src)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, matched)
}
