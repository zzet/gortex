package graphview_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func newIncomingCandidateFixture(t testing.TB, extras []*graph.Node, edges []*graph.Edge) nodeMaskFixture {
	t.Helper()
	f := newNodeMaskFixture(t)
	nodes := append([]*graph.Node{f.lower.GetNode(f.id), f.lower.GetNode(f.sibling)}, extras...)
	allocate := func(base int64) int64 {
		id, err := f.control.Catalog().CreateViewGeneration(t.Context(), store_sqlite.ViewGeneration{OwnerKind: "dedicated_graph", GraphID: "incoming-candidate-fixture", GenerationKind: "dedicated", BaseGenerationID: base, TreeOID: "same-source", ConfigHash: "same-policy", State: store_sqlite.ViewGenerationBuilding, CreatedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	lowerID := allocate(0)
	f.lower = f.control.AtGeneration(lowerID)
	f.lower.AddBatch(nodes, edges)
	if err := f.control.Catalog().PublishViewGeneration(t.Context(), lowerID, 2); err != nil {
		t.Fatal(err)
	}
	f.upperID = allocate(lowerID)
	f.upper = f.control.AtGeneration(f.upperID)
	return f
}

func TestGenerationLayerIncomingCandidatesKeepManySitesAndIgnoreUnrelatedMarkers(t *testing.T) {
	edges := make([]*graph.Edge, 300)
	for i := range edges {
		edges[i] = &graph.Edge{From: "alpha/a.cs::Run", To: "alpha/a.cs::Sibling", Kind: graph.EdgeKind("calls"), FilePath: "alpha/a.cs", Line: i + 1}
	}
	f := newIncomingCandidateFixture(t, nil, edges)
	ids := make([]string, 1024)
	rows := make([]*graph.Node, 1024)
	for i := range ids {
		ids[i] = fmt.Sprintf("alpha/metadata.cs::M%04d", i)
		rows[i] = &graph.Node{ID: ids[i], Name: "Metadata", RepoPrefix: "alpha", FilePath: "alpha/metadata.cs", Kind: graph.KindType}
	}
	f.upper.AddBatch(rows, nil)
	if err := f.upper.SetNodeIdentityReplacements(ids); err != nil {
		t.Fatal(err)
	}
	if err := f.control.PublishPayloadGeneration(t.Context(), f.upperID, 3); err != nil {
		t.Fatal(err)
	}
	view, _ := explicitMaskView(t, f)
	page, err := view.FindIncomingSourcesBounded(t.Context(), []string{f.sibling}, graph.EdgeKind("calls"), 1)
	if err != nil || page.Truncated[f.sibling] || !reflect.DeepEqual(page.Sources[f.sibling], []string{f.id}) {
		t.Fatalf("300 sites/1024 unrelated rows lost one source: %+v %v", page, err)
	}
	if f.lower.EdgeCount() != 300 || f.upper.EdgeCount() != 0 || f.upper.NodeCount() != 1024 {
		t.Fatal("fixture copied/mutated edges")
	}
}

func TestGenerationLayerIncomingCandidatesApplyEdgeFileOwnershipBeforeDedup(t *testing.T) {
	hidden, kept := "alpha/a.cs::Hidden", "alpha/a.cs::Kept"
	from, target := "alpha/a.cs::Run", "alpha/a.cs::Sibling"
	extras := []*graph.Node{{ID: hidden, Name: "Hidden", RepoPrefix: "alpha", FilePath: "alpha/a.cs", Kind: graph.KindType}, {ID: kept, Name: "Kept", RepoPrefix: "alpha", FilePath: "alpha/a.cs", Kind: graph.KindType}}
	edges := []*graph.Edge{
		{From: hidden, To: target, Kind: graph.EdgeKind("calls"), FilePath: "alpha/left.cs", Line: 1},
		{From: kept, To: target, Kind: graph.EdgeKind("calls"), FilePath: "alpha/right.cs", Line: 2},
		{From: from, To: target, Kind: graph.EdgeKind("calls"), FilePath: "alpha/left.cs", Line: 3},
		{From: from, To: target, Kind: graph.EdgeKind("calls"), FilePath: "alpha/right.cs", Line: 4},
	}
	f := newIncomingCandidateFixture(t, extras, edges)
	if err := f.upper.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: "alpha", FilePath: "alpha/left.cs", Mode: store_sqlite.OwnershipDelete}}); err != nil {
		t.Fatal(err)
	}
	if err := f.control.PublishPayloadGeneration(t.Context(), f.upperID, 3); err != nil {
		t.Fatal(err)
	}
	view, layer := explicitMaskView(t, f)
	if layer.CoversNodeID(hidden) || layer.OwnsOutEdges(from) {
		t.Fatal("fixture replaced the source rather than only one edge file")
	}
	if len(view.GetInEdges(target)) != 2 {
		t.Fatal("common edge-file ownership control failed")
	}
	page, err := view.FindIncomingSourcesBounded(t.Context(), []string{target}, graph.EdgeKind("calls"), 2)
	if err != nil || page.Truncated[target] || !reflect.DeepEqual(page.Sources[target], []string{kept, from}) {
		t.Fatalf("file ownership before source dedup: %+v %v", page, err)
	}
	limited, err := view.FindIncomingSourcesBounded(t.Context(), []string{target}, graph.EdgeKind("calls"), 1)
	if err != nil || !limited.Truncated[target] || len(limited.Sources[target]) != 0 {
		t.Fatalf("source sentinel partial: %+v %v", limited, err)
	}
}

func TestGenerationLayerIncomingCandidatesFailClosedOnMatchingWorkBudget(t *testing.T) {
	const rawLimit = 16_384
	edges := make([]*graph.Edge, rawLimit+1)
	for i := range edges {
		edges[i] = &graph.Edge{From: "alpha/a.cs::Run", To: "alpha/a.cs::Sibling", Kind: graph.EdgeKind("calls"), FilePath: "alpha/a.cs", Line: i + 1}
	}
	f := newIncomingCandidateFixture(t, nil, edges)
	if err := f.control.PublishPayloadGeneration(t.Context(), f.upperID, 3); err != nil {
		t.Fatal(err)
	}
	view, _ := explicitMaskView(t, f)
	page, err := view.FindIncomingSourcesBounded(t.Context(), []string{f.sibling}, graph.EdgeKind("calls"), 1)
	var limitErr *graph.BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "incoming-source candidate inspections" || limitErr.Limit != rawLimit || len(page.Sources) != 0 || len(page.Truncated) != 0 {
		t.Fatalf("matching work was not bounded: %+v %v", page, err)
	}
}
