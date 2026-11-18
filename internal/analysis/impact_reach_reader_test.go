package analysis

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/reach"
)

// hidingImpactReader is a request reader that is not a store and answers
// impact's reach probe with the store's bounded traversal minus the edges
// leaving hidden. miss makes the probe report a miss instead.
type hidingImpactReader struct {
	graph.Reader
	store  graph.Store
	hidden string
	miss   bool
	probes int
}

func (r *hidingImpactReader) ImpactReachContext(ctx context.Context, seedID string) (d1, d2, d3 []reach.Entry, hit, truncated bool) {
	r.probes++
	if r.miss {
		return nil, nil, nil, false, false
	}
	return reach.LookupFilteredContext(ctx, r.store, seedID, func(rows map[string][]*graph.Edge) map[string][]*graph.Edge {
		out := make(map[string][]*graph.Edge, len(rows))
		for to, edges := range rows {
			for _, edge := range edges {
				if edge.From != r.hidden {
					out[to] = append(out[to], edge)
				}
			}
		}
		return out
	})
}

func newHidingImpactGraph() *graph.Graph {
	g := graph.New()
	for _, id := range []string{"a.go::Seed", "b.go::Direct", "c.go::Middle", "d.go::Outer", "e.go::Hidden"} {
		g.AddNode(&graph.Node{ID: id, Name: id, Kind: graph.KindFunction, FilePath: id[:4]})
	}
	for _, edge := range [][2]string{
		{"b.go::Direct", "a.go::Seed"},
		{"c.go::Middle", "b.go::Direct"},
		{"d.go::Outer", "c.go::Middle"},
		{"e.go::Hidden", "a.go::Seed"},
	} {
		g.AddEdge(&graph.Edge{From: edge[0], To: edge[1], Kind: graph.EdgeCalls, Confidence: 1})
	}
	return g
}

func TestAnalyzeImpactUsesTheReadersReachTraversal(t *testing.T) {
	g := newHidingImpactGraph()
	reader := &hidingImpactReader{Reader: g, store: g, hidden: "e.go::Hidden"}

	result := AnalyzeImpactContext(t.Context(), reader, []string{"a.go::Seed"}, nil, nil)
	if reader.probes != 1 {
		t.Fatalf("reach probes = %d, want 1", reader.probes)
	}
	for depth, want := range map[int]string{1: "b.go::Direct", 2: "c.go::Middle", 3: "d.go::Outer"} {
		if got := idSet(result.ByDepth[depth]); len(got) != 1 || got[0] != want {
			t.Fatalf("depth %d = %v, want [%s]", depth, got, want)
		}
	}

	// Several seeds take the same traversal per seed.
	reader.probes = 0
	multi := AnalyzeImpactContext(t.Context(), reader, []string{"a.go::Seed", "c.go::Middle"}, nil, nil)
	if reader.probes != 2 {
		t.Fatalf("reach probes = %d, want 2", reader.probes)
	}
	if got := idSet(multi.ByDepth[1]); !setsEqual(got, []string{"b.go::Direct", "d.go::Outer"}) {
		t.Fatalf("depth 1 = %v, want Direct and Outer", got)
	}
	for _, depth := range []int{1, 2, 3} {
		for _, id := range idSet(multi.ByDepth[depth]) {
			if id == "e.go::Hidden" {
				t.Fatalf("depth %d reports the hidden caller", depth)
			}
		}
	}
}

// A probe miss leaves impact to the live walk over the reader's own edges.
func TestAnalyzeImpactReaderReachMissWalksLive(t *testing.T) {
	g := newHidingImpactGraph()
	reader := &hidingImpactReader{Reader: g, store: g, hidden: "e.go::Hidden", miss: true}

	result := AnalyzeImpactContext(t.Context(), reader, []string{"a.go::Seed"}, nil, nil)
	if reader.probes != 1 {
		t.Fatalf("reach probes = %d, want 1", reader.probes)
	}
	if got := idSet(result.ByDepth[1]); !setsEqual(got, []string{"b.go::Direct", "e.go::Hidden"}) {
		t.Fatalf("depth 1 = %v, want the live walk's Direct and Hidden", got)
	}
}
