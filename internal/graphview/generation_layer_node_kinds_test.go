package graphview

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestNodeKindProjectionGenerationMasksContextAndStaleCorrection(t *testing.T) {
	store := openTestStore(t)
	ctxPath, path, removed := "/repo/context.go", "/repo/changed.go", "/repo/deleted.go"
	baseNodes := []*graph.Node{
		{ID: ctxPath + "::C", Kind: graph.KindTable, FilePath: ctxPath, RepoPrefix: "repo"},
		{ID: path + "::Old", Kind: graph.KindContract, FilePath: path, RepoPrefix: "repo"},
		{ID: removed + "::Gone", Kind: graph.KindFunction, FilePath: removed, RepoPrefix: "repo"},
	}
	store.AddBatch(baseNodes, nil)
	_, handle := beginTestGeneration(t, store, "kind-masks")
	newNode := &graph.Node{ID: path + "::New", Kind: graph.KindFunction, FilePath: path, RepoPrefix: "repo"}
	handle.AddBatch([]*graph.Node{
		{ID: path, Kind: graph.KindFile, FilePath: path, RepoPrefix: "repo"}, newNode,
	}, nil)
	require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{
		{FilePath: path, Mode: store_sqlite.OwnershipReplace},
		{FilePath: ctxPath, Mode: store_sqlite.OwnershipContext},
		{FilePath: removed, Mode: store_sqlite.OwnershipDelete},
	}))
	publishTestGeneration(t, store, handle.ViewGeneration())
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	view := graph.NewOverlaidViewWithLayer(store, layer)
	ids := []string{baseNodes[0].ID, baseNodes[1].ID, baseNodes[2].ID, newNode.ID}
	rows, err := graph.GetNodeKindsByIDsContext(t.Context(), view, ids)
	require.NoError(t, err)
	require.Equal(t, map[string]graph.NodeKindRow{
		baseNodes[0].ID: {Kind: graph.KindTable, FilePath: ctxPath, RepoPrefix: "repo"},
		newNode.ID:      {Kind: graph.KindFunction, FilePath: path, RepoPrefix: "repo"},
	}, rows)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err = graph.GetNodeKindsByIDsContext(ctx, view, ids)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
	correction, err := store.BeginDerivedCorrection(t.Context(), store_sqlite.DerivedCorrectionRequest{
		GenerationID: handle.ViewGeneration(), Pass: "capability", FromVersion: 0, ToVersion: 1, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField},
	})
	require.NoError(t, err)
	require.NoError(t, correction.ReplaceSourceEdges(t.Context(), []string{newNode.ID}, []*graph.Edge{{From: newNode.ID, To: baseNodes[0].ID, Kind: graph.EdgeAccessesField, FilePath: path}}, nil))
	rows, err = graph.GetNodeKindsByIDsContext(t.Context(), view, ids)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Nil(t, rows, "old materialized ownership cannot certify kinds after a committed correction")
	_, err = correction.Finish(t.Context())
	require.NoError(t, err)
}
