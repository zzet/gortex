package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func checkoutCommitCacheRetirementCandidates(
	t testing.TB,
	catalog *Catalog,
) []int64 {
	t.Helper()
	ids, err := catalog.ListCheckoutCommitCacheRetirementCandidates(
		context.Background(), 0, 512,
	)
	if err != nil {
		t.Fatalf("list checkout commit cache retirement candidates: %v", err)
	}
	return ids
}

func TestCheckoutCommitCacheRetirementQueueSurvivesRefusalAndClearsOnRepin(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	const (
		graphID    = "cache-retirement-queue-graph"
		checkoutID = "cache-retirement-queue-checkout"
	)
	generationID, key := checkoutCommitCacheTestGeneration(t, catalog, graphID, "queue")
	checkoutCommitCacheTestCheckout(t, catalog, checkoutID)
	checkoutCommitCacheTestSetPin(t, catalog, checkoutID, graphID, generationID, 10, 1)

	if err := catalog.DeleteCheckoutCommitCachePins(ctx, checkoutID); err != nil {
		t.Fatal(err)
	}
	if got := checkoutCommitCacheRetirementCandidates(t, catalog); len(got) != 1 || got[0] != generationID {
		t.Fatalf("retirement candidates after pin delete=%v, want %d", got, generationID)
	}

	checkoutCommitCacheTestSetPin(t, catalog, checkoutID, graphID, generationID, 20, 1)
	if got := checkoutCommitCacheRetirementCandidates(t, catalog); len(got) != 0 {
		t.Fatalf("re-pinned generation remained a retirement candidate: %v", got)
	}
	var queued int
	if err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM checkout_commit_cache_retirements WHERE generation_id = ?`,
		generationID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("re-pin retained %d durable retirement row(s), want 0", queued)
	}

	if err := catalog.DeleteCheckoutCommitCachePins(ctx, checkoutID); err != nil {
		t.Fatal(err)
	}
	claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
		Key: key, CandidateGenerationID: generationID, LeaseToken: "cache-retirement-queue-lease",
	})
	if err != nil || !found {
		t.Fatalf("claim queued generation: found=%v claim=%+v err=%v", found, claim, err)
	}
	if got := checkoutCommitCacheRetirementCandidates(t, catalog); len(got) != 1 || got[0] != generationID {
		t.Fatalf("durable queue lost leased generation: %v", got)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); !errors.Is(err, ErrPayloadGenerationInUse) {
		t.Fatalf("retire leased queued generation error=%v, want in-use refusal", err)
	}
	if err := catalog.ReleaseReadyGenerationLease(ctx, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if got := checkoutCommitCacheRetirementCandidates(t, catalog); len(got) != 1 || got[0] != generationID {
		t.Fatalf("released generation candidates=%v, want %d", got, generationID)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("retire queued generation: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM checkout_commit_cache_retirements WHERE generation_id = ?`,
		generationID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("retired generation retained %d queue row(s), want cascade cleanup", queued)
	}
}

func TestCheckoutCommitCacheRetirementQueueWaitsForSharedHolderAndRoute(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	const graphID = "cache-retirement-shared-graph"
	generationID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "shared-queue")
	for _, checkoutID := range []string{"cache-retirement-shared-a", "cache-retirement-shared-b"} {
		checkoutCommitCacheTestCheckout(t, catalog, checkoutID)
		checkoutCommitCacheTestSetPin(t, catalog, checkoutID, graphID, generationID, 10, 1)
	}
	route := checkoutCommitCacheTestRoute(
		t, catalog, "cache-retirement-shared-b", graphID, generationID,
	)

	if err := catalog.DeleteCheckoutCommitCachePins(ctx, "cache-retirement-shared-a"); err != nil {
		t.Fatal(err)
	}
	if got := checkoutCommitCacheRetirementCandidates(t, catalog); len(got) != 0 {
		t.Fatalf("shared pinned generation exposed after first holder deletion: %v", got)
	}
	if err := catalog.DeleteCheckoutCommitCachePins(ctx, "cache-retirement-shared-b"); err != nil {
		t.Fatal(err)
	}
	if got := checkoutCommitCacheRetirementCandidates(t, catalog); len(got) != 0 {
		t.Fatalf("routed generation exposed after last holder deletion: %v", got)
	}
	if err := catalog.FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID:         "cache-retirement-shared-b",
		Slot:               RouteSlotCommit,
		GenerationID:       0,
		ExpectedRouteEpoch: route.RouteEpoch,
		State:              RoutePending,
	}); err != nil {
		t.Fatal(err)
	}
	if got := checkoutCommitCacheRetirementCandidates(t, catalog); len(got) != 1 || got[0] != generationID {
		t.Fatalf("unrouted retirement candidates=%v, want %d", got, generationID)
	}
}

