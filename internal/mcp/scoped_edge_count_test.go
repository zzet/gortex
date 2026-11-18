package mcp

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type allEdgesCountingReader struct {
	graph.Reader
	allEdges int
}

func (r *allEdgesCountingReader) AllEdges() []*graph.Edge {
	r.allEdges++
	return r.Reader.AllEdges()
}

// A bound session's edge count reads its own nodes' adjacency, across more
// than one batch, and agrees with the whole-store scan it replaces.
func TestCountEdgesWithinMatchesScopedScanWithoutAllEdges(t *testing.T) {
	g := graph.New()
	inScope := map[string]bool{}
	for i := range scopedEdgeCountBatch + 300 {
		id := fmt.Sprintf("a/f.go::N%d", i)
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, RepoPrefix: "a"})
		inScope[id] = true
		if i > 0 {
			g.AddEdge(&graph.Edge{From: id, To: fmt.Sprintf("a/f.go::N%d", i-1), Kind: graph.EdgeCalls})
		}
	}
	for i := range 50 {
		id := fmt.Sprintf("b/f.go::M%d", i)
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, RepoPrefix: "b"})
		g.AddEdge(&graph.Edge{From: id, To: "a/f.go::N0", Kind: graph.EdgeCalls})
		g.AddEdge(&graph.Edge{From: "a/f.go::N1", To: id, Kind: graph.EdgeReferences})
	}
	want := 0
	for _, e := range g.AllEdges() {
		if inScope[e.From] && inScope[e.To] {
			want++
		}
	}
	r := &allEdgesCountingReader{Reader: g}
	require.Equal(t, want, countEdgesWithin(r, inScope))
	require.Zero(t, r.allEdges, "a bound count must not scan every edge")
	require.Equal(t, g.EdgeCount(), countEdgesWithin(r, nil))
	require.Zero(t, r.allEdges)
}
