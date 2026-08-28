package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

type sparseBuildFlightFixture struct {
	store   *store_sqlite.Store
	builder *SparseGenerationBuilder
	request BuildRequest
}

func newSparseBuildFlightFixture(t testing.TB) sparseBuildFlightFixture {
	t.Helper()
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "flight-repo")
	builderGit(t, repoDir, "init", "--initial-branch=main")

	builderWriteTree(t, repoDir, builderTreeA())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "A")
	treeA := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")

	baseDir := builderTempDir(t, "flight-base")
	builderWriteTree(t, baseDir, builderTreeA())

	builderWriteTree(t, repoDir, builderTreeB())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-m", "B")
	treeB := builderGit(t, repoDir, "rev-parse", "HEAD^{tree}")
	commitB := builderGit(t, repoDir, "rev-parse", "HEAD")

	store := builderOpenStore(t, "flight-base")
	builderIndex(t, store, baseDir)
	changes, err := diffTreeChanges(context.Background(), repoDir, treeA, treeB)
	if err != nil {
		t.Fatalf("diff trees: %v", err)
	}
	target, err := source.NewGitTreeSource(context.Background(), repoDir, treeB)
	if err != nil {
		t.Fatalf("open target tree: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	return sparseBuildFlightFixture{
		store:   store,
		builder: builderNewBuilder(store),
		request: BuildRequest{
			Identity: GenerationIdentity{
				OwnerKind:           "dedicated_graph",
				GraphID:             "graph-flight",
				LayerID:             "layer-" + treeB,
				CheckoutID:          "checkout-flight",
				GenerationKind:      CommitLayerGenerationKind,
				TreeOID:             treeB,
				ProvenanceCommitOID: commitB,
				CreatedAt:           time.Now().Unix(),
			},
			Base:        store,
			Target:      target,
			Changes:     changes,
			RootPath:    baseDir,
			RepoPrefix:  builderRepoPrefix,
			WorkspaceID: builderRepoPrefix,
			ProjectID:   builderRepoPrefix,
		},
	}
}

type countingWalkSource struct {
	source.ContentSource
	walks   atomic.Int64
	entered chan struct{}
	release <-chan struct{}
}

func (s *countingWalkSource) Walk(ctx context.Context, fn func(source.FileMeta) error) error {
	if s.walks.Add(1) == 1 && s.entered != nil {
		close(s.entered)
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.ContentSource.Walk(ctx, fn)
}

type sparseBuildFlightResult struct {
	generationID int64
	report       BuildReport
	err          error
}

func payloadRequestForBuild(req BuildRequest) store_sqlite.PayloadGenerationRequest {
	return store_sqlite.PayloadGenerationRequest{
		OwnerKind:            req.Identity.OwnerKind,
		GraphID:              req.Identity.GraphID,
		LayerID:              req.Identity.LayerID,
		CheckoutID:           req.Identity.CheckoutID,
		GenerationKind:       req.Identity.GenerationKind,
		BaseGenerationID:     req.Identity.BaseGenerationID,
		LowerViewFingerprint: req.Identity.LowerViewFingerprint,
		TreeOID:              req.Identity.TreeOID,
		ProvenanceCommitOID:  req.Identity.ProvenanceCommitOID,
		ConfigHash:           req.Identity.ConfigHash,
		ExtractorVersions:    req.Identity.ExtractorVersions,
		ResolverVersion:      req.Identity.ResolverVersion,
		CreatedAt:            req.Identity.CreatedAt,
	}
}

func awaitSparseBuildFlightWaiters(
	t testing.TB,
	store *store_sqlite.Store,
	generationID, want int64,
) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if got := store.PayloadBuildFlightWaiters(generationID); got == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("payload flight %d waiters = %d, want %d",
				generationID, store.PayloadBuildFlightWaiters(generationID), want)
		case <-ticker.C:
		}
	}
}

