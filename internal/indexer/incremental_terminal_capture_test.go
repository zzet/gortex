package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestIncrementalCaptureRetainsExactPriorTerminalPayload(t *testing.T) {
	g := graph.New()
	node := &graph.Node{ID: "a.go::Run", Name: "Run", Kind: graph.KindFunction, FilePath: "a.go"}
	edge := &graph.Edge{From: node.ID, To: "unresolved::Missing", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 12, Confidence: .25, Alias: "original", Origin: graph.OriginASTInferred, Meta: map[string]any{graph.EdgeMetaResolveTerminal: true, graph.EdgeMetaResolveTerminalReason: "no_definition", "keep": "original"}}
	g.AddBatch([]*graph.Node{node}, []*graph.Edge{edge})
	_, point, _ := captureIncrementalState(g, "a.go")
	_, batch := captureIncrementalStateFromView([]*graph.Node{node}, map[string][]*graph.Edge{node.ID: {edge}}, map[string]*graph.Node{node.ID: node})
	require.Len(t, point, 1)
	require.Len(t, batch, 1)
	require.Equal(t, edge, point[0])
	require.Equal(t, edge, batch[0])
	require.NotSame(t, edge, point[0])
	require.NotSame(t, edge, batch[0])
}