func TestCheckoutCommitCacheRetirementQueueCoversCheckoutCascadeDeletion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		suffix string
		delete func(*testing.T, *Catalog, string)
	}{
		{
			name: "ordinary", suffix: "ordinary",
			delete: func(t *testing.T, catalog *Catalog, checkoutID string) {
				t.Helper()
				if err := catalog.DeleteCheckout(context.Background(), checkoutID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "incarnation guarded", suffix: "incarnation-guarded",
			delete: func(t *testing.T, catalog *Catalog, checkoutID string) {
				t.Helper()
				deleted, err := catalog.DeleteCheckoutForIncarnation(
					context.Background(), checkoutID, "incarnation-"+checkoutID,
				)
				if err != nil || !deleted {
					t.Fatalf("guarded checkout delete: deleted=%v err=%v", deleted, err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openCatalogStore(t)
			catalog := store.Catalog()
			checkoutID := "cache-retirement-cascade-" + tc.suffix
			graphID := "cache-retirement-cascade-graph-" + tc.suffix
			generationID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, tc.suffix)
			checkoutCommitCacheTestCheckout(t, catalog, checkoutID)
			checkoutCommitCacheTestSetPin(t, catalog, checkoutID, graphID, generationID, 10, 1)

			tc.delete(t, catalog, checkoutID)
			if got := checkoutCommitCacheRetirementCandidates(t, catalog); len(got) != 1 || got[0] != generationID {
				t.Fatalf("cascade retirement candidates=%v, want %d", got, generationID)
			}
		})
	}
}

func TestCheckoutCommitCacheRetirementQueueCoversGraphPinDeletion(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	const (
		familyID   = "cache-retirement-graph-family"
		checkoutID = "cache-retirement-graph-checkout"
		graphID    = "cache-retirement-graph"
	)
	seedFamilyAndCheckout(t, catalog, familyID, checkoutID, "cache-retirement-graph-incarnation")
	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID: graphID, OwnerCheckoutID: checkoutID, RepoPrefix: "cache-retirement-graph-prefix",
		FamilyID: familyID, State: DedicatedGraphStateReady,
	}); err != nil {
		t.Fatal(err)
	}
	generationID, _ := checkoutCommitCacheTestGeneration(t, catalog, graphID, "graph-delete")
	checkoutCommitCacheTestSetPin(t, catalog, checkoutID, graphID, generationID, 10, 1)

	if err := catalog.DeleteDedicatedGraph(ctx, graphID); err != nil {
		t.Fatal(err)
	}
	if got := checkoutCommitCacheRetirementCandidates(t, catalog); len(got) != 1 || got[0] != generationID {
		t.Fatalf("graph deletion retirement candidates=%v, want %d", got, generationID)
	}
}

func TestCheckoutCommitCacheRetirementQueuePaginatesPast512(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	const generations = 513
	var generationIDs []int64
	err := catalog.withTx(ctx, func(tx *sql.Tx) error {
		insertGeneration, err := tx.PrepareContext(ctx, insertViewGenerationSQL)
		if err != nil {
			return err
		}
		defer insertGeneration.Close()
		insertQueue, err := tx.PrepareContext(ctx, `
INSERT INTO checkout_commit_cache_retirements(generation_id, enqueued_at) VALUES (?, 1)`)
		if err != nil {
			return err
		}
		defer insertQueue.Close()
		generationIDs = make([]int64, 0, generations)
		for i := 0; i < generations; i++ {
			result, err := insertGeneration.ExecContext(ctx, viewGenerationInsertArgs(ViewGeneration{
				OwnerKind:      "checkout",
				LayerID:        fmt.Sprintf("retirement-page-%03d", i),
				GenerationKind: "commit",
				State:          ViewGenerationReady,
			})...)
			if err != nil {
				return err
			}
			generationID, err := result.LastInsertId()
			if err != nil {
				return err
			}
			if _, err := insertQueue.ExecContext(ctx, generationID); err != nil {
				return err
			}
			generationIDs = append(generationIDs, generationID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := catalog.ListCheckoutCommitCacheRetirementCandidates(ctx, 0, 512)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 512 {
		t.Fatalf("first retirement page=%d rows, want 512", len(first))
	}
	second, err := catalog.ListCheckoutCommitCacheRetirementCandidates(ctx, first[len(first)-1], 512)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0] != generationIDs[len(generationIDs)-1] {
		t.Fatalf("second retirement page=%v, want final generation %d", second, generationIDs[len(generationIDs)-1])
	}
}

func BenchmarkCheckoutCommitCacheRetirementCandidates4096(b *testing.B) {
	ctx := context.Background()
	store := openCatalogStore(b)
	catalog := store.Catalog()
	const generations = 4096
	err := catalog.withTx(ctx, func(tx *sql.Tx) error {
		insertGeneration, err := tx.PrepareContext(ctx, insertViewGenerationSQL)
		if err != nil {
			return err
		}
		defer insertGeneration.Close()
		insertQueue, err := tx.PrepareContext(ctx, `
INSERT INTO checkout_commit_cache_retirements(generation_id, enqueued_at) VALUES (?, 1)`)
		if err != nil {
			return err
		}
		defer insertQueue.Close()
		for i := 0; i < generations; i++ {
			result, err := insertGeneration.ExecContext(ctx, viewGenerationInsertArgs(ViewGeneration{
				OwnerKind:      "checkout",
				LayerID:        fmt.Sprintf("retirement-bench-%04d", i),
				GenerationKind: "commit",
				State:          ViewGenerationReady,
			})...)
			if err != nil {
				return err
			}
			generationID, err := result.LastInsertId()
			if err != nil {
				return err
			}
			if _, err := insertQueue.ExecContext(ctx, generationID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := catalog.ListCheckoutCommitCacheRetirementCandidates(ctx, 0, generations)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != generations {
			b.Fatalf("retirement candidates=%d, want %d", len(got), generations)
		}
	}
	b.ReportMetric(generations, "candidates/op")
}
