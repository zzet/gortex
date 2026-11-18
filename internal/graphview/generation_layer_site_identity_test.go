package graphview_test

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestGenerationLayerBoundedSiteIdentityKeepsOutgoingOwnershipIndependent(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		identity, outgoing, legacy, keep bool
	}{
		{name: "identity_only", identity: true, keep: true},
		{name: "outgoing_only_empty", outgoing: true},
		{name: "identity_and_outgoing_empty", identity: true, outgoing: true},
		{name: "legacy_carried_empty", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNodeMaskFixture(t)
			if tc.identity || tc.legacy {
				f.upper.AddNode(f.lower.GetNode(f.id))
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
			if layer.HasFile("alpha/a.cs") || layer.OwnsOutEdges(f.id) != (tc.outgoing || tc.legacy) {
				t.Fatal("site ownership prerequisites")
			}
			site := graph.EdgeSourceSite{From: f.id, Line: 1}
			kinds := []graph.EdgeKind{graph.EdgeKind("calls")}
			lower, err := f.lower.FindOutgoingSiteEdgeIdentitiesBounded(t.Context(), []graph.EdgeSourceSite{site}, kinds, 1)
			if err != nil || lower.Truncated[site] || len(lower.BySite[site]) != 1 || lower.BySite[site][0].To != f.sibling {
				t.Fatalf("lower site prerequisite: %+v %v", lower, err)
			}
			page, err := view.FindOutgoingSiteEdgeIdentitiesBounded(t.Context(), []graph.EdgeSourceSite{site}, kinds, 1)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.keep {
				want = 1
			}
			if page.Truncated[site] || len(page.BySite[site]) != want {
				t.Fatalf("site projection %+v want %d edges", page, want)
			}
			if tc.keep && (page.BySite[site][0].From != f.id || page.BySite[site][0].To != f.sibling || page.BySite[site][0].FilePath != "alpha/a.cs" || page.BySite[site][0].Line != 1) {
				t.Fatal("site projection lost source/file attribution")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			page, err = view.FindOutgoingSiteEdgeIdentitiesBounded(ctx, []graph.EdgeSourceSite{site}, kinds, 1)
			if !errors.Is(err, context.Canceled) || len(page.BySite) != 0 || len(page.Truncated) != 0 {
				t.Fatalf("site canceled partial: %+v %v", page, err)
			}
			if f.upper.EdgeCount() != 0 || f.lower.EdgeCount() != 2 {
				t.Fatal("site fixture copied or mutated adjacency")
			}
		})
	}
}
