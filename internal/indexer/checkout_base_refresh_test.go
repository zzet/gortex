package indexer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

func newDedicatedBaseRefreshWorkerTestLifecycle(
	t testing.TB,
	catalog *store_sqlite.Catalog,
	execute func(context.Context, dedicatedBaseRefreshRequest) error,
	done func(dedicatedBaseRefreshRequest, error),
) *CheckoutLifecycle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := &CheckoutLifecycle{
		catalog: catalog, logger: zap.NewNop(), transitionCtx: ctx, cancelTransitions: cancel,
		buildFailures:       newCheckoutBuildFailures(),
		baseRefreshPending:  map[string]dedicatedBaseRefreshRequest{},
		baseRefreshInFlight: map[string]struct{}{}, baseRefreshWake: make(chan struct{}, 1),
		baseRefreshExecute: execute, baseRefreshDone: done,
	}
	t.Cleanup(func() {
		l.transitionMu.Lock()
		if !l.transitionClosed {
			l.transitionClosed = true
			cancel()
		}
		l.transitionMu.Unlock()
		l.transitionWG.Wait()
	})
	return l
}

func TestDedicatedBaseRefreshCoalescesConcurrentStaleObserversByGraph(t *testing.T) {
	f := newPrimaryBaseTestFixture(t, 1)
	checkout, found, err := f.catalog.GetCheckout(f.ctx, f.ownerID)
	if err != nil || !found {
		t.Fatalf("owner checkout: found=%v err=%v", found, err)
	}

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	finished := make(chan struct{}, 1)
	var builds atomic.Int64
	l := newDedicatedBaseRefreshWorkerTestLifecycle(t, f.catalog,
		func(ctx context.Context, _ dedicatedBaseRefreshRequest) error {
			builds.Add(1)
			started <- struct{}{}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				return nil
			}
		},
		func(dedicatedBaseRefreshRequest, error) { finished <- struct{}{} },
	)

	const observers = 128
	var observersDone sync.WaitGroup
	observersDone.Add(observers)
	for i := 0; i < observers; i++ {
		go func() {
			defer observersDone.Done()
			if !l.scheduleDedicatedBaseRefreshIfNeeded(f.ctx, f.graph, checkout) {
				t.Error("stale dedicated base was not recognized")
			}
		}()
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh worker did not start")
	}
	observersDone.Wait()
	close(release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh worker did not finish")
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("physical refresh builds = %d, want exactly 1 for %d observers", got, observers)
	}
}

func TestDedicatedBaseRefreshSchedulesLegacyFullCorpusUpgrade(t *testing.T) {
	f := newPrimaryBaseTestFixture(t, 1)
	pipeline := DedicatedBasePipelineFor(config.Default().Index)
	legacy := f.createGenerationWith(
		f.graphID, f.ownerID, store_sqlite.ViewGenerationReady, f.treeOID,
		func(row *store_sqlite.ViewGeneration) {
			row.OwnerKind = checkoutLayerOwnerKind
			row.GenerationKind = "dedicated"
			row.LayerID = "legacy-primary-base"
			// Even a legacy row whose pipeline is current must be rebuilt: its
			// structural identity is not safe for sparse composition.
			row.ConfigHash = pipeline.ConfigHash
			row.ExtractorVersions = pipeline.ExtractorVersions
			row.ResolverVersion = pipeline.ResolverVersion
		},
	)
	f.graph.ActiveGenerationID = legacy
	f.upsertGraph(f.graph)
	legacyRow, found, err := f.catalog.GetViewGeneration(f.ctx, legacy)
	if err != nil || !found {
		t.Fatalf("legacy generation: found=%v err=%v", found, err)
	}
	if dedicatedBaseGenerationCurrent(legacyRow, f.graph, dedicatedBaseIdentity{
		configHash: pipeline.ConfigHash, extractorVersions: pipeline.ExtractorVersions,
		resolverVersion: pipeline.ResolverVersion,
	}) {
		t.Fatal("pipeline-current legacy corpus was mistaken for a canonical current base")
	}
	checkout, found, err := f.catalog.GetCheckout(f.ctx, f.ownerID)
	if err != nil || !found {
		t.Fatalf("owner checkout: found=%v err=%v", found, err)
	}

	done := make(chan error, 1)
	var builds atomic.Int64
	l := newDedicatedBaseRefreshWorkerTestLifecycle(t, f.catalog,
		func(context.Context, dedicatedBaseRefreshRequest) error {
			builds.Add(1)
			return nil
		},
		func(_ dedicatedBaseRefreshRequest, err error) { done <- err },
	)
	if !l.scheduleDedicatedBaseRefreshIfNeeded(f.ctx, f.graph, checkout) {
		t.Fatal("legacy full corpus was admitted instead of queued for canonical replacement")
	}
	if err := <-done; err != nil {
		t.Fatalf("legacy refresh: %v", err)
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("legacy replacement builds = %d, want 1", got)
	}
}

