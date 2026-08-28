package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func seedListedGeneration(
	t testing.TB,
	store *Store,
	base PayloadGenerationRequest,
	graphID, layerID string,
) int64 {
	t.Helper()
	base.GraphID = graphID
	base.LayerID = layerID
	generationID, handle, adopted, err := store.BeginPayloadGenerationWithStatus(context.Background(), base)
	if err != nil {
		t.Fatalf("begin listed generation %s: %v", layerID, err)
	}
	if adopted {
		t.Fatalf("listed generation %s unexpectedly adopted generation %d", layerID, generationID)
	}
	if handle != nil {
		_ = handle.Close()
	}
	if err := store.Catalog().SetViewGenerationState(
		context.Background(), generationID, ViewGenerationReady, ViewGenerationBuilding,
	); err != nil {
		t.Fatalf("mark listed generation %s ready: %v", layerID, err)
	}
	return generationID
}

func TestListViewGenerationsBeforeCursorIsExclusiveWithMissingGraph(t *testing.T) {
	store, request := payloadLifecycleRaceStore(t, "catalog-cursor")
	ctx := context.Background()
	catalog := store.Catalog()
	const (
		familyID       = "family-catalog-cursor"
		healthyGraphID = "graph-catalog-cursor-healthy"
		missingGraphID = "graph-catalog-cursor-missing"
	)
	if err := catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{
		FamilyID:          familyID,
		CommonDirIdentity: "common-catalog-cursor",
		State:             "ready",
	}); err != nil {
		t.Fatalf("upsert family: %v", err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID:    healthyGraphID,
		RepoPrefix: "repo-catalog-cursor",
		FamilyID:   familyID,
		State:      "ready",
	}); err != nil {
		t.Fatalf("upsert healthy graph: %v", err)
	}

	oldMissing := seedListedGeneration(t, store, request, missingGraphID, "missing-old")
	healthy := seedListedGeneration(t, store, request, healthyGraphID, "healthy-middle")
	newMissing := seedListedGeneration(t, store, request, missingGraphID, "missing-new")

	page, err := catalog.ListViewGenerations(ctx, ViewGenerationFilter{
		States:       []ViewGenerationState{ViewGenerationReady},
		MissingGraph: true,
		Limit:        1,
	})
	if err != nil {
		t.Fatalf("first missing-graph page: %v", err)
	}
	if len(page) != 1 || page[0].GenerationID != newMissing {
		t.Fatalf("first missing-graph page = %+v, want generation %d", page, newMissing)
	}
	page, err = catalog.ListViewGenerations(ctx, ViewGenerationFilter{
		States:             []ViewGenerationState{ViewGenerationReady},
		MissingGraph:       true,
		BeforeGenerationID: newMissing,
		Limit:              1,
	})
	if err != nil {
		t.Fatalf("second missing-graph page: %v", err)
	}
	if len(page) != 1 || page[0].GenerationID != oldMissing {
		t.Fatalf("second missing-graph page = %+v, want generation %d (healthy %d excluded)", page, oldMissing, healthy)
	}
	page, err = catalog.ListViewGenerations(ctx, ViewGenerationFilter{
		States:             []ViewGenerationState{ViewGenerationReady},
		MissingGraph:       true,
		BeforeGenerationID: oldMissing,
		Limit:              1,
	})
	if err != nil {
		t.Fatalf("terminal missing-graph page: %v", err)
	}
	if len(page) != 0 {
		t.Fatalf("exclusive cursor repeated/skipped past terminal generation: %+v", page)
	}
	if _, err := catalog.ListViewGenerations(ctx, ViewGenerationFilter{BeforeGenerationID: -1}); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Fatalf("negative cursor error = %v, want ErrCatalogInvalidValue", err)
	}
}

func TestListViewGenerationsExactPageHasNoDuplicatesOrSkips(t *testing.T) {
	store, request := payloadLifecycleRaceStore(t, "catalog-exact-page")
	ctx := context.Background()
	catalog := store.Catalog()
	const pageSize = 512
	want := make(map[int64]struct{}, pageSize)
	for i := 0; i < pageSize; i++ {
		generationID := seedListedGeneration(
			t, store, request, "graph-catalog-exact-page-missing", fmt.Sprintf("exact-%04d", i),
		)
		want[generationID] = struct{}{}
	}

	seen := make(map[int64]struct{}, pageSize)
	var beforeGenerationID int64
	pages := 0
	for {
		pages++
		rows, err := catalog.ListViewGenerations(ctx, ViewGenerationFilter{
			States:             []ViewGenerationState{ViewGenerationReady},
			MissingGraph:       true,
			BeforeGenerationID: beforeGenerationID,
			Limit:              pageSize,
		})
		if err != nil {
			t.Fatalf("list page %d: %v", pages, err)
		}
		for i, row := range rows {
			if _, duplicate := seen[row.GenerationID]; duplicate {
				t.Fatalf("generation %d repeated on page %d", row.GenerationID, pages)
			}
			if _, expected := want[row.GenerationID]; !expected {
				t.Fatalf("unexpected generation %d on page %d", row.GenerationID, pages)
			}
			if i > 0 && rows[i-1].GenerationID <= row.GenerationID {
				t.Fatalf("page %d is not strictly descending at %d then %d", pages, rows[i-1].GenerationID, row.GenerationID)
			}
			seen[row.GenerationID] = struct{}{}
		}
		if len(rows) < pageSize {
			break
		}
		beforeGenerationID = rows[len(rows)-1].GenerationID
	}
	if pages != 2 {
		t.Fatalf("pages = %d, want full page plus empty terminal page", pages)
	}
	if len(seen) != len(want) {
		t.Fatalf("listed generations = %d, want %d", len(seen), len(want))
	}
}
