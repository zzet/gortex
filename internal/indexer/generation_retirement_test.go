package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func newGenerationRetirementLifecycle(
	store *store_sqlite.Store,
	started time.Time,
) *CheckoutLifecycle {
	return &CheckoutLifecycle{
		catalog:                store.Catalog(),
		store:                  store,
		leases:                 graphview.NewLeaseManager(),
		logger:                 zap.NewNop(),
		now:                    func() time.Time { return started },
		buildingRecoveryCutoff: started.Unix(),
		owed:                   map[int64]struct{}{},
	}
}

func requireGenerationStateWithPayload(
	t testing.TB,
	store *store_sqlite.Store,
	generationID int64,
	want store_sqlite.ViewGenerationState,
) {
	t.Helper()
	row, found, err := store.Catalog().GetViewGeneration(context.Background(), generationID)
	if err != nil {
		t.Fatalf("get generation %d: %v", generationID, err)
	}
	if !found {
		t.Fatalf("generation %d is absent, want state %s", generationID, want)
	}
	if row.State != want {
		t.Fatalf("generation %d state = %s, want %s", generationID, row.State, want)
	}
	if got := contentIdentities(store.AtGeneration(generationID), builderRepoPrefix); len(got) == 0 {
		t.Fatalf("generation %d has no payload before retirement", generationID)
	}
}

func requireGenerationRetired(t testing.TB, store *store_sqlite.Store, generationID int64) {
	t.Helper()
	_, found, err := store.Catalog().GetViewGeneration(context.Background(), generationID)
	if err != nil {
		t.Fatalf("get retired generation %d: %v", generationID, err)
	}
	if found {
		t.Fatalf("generation %d catalog row survived retirement", generationID)
	}
	if got := contentIdentities(store.AtGeneration(generationID), builderRepoPrefix); len(got) != 0 {
		t.Fatalf("generation %d payload survived retirement: %v", generationID, got)
	}
}

func TestCheckoutLifecycleRetiresFailedSparseGenerationAfterLeaseDrains(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	started := time.Now()
	request := fixture.request
	request.Identity.CreatedAt = started.Add(-time.Hour).Unix()
	wantErr := errors.New("reject sparse publish")
	request.PrePublish = func(context.Context, int64) error { return wantErr }

	generationID, _, err := fixture.builder.Build(ctx, request)
	if !errors.Is(err, wantErr) {
		t.Fatalf("failed sparse build error = %v, want %v", err, wantErr)
	}
	requireGenerationStateWithPayload(t, fixture.store, generationID, store_sqlite.ViewGenerationFailed)

	lifecycle := newGenerationRetirementLifecycle(fixture.store, started)
	lease := lifecycle.leases.Acquire(generationID)
	if retired := lifecycle.sweepRetirements(ctx); retired != 0 {
		t.Fatalf("retired generations with a live lease = %d, want 0", retired)
	}
	requireGenerationStateWithPayload(t, fixture.store, generationID, store_sqlite.ViewGenerationFailed)

	lease.Release()
	if retired := lifecycle.sweepRetirements(ctx); retired != 1 {
		t.Fatalf("retired generations after lease drain = %d, want 1", retired)
	}
	requireGenerationRetired(t, fixture.store, generationID)
}

func TestCheckoutLifecycleRetiresPanickedSparseGeneration(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	started := time.Now()
	request := fixture.request
	request.Identity.CreatedAt = started.Add(-time.Hour).Unix()
	wantPanic := errors.New("panic before sparse publish")
	var generationID atomic.Int64
	request.PrePublish = func(_ context.Context, id int64) error {
		generationID.Store(id)
		panic(wantPanic)
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _, _ = fixture.builder.Build(ctx, request)
	}()
	if recovered != wantPanic {
		t.Fatalf("recovered panic = %v, want %v", recovered, wantPanic)
	}
	id := generationID.Load()
	requireGenerationStateWithPayload(t, fixture.store, id, store_sqlite.ViewGenerationFailed)

	lifecycle := newGenerationRetirementLifecycle(fixture.store, started)
	if retired := lifecycle.sweepRetirements(ctx); retired != 1 {
		t.Fatalf("retired panicked generations = %d, want 1", retired)
	}
	requireGenerationRetired(t, fixture.store, id)
}

