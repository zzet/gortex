package graphview

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestGenerationLayerCheckedOutgoingMetadataPreservesProvenanceAndContextOwnership(t *testing.T) {
	store := openTestStore(t)
	_, handle := beginTestGeneration(t, store, "checked-outgoing-metadata")
	// Building contradictory context rows exercise the same defensive rule
	// as existing layer tests; publication itself would reject these rows.
	handle.AddBatch([]*graph.Node{{ID: ctxChangedID, Name: "Changed", Kind: graph.KindFunction, FilePath: ctxChangedFile}, {ID: ctxContextID, Name: "Context", Kind: graph.KindFunction, FilePath: ctxContextFile}}, []*graph.Edge{
		{From: ctxChangedID, To: ctxContextID, Kind: graph.EdgeCalls, FilePath: ctxChangedFile, Meta: map[string]any{"via": "spring.Bean"}},
		{From: ctxContextID, To: ctxChangedID, Kind: graph.EdgeCalls, FilePath: ctxContextFile, Meta: map[string]any{"via": "spring.Bean"}},
	})
	require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{FilePath: ctxChangedFile, Mode: store_sqlite.OwnershipReplace}, {FilePath: ctxContextFile, Mode: store_sqlite.OwnershipContext}}))
	layer, err := NewGenerationLayer(handle)
	require.NoError(t, err)
	rows, truncated, err := layer.LayerOutEdgesWithMetadataContext(t.Context(), []string{ctxChangedID, ctxContextID}, 10)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Len(t, rows[ctxChangedID], 1)
	require.Equal(t, "spring.Bean", rows[ctxChangedID][0].Meta["via"])
	require.Empty(t, rows[ctxContextID])
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, _, err = layer.LayerOutEdgesWithMetadataContext(ctx, []string{ctxChangedID}, 10)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
	handle.AddNode(&graph.Node{ID: ctxChangedID, Name: "Corrected", Kind: graph.KindFunction, FilePath: ctxChangedFile})
	rows, _, err = layer.LayerOutEdgesWithMetadataContext(t.Context(), []string{ctxChangedID}, 10)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Nil(t, rows)
}
