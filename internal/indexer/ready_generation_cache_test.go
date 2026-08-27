package indexer

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

func refReadyTestManager(t testing.TB, store *store_sqlite.Store, buildBarrier, cacheMissBarrier func()) *RefViewManager {
	t.Helper()
	manager, err := NewRefViewManager(RefViewManagerConfig{
		Store: store, Builder: builderNewBuilder(store), Config: config.Default().Index,
		Logger: zap.NewNop(), buildBarrier: buildBarrier, cacheMissBarrier: cacheMissBarrier,
	})
	if err != nil {
		t.Fatalf("new ref-view manager: %v", err)
	}
	return manager
}

func coordinatorRefRequest(f *coordinatorFixture, ref string) RefViewRequest {
	return RefViewRequest{
		GraphID: f.graphID, SelectorKind: gitstate.ViewSelectorGitRef, SelectorValue: ref,
		RepoDir: f.primary, RepoPrefix: builderRepoPrefix,
		WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
	}
}

func TestReadyGenerationCheckoutThenRefReusesCommitLayer(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	cycle := coordinatorReconcile(t, f.inertCoordinator(t, CheckoutCoordinatorConfig{}))
	var builds atomic.Int64
	result, err := refReadyTestManager(t, f.store, func() { builds.Add(1) }, nil).
		EnsureRefView(context.Background(), coordinatorRefRequest(f, "refs/heads/feature"))
	if err != nil {
		t.Fatalf("select ref view: %v", err)
	}
	if !cycle.CommitBuilt || cycle.CommitGenerationID <= 0 {
		t.Fatalf("checkout cycle = %+v, want a built commit generation", cycle)
	}
	if result.State != store_sqlite.RefViewReady || result.Built || result.GenerationID != cycle.CommitGenerationID {
		t.Fatalf("ref result = %+v, want cached checkout generation %d", result, cycle.CommitGenerationID)
	}
	if builds.Load() != 0 {
		t.Fatalf("%d ref builds ran on a checkout cache hit", builds.Load())
	}
}

func TestReadyGenerationRefThenCheckoutReusesCommitLayer(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	refResult, err := refReadyTestManager(t, f.store, nil, nil).
		EnsureRefView(context.Background(), coordinatorRefRequest(f, "refs/heads/feature"))
	if err != nil {
		t.Fatalf("build ref view: %v", err)
	}
	cycle := coordinatorReconcile(t, f.inertCoordinator(t, CheckoutCoordinatorConfig{}))
	if !refResult.Built || refResult.GenerationID <= 0 {
		t.Fatalf("ref result = %+v, want a cold build", refResult)
	}
	if cycle.CommitBuilt || !cycle.CommitReused || cycle.CommitGenerationID != refResult.GenerationID {
		t.Fatalf("checkout cycle = %+v, want ref generation %d reused", cycle, refResult.GenerationID)
	}
	if route := f.route(); route.CommitGenerationID != refResult.GenerationID {
		t.Fatalf("route = %+v, want generation %d", route, refResult.GenerationID)
	}
}

func TestRefViewReadyCacheSurvivesManagerRestart(t *testing.T) {
	f := newRefViewFixture(t)
	commit, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/first", commit)
	f.setRef("refs/heads/after-restart", commit)
	first, err := refReadyTestManager(t, f.store, nil, nil).
		EnsureRefView(context.Background(), f.request("refs/heads/first"))
	if err != nil {
		t.Fatalf("seed ref view: %v", err)
	}
	var builds atomic.Int64
	second, err := refReadyTestManager(t, f.store, func() { builds.Add(1) }, nil).
		EnsureRefView(context.Background(), f.request("refs/heads/after-restart"))
	if err != nil {
		t.Fatalf("select after manager restart: %v", err)
	}
	if !first.Built || second.Built || second.GenerationID != first.GenerationID || builds.Load() != 0 {
		t.Fatalf("first=%+v second=%+v builds=%d, want restart cache reuse", first, second, builds.Load())
	}
}

