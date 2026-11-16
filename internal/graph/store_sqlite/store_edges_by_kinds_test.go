package store_sqlite

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The kind-filtered by-ID reads must hold their plan on a store that has not
// yet had planner statistics refreshed too (TestHotQueryPlansLocked covers the
// ANALYZEd regime): a fresh store is what a first enrichment pass runs on.
func TestEdgesByNodeIDsKindsPlansWithoutStats(t *testing.T) {
	s := openCatalogStore(t)
	cases := []struct {
		name  string
		query string
		args  int
		want  string
	}{
		{"out", edgesByNodeIDsKindsQuery(true, 3, 4), 8, "SEARCH edges USING INDEX edges_by_from_line_kind (view_gen=? AND from_id=?)"},
		{"in", edgesByNodeIDsKindsQuery(false, 3, 2), 6, "SEARCH edges USING INDEX edges_by_to (view_gen=? AND to_id=? AND kind=?)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			joined := strings.Join(explainQueryPlan(t, s, tc.query, tc.args), "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("plan missing %q:\n%s", tc.want, joined)
			}
			for _, forbidden := range []string{"SCAN edges", "USE TEMP B-TREE"} {
				if strings.Contains(joined, forbidden) {
					t.Fatalf("plan contains forbidden %q:\n%s", forbidden, joined)
				}
			}
		})
	}
}

// A hub node's unrequested rows must not be materialised: the filtered read
// returns only the requested kinds and allocates a small fraction of the
// unfiltered read over the same IDs.
func TestEdgesByNodeIDsKindsSkipsHubRows(t *testing.T) {
	s := openCatalogStore(t)
	const fanIn = 2000
	nodes := []*graph.Node{{ID: "repo/hub.go::Hub", Name: "Hub", Kind: graph.KindType, FilePath: "repo/hub.go", Language: "go"}}
	var edges []*graph.Edge
	for i := 0; i < 4; i++ {
		edges = append(edges, &graph.Edge{From: fmt.Sprintf("repo/hub.go::Hub.m%d", i), To: "repo/hub.go::Hub", Kind: graph.EdgeMemberOf, FilePath: "repo/hub.go", Line: i + 1})
	}
	for i := 0; i < fanIn; i++ {
		from := fmt.Sprintf("repo/use%04d.go::caller", i)
		edges = append(edges,
			&graph.Edge{From: from, To: "repo/hub.go::Hub", Kind: graph.EdgeReferences, FilePath: fmt.Sprintf("repo/use%04d.go", i), Line: 3, Meta: map[string]any{"ref_context": "type_position", "receiver": "hub"}},
			&graph.Edge{From: from, To: "repo/hub.go::Hub", Kind: graph.EdgeTypedAs, FilePath: fmt.Sprintf("repo/use%04d.go", i), Line: 4},
		)
	}
	s.AddBatch(nodes, edges)

	ids := []string{"repo/hub.go::Hub"}
	kinds := []graph.EdgeKind{graph.EdgeMemberOf, graph.EdgeParamOf}
	got := s.GetInEdgesByNodeIDsKinds(ids, kinds)
	if n := len(got["repo/hub.go::Hub"]); n != 4 {
		t.Fatalf("filtered in-edges = %d, want the 4 member_of edges", n)
	}
	for _, e := range got["repo/hub.go::Hub"] {
		if e.Kind != graph.EdgeMemberOf {
			t.Fatalf("filtered read returned %s edge", e.Kind)
		}
	}
	if n := len(s.GetInEdgesByNodeIDs(ids)["repo/hub.go::Hub"]); n != 4+2*fanIn {
		t.Fatalf("unfiltered in-edges = %d, want %d", n, 4+2*fanIn)
	}
	full := testing.AllocsPerRun(3, func() { _ = s.GetInEdgesByNodeIDs(ids) })
	filtered := testing.AllocsPerRun(3, func() { _ = s.GetInEdgesByNodeIDsKinds(ids, kinds) })
	t.Logf("hub in-edge read allocs: unfiltered=%.0f filtered=%.0f", full, filtered)
	if filtered*20 > full {
		t.Fatalf("filtered read allocs = %.0f, want < 1/20 of unfiltered %.0f", filtered, full)
	}
}

