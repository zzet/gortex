package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestApplyResolvedOutEdges_SkipsJSLexicalBindingEdges(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "app.ts", Kind: graph.KindFile, FilePath: "app.ts", Language: "typescript"},
		{ID: "app.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "app.ts", Language: "typescript"},
		{ID: "v1.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "v1.ts", Language: "typescript"},
		{ID: "v1.ts::keep", Kind: graph.KindFunction, Name: "keep", FilePath: "v1.ts", Language: "typescript"},
	}, []*graph.Edge{
		{
			From: "app.ts::run", To: "v1.ts::helper", Kind: graph.EdgeCalls,
			FilePath: "app.ts", Line: 2, Origin: graph.OriginASTResolved, Confidence: 0.9,
			Meta: map[string]any{graph.JSLexicalBindingMeta: graph.JSLexicalLocal},
		},
		{
			From: "app.ts::run", To: "v1.ts::keep", Kind: graph.EdgeCalls,
			FilePath: "app.ts", Line: 3, Origin: graph.OriginASTResolved, Confidence: 0.9,
		},
	})

	reuse, _, _ := captureIncrementalState(g, "app.ts")
	require.NotContains(t, reuse, reuseKey{from: "app.ts::run", kind: graph.EdgeCalls, name: "helper"})
	require.Contains(t, reuse, reuseKey{from: "app.ts::run", kind: graph.EdgeCalls, name: "keep"})

	freshUnstamped := &graph.Edge{From: "app.ts::run", To: graph.UnresolvedMarker + "keep", Kind: graph.EdgeCalls, FilePath: "app.ts", Line: 3}
	freshLocal := &graph.Edge{
		From: "app.ts::run", To: graph.UnresolvedMarker + "keep", Kind: graph.EdgeCalls, FilePath: "app.ts", Line: 4,
		Meta: map[string]any{graph.JSLexicalBindingMeta: graph.JSLexicalLocal},
	}
	freshParam := &graph.Edge{
		From: "app.ts::run", To: graph.UnresolvedMarker + "keep", Kind: graph.EdgeCalls, FilePath: "app.ts", Line: 5,
		Meta: map[string]any{graph.JSLexicalBindingMeta: graph.JSPendingParameter},
	}
	reused := applyResolvedOutEdges(g, []*graph.Edge{freshUnstamped, freshLocal, freshParam}, reuse, nil)
	require.Equal(t, 1, reused)
	require.Equal(t, "v1.ts::keep", freshUnstamped.To)
	require.Equal(t, graph.UnresolvedMarker+"keep", freshLocal.To)
	require.Equal(t, graph.UnresolvedMarker+"keep", freshParam.To)
}