func TestSparseGenerationBuilderCoalescesConcurrentPhysicalPasses(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	const callers = 8
	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	counting := &countingWalkSource{
		ContentSource: fixture.request.Target,
		entered:       entered,
		release:       release,
	}
	fixture.request.Target = counting

	start := make(chan struct{})
	results := make(chan sparseBuildFlightResult, callers)
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			generationID, report, err := fixture.builder.Build(context.Background(), fixture.request)
			results <- sparseBuildFlightResult{generationID: generationID, report: report, err: err}
		}()
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("physical sparse build did not reach its index pass")
	}

	generationID, _, adopted, err := fixture.store.BeginPayloadGenerationWithStatus(
		context.Background(), payloadRequestForBuild(fixture.request),
	)
	if err != nil || !adopted {
		t.Fatalf("observe building generation: adopted=%t err=%v", adopted, err)
	}
	awaitSparseBuildFlightWaiters(t, fixture.store, generationID, callers-1)
	close(release)
	released = true

	physical := 0
	coalesced := 0
	for i := 0; i < callers; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("build %d: %v", i, result.err)
		}
		if result.generationID != generationID {
			t.Errorf("build %d generation = %d, want shared %d", i, result.generationID, generationID)
		}
		if result.report.Coalesced {
			coalesced++
		} else {
			physical++
		}
	}
	if physical != 1 || coalesced != callers-1 {
		t.Fatalf("physical=%d coalesced=%d, want 1 and %d", physical, coalesced, callers-1)
	}
	if got := counting.walks.Load(); got != 1 {
		t.Fatalf("physical index walks = %d, want 1", got)
	}
}

