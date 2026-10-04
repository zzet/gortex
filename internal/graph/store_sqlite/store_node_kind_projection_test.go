package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestNodeKindProjectionSQLChunksGenerationAndNoMetadataDecode(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "kinds.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	var nodes []*graph.Node
	var ids []string
	for i := 0; i < lookupChunkSize+3; i++ {
		id := fmt.Sprintf("repo/file.go::N%d", i)
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, FilePath: "repo/file.go", RepoPrefix: "repo", Meta: map[string]any{"body": "full metadata"}})
		ids = append(ids, id)
	}
	s.AddBatch(nodes, nil)
	positive := s.AtGeneration(7)
	positive.AddNode(&graph.Node{ID: ids[0], Kind: graph.KindContract, FilePath: "repo/contract.go", RepoPrefix: "other"})
	// Structural reads must succeed even when a full-node payload is corrupt.
	_, err = s.db.Exec(`UPDATE nodes SET meta = ? WHERE view_gen = 0`, []byte{0xff, 0xfe})
	require.NoError(t, err)
	ids = append(ids, "", ids[0], ids[0], "missing")
	rows, err := s.GetNodeKindsByIDsContext(t.Context(), ids)
	require.NoError(t, err)
	require.Len(t, rows, lookupChunkSize+3)
	require.Equal(t, graph.NodeKindRow{Kind: graph.KindFunction, FilePath: "repo/file.go", RepoPrefix: "repo"}, rows[ids[0]])
	rows, err = positive.GetNodeKindsByIDsContext(t.Context(), ids)
	require.NoError(t, err)
	require.Equal(t, map[string]graph.NodeKindRow{ids[0]: {Kind: graph.KindContract, FilePath: "repo/contract.go", RepoPrefix: "other"}}, rows)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err = s.GetNodeKindsByIDsContext(ctx, ids)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
	require.NoError(t, s.Close())
	rows, err = s.GetNodeKindsByIDsContext(t.Context(), ids)
	require.Error(t, err)
	require.Nil(t, rows)
}
