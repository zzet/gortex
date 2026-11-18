package query

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// admissionGateSpy is a selected reader that counts neighborhood statements
// and adjacency reads. It declines every statement, so each walk also runs
// the hop-by-hop path and must reuse the seed read that chose it.
type admissionGateSpy struct {
	*graph.Graph
	statements int
	reads      map[string]int
}

func (s *admissionGateSpy) BFSAdmissionContext(context.Context, graph.BFSAdmissionRequest) (graph.BFSAdmissionSnapshot, bool, error) {
	s.statements++
	return graph.BFSAdmissionSnapshot{}, false, nil
}

func (s *admissionGateSpy) ReadKindAdjacency(ctx context.Context, id string, dir graph.Direction, kinds []graph.EdgeKind) (graph.KindAdjacency, error) {
	s.reads[id]++
	return graph.ReadKindAdjacency(ctx, s.Graph, id, dir, kinds)
}

// TestScopedWalkReadsNeighborhoodStatementOnlyForWideSeeds: the statement
// costs several adjacency reads, so a seed below the fan-out threshold walks
// hop by hop, and either way the seed's adjacency is read exactly once.
func TestScopedWalkReadsNeighborhoodStatementOnlyForWideSeeds(t *testing.T) {
	g := graph.New()
	add := func(id string) { g.AddNode(&graph.Node{ID: id, Name: id, Kind: graph.KindFunction, RepoPrefix: "r"}) }
	add("r/a.go::Narrow")
	add("r/a.go::Wide")
	for i := range bfsAdmissionMinFanout {
		callee := fmt.Sprintf("r/b.go::Callee%d", i)
		add(callee)
		g.AddEdge(&graph.Edge{From: "r/a.go::Wide", To: callee, Kind: graph.EdgeCalls, Line: i + 1})
	}
	for i := range bfsAdmissionMinFanout - 1 {
		g.AddEdge(&graph.Edge{From: "r/a.go::Narrow", To: fmt.Sprintf("r/b.go::Callee%d", i), Kind: graph.EdgeCalls, Line: i + 1})
	}
	opts := QueryOptions{RepoAllow: map[string]bool{"r": true}, Depth: 3, Limit: 20}
	for _, tc := range []struct {
		seed       string
		statements int
	}{
		{"r/a.go::Narrow", 0},
		{"r/a.go::Wide", 1},
	} {
		t.Run(tc.seed, func(t *testing.T) {
			spy := &admissionGateSpy{Graph: g, reads: map[string]int{}}
			got := NewEngine(g).WithReader(spy).GetCallChain(tc.seed, opts)
			want := NewEngine(g).GetCallChain(tc.seed, opts)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("walk changed the result:\n got %+v\nwant %+v", got, want)
			}
			if spy.statements != tc.statements {
				t.Fatalf("neighborhood statements = %d, want %d", spy.statements, tc.statements)
			}
			if spy.reads[tc.seed] != 1 {
				t.Fatalf("seed adjacency read %d times, want once", spy.reads[tc.seed])
			}
		})
	}
}
