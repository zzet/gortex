package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
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
	var physicalPasses atomic.Int64
	fixture.builder.beforePhysicalPass = func(int64) error {
		if physicalPasses.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	}

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
	if got := physicalPasses.Load(); got != 1 {
		t.Fatalf("physical index passes = %d, want 1", got)
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
	var physicalPasses atomic.Int64
	fixture.builder.beforePhysicalPass = func(int64) error {
		if physicalPasses.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	}

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
	if got := physicalPasses.Load(); got != 1 {
		t.Fatalf("physical index passes = %d, want 1", got)
	}
}

func TestSparseGenerationBuilderFailureCompletesFlightBeforeRetry(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	var physicalPasses atomic.Int64
	fixture.builder.beforePhysicalPass = func(int64) error {
		physicalPasses.Add(1)
		return nil
	}
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
	if got := physicalPasses.Load(); got != 2 {
		t.Fatalf("physical index passes = %d, want failed pass plus retry", got)
	}
}

func TestSparseGenerationBuilderPanicCompletesFlightBeforePropagating(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	var physicalPasses atomic.Int64
	fixture.builder.beforePhysicalPass = func(int64) error {
		physicalPasses.Add(1)
		return nil
	}
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
	if got := physicalPasses.Load(); got != 2 {
		t.Fatalf("physical index passes = %d, want panicked pass plus retry", got)
	}
}

func TestSparseGenerationBuilderConvertsOnlyStorePanicToFailure(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	storageErr := fixture.store.AddBatchChecked([]*graph.Node{{
		ID: "invalid-meta", Kind: graph.KindFunction,
		Meta: map[string]any{"unsupported": make(chan int)},
	}}, nil)
	if storageErr == nil {
		t.Fatal("unsupported metadata unexpectedly produced no storage error")
	}
	var typed *store_sqlite.StorageError
	if !errors.As(storageErr, &typed) {
		t.Fatalf("storage error = %T %v, want *StorageError", storageErr, storageErr)
	}
	fixture.request.PrePublish = func(context.Context, int64) error {
		panic(storageErr)
	}

	generationID, _, err := fixture.builder.Build(context.Background(), fixture.request)
	if err == nil || !errors.As(err, &typed) {
		t.Fatalf("Build error = %T %v, want returned StorageError", err, err)
	}
	row, found, getErr := fixture.store.Catalog().GetViewGeneration(context.Background(), generationID)
	if getErr != nil || !found {
		t.Fatalf("failed generation: found=%v err=%v", found, getErr)
	}
	if row.State != store_sqlite.ViewGenerationFailed ||
		row.Error != "graph storage write failed; see daemon log" {
		t.Fatalf("failed generation = %+v", row)
	}

	fixture.request.PrePublish = nil
	retryID, _, retryErr := fixture.builder.Build(context.Background(), fixture.request)
	if retryErr != nil {
		t.Fatalf("retry after storage failure: %v", retryErr)
	}
	if retryID == generationID {
		t.Fatalf("retry reused failed generation %d", retryID)
	}
}

func TestSparseGenerationBuilderAbandonsWithDetachedContextAfterCanceledStoragePanic(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	storageErr := fixture.store.AddBatchChecked([]*graph.Node{{
		ID: "invalid-meta-canceled-cleanup", Kind: graph.KindFunction,
		Meta: map[string]any{"unsupported": make(chan int)},
	}}, nil)
	if storageErr == nil {
		t.Fatal("unsupported metadata unexpectedly produced no storage error")
	}
	var typed *store_sqlite.StorageError
	if !errors.As(storageErr, &typed) {
		t.Fatalf("storage error = %T %v, want *StorageError", storageErr, storageErr)
	}

	buildCtx, cancelBuild := context.WithCancel(context.Background())
	defer cancelBuild()
	fixture.request.PrePublish = func(context.Context, int64) error {
		cancelBuild()
		panic(storageErr)
	}

	var (
		cleanupCalled      bool
		cleanupContextErr  error
		cleanupRemaining   time.Duration
		cleanupHasDeadline bool
	)
	fixture.builder.failViewGeneration = func(cleanupCtx context.Context, generationID int64, reason string) error {
		cleanupCalled = true
		cleanupContextErr = cleanupCtx.Err()
		deadline, ok := cleanupCtx.Deadline()
		cleanupHasDeadline = ok
		if ok {
			cleanupRemaining = time.Until(deadline)
		}
		return fixture.store.Catalog().FailViewGeneration(cleanupCtx, generationID, reason)
	}

	generationID, _, err := fixture.builder.Build(buildCtx, fixture.request)
	if err == nil || !errors.As(err, &typed) {
		t.Fatalf("Build error = %T %v, want returned StorageError", err, err)
	}
	if !errors.Is(buildCtx.Err(), context.Canceled) {
		t.Fatalf("build context error = %v, want context canceled", buildCtx.Err())
	}
	if !cleanupCalled {
		t.Fatal("abandoned generation did not attempt durable cleanup")
	}
	if cleanupContextErr != nil {
		t.Fatalf("cleanup context was already canceled: %v", cleanupContextErr)
	}
	if !cleanupHasDeadline || cleanupRemaining <= 0 || cleanupRemaining > generationAbandonTimeout {
		t.Fatalf("cleanup deadline remaining = %v present=%t, want live deadline within %v",
			cleanupRemaining, cleanupHasDeadline, generationAbandonTimeout)
	}
	row, found, getErr := fixture.store.Catalog().GetViewGeneration(context.Background(), generationID)
	if getErr != nil || !found {
		t.Fatalf("failed generation: found=%v err=%v", found, getErr)
	}
	if row.State != store_sqlite.ViewGenerationFailed ||
		row.Error != "graph storage write failed; see daemon log" {
		t.Fatalf("failed generation = %+v", row)
	}
}

