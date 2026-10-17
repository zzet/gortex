package graphview_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestGenerationLayerIdentityBoundedAdjacencyMatchesOwnership(t *testing.T) {
	for _, tc := range []struct {
		name             string
		legacy, outgoing bool
	}{
		{name: "identity_only"},
		{name: "independent_outgoing", outgoing: true},
		{name: "legacy_carried", legacy: true, outgoing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNodeMaskFixture(t)
			f.upper.AddBatch([]*graph.Node{f.lower.GetNode(f.id)}, nil)
			kind := "identity_replace"
			if tc.legacy {
				kind = "legacy_tombstone"
			}
			setExplicitMaskFixtureRow(t, f, f.id, kind)
			if tc.outgoing && !tc.legacy {
				if err := f.upper.SetEdgeSourceMasks([]store_sqlite.EdgeSourceMask{{SourceID: f.id, Mode: store_sqlite.OwnershipReplace}}); err != nil {
					t.Fatal(err)
				}
			}
			view, _ := explicitMaskView(t, f)
			ids, kinds := []string{f.id, f.sibling}, []graph.EdgeKind{graph.EdgeKind("calls")}
			for _, read := range []struct {
				name         string
				lower, upper func(context.Context, []string, []graph.EdgeKind, int) (graph.BoundedEdgeIdentityProjection, error)
				replacedKey  string
			}{
				{"outgoing", f.lower.FindOutgoingEdgeIdentitiesBounded, view.FindOutgoingEdgeIdentitiesBounded, f.id},
				{"incoming", f.lower.FindIncomingEdgeIdentitiesBounded, view.FindIncomingEdgeIdentitiesBounded, f.sibling},
			} {
				want, err := read.lower(context.Background(), ids, kinds, 1)
				if err != nil {
					t.Fatal(err)
				}
				if len(want.ByEndpoint[f.id]) != 1 || len(want.ByEndpoint[f.sibling]) != 1 || len(want.Truncated) != 0 {
					t.Fatalf("invalid lower control: %+v", want)
				}
				if tc.outgoing {
					delete(want.ByEndpoint, read.replacedKey)
				}
				got, err := read.upper(context.Background(), ids, kinds, 1)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("%s ownership: got=%+v want=%+v err=%v", read.name, got, want, err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				canceled, err := read.upper(ctx, ids, kinds, 1)
				if err != context.Canceled || len(canceled.ByEndpoint) != 0 || len(canceled.Truncated) != 0 {
					t.Fatalf("%s cancellation returned authoritative rows: %+v err=%v", read.name, canceled, err)
				}
			}
		})
	}
}
