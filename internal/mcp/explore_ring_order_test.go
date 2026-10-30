package mcp

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestExploreRingOrdersEligibleNeighborsBeforeCap(t *testing.T) {
	a := &graph.Node{ID: "repo/a", Kind: graph.KindFunction}
	b := &graph.Node{ID: "repo/b", Kind: graph.KindMethod}
	z := &graph.Node{ID: "repo/z", Kind: graph.KindFunction}
	self := &graph.Node{ID: "repo/self", Kind: graph.KindFunction}
	file := &graph.Node{ID: "repo/file.go", Kind: graph.KindFile}
	for _, input := range [][]*graph.Node{
		{z, nil, b, self, file, a},
		{a, b, z, file, self},
		{file, self, b, a, z},
	} {
		first := input[0]
		got, complete := ringNeighborsProjection(input, self.ID, 2)
		if complete || len(got) != 2 || got[0] != a || got[1] != b {
			t.Fatalf("projection=%v complete=%v, want a,b and incomplete", got, complete)
		}
		if input[0] != first {
			t.Fatal("projection changed the traversal's input order")
		}
	}
	got, complete := ringNeighborsProjection([]*graph.Node{b, self, a}, self.ID, 2)
	if !complete || len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("complete projection=%v complete=%v", got, complete)
	}
}
