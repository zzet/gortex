package graphview_test

import (
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestGenerationLayerGroupedIncomingPagesKeepPerFileOwnershipAndIdentity(t *testing.T) {
	const from, target = "alpha/a.cs::Run", "alpha/a.cs::Sibling"
	const hidden, kept = "alpha/a.cs::Hidden", "alpha/a.cs::Kept"
	extras := []*graph.Node{
		{ID: hidden, Name: "Hidden", RepoPrefix: "alpha", FilePath: "alpha/a.cs", Kind: graph.KindType},
		{ID: kept, Name: "Kept", RepoPrefix: "alpha", FilePath: "alpha/a.cs", Kind: graph.KindType},
	}
	edges := make([]*graph.Edge, 300)
	for i := range edges {
		edges[i] = &graph.Edge{From: from, To: target, Kind: graph.EdgeKind("calls"), FilePath: "alpha/left.cs", Line: i + 1}
	}
	edges = append(edges,
		&graph.Edge{From: hidden, To: target, Kind: graph.EdgeKind("calls"), FilePath: "alpha/left.cs", Line: 301},
		&graph.Edge{From: from, To: target, Kind: graph.EdgeKind("calls"), FilePath: "alpha/right.cs", Line: 302},
		&graph.Edge{From: kept, To: target, Kind: graph.EdgeKind("calls"), FilePath: "", Line: 303},
	)
	f := newIncomingCandidateFixture(t, extras, edges)
	if err := f.upper.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: "alpha", FilePath: "alpha/left.cs", Mode: store_sqlite.OwnershipDelete}}); err != nil {
		t.Fatal(err)
	}
	carried := f.lower.GetNode(from)
	if carried == nil {
		t.Fatal("missing source identity precondition")
	}
	f.upper.AddNode(carried)
	if err := f.upper.SetNodeIdentityReplacements([]string{from}); err != nil {
		t.Fatal(err)
	}
	if err := f.control.PublishPayloadGeneration(t.Context(), f.upperID, 3); err != nil {
		t.Fatal(err)
	}
	view, layer := explicitMaskView(t, f)
	if layer.CoversNodeID(from) || layer.OwnsOutEdges(from) || !layer.OwnsNodeIdentity(from) {
		t.Fatal("fixture did not separate carried identity, source ownership, and edge-file ownership")
	}
	if got := len(view.GetInEdges(target)); got != 2 {
		t.Fatalf("common-reader ownership control: %d edges, want2", got)
	}
	for _, limit := range []int{1, 2} {
		projection, err := view.FindIncomingSourcesBounded(t.Context(), []string{target}, graph.EdgeKind("calls"), limit)
		if err != nil {
			t.Fatal(err)
		}
		if limit == 1 {
			if !projection.Truncated[target] || len(projection.Sources[target]) != 0 {
				t.Fatalf("grouped sentinel returned partial sources: %+v", projection)
			}
		} else if projection.Truncated[target] || !reflect.DeepEqual(projection.Sources[target], []string{kept, from}) {
			t.Fatalf("grouped page lost visible provenance after many hidden sites: %+v", projection)
		}
	}
	if f.lower.EdgeCount() != len(edges) || f.upper.EdgeCount() != 0 {
		t.Fatal("projection changed/copied physical edge input")
	}
}
