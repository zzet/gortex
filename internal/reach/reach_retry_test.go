package reach

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// churningStore retires every reach generation while a traversal reads the
// seed's callers, so every lazy computation crosses a topology change.
type churningStore struct {
	graph.Store
	seed     string
	computes atomic.Int32
}

func (s *churningStore) GetInEdgesByNodeIDsContext(_ context.Context, ids []string, _ int) (map[string][]*graph.Edge, bool, error) {
	if len(ids) == 1 && ids[0] == s.seed {
		s.computes.Add(1)
		InvalidateIndex()
	}
	return s.GetInEdgesByNodeIDs(ids), false, nil
}

// A lookup crossed by a topology change retries once, then answers with what
// it read as a lower bound instead of spinning until its deadline.
func TestLookupRetriesOnceThenReportsALowerBound(t *testing.T) {
	base := graph.New()
	base.AddNode(&graph.Node{ID: "seed", Kind: graph.KindFunction, Name: "seed"})
	base.AddNode(&graph.Node{ID: "caller", Kind: graph.KindFunction, Name: "caller"})
	base.AddEdge(&graph.Edge{From: "caller", To: "seed", Kind: graph.EdgeCalls, Confidence: 1})
	store := &churningStore{Store: base, seed: "seed"}

	started := time.Now()
	d1, _, _, hit, truncated := LookupContext(t.Context(), store, "seed")
	elapsed := time.Since(started)
	if got := store.computes.Load(); got != 2 {
		t.Fatalf("lookup computed %d times in %s, want one retry", got, elapsed)
	}
	if !hit || !truncated {
		t.Fatalf("hit=%v truncated=%v, want a lower-bound hit", hit, truncated)
	}
	if got := joinIDs(d1); got != "caller" {
		t.Fatalf("d1 = %q, want the callers read as lower-bound evidence", got)
	}
	if elapsed > lookupTimeout/3 {
		t.Fatalf("lookup took %s", elapsed)
	}
}
