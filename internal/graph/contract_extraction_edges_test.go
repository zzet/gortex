package graph

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContractExtractionEdgesMatchFullSelectedMetadataAndMasks(t *testing.T) {
	base := New()
	for _, id := range []string{"source.go::Caller", "target.go::Old", "target.go::Kept"} {
		base.AddNode(&Node{ID: id, Kind: KindFunction, FilePath: IDFile(id)})
	}
	for _, row := range []*Edge{
		{From: "source.go::Caller", To: "target.go::Old", Kind: EdgeCalls, FilePath: "foreign.go", Meta: map[string]any{"via": "spring.Bean"}},
		{From: "source.go::Caller", To: "target.go::Kept", Kind: EdgeConsumes, FilePath: "foreign.go", Meta: map[string]any{MetaDIToken: "TOKEN", "nested": map[string]any{"kept": true}}},
		{From: "source.go::Caller", To: "target.go::Kept", Kind: EdgeReferences, FilePath: "foreign.go"},
	} {
		base.AddEdge(row)
	}
	layer := NewOverlayLayer()
	layer.MarkFile("target.go", false)
	layer.AddNode("target.go", &Node{ID: "target.go::Kept", Kind: KindFunction, FilePath: "target.go"})
	own := &Edge{From: "source.go::Caller", To: "target.go::Kept", Kind: EdgeProvides, FilePath: "own.go", Confidence: .9, Meta: map[string]any{MetaDIToken: "OWN"}}
	layer.AddEdge(own)
	view := NewOverlaidViewWithLayer(base, layer)
	ids := []string{"source.go::Caller"}
	full, truncated, err := GetOutEdgesByNodeIDsWithMetadataContext(t.Context(), view, ids, 10)
	require.NoError(t, err)
	require.False(t, truncated)
	selected, truncated, err := ContractExtractionEdgesContext(t.Context(), view, ids, 10)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, contractExtractionEdgeRows(full), selected)
	require.Len(t, selected[ids[0]], 2, "a removed endpoint cannot be revived by the narrow physical projection")
	rows, truncated, err := ContractExtractionEdgesContext(t.Context(), view, ids, 1)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Nil(t, rows)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, _, err = ContractExtractionEdgesContext(ctx, view, ids, 10)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
}

func TestContractExtractionEdgesRetainEmptyRevisionFailure(t *testing.T) {
	layer := &outgoingEndpointProjectionProbe{OverlayLayer: NewOverlayLayer(), err: ErrContractProjectionStale}
	rows, truncated, err := ContractExtractionEdgesContext(t.Context(), NewOverlaidViewWithLayer(New(), layer), nil, 10)
	require.ErrorIs(t, err, ErrContractProjectionStale)
	require.Nil(t, rows)
	require.False(t, truncated)
	require.Equal(t, 1, layer.calls)
}
