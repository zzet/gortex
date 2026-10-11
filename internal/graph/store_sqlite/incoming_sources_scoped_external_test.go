package store_sqlite_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestIncomingSourcePublicGenerationLayerUsesSingleConnectionBridge(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "one-pool-incoming.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	allocate := func(base int64) int64 {
		id, err := s.Catalog().CreateViewGeneration(t.Context(), store_sqlite.ViewGeneration{OwnerKind: "dedicated_graph", GraphID: "same-pool-fixture", GenerationKind: "dedicated", BaseGenerationID: base, TreeOID: "same-source", ConfigHash: "same-policy", State: store_sqlite.ViewGenerationBuilding, CreatedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	lowerID := allocate(0)
	lower := s.AtGeneration(lowerID)
	const from, target = "repo/a.go::From", "repo/a.go::Target"
	nodes := []*graph.Node{{ID: from, Name: "From", FilePath: "repo/a.go", RepoPrefix: "repo", Kind: graph.KindType}, {ID: target, Name: "Target", FilePath: "repo/a.go", RepoPrefix: "repo", Kind: graph.KindType}}
	lower.AddBatch(nodes, []*graph.Edge{{From: from, To: target, Kind: graph.EdgeKind("calls"), FilePath: "repo/a.go", Line: 1}})
	if err := s.Catalog().PublishViewGeneration(t.Context(), lowerID, 2); err != nil {
		t.Fatal(err)
	}
	upperID := allocate(lowerID)
	upper := s.AtGeneration(upperID)
	upper.AddBatch(nodes, nil)
	if err := upper.SetNodeIdentityReplacements([]string{from, target}); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishPayloadGeneration(t.Context(), upperID, 3); err != nil {
		t.Fatal(err)
	}
	layer, err := graphview.NewGenerationLayer(upper)
	if err != nil {
		t.Fatal(err)
	}
	if !layer.OwnsNodeIdentity(from) || layer.OwnsOutEdges(from) || layer.CoversNodeID(from) {
		t.Fatal("not a detached identity-only fixture")
	}
	store_sqlite.SetIncomingSourceReaderPoolSizeForTest(s, 1)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	view := graph.NewOverlaidViewWithLayer(lower, layer)
	page, err := view.FindIncomingSourcesBounded(ctx, []string{target}, graph.EdgeKind("calls"), 1)
	if err != nil || page.Truncated[target] || !reflect.DeepEqual(page.Sources[target], []string{from}) {
		t.Fatalf("actual public pool-one projection: %+v %v", page, err)
	}
	if exists, err := upper.IncomingSourceNodeExists(ctx, target, nil); err != nil || !exists {
		t.Fatalf("public projection leaked transaction: %v %v", exists, err)
	}
}
