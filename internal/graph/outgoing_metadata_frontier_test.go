package graph

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type outgoingEndpointProjectionProbe struct {
	*OverlayLayer
	calls int
	ids   []string
	err   error
}

func (l *outgoingEndpointProjectionProbe) LayerContractIDProjectionContext(ctx context.Context, ids []string) (ContractFileProjection, error) {
	l.calls++
	l.ids = append(l.ids, ids...)
	if l.err != nil {
		return ContractFileProjection{}, l.err
	}
	return l.OverlayLayer.LayerContractIDProjectionContext(ctx, ids)
}

func TestCheckedOutgoingMetadataHydratesOnlySurvivingEndpoints(t *testing.T) {
	for _, foreignRecording := range []bool{false, true} {
		t.Run(map[bool]string{false: "all_base_rows_replaced", true: "foreign_recording_survives"}[foreignRecording], func(t *testing.T) {
			const caller = "source.go::Caller"
			const hidden = "source.go::Hidden"
			const target = "target.go::External"
			base := New()
			for _, id := range []string{caller, hidden, target} {
				path := "source.go"
				if id == target {
					path = "target.go"
				}
				base.AddNode(&Node{ID: id, Kind: KindFunction, FilePath: path})
			}
			base.AddEdge(&Edge{From: caller, To: hidden, Kind: EdgeCalls, FilePath: "source.go", Meta: map[string]any{"via": "old"}})
			foreign := &Edge{From: caller, To: target, Kind: EdgeCalls, FilePath: "foreign.go", Meta: map[string]any{"via": "foreign", "nested": map[string]any{"kept": true}}}
			if foreignRecording {
				base.AddEdge(foreign)
			}
			layer := &outgoingEndpointProjectionProbe{OverlayLayer: NewOverlayLayer()}
			layer.MarkFile("source.go", false)
			layer.AddNode("source.go", &Node{ID: caller, Kind: KindFunction, FilePath: "source.go"})
			replacement := &Edge{From: caller, To: target, Kind: EdgeReferences, FilePath: "source.go", Confidence: .9, Meta: map[string]any{"via": "replacement", "nested": map[string]any{"kept": true}}}
			layer.AddEdge(replacement)
			view := NewOverlaidViewWithLayer(base, layer)
			rows, truncated, err := GetOutEdgesByNodeIDsWithMetadataContext(t.Context(), view, []string{caller}, 2)
			require.NoError(t, err)
			require.False(t, truncated)
			want := []*Edge{replacement}
			if foreignRecording {
				want = append([]*Edge{foreign}, want...)
			}
			require.Equal(t, want, rows[caller], "recording ownership and complete edge metadata must be preserved")
			require.Len(t, base.GetOutEdges(caller), 1+map[bool]int{false: 0, true: 1}[foreignRecording], "composition must not mutate base rows")
			require.Equal(t, 1, layer.calls, "even an empty endpoint frontier must retain the checked revision fence")
			require.NotContains(t, layer.ids, hidden, "already-replaced recording rows must not hydrate their endpoints")
			if foreignRecording {
				require.Equal(t, []string{caller}, layer.ids, "a surviving foreign recording still needs source identity evidence")
			} else {
				require.Empty(t, layer.ids)
			}
		})
	}
}

func TestCheckedOutgoingMetadataEmptyFrontierStillChecksRevision(t *testing.T) {
	const caller = "source.go::Caller"
	base := New()
	base.AddNode(&Node{ID: caller, Kind: KindFunction, FilePath: "source.go"})
	base.AddEdge(&Edge{From: caller, To: "source.go::Old", Kind: EdgeCalls, FilePath: "source.go"})
	layer := &outgoingEndpointProjectionProbe{OverlayLayer: NewOverlayLayer(), err: ErrContractProjectionStale}
	layer.MarkFile("source.go", true)
	rows, truncated, err := GetOutEdgesByNodeIDsWithMetadataContext(t.Context(), NewOverlaidViewWithLayer(base, layer), []string{caller}, 10)
	require.ErrorIs(t, err, ErrContractProjectionStale)
	require.Nil(t, rows)
	require.False(t, truncated)
	require.Equal(t, 1, layer.calls)
	require.Empty(t, layer.ids)
}

func TestCheckedOutgoingMetadataRemovedSourceSkipsForeignEndpoints(t *testing.T) {
	const caller = "source.go::Caller"
	base := New()
	base.AddNode(&Node{ID: caller, Kind: KindFunction, FilePath: "source.go"})
	base.AddEdge(&Edge{From: caller, To: "target.go::Target", Kind: EdgeCalls, FilePath: "foreign.go"})
	layer := &outgoingEndpointProjectionProbe{OverlayLayer: NewOverlayLayer()}
	layer.MarkRemoved("Caller", caller)
	rows, truncated, err := GetOutEdgesByNodeIDsWithMetadataContext(t.Context(), NewOverlaidViewWithLayer(base, layer), []string{caller}, 10)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Empty(t, rows[caller])
	require.Equal(t, 1, layer.calls)
	require.Empty(t, layer.ids, "source adjacency removal decides even foreign-recorded rows before endpoint reads")
}