func TestSparseGenerationBuilderRetainsProcessFailureWhenCatalogFailureWriteFails(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	storageErr := fixture.store.AddBatchChecked([]*graph.Node{{
		ID: "invalid-meta-fallback", Kind: graph.KindFunction,
		Meta: map[string]any{"unsupported": make(chan int)},
	}}, nil)
	if storageErr == nil {
		t.Fatal("unsupported metadata unexpectedly produced no storage error")
	}
	failures := newCheckoutBuildFailures()
	fixture.builder.buildFailures = failures
	fixture.builder.beforePhysicalPass = func(int64) error { return storageErr }
	fixture.builder.failViewGeneration = func(context.Context, int64, string) error {
		return errors.New("catalog cannot grow")
	}

	generationID, _, err := fixture.builder.Build(context.Background(), fixture.request)
	if err == nil {
		t.Fatal("Build unexpectedly succeeded")
	}
	reason, ok := failures.failure(fixture.request.Identity.CheckoutID, generationID)
	if !ok || reason != "graph storage write failed; see daemon log" {
		t.Fatalf("process failure = (%q, %t)", reason, ok)
	}
	row, found, getErr := fixture.store.Catalog().GetViewGeneration(context.Background(), generationID)
	if getErr != nil || !found || row.State != store_sqlite.ViewGenerationBuilding {
		t.Fatalf("durable generation = found=%v err=%v row=%+v, want building residue", found, getErr, row)
	}

	fixture.builder.beforePhysicalPass = nil
	fixture.builder.failViewGeneration = nil
	retryID, _, retryErr := fixture.builder.Build(context.Background(), fixture.request)
	if retryErr != nil {
		t.Fatalf("retry after catalog failure: %v", retryErr)
	}
	if retryID != generationID {
		t.Fatalf("retry generation = %d, want adopted building generation %d", retryID, generationID)
	}
	if _, stillFailed := failures.failure(fixture.request.Identity.CheckoutID, retryID); stillFailed {
		t.Fatal("successful retry retained process-local failure")
	}
}

