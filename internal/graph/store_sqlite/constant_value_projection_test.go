package store_sqlite

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"path/filepath"
	"testing"
)

func TestComposedConstantValuesPhysicalGenerationIsolationAndLifetime(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "constants.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	const id = "repo/c.go::C"
	const path = "repo/c.go"
	for gen, value := range map[int64]string{0: "base", 1: "derived"} {
		h := s.AtGeneration(gen)
		h.AddBatch([]*graph.Node{{ID: id, Kind: graph.KindConstant, FilePath: path, RepoPrefix: "repo"}}, nil)
		require.NoError(t, h.BulkSetConstantValues("repo", []graph.ConstantValueRow{{NodeID: id, FilePath: path, Value: value}}))
		require.NoError(t, h.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: path, ContentHash: value, NodeCount: 1}}))
	}
	for gen, value := range map[int64]string{0: "base", 1: "derived"} {
		h := s.AtGeneration(gen)
		p, err := h.ReadConstantValueProjectionContext(context.Background(), []string{id}, nil)
		require.NoError(t, err)
		require.Equal(t, value, p.Rows[id].Value)
		require.Equal(t, value, p.Files[graph.ConstantFileKey{RepoPrefix: "repo", FilePath: path}].ContentHash)
		values, err := h.ConstantValuesByNodeIDsContext(context.Background(), []string{id})
		require.NoError(t, err)
		require.Equal(t, map[string]string{id: value}, values)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := s.ReadConstantValueProjectionContext(ctx, []string{id}, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, graph.ConstantValueProjection{}, p)
	require.NoError(t, s.Close())
	p, err = s.ReadConstantValueProjectionContext(context.Background(), nil, nil)
	require.Error(t, err)
	require.Equal(t, graph.ConstantValueProjection{}, p)
}