// Across kinds on one source line the kind-filtered outgoing read orders by
// kind name, whatever the insertion order: the statement's ORDER BY is
// explicit, so the order does not follow the plan. tstypes applySuper's fallback loop takes the first claimable
// extends-or-implements stub across both kinds, so this tie-break decides
// which stub it retargets on SQLite; the unfiltered batch read has no
// ORDER BY and its cross-kind order follows the plan.
func TestOutEdgesByNodeIDsKindsCrossKindTieBreak(t *testing.T) {
	s := openCatalogStore(t)
	const from = "repo/a.ts::Widget"
	nodes := []*graph.Node{{ID: from, Name: "Widget", Kind: graph.KindType, FilePath: "repo/a.ts", Language: "typescript"}}
	// Inserted against the expected order: implements before extends on
	// line 5, member_of first, a later line before an earlier one.
	edges := []*graph.Edge{
		{From: from, To: "repo/a.ts::Mod", Kind: graph.EdgeMemberOf, FilePath: "repo/a.ts", Line: 5},
		{From: from, To: "unresolved::Base", Kind: graph.EdgeImplements, FilePath: "repo/a.ts", Line: 5},
		{From: from, To: "unresolved::Other.Base", Kind: graph.EdgeExtends, FilePath: "repo/a.ts", Line: 5},
		{From: from, To: "unresolved::Late", Kind: graph.EdgeImplements, FilePath: "repo/a.ts", Line: 9},
		{From: from, To: "unresolved::Early", Kind: graph.EdgeExtends, FilePath: "repo/a.ts", Line: 2},
	}
	s.AddBatch(nodes, edges)

	type key struct {
		line int
		kind graph.EdgeKind
		to   string
	}
	want := []key{
		{2, graph.EdgeExtends, "unresolved::Early"},
		{5, graph.EdgeExtends, "unresolved::Other.Base"},
		{5, graph.EdgeImplements, "unresolved::Base"},
		{5, graph.EdgeMemberOf, "repo/a.ts::Mod"},
		{9, graph.EdgeImplements, "unresolved::Late"},
	}
	kinds := []graph.EdgeKind{graph.EdgeMemberOf, graph.EdgeImplements, graph.EdgeExtends}
	got := s.GetOutEdgesByNodeIDsKinds([]string{from}, kinds)[from]
	if len(got) != len(want) {
		t.Fatalf("got %d edges, want %d", len(got), len(want))
	}
	for i, e := range got {
		if k := (key{e.Line, e.Kind, e.To}); k != want[i] {
			t.Fatalf("edge %d = %+v, want %+v", i, k, want[i])
		}
	}
}

// Pin the filtered reader's own order, not equivalence with an unordered
// batch query. Same-kind calls at different sites may come back in a
// different order from that query after ANALYZE.
func TestEdgesByNodeIDsKindsWithinKindOrderWithStats(t *testing.T) {
	s := openCatalogStore(t)
	const from = "repo/App.java::handle"
	edges := []*graph.Edge{
		{From: from, To: "hub", Kind: graph.EdgeCalls, Line: 30},
		{From: from, To: "z", Kind: graph.EdgeCalls, Line: 3},
		{From: from, To: "a", Kind: graph.EdgeCalls, Line: 3},
		{From: from, To: "hub", Kind: graph.EdgeCalls, Line: 3},
	}
	s.AddBatch(nil, edges)
	if _, err := s.writerDB.Exec("ANALYZE"); err != nil {
		t.Fatal(err)
	}
	ids := []string{from, "hub"}
	for len(ids) < 5000 {
		ids = append(ids, fmt.Sprintf("missing::%04d", len(ids)))
	}
	kinds := []graph.EdgeKind{graph.EdgeCalls}
	// Outgoing: line, then insertion at a same-line/same-kind tie (not
	// target name order). Incoming: insertion within the requested kind.
	for _, tc := range []struct {
		name string
		got  []*graph.Edge
		want []*graph.Edge
	}{
		{"out", s.GetOutEdgesByNodeIDsKinds(ids, kinds)[from], []*graph.Edge{edges[1], edges[2], edges[3], edges[0]}},
		{"in", s.GetInEdgesByNodeIDsKinds(ids, kinds)["hub"], []*graph.Edge{edges[0], edges[3]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.got) != len(tc.want) {
				t.Fatalf("got %d edges, want %d", len(tc.got), len(tc.want))
			}
			for i, got := range tc.got {
				want := tc.want[i]
				if got.From != want.From || got.To != want.To || got.Line != want.Line {
					t.Errorf("edge %d = %s->%s at %d, want %s->%s at %d",
						i, got.From, got.To, got.Line, want.From, want.To, want.Line)
				}
			}
		})
	}
}
