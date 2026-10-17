package graphview_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The same real positive lower owns the source edge in every case. Node
// identity and outgoing-set ownership are independently varied in the upper.
func TestGenerationLayerBoundedIncomingSourcesSeparatesIdentityAndOutgoing(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		identity, outgoing, legacy, carried bool
		wantSource                          bool
	}{
		{name: "unchanged", wantSource: true},
		{name: "identity_only", identity: true, carried: true, wantSource: true},
		{name: "outgoing_only_empty", outgoing: true},
		{name: "identity_and_outgoing_empty", identity: true, outgoing: true, carried: true},
		{name: "legacy_carried_empty", legacy: true, carried: true},
		{name: "legacy_removed", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNodeMaskFixture(t)
			if tc.carried {
				node := f.lower.GetNode(f.id)
				if node == nil {
					t.Fatal("missing real lower source")
				}
				node.Name = "UpdatedSource"
				f.upper.AddNode(node)
			}
			if tc.identity {
				if err := f.upper.SetNodeIdentityReplacements([]string{f.id}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.legacy {
				if err := f.upper.SetNodeTombstones([]string{f.id}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.outgoing {
				if err := f.upper.SetEdgeSourceMasks([]store_sqlite.EdgeSourceMask{{SourceID: f.id, Mode: store_sqlite.OwnershipReplace}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.control.PublishPayloadGeneration(t.Context(), f.upperID, 3); err != nil {
				t.Fatal(err)
			}
			view, layer := explicitMaskView(t, f)
			if layer.HasFile("alpha/a.cs") {
				t.Fatal("fixture acquired whole-file ownership")
			}
			if layer.OwnsOutEdges(f.id) != (tc.outgoing || tc.legacy) {
				t.Fatal("outgoing fixture admission mismatch")
			}
			if f.upper.EdgeCount() != 0 {
				t.Fatal("empty replacement fixture copied an edge")
			}
			lower, err := f.lower.FindIncomingSourcesBounded(t.Context(), []string{f.sibling}, graph.EdgeKind("calls"), 1)
			if err != nil || lower.Truncated[f.sibling] || !reflect.DeepEqual(lower.Sources[f.sibling], []string{f.id}) {
				t.Fatalf("lower prerequisite: %+v %v", lower, err)
			}
			var want []string
			if tc.wantSource {
				want = []string{f.id}
			}
			common := view.GetInEdges(f.sibling)
			if len(common) != len(want) || len(common) == 1 && common[0].From != f.id {
				t.Fatalf("common-reader control: %+v want %v", common, want)
			}
			page, err := view.FindIncomingSourcesBounded(t.Context(), []string{f.sibling, f.sibling}, graph.EdgeKind("calls"), 1)
			if err != nil {
				t.Fatal(err)
			}
			if page.Truncated[f.sibling] || !reflect.DeepEqual(page.Sources[f.sibling], want) {
				t.Fatalf("specialized source ownership: %+v want %v", page, want)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			page, err = view.FindIncomingSourcesBounded(ctx, []string{f.sibling}, graph.EdgeKind("calls"), 1)
			if !errors.Is(err, context.Canceled) || len(page.Sources) != 0 || len(page.Truncated) != 0 {
				t.Fatalf("canceled partial projection: %+v %v", page, err)
			}
			if f.lower.GetNode(f.id).Name != "Run" || f.control.GetNode(f.id).Name != "gen0 poison" {
				t.Fatal("query mutated lower or gen0")
			}
		})
	}
}