func TestSparseGenerationBuilderFollowerCancellationLeavesLeaderRunning(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	counting := &countingWalkSource{
		ContentSource: fixture.request.Target,
		entered:       entered,
		release:       release,
	}
	fixture.request.Target = counting

	leaderResult := make(chan sparseBuildFlightResult, 1)
	go func() {
		generationID, report, err := fixture.builder.Build(context.Background(), fixture.request)
		leaderResult <- sparseBuildFlightResult{generationID: generationID, report: report, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("leader did not reach its index pass")
	}
	generationID, _, adopted, err := fixture.store.BeginPayloadGenerationWithStatus(
		context.Background(), payloadRequestForBuild(fixture.request),
	)
	if err != nil || !adopted {
		t.Fatalf("observe building generation: adopted=%t err=%v", adopted, err)
	}

	followerCtx, cancelFollower := context.WithCancel(context.Background())
	followerResult := make(chan sparseBuildFlightResult, 1)
	go func() {
		generationID, report, err := fixture.builder.Build(followerCtx, fixture.request)
		followerResult <- sparseBuildFlightResult{generationID: generationID, report: report, err: err}
	}()
	awaitSparseBuildFlightWaiters(t, fixture.store, generationID, 1)
	cancelFollower()
	follower := <-followerResult
	if !errors.Is(follower.err, context.Canceled) {
		t.Fatalf("follower error = %v, want context canceled", follower.err)
	}
	if follower.generationID != generationID || !follower.report.Coalesced {
		t.Fatalf("follower generation=%d coalesced=%t, want %d and true",
			follower.generationID, follower.report.Coalesced, generationID)
	}
	select {
	case result := <-leaderResult:
		t.Fatalf("leader stopped with follower: %+v", result)
	default:
	}

	close(release)
	released = true
	leader := <-leaderResult
	if leader.err != nil {
		t.Fatalf("leader build: %v", leader.err)
	}
	if leader.generationID != generationID || leader.report.Coalesced {
		t.Fatalf("leader generation=%d coalesced=%t, want %d and false",
			leader.generationID, leader.report.Coalesced, generationID)
	}
	if got := counting.walks.Load(); got != 1 {
		t.Fatalf("physical index walks = %d, want 1", got)
	}
}

func TestSparseGenerationBuilderFailureCompletesFlightBeforeRetry(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	counting := &countingWalkSource{ContentSource: fixture.request.Target}
	fixture.request.Target = counting
	wantErr := errors.New("reject first publish")
	var attempts atomic.Int64
	fixture.request.PrePublish = func(context.Context, int64) error {
		if attempts.Add(1) == 1 {
			return wantErr
		}
		return nil
	}

	failedID, failedReport, err := fixture.builder.Build(context.Background(), fixture.request)
	if !errors.Is(err, wantErr) {
		t.Fatalf("first build error = %v, want %v", err, wantErr)
	}
	if failedReport.Coalesced {
		t.Fatal("failed physical writer was reported as coalesced")
	}

	retryID, retryReport, err := fixture.builder.Build(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("retry build: %v", err)
	}
	if retryReport.Coalesced {
		t.Fatal("retry physical writer was reported as coalesced")
	}
	if retryID == failedID {
		t.Fatalf("retry reused failed generation %d", retryID)
	}
	if got := counting.walks.Load(); got != 2 {
		t.Fatalf("physical index walks = %d, want failed pass plus retry", got)
	}
}

func TestSparseGenerationBuilderPanicCompletesFlightBeforePropagating(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	counting := &countingWalkSource{ContentSource: fixture.request.Target}
	fixture.request.Target = counting
	wantPanic := errors.New("panic before publish")
	var failedID atomic.Int64
	var attempts atomic.Int64
	fixture.request.PrePublish = func(_ context.Context, generationID int64) error {
		if attempts.Add(1) == 1 {
			failedID.Store(generationID)
			panic(wantPanic)
		}
		return nil
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _, _ = fixture.builder.Build(context.Background(), fixture.request)
	}()
	if recovered != wantPanic {
		t.Fatalf("recovered panic = %v, want %v", recovered, wantPanic)
	}

	retryID, retryReport, err := fixture.builder.Build(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("retry after panic: %v", err)
	}
	if retryReport.Coalesced {
		t.Fatal("retry after panic was reported as coalesced")
	}
	if retryID == failedID.Load() {
		t.Fatalf("retry reused panicked generation %d", retryID)
	}
	if got := counting.walks.Load(); got != 2 {
		t.Fatalf("physical index walks = %d, want panicked pass plus retry", got)
	}
}

func benchmarkSparseGenerationBuilderFlightIteration(
	b *testing.B,
	fixture sparseBuildFlightFixture,
	callers, iteration int,
) int64 {
	b.Helper()
	request := fixture.request
	request.Identity.LayerID = fmt.Sprintf("%s-bench-%d", request.Identity.LayerID, iteration)
	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	counting := &countingWalkSource{
		ContentSource: request.Target,
		entered:       entered,
		release:       release,
	}
	request.Target = counting

	start := make(chan struct{})
	results := make(chan sparseBuildFlightResult, callers)
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			generationID, report, err := fixture.builder.Build(context.Background(), request)
			results <- sparseBuildFlightResult{generationID: generationID, report: report, err: err}
		}()
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		b.Fatal("physical sparse build did not reach its index pass")
	}
	generationID, _, adopted, err := fixture.store.BeginPayloadGenerationWithStatus(
		context.Background(), payloadRequestForBuild(request),
	)
	if err != nil || !adopted {
		b.Fatalf("observe building generation: adopted=%t err=%v", adopted, err)
	}
	awaitSparseBuildFlightWaiters(b, fixture.store, generationID, int64(callers-1))
	close(release)
	released = true

	physicalReports := 0
	for i := 0; i < callers; i++ {
		result := <-results
		if result.err != nil {
			b.Fatalf("build %d: %v", i, result.err)
		}
		if result.generationID != generationID {
			b.Fatalf("build %d generation = %d, want shared %d", i, result.generationID, generationID)
		}
		if !result.report.Coalesced {
			physicalReports++
		}
	}
	if physicalReports != 1 {
		b.Fatalf("physical reports = %d, want 1", physicalReports)
	}
	if walks := counting.walks.Load(); walks != 1 {
		b.Fatalf("physical index walks = %d, want 1", walks)
	}
	return counting.walks.Load()
}

func BenchmarkSparseGenerationBuilderCoalescedPhysicalPass(b *testing.B) {
	for _, callers := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("%d_callers", callers), func(b *testing.B) {
			fixture := newSparseBuildFlightFixture(b)
			var physicalPasses int64
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				physicalPasses += benchmarkSparseGenerationBuilderFlightIteration(b, fixture, callers, i)
			}
			b.ReportMetric(float64(physicalPasses)/float64(b.N), "physical-builds/op")
		})
	}
}