func TestDedicatedBaseRefreshDoesNotTreatSparseGenerationAsFullCorpus(t *testing.T) {
	f := newPrimaryBaseTestFixture(t, 1)
	sparse := f.createGenerationWith(
		f.graphID, f.ownerID, store_sqlite.ViewGenerationReady, f.treeOID,
		func(row *store_sqlite.ViewGeneration) {
			row.OwnerKind = checkoutLayerOwnerKind
			row.GenerationKind = CommitLayerGenerationKind
			row.LayerID = "commit:not-a-base"
			row.BaseGenerationID = f.generation
		},
	)
	f.graph.ActiveGenerationID = sparse
	f.upsertGraph(f.graph)
	row, found, err := f.catalog.GetViewGeneration(f.ctx, sparse)
	if err != nil || !found {
		t.Fatalf("sparse generation: found=%v err=%v", found, err)
	}
	if dedicatedBaseGenerationRefreshable(row, f.graph) {
		t.Fatal("sparse commit generation was classified as a refreshable full corpus")
	}
}

func TestDedicatedBaseRefreshFailureRetriesOnlyAfterNewAdmission(t *testing.T) {
	wantErr := errors.New("refresh failed")
	done := make(chan error, 3)
	var builds atomic.Int64
	l := newDedicatedBaseRefreshWorkerTestLifecycle(t, nil,
		func(context.Context, dedicatedBaseRefreshRequest) error {
			builds.Add(1)
			return wantErr
		},
		func(_ dedicatedBaseRefreshRequest, err error) { done <- err },
	)
	req := dedicatedBaseRefreshRequest{
		graphID: "retry-graph", checkoutID: "retry-checkout", familyID: "retry-family",
	}

	l.scheduleDedicatedBaseRefresh(req)
	if err := <-done; !errors.Is(err, wantErr) {
		t.Fatalf("first refresh error = %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("refresh retried without new admission: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	l.scheduleDedicatedBaseRefresh(req)
	if err := <-done; !errors.Is(err, wantErr) {
		t.Fatalf("second refresh error = %v", err)
	}
	if got := builds.Load(); got != 2 {
		t.Fatalf("physical refresh builds = %d, want 2 admitted attempts", got)
	}
}

func TestDedicatedBaseRefreshFailureKeepsPublishedBaseFallback(t *testing.T) {
	f := newPrimaryBaseTestFixture(t, 1)
	wantErr := errors.New("refresh failed before publication")
	done := make(chan error, 1)
	var l *CheckoutLifecycle
	l = newDedicatedBaseRefreshWorkerTestLifecycle(t, f.catalog,
		func(_ context.Context, req dedicatedBaseRefreshRequest) error {
			// A real refresh allocates a replacement generation inside the sparse
			// builder. Simulate that nested attempt reaching its own terminal
			// fallback: it must not overwrite the graph/base refresh verdict.
			l.buildFailures.start(req.checkoutID, f.generation+1)
			l.buildFailures.record(req.checkoutID, f.generation+1, "replacement generation failed")
			return wantErr
		},
		func(_ dedicatedBaseRefreshRequest, err error) { done <- err },
	)

	checkout, found, err := f.catalog.GetCheckout(f.ctx, f.ownerID)
	if err != nil || !found {
		t.Fatalf("owner checkout: found=%v err=%v", found, err)
	}
	if !l.scheduleDedicatedBaseRefreshIfNeeded(f.ctx, f.graph, checkout) {
		t.Fatal("stale base was not admitted for refresh")
	}
	if err := <-done; !errors.Is(err, wantErr) {
		t.Fatalf("refresh error = %v, want %v", err, wantErr)
	}

	graph, found, err := f.catalog.GetDedicatedGraph(f.ctx, f.graphID)
	if err != nil || !found {
		t.Fatalf("dedicated graph after failed refresh: found=%v err=%v", found, err)
	}
	if graph.ActiveGenerationID != f.generation {
		t.Fatalf("active generation after failed refresh = %d, want sealed fallback %d", graph.ActiveGenerationID, f.generation)
	}
	if graph.State != store_sqlite.DedicatedGraphStateRefreshing {
		t.Fatalf("graph state after failed refresh = %q, want labeled fallback marker %q",
			graph.State, store_sqlite.DedicatedGraphStateRefreshing)
	}
	generation, found, err := f.catalog.GetViewGeneration(f.ctx, f.generation)
	if err != nil || !found {
		t.Fatalf("fallback generation after failed refresh: found=%v err=%v", found, err)
	}
	if generation.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("fallback generation state after failed refresh = %q, want %q", generation.State, store_sqlite.ViewGenerationReady)
	}
	if reason, failed := l.DedicatedBaseRefreshFailure(f.graphID, f.generation); !failed {
		t.Fatal("failed refresh remained indistinguishable from an in-progress refresh")
	} else if reason != "dedicated base refresh failed; see daemon log" {
		t.Fatalf("refresh failure reason = %q", reason)
	}
	if reason, failed := l.CheckoutBuildFailure(checkout.CheckoutID, f.generation+1); !failed ||
		reason != "replacement generation failed" {
		t.Fatalf("nested replacement failure = (%q, %t), want independent terminal verdict", reason, failed)
	}
}

func TestDedicatedBaseRefreshCancellationDoesNotRequeue(t *testing.T) {
	started := make(chan struct{}, 1)
	done := make(chan error, 2)
	l := newDedicatedBaseRefreshWorkerTestLifecycle(t, nil,
		func(ctx context.Context, _ dedicatedBaseRefreshRequest) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		},
		func(_ dedicatedBaseRefreshRequest, err error) { done <- err },
	)
	l.scheduleDedicatedBaseRefresh(dedicatedBaseRefreshRequest{
		graphID: "cancel-graph", checkoutID: "cancel-checkout", familyID: "cancel-family",
	})
	<-started
	l.cancelTransitions()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh error = %v, want context.Canceled", err)
	}
	select {
	case err := <-done:
		t.Fatalf("cancelled refresh requeued itself: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

func BenchmarkDedicatedBaseRefreshCoalescedAdmission(b *testing.B) {
	l := &CheckoutLifecycle{
		transitionCtx:       context.Background(),
		baseRefreshPending:  map[string]dedicatedBaseRefreshRequest{},
		baseRefreshInFlight: map[string]struct{}{"benchmark-graph": {}},
		baseRefreshWake:     make(chan struct{}, 1),
	}
	req := dedicatedBaseRefreshRequest{
		graphID: "benchmark-graph", checkoutID: "benchmark-checkout", familyID: "benchmark-family",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.scheduleDedicatedBaseRefresh(req)
	}
}
