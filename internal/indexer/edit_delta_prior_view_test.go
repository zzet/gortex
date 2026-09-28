package indexer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

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

// The pre-warm keeps a likely file's rows below (the payload's comparison
// source) beside its prior view, so the first delta over the stack reads
// neither again; the kept rows are the composed ones, placeholder-keyed rows
// recorded at the file included.
func TestPrewarmKeepsALikelyFilesRowsBelowBesideItsPriorView(t *testing.T) {
	resetEditDeltaGoInventories()
	resetEditDeltaProvides()
	resetEditDeltaDeps()
	resetEditDeltaPriorViews()
	t.Cleanup(resetEditDeltaGoInventories)
	t.Cleanup(resetEditDeltaProvides)
	t.Cleanup(resetEditDeltaDeps)
	t.Cleanup(resetEditDeltaPriorViews)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()
	base := commitLayerBase{Reader: view.Reader, stack: []int64{8000001, 8000002}}
	key, _ := editDeltaBaseCacheKey(base, c.builder.Store)
	editDeltaPrewarmLowPriorityPause = 0
	t.Cleanup(func() { editDeltaPrewarmLowPriorityPause = 50 * time.Millisecond })
	if !c.builder.PrewarmEditDeltaStack(context.Background(), openFixed(base), c.repoPrefix, c.workspaceID, c.projectID, []string{"a/a.go"}) {
		t.Fatal("the pre-warm did not run")
	}
	path := c.repoPrefix + "/a/a.go"
	editDeltaBelowRows.Lock()
	rows, kept := editDeltaBelowRows.byKey[key][path]
	editDeltaBelowRows.Unlock()
	if !kept {
		t.Fatal("the pre-warm left the likely file's rows below unkept")
	}
	recorded, ok := graph.RecordedEdgesOf(base)
	if !ok {
		t.Fatal("fixture precondition: the base serves recorded rows")
	}
	wantNodes, wantEdges := len(base.GetFileNodes(path)), len(recorded.RecordedEdgesAt([]string{path}))
	if wantNodes == 0 || wantEdges == 0 {
		t.Fatalf("fixture precondition: a.go holds %d nodes, %d recorded rows", wantNodes, wantEdges)
	}
	if len(rows.nodes) != wantNodes || len(rows.recorded) != wantEdges {
		t.Fatalf("kept %d nodes, %d rows; the composition holds %d, %d", len(rows.nodes), len(rows.recorded), wantNodes, wantEdges)
	}
}
