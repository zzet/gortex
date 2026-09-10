package store_sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

func TestGenerationDependencyRevisionBeginCoalescingAndListing(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "generation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	req := PayloadGenerationRequest{OwnerKind: "dependency-test", GenerationKind: "commit", LayerID: "named",
		TreeOID: "tree", ConfigHash: "config", ExtractorVersions: "extractors", ResolverVersion: "resolver", DependencyRevision: "cohort-v1:a"}
	a, handle, adopted, err := store.BeginPayloadGenerationWithStatus(ctx, req)
	if err != nil || a <= 0 || handle == nil || adopted {
		t.Fatalf("initial begin id=%d handle=%v adopted=%v err=%v", a, handle != nil, adopted, err)
	}
	same, _, adopted, err := store.BeginPayloadGenerationWithStatus(ctx, req)
	if err != nil || !adopted || same != a {
		t.Fatalf("same revision id=%d adopted=%v err=%v", same, adopted, err)
	}
	req.DependencyRevision = "cohort-v1:b"
	b, _, adopted, err := store.BeginPayloadGenerationWithStatus(ctx, req)
	if err != nil || adopted || b <= 0 || b == a {
		t.Fatalf("different revision id=%d adopted=%v err=%v", b, adopted, err)
	}
	catalog := store.Catalog()
	for id, revision := range map[int64]string{a: "cohort-v1:a", b: "cohort-v1:b"} {
		row, found, err := catalog.GetViewGeneration(ctx, id)
		if err != nil || !found || row.DependencyRevision != revision || row.State != ViewGenerationBuilding {
			t.Fatalf("row id=%d value=%+v found=%v err=%v", id, row, found, err)
		}
	}
	rows, err := catalog.ListViewGenerations(ctx, ViewGenerationFilter{OwnerKind: "dependency-test", Limit: 10})
	if err != nil || len(rows) != 2 || rows[0].GenerationID != b || rows[0].DependencyRevision != "cohort-v1:b" || rows[1].GenerationID != a || rows[1].DependencyRevision != "cohort-v1:a" {
		t.Fatalf("listing=%+v err=%v", rows, err)
	}
}

func TestGenerationDependencyRevisionUnnamedAndCreateRemainDistinct(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "generation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	req := PayloadGenerationRequest{OwnerKind: "dependency-test", GenerationKind: "commit", DependencyRevision: "cohort-v1:a"}
	a, _, err := store.BeginPayloadGeneration(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := store.BeginPayloadGeneration(ctx, req)
	if err != nil || a <= 0 || b <= 0 || a == b {
		t.Fatalf("unnamed generations coalesced: a=%d b=%d err=%v", a, b, err)
	}
	id, err := store.Catalog().CreateViewGeneration(ctx, ViewGeneration{OwnerKind: "dependency-test", GenerationKind: "commit", State: ViewGenerationBuilding, DependencyRevision: "cohort-v1:direct"})
	if err != nil {
		t.Fatal(err)
	}
	row, found, err := store.Catalog().GetViewGeneration(ctx, id)
	if err != nil || !found || row.DependencyRevision != "cohort-v1:direct" {
		t.Fatalf("direct create roundtrip=%+v found=%v err=%v", row, found, err)
	}
}
