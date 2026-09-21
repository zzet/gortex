package graphview_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestGenerationLayerIncomingScopePushesOuterOwnershipBeforeInnerSentinel(t *testing.T) {
	const target = "alpha/a.cs::Sibling"
	nodes := make([]*graph.Node, 301)
	edges := make([]*graph.Edge, len(nodes))
	masks := make([]store_sqlite.EdgeSourceMask, 300)
	for i := range nodes {
		id := fmt.Sprintf("alpha/sources.cs::S%04d", i)
		nodes[i] = &graph.Node{ID: id, Name: "Source", RepoPrefix: "alpha", FilePath: "alpha/sources.cs", Kind: graph.KindType}
		edges[i] = &graph.Edge{From: id, To: target, Kind: graph.EdgeKind("calls"), FilePath: "alpha/sources.cs", Line: i + 1}
		if i < len(masks) {
			masks[i] = store_sqlite.EdgeSourceMask{SourceID: id, Mode: store_sqlite.OwnershipReplace}
		}
	}
	f := newIncomingCandidateFixture(t, nodes, edges)
	// The inner layer has real identity-only target metadata, no edge ownership.
	f.upper.AddNode(f.lower.GetNode(target))
	if err := f.upper.SetNodeIdentityReplacements([]string{target}); err != nil {
		t.Fatal(err)
	}
	if err := f.control.PublishPayloadGeneration(t.Context(), f.upperID, 3); err != nil {
		t.Fatal(err)
	}
	inner, _ := explicitMaskView(t, f)
	before, err := inner.FindIncomingSourcesBounded(t.Context(), []string{target}, graph.EdgeKind("calls"), 1)
	if err != nil || !before.Truncated[target] {
		t.Fatalf("inner must have301 eligible sources: %+v %v", before, err)
	}
	outerID, err := f.control.Catalog().CreateViewGeneration(t.Context(), store_sqlite.ViewGeneration{OwnerKind: "dedicated_graph", GraphID: "incoming-candidate-fixture", GenerationKind: "dedicated", BaseGenerationID: f.upperID, TreeOID: "same-source", ConfigHash: "same-policy", State: store_sqlite.ViewGenerationBuilding, CreatedAt: 4})
	if err != nil {
		t.Fatal(err)
	}
	outer := f.control.AtGeneration(outerID)
	if err := outer.SetEdgeSourceMasks(masks); err != nil {
		t.Fatal(err)
	}
	if err := f.control.PublishPayloadGeneration(t.Context(), outerID, 5); err != nil {
		t.Fatal(err)
	}
	layer, err := graphview.NewGenerationLayer(outer)
	if err != nil {
		t.Fatal(err)
	}
	view := graph.NewOverlaidViewWithLayer(inner, layer)
	common := view.GetInEdges(target)
	want := nodes[len(nodes)-1].ID
	if len(common) != 1 || common[0].From != want {
		t.Fatalf("canonical nested oracle: %+v", common)
	}
	budget := &graph.IncomingSourceBudget{}
	page, err := view.FindIncomingSourcesScoped(t.Context(), []string{target}, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, budget)
	if err != nil || page.Truncated[target] || !reflect.DeepEqual(page.Sources[target], []string{want}) {
		t.Fatalf("outer filtering happened after inner sentinel: %+v %v", page, err)
	}
	if inspected := graph.MaxIncomingSourceCandidateRows - budget.Remaining(); inspected != 301 {
		t.Fatalf("nested projection duplicated physical candidates: %d", inspected)
	}
	public, err := view.FindIncomingSourcesBounded(t.Context(), []string{target}, graph.EdgeKind("calls"), 1)
	if err != nil || !reflect.DeepEqual(public, page) {
		t.Fatalf("public/scoped parity: %+v %+v %v", public, page, err)
	}
	if outer.NodeCount() != 0 || outer.EdgeCount() != 0 || f.lower.EdgeCount() != 301 {
		t.Fatal("fixture/query copied graph payload")
	}
}
