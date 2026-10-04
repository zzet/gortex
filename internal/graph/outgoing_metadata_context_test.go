package graph

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckedOutgoingMetadataCompositionPreservesRecordingAndEndpointOwnership(t *testing.T) {
	base := New()
	for _, id := range []string{"source.go::Caller", "target.go::Old", "target.go::Kept"} {
		path := "target.go"
		if id == "source.go::Caller" {
			path = "source.go"
		}
		base.AddNode(&Node{ID: id, Kind: KindFunction, FilePath: path})
	}
	base.AddEdge(&Edge{From: "source.go::Caller", To: "target.go::Old", Kind: EdgeCalls, FilePath: "source.go", Meta: map[string]any{"via": "spring.Bean"}})
	base.AddEdge(&Edge{From: "source.go::Caller", To: "target.go::Kept", Kind: EdgeCalls, FilePath: "source.go", Meta: map[string]any{"via": "ordinary"}})
	layer := NewOverlayLayer()
	layer.MarkFile("target.go", false)
	layer.AddNode("target.go", &Node{ID: "target.go::Kept", Kind: KindFunction, FilePath: "target.go"})
	view := NewOverlaidViewWithLayer(base, layer)
	rows, truncated, err := GetOutEdgesByNodeIDsWithMetadataContext(t.Context(), view, []string{"source.go::Caller"}, 10)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Len(t, rows["source.go::Caller"], 1)
	require.Equal(t, "target.go::Kept", rows["source.go::Caller"][0].To)
	require.Equal(t, "ordinary", rows["source.go::Caller"][0].Meta["via"])
	layer.MarkFile("source.go", true)
	rows, truncated, err = GetOutEdgesByNodeIDsWithMetadataContext(t.Context(), view, []string{"source.go::Caller"}, 10)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Empty(t, rows["source.go::Caller"], "recording-file tombstone removes inherited outgoing evidence")
}

func TestCheckedOutgoingMetadataUsesTotalLimitAndPropagatesCancellation(t *testing.T) {
	base := New()
	for _, id := range []string{"a", "b"} {
		base.AddEdge(&Edge{From: id, To: "target", Kind: EdgeCalls, Meta: map[string]any{"via": "spring.Bean"}})
	}
	view := NewOverlaidViewWithLayer(base, NewOverlayLayer())
	rows, truncated, err := GetOutEdgesByNodeIDsWithMetadataContext(t.Context(), view, []string{"a", "a", "b"}, 1)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Nil(t, rows, "bounded complete projection must not return usable partial adjacency")
	rows, truncated, err = GetOutEdgesByNodeIDsWithMetadataContext(t.Context(), view, []string{"a", "a", "b"}, 2)
	require.NoError(t, err)
	require.False(t, truncated, "duplicate IDs must not consume the total bound twice")
	require.Equal(t, "spring.Bean", rows["a"][0].Meta["via"])
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, _, err = GetOutEdgesByNodeIDsWithMetadataContext(ctx, view, []string{"a"}, 2)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
}
