package graph

import (
	"reflect"
	"testing"
)

// kindReaderlessStore hides the optional capability, as every wrapping store
// (overlay, delta writer, request-scoped readers) does.
type kindReaderlessStore struct{ Store }

func TestEdgesByNodeIDsOfKindsFallbackMatchesCapability(t *testing.T) {
	g := New()
	edges := []*Edge{
		{From: "a", To: "t", Kind: EdgeCalls, Line: 9},
		{From: "a", To: "t", Kind: EdgeReferences, Line: 1},
		{From: "a", To: "u", Kind: EdgeExtends, Line: 2},
		{From: "a", To: "t", Kind: EdgeCalls, Line: 3},
		{From: "m", To: "t", Kind: EdgeMemberOf, Line: 1},
		{From: "p", To: "m", Kind: EdgeParamOf, Line: 1},
		{From: "b", To: "t", Kind: EdgeCalls, Line: 4},
	}
	g.AddBatch(nil, edges)
	wrapped := kindReaderlessStore{Store: g}
	if _, ok := Store(wrapped).(EdgesByNodeIDsKindsReader); ok {
		t.Fatal("wrapper unexpectedly exposes the capability")
	}
	ids := []string{"a", "t", "m", "missing", "", "a"}
	for _, kinds := range [][]EdgeKind{{EdgeCalls}, {EdgeCalls, EdgeExtends}, {EdgeMemberOf, EdgeParamOf}, nil} {
		for _, dir := range []struct {
			name     string
			read     func(Store, []string, []EdgeKind) map[string][]*Edge
			unfilter func(Store, []string) map[string][]*Edge
		}{
			{"out", OutEdgesByNodeIDsOfKinds, Store.GetOutEdgesByNodeIDs},
			{"in", InEdgesByNodeIDsOfKinds, Store.GetInEdgesByNodeIDs},
		} {
			want := nonEmpty(filterEdgeMapKinds(dir.unfilter(g, ids), kinds))
			if kinds == nil {
				want = nonEmpty(dir.unfilter(g, ids))
			}
			direct := nonEmpty(dir.read(g, ids, kinds))
			fallback := nonEmpty(dir.read(wrapped, ids, kinds))
			if !reflect.DeepEqual(direct, want) {
				t.Fatalf("%s %v capability = %v, want %v", dir.name, kinds, direct, want)
			}
			if !reflect.DeepEqual(fallback, want) {
				t.Fatalf("%s %v fallback = %v, want %v", dir.name, kinds, fallback, want)
			}
		}
	}
	got := OutEdgesByNodeIDsOfKinds(g, []string{"a"}, []EdgeKind{EdgeCalls})["a"]
	if len(got) != 2 || got[0].Line != 9 || got[1].Line != 3 {
		t.Fatalf("in-memory filtered read must keep insertion order, got %v", got)
	}
}

func nonEmpty(in map[string][]*Edge) map[string][]*Edge {
	out := make(map[string][]*Edge)
	for id, edges := range in {
		if len(edges) > 0 {
			out[id] = edges
		}
	}
	return out
}
