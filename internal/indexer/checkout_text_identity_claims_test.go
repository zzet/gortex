package indexer

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// This exercises the actual coordinator path-folding method, not a public
// search route or immutable positive-base corpus selection. All storage is
// explicit and private. Root's isolated package harness supplies init isolation.
func TestCheckoutTextLayerClaimsKeepIdentitySeparateFromFileInventory(t *testing.T) {
	s := builderOpenStoreAt(t, filepath.Join(t.TempDir(), "text-identity.sqlite"))
	t.Cleanup(func() { _ = s.Close() })
	newGeneration := func() (int64, *store_sqlite.Store) {
		t.Helper()
		id, err := s.Catalog().CreateViewGeneration(t.Context(), store_sqlite.ViewGeneration{
			OwnerKind: "dedicated_graph", GraphID: "text-identity-fixture", GenerationKind: "dedicated",
			TreeOID: "same-source", ConfigHash: "same-policy", State: store_sqlite.ViewGenerationBuilding, CreatedAt: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id, s.AtGeneration(id)
	}
	coordinator := &CheckoutCoordinator{store: s, repoPrefix: "alpha"}
	paths := map[string]struct{}{"keep.cs": {}, "unrelated.cs": {}}
	want := map[string]struct{}{"keep.cs": {}, "unrelated.cs": {}}
	id, upper := newGeneration()
	upper.AddBatch([]*graph.Node{
		{ID: "alpha/keep.cs::Run", Name: "Changed metadata", RepoPrefix: "alpha", FilePath: "alpha/keep.cs"},
		{ID: "alpha/not-in-file-inventory.cs::Derived", Name: "Derived", RepoPrefix: "alpha", FilePath: "alpha/not-in-file-inventory.cs"},
	}, nil)
	if err := upper.SetNodeIdentityReplacements([]string{"alpha/keep.cs::Run", "alpha/not-in-file-inventory.cs::Derived"}); err != nil {
		t.Fatal(err)
	}
	if err := upper.SetNodeTombstones([]string{"alpha/keep.cs::Gone"}); err != nil {
		t.Fatal(err)
	}
	if files, err := upper.FileMasks(); err != nil || len(files) != 0 {
		t.Fatalf("identity fixture gained file claims: %v %v", files, err)
	}
	if err := s.PublishPayloadGeneration(t.Context(), id, 2); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.applyLayerClaims(id, paths); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("node-only claims changed text paths: %v", paths)
	}
	if upper.NodeCount() != 2 || upper.EdgeCount() != 0 {
		t.Fatal("fixture unexpectedly copied whole files or edges")
	}
	legacy, err := upper.NodeTombstones()
	if err != nil || !reflect.DeepEqual(legacy, []string{"alpha/keep.cs::Gone"}) {
		t.Fatalf("legacy export reclassified identity-only rows: %v %v", legacy, err)
	}

	// Independent real file claims still add paths, and foreign repo claims
	// remain outside this coordinator's file namespace.
	fileID, fileUpper := newGeneration()
	fileUpper.AddBatch([]*graph.Node{
		{ID: "alpha/new.cs", Name: "new.cs", Kind: graph.KindFile, RepoPrefix: "alpha", FilePath: "alpha/new.cs"},
		{ID: "beta/foreign.cs", Name: "foreign.cs", Kind: graph.KindFile, RepoPrefix: "beta", FilePath: "beta/foreign.cs"},
	}, nil)
	if err := fileUpper.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: "alpha", FilePath: "alpha/new.cs", Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: "beta", FilePath: "beta/foreign.cs", Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishPayloadGeneration(t.Context(), fileID, 2); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.applyLayerClaims(fileID, paths); err != nil {
		t.Fatal(err)
	}
	want["new.cs"] = struct{}{}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("file claim scope changed: %v", paths)
	}

	// A malformed explicit identity must fail layer construction, not become
	// an implicit file claim or a partially accepted path set.
	invalidID, invalid := newGeneration()
	if err := invalid.SetNodeIdentityReplacements([]string{"alpha/missing.cs::Missing"}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.applyLayerClaims(invalidID, paths); err == nil {
		t.Fatal("missing carried row accepted")
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("failed layer changed paths: %v", paths)
	}
}
