package rerank

import (
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type timingEdgeReader struct {
	*graph.Graph
	out, in int
}

func (g *timingEdgeReader) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	g.out++
	return g.Graph.GetOutEdgesByNodeIDs(ids)
}
func (g *timingEdgeReader) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	g.in++
	return g.Graph.GetInEdgesByNodeIDs(ids)
}

func TestTimingObserverPreservesPreparationReadsAndRanking(t *testing.T) {
	if newPrepareTiming(nil) != nil {
		t.Fatal("nil observer must not collect clocks")
	}
	for _, seeded := range []bool{false, true} {
		run := func(observed bool) ([]*Candidate, Timing, []int) {
			g := &timingEdgeReader{Graph: graph.New()}
			a := &graph.Node{ID: "a", Name: "Alpha", Kind: graph.KindFunction}
			b := &graph.Node{ID: "b", Name: "AlphaHelper", Kind: graph.KindFunction}
			g.AddNode(a)
			g.AddNode(b)
			g.AddEdge(&graph.Edge{From: "a", To: "b", Kind: graph.EdgeCalls})
			cands := []*Candidate{{Node: b, TextRank: 1, VectorRank: -1}, {Node: a, TextRank: 0, VectorRank: -1}}
			metrics, centrality := 0, 0
			ctx := &Context{Graph: g, AnalysisMetricsOf: func([]string) map[string]AnalysisMetric { metrics++; return nil }, BatchedCentrality: func([]string, []string) CentralityResult {
				centrality++
				return CentralityResult{Scores: map[string]float64{"a": 1, "b": .5}}
			}}
			var total Timing
			if observed {
				ctx.ObserveTiming = func(timing Timing) { total.Add(timing) }
			}
			if seeded {
				ctx.SeedEdgeCaches(g.Graph.GetOutEdgesByNodeIDs([]string{"a", "b"}), g.Graph.GetInEdgesByNodeIDs([]string{"a", "b"}), true)
			}
			NewDefault().Rerank("Alpha", cands, ctx)
			return cands, total, []int{metrics, centrality, g.out, g.in}
		}
		plain, _, reads := run(false)
		observed, timing, observedReads := run(true)
		if !reflect.DeepEqual(plain, observed) || !reflect.DeepEqual(reads, observedReads) {
			t.Fatalf("seeded=%v observer changed ranking/reads: %v %v", seeded, reads, observedReads)
		}
		if reads[0] != 1 || reads[1] != 1 || timing.PrepareCalls != 1 || timing.ScoringCalls != 1 || timing.Candidates != 2 {
			t.Fatalf("unexpected execution/counts: %+v reads=%v", timing, reads)
		}
		sum := timing.Metrics + timing.Bookkeeping + timing.Outgoing + timing.Incoming + timing.MergeFan + timing.Centrality
		if timing.Prepare != sum {
			t.Fatalf("exclusive legs do not sum to preparation: %+v", timing)
		}
		if seeded {
			if timing.MissingOut != 0 || timing.MissingIn != 0 || timing.OutRows != 0 || timing.InRows != 0 || reads[2] != 0 || reads[3] != 0 {
				t.Fatalf("seeded caches fetched again: %+v reads=%v", timing, reads)
			}
		} else if timing.MissingOut != 2 || timing.MissingIn != 2 || timing.OutRows != 1 || timing.InRows != 1 || reads[2] != 1 || reads[3] != 1 {
			t.Fatalf("missing fetch counts: %+v reads=%v", timing, reads)
		}
	}
}