func TestCheckoutLifecycleStartupSweepRetiresAbandonedBuildingGeneration(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	started := time.Now()
	request := fixture.request
	request.Identity.CreatedAt = started.Add(-time.Hour).Unix()
	wantErr := errors.New("leave populated generation for crash fixture")
	request.PrePublish = func(context.Context, int64) error { return wantErr }

	generationID, _, err := fixture.builder.Build(ctx, request)
	if !errors.Is(err, wantErr) {
		t.Fatalf("fixture build error = %v, want %v", err, wantErr)
	}
	if err := fixture.store.Catalog().SetViewGenerationState(
		ctx,
		generationID,
		store_sqlite.ViewGenerationBuilding,
		store_sqlite.ViewGenerationFailed,
	); err != nil {
		t.Fatalf("restore crash-left building state: %v", err)
	}
	requireGenerationStateWithPayload(t, fixture.store, generationID, store_sqlite.ViewGenerationBuilding)

	lifecycle := newGenerationRetirementLifecycle(fixture.store, started)
	if err := lifecycle.Seed(ctx); err != nil {
		t.Fatalf("seed lifecycle with crash-left generation: %v", err)
	}
	requireGenerationRetired(t, fixture.store, generationID)
}

func TestCheckoutLifecycleRetirementRetriesAfterActiveBuildFlight(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	ctx := context.Background()
	started := time.Now()
	request := fixture.request
	request.Identity.CreatedAt = started.Add(-time.Hour).Unix()
	entered := make(chan struct{})
	release := make(chan struct{})
	var generationID atomic.Int64
	request.PrePublish = func(_ context.Context, id int64) error {
		generationID.Store(id)
		close(entered)
		<-release
		return nil
	}

	resultCh := make(chan sparseBuildFlightResult, 1)
	go func() {
		id, report, err := fixture.builder.Build(ctx, request)
		resultCh <- sparseBuildFlightResult{generationID: id, report: report, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("sparse build did not reach pre-publish hook")
	}
	id := generationID.Load()
	if !fixture.store.PayloadBuildFlightActive(id) {
		t.Fatalf("generation %d has no active payload build flight", id)
	}
	requireGenerationStateWithPayload(t, fixture.store, id, store_sqlite.ViewGenerationBuilding)

	lifecycle := newGenerationRetirementLifecycle(fixture.store, started)
	lifecycle.oweRetirement(id)
	if retired := lifecycle.sweepRetirements(ctx); retired != 0 {
		t.Fatalf("retired generations while writer active = %d, want 0", retired)
	}
	requireGenerationStateWithPayload(t, fixture.store, id, store_sqlite.ViewGenerationBuilding)

	close(release)
	var result sparseBuildFlightResult
	select {
	case result = <-resultCh:
	case <-time.After(10 * time.Second):
		t.Fatal("sparse build did not finish after release")
	}
	if result.err != nil {
		t.Fatalf("finish sparse build: %v", result.err)
	}
	if result.generationID != id {
		t.Fatalf("published generation = %d, want %d", result.generationID, id)
	}
	if fixture.store.PayloadBuildFlightActive(id) {
		t.Fatalf("generation %d payload build flight survived completion", id)
	}
	requireGenerationStateWithPayload(t, fixture.store, id, store_sqlite.ViewGenerationReady)

	if retired := lifecycle.sweepRetirements(ctx); retired != 1 {
		t.Fatalf("retired generations after writer completion = %d, want 1", retired)
	}
	requireGenerationRetired(t, fixture.store, id)
}

func BenchmarkCheckoutLifecycleRetirementCandidateScan(b *testing.B) {
	fixture := newSparseBuildFlightFixture(b)
	ctx := context.Background()
	const generationsPerState = 256
	createdAt := time.Now().Add(-time.Hour).Unix()

	seed := func(state store_sqlite.ViewGenerationState, suffix string) {
		b.Helper()
		for i := 0; i < generationsPerState; i++ {
			req := payloadRequestForBuild(fixture.request)
			req.LayerID = fmt.Sprintf("retirement-%s-%d", suffix, i)
			req.CreatedAt = createdAt
			generationID, _, _, err := fixture.store.BeginPayloadGenerationWithStatus(ctx, req)
			if err != nil {
				b.Fatalf("begin %s generation %d: %v", suffix, i, err)
			}
			if state != store_sqlite.ViewGenerationBuilding {
				if err := fixture.store.Catalog().SetViewGenerationState(
					ctx, generationID, state, store_sqlite.ViewGenerationBuilding,
				); err != nil {
					b.Fatalf("mark %s generation %d: %v", suffix, i, err)
				}
			}
		}
	}
	seed(store_sqlite.ViewGenerationFailed, "failed")
	seed(store_sqlite.ViewGenerationBuilding, "building")

	lifecycle := newGenerationRetirementLifecycle(fixture.store, time.Now())
	served := map[string]struct{}{}

	b.ReportAllocs()
	b.ResetTimer()
	var candidates int
	for i := 0; i < b.N; i++ {
		candidates = len(lifecycle.orphanedGenerations(ctx, served, nil))
	}
	b.ReportMetric(float64(candidates), "candidates/op")
}
