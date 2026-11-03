package graph

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fixtureMetadataContextReader interface {
	FileMetasByPathsContext(context.Context, string, []string) (map[string]FileMetaRow, error)
}

func TestGraphFileMetadataContextPreservesRequestedRowsAndCaller(t *testing.T) {
	g := New()
	require.NoError(t, g.SetFileMetas("repo", []FileMetaRow{{FilePath: "repo/a.go", ContentHash: "actual-a", Size: 13}, {FilePath: "repo/b.go", ContentHash: "actual-b", Size: 17}}))
	reader, supported := any(g).(fixtureMetadataContextReader)
	require.True(t, supported, "the supported in-memory graph must retain canonical content matching with the caller context")
	rows, err := reader.FileMetasByPathsContext(t.Context(), "repo", []string{"repo/a.go", "repo/missing.go"})
	require.NoError(t, err)
	legacy, err := g.FileMetasByPaths("repo", []string{"repo/a.go", "repo/missing.go"})
	require.NoError(t, err)
	require.Equal(t, legacy, rows)
	require.Len(t, rows, 1)
	delete(rows, "repo/a.go")
	again, err := reader.FileMetasByPathsContext(t.Context(), "repo", []string{"repo/a.go"})
	require.NoError(t, err)
	require.Len(t, again, 1, "the returned map is caller-owned")
	caller, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err = reader.FileMetasByPathsContext(caller, "repo", []string{"repo/a.go"})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	rows, err = reader.FileMetasByPathsContext(expired, "repo", []string{"repo/a.go"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, rows)
}

func TestGraphFileMetadataContextContentionNeverBlocksTheCaller(t *testing.T) {
	g := New()
	reader, supported := any(g).(fixtureMetadataContextReader)
	require.True(t, supported)
	g.fileMetasMu.Lock()
	defer g.fileMetasMu.Unlock()
	rows, err := reader.FileMetasByPathsContext(t.Context(), "repo", []string{"repo/a.go"})
	require.Error(t, err, "a held real metadata mutex cannot become an unbounded wait")
	require.Nil(t, rows, "contention cannot certify canonical spans")
	caller, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err = reader.FileMetasByPathsContext(caller, "repo", []string{"repo/a.go"})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	rows, err = reader.FileMetasByPathsContext(expired, "repo", []string{"repo/a.go"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, rows)
}
