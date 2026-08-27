package store_sqlite

import (
	"context"
	"testing"
)

func TestPublishedCanonicalWinnerCanWithdrawSourceCapability(t *testing.T) {
	store := openCatalogStore(t)
	catalog := store.Catalog()
	key := readyGenerationCacheLifecycleKey()
	ctx := context.Background()
	generationID, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind:           "ref",
		GraphID:             key.GraphID,
		LayerID:             "canonical-withdrawal",
		GenerationKind:      "commit",
		BaseGenerationID:    key.BaseGenerationID,
		TreeOID:             key.TreeOID,
		ProvenanceCommitOID: "canonical-withdrawal-commit",
		ConfigHash:          key.IndexConfigHash,
		ExtractorVersions:   key.ExtractorFingerprint,
		ResolverVersion:     key.SchemaPipelineEpoch,
		State:               ViewGenerationBuilding,
		CreatedAt:           10,
	})
	if err != nil {
		t.Fatalf("create canonical candidate: %v", err)
	}
	if err := store.AtGeneration(generationID).SetProducerState(ProducerCompleteness{
		Producer: "source.snapshot",
		State:    ProducerStateComplete,
	}); err != nil {
		t.Fatalf("seed canonical candidate source capability: %v", err)
	}
	if err := catalog.PublishViewGeneration(ctx, generationID, 11); err != nil {
		t.Fatalf("publish canonical candidate: %v", err)
	}

	claim, found, err := catalog.ClaimReadyGeneration(context.Background(), ClaimReadyGenerationRequest{
		Key:                   key,
		CandidateGenerationID: generationID,
	})
	if err != nil {
		t.Fatalf("claim canonical winner before withdrawal: %v", err)
	}
	if !found || claim.WinnerGenerationID != generationID {
		t.Fatalf("canonical claim before withdrawal = (%+v, found=%v), want generation %d", claim, found, generationID)
	}
	if err := catalog.ReleaseReadyGenerationLease(context.Background(), claim.LeaseToken); err != nil {
		t.Fatalf("release pre-withdrawal lease: %v", err)
	}

	if !store.ScheduleProducerWithdrawal(generationID, "source.snapshot", "published winner lost Git object") {
		t.Fatal("published winner withdrawal schedule rejected")
	}
	store.producerWithdrawals.close()
	availability, err := catalog.ReadProducerAvailability(context.Background(), generationID, "source.snapshot")
	if err != nil {
		t.Fatalf("read withdrawn canonical winner capability: %v", err)
	}
	if !availability.Declared || availability.State != ProducerStateUnavailable {
		t.Fatalf("canonical winner source capability = %+v, want unavailable", availability)
	}

	// V1 deliberately leaves canonical eligibility unchanged: structural graph
	// reuse still works after source bytes disappear. The ref-cache slice must
	// specify degraded-winner replacement and source restoration before it may
	// reject or replace this winner.
	claim, found, err = catalog.ClaimReadyGeneration(context.Background(), ClaimReadyGenerationRequest{
		Key:                   key,
		CandidateGenerationID: generationID,
	})
	if err != nil {
		t.Fatalf("claim canonical winner after withdrawal: %v", err)
	}
	if !found || claim.WinnerGenerationID != generationID {
		t.Fatalf("canonical claim after withdrawal = (%+v, found=%v), want unchanged winner %d", claim, found, generationID)
	}
	if err := catalog.ReleaseReadyGenerationLease(context.Background(), claim.LeaseToken); err != nil {
		t.Fatalf("release post-withdrawal lease: %v", err)
	}
}
