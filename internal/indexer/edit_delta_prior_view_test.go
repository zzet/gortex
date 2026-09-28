package indexer

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// A changed file's prior adjacency is kept per stack: while the delta has
// written nothing, every answer equals the composed read, a second delta
// reads nothing again, and a delta that has written anything reads through.
func TestEditDeltaPriorViewIsKeptPerStack(t *testing.T) {
	resetEditDeltaPriorViews()
	t.Cleanup(resetEditDeltaPriorViews)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()
	var ids []string
	for _, n := range view.Reader.GetFileNodes(builderRepoPrefix + "/a/a.go") {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	render := func(in, out map[string][]*graph.Edge) string {
		var rows []string
		for dir, m := range map[string]map[string][]*graph.Edge{"in": in, "out": out} {
			for id, edges := range m {
				for _, e := range edges {
					rows = append(rows, fmt.Sprintf("%s %s %s-%s->%s@%d", dir, id, e.From, e.Kind, e.To, e.Line))
				}
			}
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n")
	}
	plain := graph.NewDeltaWriter(view.Reader, nil)
	want := render(plain.GetInEdgesByNodeIDs(ids), plain.GetOutEdgesByNodeIDs(ids))
	if want == "" {
		t.Fatal("fixture precondition: a.go has no adjacency")
	}
	reads := 0
	for delta := 0; delta < 2; delta++ {
		dw := graph.NewDeltaWriter(view.Reader, nil)
		before := priorViewLoads.Load()
		in, out, ok := editDeltaPriorEdges(dw, "stack")(ids)
		if !ok {
			t.Fatalf("delta %d: an untouched delta did not use the kept rows", delta)
		}
		if got := render(in, out); got != want {
			t.Fatalf("delta %d:\n%s\nwant:\n%s", delta, got, want)
		}
		if priorViewLoads.Load() != before {
			reads++
		}
	}
	if reads != 1 {
		t.Fatalf("the stack's rows were read on %d deltas, want the first only", reads)
	}
	touched := graph.NewDeltaWriter(view.Reader, nil)
	touched.EvictFile(builderRepoPrefix + "/b/b.go")
	if _, _, ok := editDeltaPriorEdges(touched, "stack")(ids); ok {
		t.Fatal("a delta that has written answered from the kept rows")
	}
}
