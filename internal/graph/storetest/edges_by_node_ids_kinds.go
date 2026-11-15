package storetest

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// testEdgesByNodeIDsKinds pins the kind-filtered batched adjacency read: per
// node, the result must be exactly the unfiltered batch read restricted to the
// requested kinds, with the full payload (Meta included) and, within each
// kind, the unfiltered read's order. Both the optional capability (when the
// backend has it) and the graph-level helper (capability or fallback) are
// held to that.
func testEdgesByNodeIDsKinds(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	for _, id := range []string{"hub", "m", "a", "b", "c"} {
		s.AddNode(mkNode(id, id, "x.go", graph.KindFunction))
	}
	add := func(from, to string, kind graph.EdgeKind, line int, meta map[string]any) {
		e := mkEdge(from, to, kind)
		e.Line = line
		e.Meta = meta
		s.AddEdge(e)
	}
	// Seeded out of line order, with interleaved kinds and duplicate
	// (from, to, kind) pairs on different lines, so order and payload
	// differences between the filtered and unfiltered reads are observable.
	add("m", "hub", graph.EdgeCalls, 30, map[string]any{"semantic_source": "x"})
	add("m", "hub", graph.EdgeReferences, 5, map[string]any{"ref": "r1"})
	add("m", "a", graph.EdgeCalls, 10, nil)
	add("m", "hub", graph.EdgeMemberOf, 1, nil)
	add("m", "b", graph.EdgeExtends, 2, map[string]any{"resolution": "stub"})
	add("m", "hub", graph.EdgeCalls, 7, nil)
	add("m", "c", graph.EdgeReads, 8, nil)
	add("a", "hub", graph.EdgeReferences, 3, nil)
	add("b", "hub", graph.EdgeCalls, 4, nil)
	add("c", "hub", graph.EdgeParamOf, 1, nil)
	add("a", "hub", graph.EdgeMemberOf, 2, nil)

	ids := []string{"m", "hub", "a", "missing", "", "m"}
	kindSets := [][]graph.EdgeKind{
		{graph.EdgeCalls},
		{graph.EdgeCalls, graph.EdgeMemberOf, graph.EdgeExtends},
		{graph.EdgeMemberOf, graph.EdgeParamOf},
		{graph.EdgeImplements},
	}
	reader, hasCapability := s.(graph.EdgesByNodeIDsKindsReader)
	for _, kinds := range kindSets {
		wantOut := restrictEdgeMap(s.GetOutEdgesByNodeIDs(ids), kinds)
		wantIn := restrictEdgeMap(s.GetInEdgesByNodeIDs(ids), kinds)
		assertKindFilteredEdges(t, "OutEdgesByNodeIDsOfKinds", kinds, wantOut, graph.OutEdgesByNodeIDsOfKinds(s, ids, kinds))
		assertKindFilteredEdges(t, "InEdgesByNodeIDsOfKinds", kinds, wantIn, graph.InEdgesByNodeIDsOfKinds(s, ids, kinds))
		if hasCapability {
			assertKindFilteredEdges(t, "GetOutEdgesByNodeIDsKinds", kinds, wantOut, reader.GetOutEdgesByNodeIDsKinds(ids, kinds))
			assertKindFilteredEdges(t, "GetInEdgesByNodeIDsKinds", kinds, wantIn, reader.GetInEdgesByNodeIDsKinds(ids, kinds))
		}
	}

	// Empty kinds is the unfiltered read.
	all := restrictEdgeMap(s.GetOutEdgesByNodeIDs(ids), nil)
	assertKindFilteredEdges(t, "OutEdgesByNodeIDsOfKinds(nil kinds)", nil, all, graph.OutEdgesByNodeIDsOfKinds(s, ids, nil))
	if got := graph.OutEdgesByNodeIDsOfKinds(s, nil, []graph.EdgeKind{graph.EdgeCalls}); len(got) != 0 {
		t.Fatalf("OutEdgesByNodeIDsOfKinds(nil ids) returned %d entries", len(got))
	}
	if got := graph.InEdgesByNodeIDsOfKinds(s, []string{""}, []graph.EdgeKind{graph.EdgeCalls}); len(nonEmptyEdgeLists(got)) != 0 {
		t.Fatalf("InEdgesByNodeIDsOfKinds([\"\"]) returned %v", got)
	}
	// Payload survives the filtered read: Meta is decoded, not dropped.
	out := graph.OutEdgesByNodeIDsOfKinds(s, []string{"m"}, []graph.EdgeKind{graph.EdgeExtends, graph.EdgeCalls})
	var sawMeta bool
	for _, e := range out["m"] {
		if e.Kind == graph.EdgeExtends {
			sawMeta = e.Meta["resolution"] == "stub"
		}
	}
	if !sawMeta {
		t.Fatalf("filtered extends edge lost its meta: %v", describeEdges(out["m"]))
	}
}

// restrictEdgeMap is the reference filter: every non-empty ID's edges of the
// requested kinds (all kinds when nil), in the read's order.
func restrictEdgeMap(in map[string][]*graph.Edge, kinds []graph.EdgeKind) map[string][]*graph.Edge {
	want := make(map[graph.EdgeKind]bool, len(kinds))
	for _, k := range kinds {
		want[k] = true
	}
	out := make(map[string][]*graph.Edge)
	for id, edges := range in {
		for _, e := range edges {
			if e != nil && (len(kinds) == 0 || want[e.Kind]) {
				out[id] = append(out[id], e)
			}
		}
	}
	return out
}

func nonEmptyEdgeLists(in map[string][]*graph.Edge) map[string][]*graph.Edge {
	out := make(map[string][]*graph.Edge)
	for id, edges := range in {
		if len(edges) > 0 {
			out[id] = edges
		}
	}
	return out
}

// assertKindFilteredEdges compares per ID: the same edges with the same
// payload, and the same sequence within every kind.
func assertKindFilteredEdges(t *testing.T, label string, kinds []graph.EdgeKind, want, got map[string][]*graph.Edge) {
	t.Helper()
	got = nonEmptyEdgeLists(got)
	want = nonEmptyEdgeLists(want)
	if len(got) != len(want) {
		t.Fatalf("%s(%v): ids = %v, want %v", label, kinds, edgeMapIDs(got), edgeMapIDs(want))
	}
	for id, wantEdges := range want {
		gotEdges := got[id]
		if len(gotEdges) != len(wantEdges) {
			t.Fatalf("%s(%v)[%s] = %v, want %v", label, kinds, id, describeEdges(gotEdges), describeEdges(wantEdges))
		}
		perKind := func(edges []*graph.Edge) map[graph.EdgeKind][]string {
			out := make(map[graph.EdgeKind][]string)
			for _, e := range edges {
				out[e.Kind] = append(out[e.Kind], describeEdge(e))
			}
			return out
		}
		if g, w := perKind(gotEdges), perKind(wantEdges); !reflect.DeepEqual(g, w) {
			t.Fatalf("%s(%v)[%s] per-kind sequence = %v, want %v", label, kinds, id, g, w)
		}
	}
}

func edgeMapIDs(in map[string][]*graph.Edge) []string {
	ids := make([]string, 0, len(in))
	for id := range in {
		ids = append(ids, id)
	}
	return ids
}

func describeEdges(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, describeEdge(e))
	}
	return out
}

func describeEdge(e *graph.Edge) string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s->%s:%s@%s:%d conf=%v label=%s origin=%s tier=%s meta=%v",
		e.From, e.To, e.Kind, e.FilePath, e.Line, e.Confidence, e.ConfidenceLabel, e.Origin, e.Tier, e.Meta)
}