func TestRefViewsConvergeConcurrentColdBuilds(t *testing.T) {
	f := newRefViewFixture(t)
	commit, _ := f.commitTree(builderTreeB(), "B")
	refs := []string{"refs/heads/race-a", "refs/heads/race-b"}
	for _, ref := range refs {
		f.setRef(ref, commit)
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var builds atomic.Int64
	manager := refReadyTestManager(t, f.store, func() { builds.Add(1) }, func() {
		entered <- struct{}{}
		<-release
	})
	type outcome struct {
		ref string
		got RefViewResult
		err error
	}
	results := make(chan outcome, 2)
	for _, ref := range refs {
		ref := ref
		go func() {
			got, err := manager.EnsureRefView(context.Background(), f.request(ref))
			results <- outcome{ref: ref, got: got, err: err}
		}()
	}
	for range refs {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("both cold ref builds did not reach the cache-miss barrier")
		}
	}
	close(release)
	var winner int64
	for range refs {
		out := <-results
		if out.err != nil {
			t.Fatalf("select %s: %v", out.ref, out.err)
		}
		if out.got.State != store_sqlite.RefViewReady || !out.got.Built {
			t.Fatalf("select %s = %+v, want a completed cold pass", out.ref, out.got)
		}
		if winner == 0 {
			winner = out.got.GenerationID
		} else if out.got.GenerationID != winner {
			t.Fatalf("select %s used generation %d, want canonical %d", out.ref, out.got.GenerationID, winner)
		}
	}
	if builds.Load() != 2 {
		t.Fatalf("%d builds ran, want the two deliberately concurrent cold passes", builds.Load())
	}
	for _, ref := range refs {
		rows := f.builds(f.viewID(ref))
		if len(rows) != 1 || rows[0].State != store_sqlite.ViewGenerationReady || rows[0].GenerationID != winner {
			t.Fatalf("build rows for %s = %+v, want canonical generation %d", ref, rows, winner)
		}
	}
	generations := f.generations()
	if len(generations) != 1 || generations[0].GenerationID != winner {
		t.Fatalf("generations = %+v, want only canonical winner %d after loser retirement", generations, winner)
	}
}

func TestRefViewRebuildsWithdrawnGenerationWhenLocalClosureIsComplete(t *testing.T) {
	f := newRefViewFixture(t)
	commit, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/withdrawn", commit)
	var builds atomic.Int64
	manager := refReadyTestManager(t, f.store, func() { builds.Add(1) }, nil)
	first, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/withdrawn"))
	if err != nil {
		t.Fatalf("seed ref view: %v", err)
	}
	if err := f.catalog.WithdrawProducer(context.Background(), first.GenerationID, commitLayerSourceSnapshotCapability, "test withdrawal"); err != nil {
		t.Fatalf("withdraw source snapshot: %v", err)
	}
	second, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/withdrawn"))
	if err != nil {
		t.Fatalf("rebuild withdrawn source: %v", err)
	}
	if !second.Built || second.GenerationID == first.GenerationID || builds.Load() != 2 {
		t.Fatalf("first=%+v second=%+v builds=%d, want one verified recovery build", first, second, builds.Load())
	}
	availability, err := f.catalog.ReadProducerAvailability(context.Background(), second.GenerationID, commitLayerSourceSnapshotCapability)
	if err != nil {
		t.Fatalf("read recovered source availability: %v", err)
	}
	if !availability.Declared || availability.State != store_sqlite.ProducerStateComplete {
		t.Fatalf("recovered source availability = %+v, want complete", availability)
	}
}

func BenchmarkRefViewReadyCacheHit(b *testing.B) {
	f := newRefViewFixture(b)
	commit, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/benchmark", commit)
	var builds atomic.Int64
	manager := refReadyTestManager(b, f.store, func() { builds.Add(1) }, nil)
	seed, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/benchmark"))
	if err != nil {
		b.Fatalf("seed ref view: %v", err)
	}
	builds.Store(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := f.request("refs/heads/benchmark")
		req.EnrichmentProfile = fmt.Sprintf("bench-%d", i)
		got, err := manager.EnsureRefView(context.Background(), req)
		if err != nil || got.Built || got.GenerationID != seed.GenerationID {
			b.Fatalf("cache hit %d = %+v err=%v, want generation %d without a build", i, got, err, seed.GenerationID)
		}
	}
	b.StopTimer()
	buildCount := builds.Load()
	b.ReportMetric(float64(buildCount)/float64(b.N), "builds/op")
	if buildCount != 0 {
		b.Fatalf("%d builds ran across %d ready-cache hits", buildCount, b.N)
	}
}
