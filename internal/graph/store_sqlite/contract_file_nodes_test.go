package store_sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractFileNodesSQLitePreservesFullRowsAndSkipsUnrelatedPayload(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	require.NoError(t, err)
	closed := false
	t.Cleanup(func() {
		if !closed {
			require.NoError(t, s.Close())
		}
	})
	path := "repo/handler.go"
	node := &graph.Node{ID: path + "::Handler", Kind: graph.KindFunction, Name: "Handler", FilePath: path, RepoPrefix: "repo", Language: "go", WorkspaceID: "workspace", ProjectID: "project", Meta: map[string]any{"signature": "func Handler()", "nested": map[string]any{"value": "kept"}}}
	target := &graph.Node{ID: "contract", Kind: graph.KindContract, FilePath: "canonical.go"}
	require.NoError(t, s.AddBatchChecked([]*graph.Node{node, target}, []*graph.Edge{{From: node.ID, To: target.ID, Kind: graph.EdgeProvides, FilePath: path}}))
	_, err = s.writerDB.Exec(`UPDATE nodes SET meta = ? WHERE id = ? AND view_gen=0`, []byte("invalid canonical metadata"), target.ID)
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`UPDATE edges SET meta = ? WHERE from_id = ? AND view_gen=0`, []byte("invalid ordinary edge metadata"), node.ID)
	require.NoError(t, err)
	rows, err := graph.ContractFileNodesContext(t.Context(), s, "repo", []string{path, path, "missing.go"})
	require.NoError(t, err, "a node consumer must not decode outgoing edges or canonical groups")
	require.Equal(t, []*graph.Node{node}, rows[path])
	require.Empty(t, rows["missing.go"])
	_, err = s.writerDB.Exec(`UPDATE nodes SET meta = ? WHERE id = ? AND view_gen=0`, []byte("invalid selected metadata"), node.ID)
	require.NoError(t, err)
	rows, err = graph.ContractFileNodesContext(t.Context(), s, "repo", []string{path})
	require.Error(t, err)
	require.Nil(t, rows)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err = graph.ContractFileNodesContext(ctx, s, "repo", nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
	require.NoError(t, s.Close())
	closed = true
	rows, err = graph.ContractFileNodesContext(t.Context(), s, "repo", nil)
	require.Error(t, err, "even an empty checked read must detect a closed store")
	require.Nil(t, rows)
}

func TestContractFileNodesSQLiteKeepsPhysicalGenerationScope(t *testing.T) {
	s := newTestProjectionStore(t)
	path := "repo/handler.go"
	base := &graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, RepoPrefix: "repo", Meta: map[string]any{"stamp": "base"}}
	require.NoError(t, s.AddBatchChecked([]*graph.Node{base}, nil))
	_, selected, err := s.BeginPayloadGeneration(t.Context(), PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "file-nodes", LayerID: "selected", GenerationKind: "dirty", ConfigHash: "config", ExtractorVersions: `{"go":"1"}`, ResolverVersion: "test", CreatedAt: 1})
	require.NoError(t, err)
	current := *base
	current.Meta = map[string]any{"stamp": "selected"}
	require.NoError(t, selected.AddBatchChecked([]*graph.Node{&current}, nil))
	for _, row := range []struct {
		reader *Store
		want   *graph.Node
	}{{s, base}, {selected, &current}} {
		nodes, err := graph.ContractFileNodesContext(t.Context(), row.reader, "repo", []string{path})
		require.NoError(t, err)
		require.Equal(t, []*graph.Node{row.want}, nodes[path])
	}
}