func TestSparseGenerationCoalescesPlanningBeforePhysicalPass(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	request := fixture.request
	request.Identity.CreatedAt = time.Now().Unix()
	entered := make(chan struct{})
	release := make(chan struct{})
	var physicalPasses atomic.Int64
	fixture.builder.beforePhysicalPass = func(int64) error {
		if physicalPasses.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	}

	const callers = 16
	var preparations atomic.Int64
	start := make(chan struct{})
	results := make(chan sparseBuildFlightResult, callers)
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			generationID, report, err := fixture.builder.buildPrepared(
				context.Background(), time.Now(), request.Identity,
				func(_ context.Context, identity GenerationIdentity) (BuildRequest, func(), error) {
					preparations.Add(1)
					prepared := request
					prepared.Identity = identity
					return prepared, nil, nil
				},
			)
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
		context.Background(), payloadRequestForBuild(request),
	)
	if err != nil || !adopted {
		t.Fatalf("observe building generation: adopted=%t err=%v", adopted, err)
	}
	awaitSparseBuildFlightWaiters(t, fixture.store, generationID, callers-1)
	if got := preparations.Load(); got != 1 {
		t.Fatalf("preparations before release = %d, want 1", got)
	}
	if got := physicalPasses.Load(); got != 1 {
		t.Fatalf("physical index passes before release = %d, want 1", got)
	}
	close(release)

	physicalReports := 0
	for i := 0; i < callers; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("build %d: %v", i, result.err)
		}
		if result.generationID != generationID {
			t.Fatalf("build %d generation = %d, want shared %d", i, result.generationID, generationID)
		}
		if !result.report.Coalesced {
			physicalReports++
		}
	}
	if physicalReports != 1 {
		t.Fatalf("physical reports = %d, want 1", physicalReports)
	}

	readyFixture := newSparseBuildFlightFixture(t)
	readyRequest := readyFixture.request
	readyRequest.Identity.CreatedAt = time.Now().Unix()
	readyEntered := make(chan struct{})
	readyRelease := make(chan struct{})
	readyReleased := false
	defer func() {
		if !readyReleased {
			close(readyRelease)
		}
	}()
	var readyPhysicalPasses atomic.Int64
	readyFixture.builder.beforePhysicalPass = func(int64) error {
		if readyPhysicalPasses.Add(1) == 1 {
			close(readyEntered)
		}
		<-readyRelease
		return nil
	}

	adoptedBeforeJoin := make(chan struct{})
	allowReadyJoin := make(chan struct{})
	readyJoinAllowed := false
	defer func() {
		if !readyJoinAllowed {
			close(allowReadyJoin)
		}
	}()
	var blockedReadyJoin atomic.Bool
	readyFixture.builder.beforePayloadFlightJoin = func(_ int64, adopted bool) {
		if adopted && blockedReadyJoin.CompareAndSwap(false, true) {
			close(adoptedBeforeJoin)
			<-allowReadyJoin
		}
	}
	var readyPreparations atomic.Int64
	readyResults := make(chan sparseBuildFlightResult, 2)
	buildReadyRace := func() {
		generationID, report, err := readyFixture.builder.buildPrepared(
			context.Background(), time.Now(), readyRequest.Identity,
			func(_ context.Context, identity GenerationIdentity) (BuildRequest, func(), error) {
				readyPreparations.Add(1)
				prepared := readyRequest
				prepared.Identity = identity
				return prepared, nil, nil
			},
		)
		readyResults <- sparseBuildFlightResult{generationID: generationID, report: report, err: err}
	}
	go buildReadyRace()
	select {
	case <-readyEntered:
	case <-time.After(20 * time.Second):
		t.Fatal("ready-race leader did not reach its index pass")
	}
	go buildReadyRace()
	select {
	case <-adoptedBeforeJoin:
	case <-time.After(20 * time.Second):
		t.Fatal("ready-race follower did not adopt the building generation")
	}
	close(readyRelease)
	readyReleased = true
	leaderResult := <-readyResults
	if leaderResult.err != nil {
		t.Fatalf("ready-race leader: %v", leaderResult.err)
	}
	if leaderResult.report.Coalesced {
		t.Fatal("ready-race leader reported coalesced")
	}
	close(allowReadyJoin)
	readyJoinAllowed = true
	readyResult := <-readyResults
	if readyResult.err != nil {
		t.Fatalf("ready reuse: %v", readyResult.err)
	}
	if readyResult.generationID != leaderResult.generationID || !readyResult.report.Coalesced {
		t.Fatalf(
			"ready reuse = generation %d coalesced=%t, want %d true",
			readyResult.generationID, readyResult.report.Coalesced, leaderResult.generationID,
		)
	}
	if got := readyPreparations.Load(); got != 1 {
		t.Fatalf("ready-race preparations = %d, want 1", got)
	}
	if got := readyPhysicalPasses.Load(); got != 1 {
		t.Fatalf("ready-race physical index passes = %d, want 1", got)
	}
}

func benchmarkSparseGenerationBuilderFlightIteration(
	b *testing.B,
	fixture sparseBuildFlightFixture,
	callers, iteration int,
) (int64, int64) {
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
	var physicalPasses atomic.Int64
	fixture.builder.beforePhysicalPass = func(int64) error {
		if physicalPasses.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	}

	var preparations atomic.Int64
	start := make(chan struct{})
	results := make(chan sparseBuildFlightResult, callers)
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			generationID, report, err := fixture.builder.buildPrepared(
				context.Background(), time.Now(), request.Identity,
				func(_ context.Context, identity GenerationIdentity) (BuildRequest, func(), error) {
					preparations.Add(1)
					prepared := request
					prepared.Identity = identity
					return prepared, nil, nil
				},
			)
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
	if passes := physicalPasses.Load(); passes != 1 {
		b.Fatalf("physical index passes = %d, want 1", passes)
	}
	return physicalPasses.Load(), preparations.Load()
}

func BenchmarkSparseGenerationBuilderCoalescedPhysicalPass(b *testing.B) {
	for _, callers := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("%d_callers", callers), func(b *testing.B) {
			fixture := newSparseBuildFlightFixture(b)
			var physicalPasses, preparations int64
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				physical, prepared := benchmarkSparseGenerationBuilderFlightIteration(b, fixture, callers, i)
				physicalPasses += physical
				preparations += prepared
			}
			b.ReportMetric(float64(physicalPasses)/float64(b.N), "physical-builds/op")
			b.ReportMetric(float64(preparations)/float64(b.N), "preparations/op")
		})
	}
}
