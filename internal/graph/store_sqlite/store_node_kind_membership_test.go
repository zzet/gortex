package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestNodeKindMembershipSQLParityChunksAndGeneration(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "membership.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	kinds := []graph.NodeKind{graph.KindContract, graph.KindContractBridge, graph.KindConfigKey, graph.KindFunction, graph.NodeKind("future_kind")}
	var nodes []*graph.Node
	var ids []string
	for i := 0; i < lookupChunkSize+3; i++ {
		id := fmt.Sprintf("repo/f.go::N%d", i)
		ids = append(ids, id)
		nodes = append(nodes, &graph.Node{ID: id, Kind: kinds[i%len(kinds)], FilePath: "repo/f.go", RepoPrefix: "repo"})
	}
	s.AddBatch(nodes, nil)
	positive := s.AtGeneration(7)
	positive.AddNode(&graph.Node{ID: ids[0], Kind: graph.KindFunction})
	positive.AddNode(&graph.Node{ID: ids[3], Kind: graph.KindConfigKey})
	_, err = s.writerDB.Exec(`UPDATE nodes SET meta = ? WHERE view_gen = 0`, []byte{0xff})
	require.NoError(t, err)
	ids = append(ids, "", ids[0], "missing")
	owned := []graph.NodeKind{graph.KindContract, graph.KindContractBridge, graph.KindConfigKey, graph.KindContract}
	before := append([]string(nil), ids...)
	structural, err := s.GetNodeKindsByIDsContext(t.Context(), ids)
	require.NoError(t, err)
	want := map[string]struct{}{}
	for id, row := range structural {
		if row.Kind == graph.KindContract || row.Kind == graph.KindContractBridge || row.Kind == graph.KindConfigKey {
			want[id] = struct{}{}
		}
	}
	got, err := s.GetNodeIDsByKindsContext(t.Context(), ids, owned)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, before, ids)
	got, err = positive.GetNodeIDsByKindsContext(t.Context(), ids, owned)
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{ids[3]: {}}, got)
	got, err = s.GetNodeIDsByKindsContext(t.Context(), ids, []graph.NodeKind{graph.NodeKind("future_kind")})
	require.NoError(t, err)
	require.Contains(t, got, ids[4])
	for _, input := range [][]string{nil, {"", "missing"}} {
		got, err = s.GetNodeIDsByKindsContext(t.Context(), input, owned)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Empty(t, got)
	}
	got, err = s.GetNodeIDsByKindsContext(t.Context(), ids, nil)
	require.NoError(t, err)
	require.Empty(t, got)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err = s.GetNodeIDsByKindsContext(ctx, ids, owned)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, got)
	require.NoError(t, s.Close())
	got, err = s.GetNodeIDsByKindsContext(t.Context(), ids, owned)
	require.Error(t, err)
	require.Nil(t, got)
}
