package reach

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func keepAll(rows map[string][]*graph.Edge) map[string][]*graph.Edge { return rows }

// hideFrom returns a filter that drops every edge leaving one of ids.
func hideFrom(ids ...string) EdgeFilter {
	hidden := make(map[string]bool, len(ids))
	for _, id := range ids {
		hidden[id] = true
	}
	return func(rows map[string][]*graph.Edge) map[string][]*graph.Edge {
		out := make(map[string][]*graph.Edge, len(rows))
		for to, edges := range rows {
			for _, edge := range edges {
				if !hidden[edge.From] {
					out[to] = append(out[to], edge)
				}
			}
		}
		return out
	}
}

func TestLookupFilteredContextKeepingEveryEdgeMatchesLookupContext(t *testing.T) {
	g, ids := newCallChain(t, 5)
	want1, want2, want3, wantHit, wantTruncated := LookupContext(t.Context(), g, ids[4])
	d1, d2, d3, hit, truncated := LookupFilteredContext(t.Context(), g, ids[4], keepAll)
	if hit != wantHit || truncated != wantTruncated {
		t.Fatalf("status hit=%v truncated=%v, want hit=%v truncated=%v", hit, truncated, wantHit, wantTruncated)
	}
	for i, pair := range [][2][]Entry{{d1, want1}, {d2, want2}, {d3, want3}} {
		if got, want := joinIDs(pair[0]), joinIDs(pair[1]); got != want {
			t.Fatalf("d%d = %q, want %q", i+1, got, want)
		}
	}
}

func TestLookupFilteredContextStopsAtHiddenEdges(t *testing.T) {
	g, ids := newCallChain(t, 5)
	// fn-2 -> fn-3 is hidden, so nothing past fn-3 reaches the seed.
	d1, d2, d3, hit, truncated := LookupFilteredContext(t.Context(), g, ids[4], hideFrom(ids[2]))
	if !hit || truncated {
		t.Fatalf("status hit=%v truncated=%v, want an exact hit", hit, truncated)
	}
	if got := joinIDs(d1); got != ids[3] {
		t.Fatalf("d1 = %q, want %q", got, ids[3])
	}
	if len(d2)+len(d3) != 0 {
		t.Fatalf("d2=%v d3=%v, want nothing past the hidden edge", d2, d3)
	}
}

// A published record was built over every edge, hidden ones included, so a
// filtered lookup recomputes even when a current record is stamped.
func TestLookupFilteredContextNeverReadsAPublishedRecord(t *testing.T) {
	g, ids := newCallChain(t, 3)
	seed := g.GetNode(ids[2])
	if seed == nil {
		t.Fatal("seed node missing")
	}
	// A current, complete record that claims no callers at all.
	published := *seed
	published.Meta = map[string]any{
		MetaReachBuild:     BuildCounter(),
		MetaReachEpoch:     reachProcessEpoch,
		MetaReachComplete:  true,
		MetaReachTruncated: false,
	}
	g.AddNode(&published)

	cached1, cached2, _, hit, _ := LookupContext(t.Context(), g, ids[2])
	if !hit || len(cached1)+len(cached2) != 0 {
		t.Fatalf("control: LookupContext d1=%v d2=%v hit=%v, want the published empty record", cached1, cached2, hit)
	}
	d1, d2, _, hit, truncated := LookupFilteredContext(t.Context(), g, ids[2], keepAll)
	if !hit || truncated {
		t.Fatalf("status hit=%v truncated=%v, want an exact hit", hit, truncated)
	}
	if got := joinIDs(d1); got != ids[1] {
		t.Fatalf("d1 = %q, want %q; the published record was read", got, ids[1])
	}
	if got := joinIDs(d2); got != ids[0] {
		t.Fatalf("d2 = %q, want %q", got, ids[0])
	}
}

// The edge budget is charged for every row read, visible or not: hidden rows
// that exhaust it end the traversal as a lower bound instead of letting it
// read further.
func TestLookupFilteredContextChargesHiddenRowsToTheBudget(t *testing.T) {
	g := graph.New()
	nodes := []*graph.Node{
		{ID: "seed", Name: "seed", Kind: graph.KindFunction},
		{ID: "caller", Name: "caller", Kind: graph.KindFunction},
		{ID: "outer", Name: "outer", Kind: graph.KindFunction},
	}
	edges := []*graph.Edge{
		{From: "caller", To: "seed", Kind: graph.EdgeCalls, Confidence: 1},
		{From: "outer", To: "caller", Kind: graph.EdgeCalls, Confidence: 1},
	}
	hidden := make([]string, 0, maxLookupEdges-1)
	for i := range maxLookupEdges - 1 {
		id := "hidden-" + iToA(i)
		hidden = append(hidden, id)
		nodes = append(nodes, &graph.Node{ID: id, Name: id, Kind: graph.KindFunction})
		edges = append(edges, &graph.Edge{From: id, To: "seed", Kind: graph.EdgeCalls, Confidence: 1})
	}
	g.AddBatch(nodes, edges)

	d1, d2, d3, hit, truncated := LookupFilteredContext(t.Context(), g, "seed", hideFrom(hidden...))
	if !hit {
		t.Fatal("expected a bounded hit")
	}
	if got := joinIDs(d1); got != "caller" {
		t.Fatalf("d1 = %q, want caller", got)
	}
	if len(d2)+len(d3) != 0 {
		t.Fatalf("d2=%v d3=%v, want the exhausted budget to stop the walk", d2, d3)
	}
	if !truncated {
		t.Fatal("a walk stopped by the budget must report truncation")
	}
}
