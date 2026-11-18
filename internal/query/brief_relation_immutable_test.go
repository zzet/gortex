package query

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// Projection layers return immutable full nodes shared by concurrent readers.
// A brief BFS response may remove metadata only from its own response copies.
func TestBriefRelationsKeepImmutableReaderMetadata(t *testing.T) {
	for _, forward := range []bool{false, true} {
		name := "callers"
		if forward {
			name = "callees"
		}
		t.Run(name, func(t *testing.T) {
			base := graph.New()
			layer := graph.NewOverlayLayer()
			layer.MarkFile("file.go", false)
			caller := &graph.Node{ID: "file.go::Caller", Name: "Caller", FilePath: "file.go", Kind: graph.KindFunction, Meta: map[string]any{"doc": "original caller documentation"}}
			callee := &graph.Node{ID: "file.go::Callee", Name: "Callee", FilePath: "file.go", Kind: graph.KindFunction, Meta: map[string]any{"doc": "original callee documentation"}}
			layer.AddNode("file.go", caller)
			layer.AddNode("file.go", callee)
			layer.AddEdge(&graph.Edge{From: caller.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: "file.go", Line: 5, Provisional: true})
			reader := graph.NewOverlaidViewWithLayer(base, layer)
			eng := NewEngine(base).WithReader(reader)
			options := QueryOptions{Depth: 1, Limit: 10, Detail: "brief"}
			var response *SubGraph
			if forward {
				response = eng.GetCallChain(caller.ID, options)
			} else {
				response = eng.GetCallers(callee.ID, options)
			}
			require.Len(t, response.Nodes, 2)
			for _, node := range response.Nodes {
				require.Nil(t, node.Meta)
			}
			require.Equal(t, map[string]any{"doc": "original caller documentation"}, reader.GetNode(caller.ID).Meta)
			require.Equal(t, map[string]any{"doc": "original callee documentation"}, reader.GetNode(callee.ID).Meta)
			require.Same(t, caller, layer.NodeByID(caller.ID))
			require.Same(t, callee, layer.NodeByID(callee.ID))
		})
	}
}

func TestBriefResultDoesNotRewriteReaderOwnedSlice(t *testing.T) {
	node := &graph.Node{ID: "owned", Meta: map[string]any{"doc": "retained"}}
	readerNodes := []*graph.Node{node}
	result := &SubGraph{Nodes: readerNodes}
	stripMeta(result)
	require.Same(t, node, readerNodes[0], "brief rendering cannot replace a pointer in a reader-owned slice")
	require.Equal(t, map[string]any{"doc": "retained"}, node.Meta)
	require.Nil(t, result.Nodes[0].Meta)
}

func TestBriefRelationsConcurrentFullReadsKeepMetadata(t *testing.T) {
	base := graph.New()
	layer := graph.NewOverlayLayer()
	layer.MarkFile("file.go", false)
	caller := &graph.Node{ID: "file.go::Caller", Name: "Caller", FilePath: "file.go", Kind: graph.KindFunction, Meta: map[string]any{"doc": "full caller"}}
	callee := &graph.Node{ID: "file.go::Callee", Name: "Callee", FilePath: "file.go", Kind: graph.KindFunction, Meta: map[string]any{"doc": "full callee"}}
	layer.AddNode("file.go", caller)
	layer.AddNode("file.go", callee)
	layer.AddEdge(&graph.Edge{From: caller.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: "file.go", Line: 5, Provisional: true})
	reader := graph.NewOverlaidViewWithLayer(base, layer)
	const workers, rounds = 8, 16
	type observed struct{ caller, callee any }
	answers := make(chan observed, workers*rounds)
	start := make(chan struct{})
	var joined sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		eng := NewEngine(base).WithReader(reader)
		joined.Go(func() {
			<-start
			for round := 0; round < rounds; round++ {
				options := QueryOptions{Depth: 1, Limit: 10, Detail: "brief"}
				if worker%2 == 0 {
					eng.GetCallChain(caller.ID, options)
				} else {
					eng.GetCallers(callee.ID, options)
				}
				// These are subsequent actual full-symbol reads from the same
				// immutable reader, not a test-owned copy of its metadata.
				answers <- observed{eng.GetSymbol(caller.ID).Meta["doc"], eng.GetSymbol(callee.ID).Meta["doc"]}
			}
		})
	}
	close(start)
	joined.Wait()
	close(answers)
	count := 0
	for answer := range answers {
		count++
		require.Equal(t, "full caller", answer.caller)
		require.Equal(t, "full callee", answer.callee)
	}
	require.Equal(t, workers*rounds, count)
}
