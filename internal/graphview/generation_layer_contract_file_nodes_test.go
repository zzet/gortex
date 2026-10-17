package graphview

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractFileNodesGenerationMasksAndRevision(t *testing.T) {
	store := openTestStore(t)
	contextPath, changed, deleted := "repo/context.go", "repo/changed.go", "repo/deleted.go"
	base := []*graph.Node{
		{ID: contextPath, Kind: graph.KindFile, FilePath: contextPath, RepoPrefix: "repo", Meta: map[string]any{"stamp": "base"}},
		{ID: changed, Kind: graph.KindFile, FilePath: changed, RepoPrefix: "repo"},
		{ID: deleted, Kind: graph.KindFile, FilePath: deleted, RepoPrefix: "repo"},
	}
	require.NoError(t, store.AddBatchChecked(base, nil))
	_, handle := beginTestGeneration(t, store, "file-nodes-masks")
	current := &graph.Node{ID: changed, Kind: graph.KindFile, FilePath: changed, RepoPrefix: "repo", Meta: map[string]any{"stamp": "selected"}}
	require.NoError(t, handle.AddBatchChecked([]*graph.Node{current}, nil))
	require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{
		{FilePath: contextPath, Mode: store_sqlite.OwnershipContext},
		{FilePath: changed, Mode: store_sqlite.OwnershipReplace},
		{FilePath: deleted, Mode: store_sqlite.OwnershipDelete},
	}))
	publishTestGeneration(t, store, handle.ViewGeneration())
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	view := graph.NewOverlaidViewWithLayer(store, layer)
	rows, err := graph.ContractFileNodesContext(t.Context(), view, "repo", []string{contextPath, changed, deleted, "missing.go"})
	require.NoError(t, err)
	require.Equal(t, []*graph.Node{base[0]}, rows[contextPath])
	require.Equal(t, []*graph.Node{current}, rows[changed])
	require.Empty(t, rows[deleted])
	require.Empty(t, rows["missing.go"])
	correction, err := store.BeginDerivedCorrection(t.Context(), store_sqlite.DerivedCorrectionRequest{GenerationID: handle.ViewGeneration(), Pass: "capability", FromVersion: 0, ToVersion: 1, EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
	require.NoError(t, err)
	require.NoError(t, correction.ReplaceSourceEdges(t.Context(), []string{current.ID}, []*graph.Edge{{From: current.ID, To: contextPath, Kind: graph.EdgeAccessesField, FilePath: changed}}, nil))
	rows, err = graph.ContractFileNodesContext(t.Context(), view, "repo", []string{changed})
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Nil(t, rows)
	_, err = correction.Finish(t.Context())
	require.NoError(t, err)
}
