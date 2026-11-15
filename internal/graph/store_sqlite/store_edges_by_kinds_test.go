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
