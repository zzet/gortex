package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// A pass's provides rows come from the installed source when one is set: the
// graph's own provides edges are not scanned, and the index is built from
// the source's rows only.
func TestProvidesRowsComeFromTheInstalledSource(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{{ID: "repo/m.ts::M", Kind: graph.KindType, Name: "M", FilePath: "repo/m.ts", RepoPrefix: "repo"}},
		[]*graph.Edge{{From: "repo/m.ts::M", To: graph.UnresolvedMarker + "Scanned", Kind: graph.EdgeProvides, FilePath: "repo/m.ts",
			Meta: map[string]any{graph.MetaDIProvidesFor: "Logger", graph.MetaDIBinding: graph.DIBindingUseClass}}})
	r := New(g)
	calls := 0
	r.SetProvidesRowsSource(func() map[string][]*graph.Edge {
		calls++
		return map[string][]*graph.Edge{"repo": {{From: "repo/m.ts::M", To: graph.UnresolvedMarker + "Sourced", Kind: graph.EdgeProvides,
			Meta: map[string]any{graph.MetaDIProvidesFor: "Logger", graph.MetaDIBinding: graph.DIBindingUseClass}}}}
	})
	p := newPendingFrontierPassIndexes(r)
	p.ensureProvides([]string{"repo"})
	if calls != 1 {
		t.Fatalf("the source answered %d times", calls)
	}
	if _, ok := r.providesForIdx["Logger"]["Sourced"]; !ok {
		t.Fatalf("the index was not built from the source: %v", r.providesForIdx)
	}
	if _, ok := r.providesForIdx["Logger"]["Scanned"]; ok {
		t.Fatalf("the graph's rows were scanned despite the source: %v", r.providesForIdx)
	}
}

// A pass whose pending set spans a reference with no repository (the whole
// index is built) builds it from the installed source too.
func TestProvidesWholeIndexComesFromTheInstalledSource(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{{ID: "repo/m.ts::M", Kind: graph.KindType, Name: "M", FilePath: "repo/m.ts", RepoPrefix: "repo"}},
		[]*graph.Edge{{From: "repo/m.ts::M", To: graph.UnresolvedMarker + "Scanned", Kind: graph.EdgeProvides, FilePath: "repo/m.ts",
			Meta: map[string]any{graph.MetaDIProvidesFor: "Logger", graph.MetaDIBinding: graph.DIBindingUseClass}}})
	r := New(g)
	r.SetProvidesRowsSource(func() map[string][]*graph.Edge {
		return map[string][]*graph.Edge{"repo": {{From: "repo/m.ts::M", To: graph.UnresolvedMarker + "Sourced", Kind: graph.EdgeProvides,
			Meta: map[string]any{graph.MetaDIProvidesFor: "Logger", graph.MetaDIBinding: graph.DIBindingUseClass}}}}
	})
	p := newPendingFrontierPassIndexes(r)
	p.ensureProvides([]string{"repo", ""})
	if _, ok := r.providesForIdx["Logger"]["Sourced"]; !ok {
		t.Fatalf("the whole index was not built from the source: %v", r.providesForIdx)
	}
	if _, ok := r.providesForIdx["Logger"]["Scanned"]; ok {
		t.Fatalf("the whole index scanned the graph despite the source: %v", r.providesForIdx)
	}
}
