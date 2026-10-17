package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestCheckoutLifecycleDeferredSeedRetirementRunsAfterReadySweep(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	started := time.Now()
	request := fixture.request
	request.Identity.CreatedAt = started.Add(-time.Hour).Unix()
	wantErr := errors.New("leave populated generation for deferred seed fixture")
	request.PrePublish = func(context.Context, int64) error { return wantErr }
	generationID, _, err := fixture.builder.Build(ctx, request)
	if !errors.Is(err, wantErr) {
		t.Fatalf("fixture build error = %v, want %v", err, wantErr)
	}
	if err := fixture.store.Catalog().SetViewGenerationState(ctx, generationID, store_sqlite.ViewGenerationBuilding, store_sqlite.ViewGenerationFailed); err != nil {
		t.Fatalf("restore crash-left building state: %v", err)
	}
	lifecycle := newGenerationRetirementLifecycle(fixture.store, started)
	lifecycle.EnableDeferredSeedRetirements()
	if err := lifecycle.Seed(ctx); err != nil {
		t.Fatalf("seed lifecycle: %v", err)
	}
	requireGenerationStateWithPayload(t, fixture.store, generationID, store_sqlite.ViewGenerationBuilding)
	for attempt := 0; attempt < 10; attempt++ {
		_, pending, sweepErr := lifecycle.SweepDeferredRetirements(ctx)
		if sweepErr != nil {
			t.Fatalf("deferred sweep attempt %d: %v", attempt, sweepErr)
		}
		if !pending {
			requireGenerationRetired(t, fixture.store, generationID)
			return
		}
	}
	t.Fatal("deferred retirement did not drain")
}

func TestDeferredRetirementRotatesPastPinnedGeneration(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	started := time.Now()
	buildFailed := func(label string) int64 {
		request := fixture.request
		request.Identity.CreatedAt = started.Add(-time.Hour).Unix()
		wantErr := errors.New(label)
		request.PrePublish = func(context.Context, int64) error { return wantErr }
		generationID, _, err := fixture.builder.Build(ctx, request)
		if !errors.Is(err, wantErr) {
			t.Fatalf("%s build error = %v, want %v", label, err, wantErr)
		}
		return generationID
	}
	eligible := buildFailed("eligible")
	pinned := buildFailed("pinned")
	if pinned <= eligible {
		t.Fatalf("generation order pinned=%d eligible=%d", pinned, eligible)
	}
	lifecycle := newGenerationRetirementLifecycle(fixture.store, started)
	lease := lifecycle.leases.Acquire(pinned)
	defer lease.Release()
	// The newest generation is selected first. A lease refusal is expected
	// retention: it stays queued and pending without an operational error.
	retired, pending, err := lifecycle.SweepDeferredRetirements(ctx)
	if retired != 0 || !pending || err != nil {
		t.Fatalf("pinned sweep retired=%d pending=%v err=%v, want (0,true,<nil>)", retired, pending, err)
	}
	requireGenerationStateWithPayload(t, fixture.store, pinned, store_sqlite.ViewGenerationFailed)
	_, pending, err = lifecycle.SweepDeferredRetirements(ctx)
	if err != nil {
		t.Fatalf("eligible sweep: %v", err)
	}
	if !pending {
		t.Fatal("pinned generation should remain pending")
	}
	requireGenerationRetired(t, fixture.store, eligible)
	requireGenerationStateWithPayload(t, fixture.store, pinned, store_sqlite.ViewGenerationFailed)
}

func TestDeferredRetirementCancellationDoesNotWaitForLegacySweep(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	lifecycle := newGenerationRetirementLifecycle(fixture.store, time.Now())
	lifecycle.retirementSweepMu.Lock()
	defer lifecycle.retirementSweepMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, _, err := lifecycle.SweepDeferredRetirements(ctx); result <- err }()
	time.Sleep(2 * deferredRetirementLockPoll)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sweep error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled sweep waited for the legacy retirement mutex")
	}
}

func TestDeferredRetirementSliceResumesAfterLifecycleReconstruction(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	started := time.Now()
	request := fixture.request
	request.Identity.CreatedAt = started.Add(-time.Hour).Unix()
	wantErr := errors.New("leave payload for partial retirement")
	request.PrePublish = func(context.Context, int64) error { return wantErr }
	generationID, _, err := fixture.builder.Build(ctx, request)
	if !errors.Is(err, wantErr) {
		t.Fatalf("fixture build error = %v, want %v", err, wantErr)
	}
	if err := fixture.store.RetirePayloadGenerationSlice(ctx, generationID, nil, 0); !errors.Is(err, store_sqlite.ErrCatalogInvalidValue) {
		t.Fatalf("zero slice error = %v, want invalid value", err)
	}
	err = fixture.store.RetirePayloadGenerationSlice(ctx, generationID, nil, time.Nanosecond)
	if !errors.Is(err, store_sqlite.ErrPayloadSweepBudgetExhausted) {
		t.Fatalf("partial slice error = %v, want budget exhausted", err)
	}
	lifecycle := newGenerationRetirementLifecycle(fixture.store, started)
	for attempt := 0; attempt < 10; attempt++ {
		_, pending, sweepErr := lifecycle.SweepDeferredRetirements(ctx)
		if sweepErr != nil {
			t.Fatalf("reconstructed sweep attempt %d: %v", attempt, sweepErr)
		}
		if !pending {
			requireGenerationRetired(t, fixture.store, generationID)
			return
		}
	}
	t.Fatal("reconstructed lifecycle did not rediscover and drain partial retirement")
}

func TestDeferredRetirementIncompleteInventorySkipsDedicatedInference(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	lifecycle := newGenerationRetirementLifecycle(fixture.store, time.Now())
	sentinel := errors.New("next inventory scan failed")
	listCalls := 0
	inferCalls := 0

	candidates, err := lifecycle.discoverDeferredRetirementsWith(
		context.Background(),
		map[string]struct{}{},
		nil,
		func(context.Context, store_sqlite.ViewGenerationFilter) ([]store_sqlite.ViewGeneration, error) {
			listCalls++
			switch listCalls {
			case 1:
				return []store_sqlite.ViewGeneration{
					{GenerationID: 41, CheckoutID: "orphan", State: store_sqlite.ViewGenerationSuperseded},
					{GenerationID: 42, GraphID: "partial-graph", OwnerKind: checkoutLayerOwnerKind, GenerationKind: DedicatedBaseGenerationKind, State: store_sqlite.ViewGenerationSuperseded},
				}, nil
			case 2:
				return nil, sentinel
			default:
				t.Fatalf("unexpected inventory call %d after partial scan failure", listCalls)
				return nil, nil
			}
		},
		func(context.Context, []store_sqlite.ViewGeneration) []store_sqlite.ViewGeneration {
			inferCalls++
			return nil
		},
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("discovery error = %v, want sentinel", err)
	}
	if listCalls != 2 {
		t.Fatalf("inventory calls = %d, want 2", listCalls)
	}
	if inferCalls != 0 {
		t.Fatalf("dedicated ancestry inference calls = %d, want 0", inferCalls)
	}
	if len(candidates) != 1 || candidates[0] != 41 {
		t.Fatalf("partial discovery candidates = %v, want [41]", candidates)
	}
}
