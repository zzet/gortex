package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
)

type routedDirtyBuildFixture struct {
	sparse  sparseBuildFlightFixture
	request DirtyLayerRequest
	sample  gitstate.DirtySnapshot
}

func newRoutedDirtyBuildFixture(tb testing.TB) routedDirtyBuildFixture {
	tb.Helper()
	fixture := newSparseBuildFlightFixture(tb)
	builderGit(tb, fixture.request.RootPath, "init")
	builderGit(tb, fixture.request.RootPath, "config", "user.name", "Gortex Test")
	builderGit(tb, fixture.request.RootPath, "config", "user.email", "gortex@example.invalid")
	builderGit(tb, fixture.request.RootPath, "add", ".")
	builderGit(tb, fixture.request.RootPath, "commit", "-m", "base")
	if err := os.WriteFile(
		filepath.Join(fixture.request.RootPath, "routed_dirty.go"),
		[]byte("package fixture\n\nfunc RoutedDirty() {}\n"),
		0o600,
	); err != nil {
		tb.Fatalf("write dirty fixture: %v", err)
	}
	sample, err := gitstate.SampleDirty(context.Background(), fixture.request.RootPath)
	if err != nil {
		tb.Fatalf("sample dirty fixture: %v", err)
	}
	checkoutID := fixture.request.Identity.CheckoutID
	if checkoutID == "" {
		checkoutID = "routed-dirty-checkout"
	}
	coordinator := &CheckoutCoordinator{
		checkoutID: checkoutID,
		configHash: fixture.request.Identity.ConfigHash,
		extractors: fixture.request.Identity.ExtractorVersions,
	}
	identity := coordinator.dirtyIdentity(
		fixture.request.Identity.GraphID,
		fixture.request.Identity.BaseGenerationID,
		sample,
	)
	return routedDirtyBuildFixture{
		sparse: fixture,
		request: DirtyLayerRequest{
			Identity:     identity,
			Base:         fixture.request.Base,
			CheckoutRoot: fixture.request.RootPath,
			RepoPrefix:   fixture.request.RepoPrefix,
			WorkspaceID:  fixture.request.WorkspaceID,
			ProjectID:    fixture.request.ProjectID,
		},
		sample: sample,
	}
}

func TestRoutedDirtyIdentitySkipsPreflightSampling(t *testing.T) {
	t.Run("authoritative identity", func(t *testing.T) {
		fixture := newRoutedDirtyBuildFixture(t)
		identity := fixture.request.Identity
		if identity.TreeOID != fixture.sample.HeadTree {
			t.Fatalf("tree OID = %q, want %q", identity.TreeOID, fixture.sample.HeadTree)
		}
		if identity.ProvenanceCommitOID != fixture.sample.HeadCommit {
			t.Fatalf("commit OID = %q, want %q", identity.ProvenanceCommitOID, fixture.sample.HeadCommit)
		}
		if identity.LowerViewFingerprint != fixture.sample.Fingerprint {
			t.Fatalf("fingerprint = %q, want %q", identity.LowerViewFingerprint, fixture.sample.Fingerprint)
		}
	})

	t.Run("concurrent routed callers", func(t *testing.T) {
		fixture := newRoutedDirtyBuildFixture(t)
		const callers = 16
		var identitySamples atomic.Int64
		var buildSamples atomic.Int64
		entered := make(chan struct{})
		release := make(chan struct{})
		request := fixture.request
		request.identitySampler = func(ctx context.Context, root string) (gitstate.DirtySnapshot, error) {
			identitySamples.Add(1)
			return gitstate.SampleDirty(ctx, root)
		}
		request.buildSampler = func(ctx context.Context, root string) (gitstate.DirtySnapshot, error) {
			if buildSamples.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-release:
			case <-ctx.Done():
				return gitstate.DirtySnapshot{}, ctx.Err()
			}
			return gitstate.SampleDirty(ctx, root)
		}

		start := make(chan struct{})
		results := make(chan sparseBuildFlightResult, callers)
		for range callers {
			go func() {
				<-start
				generationID, report, err := fixture.sparse.builder.BuildDirtyLayer(context.Background(), request)
				results <- sparseBuildFlightResult{generationID: generationID, report: report, err: err}
			}()
		}
		close(start)
		select {
		case <-entered:
		case <-time.After(20 * time.Second):
			t.Fatal("dirty build did not reach leader sampling")
		}
		generationID, _, adopted, err := fixture.sparse.store.BeginPayloadGenerationWithStatus(
			context.Background(),
			payloadRequestForBuild(BuildRequest{Identity: request.Identity}),
		)
		if err != nil || !adopted {
			close(release)
			t.Fatalf("observe dirty build: adopted=%t err=%v", adopted, err)
		}
		awaitSparseBuildFlightWaiters(t, fixture.sparse.store, generationID, callers-1)
		if got := identitySamples.Load(); got != 0 {
			close(release)
			t.Fatalf("preflight identity samples before release = %d, want 0", got)
		}
		if got := buildSamples.Load(); got != 1 {
			close(release)
			t.Fatalf("leader samples before release = %d, want 1", got)
		}
		close(release)

		physicalReports := 0
		for i := range callers {
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
		if got := identitySamples.Load(); got != 0 {
			t.Fatalf("preflight identity samples = %d, want 0", got)
		}
		if got := buildSamples.Load(); got != 1 {
			t.Fatalf("leader samples = %d, want 1", got)
		}
	})

	t.Run("ready follower reuse", func(t *testing.T) {
		fixture := newRoutedDirtyBuildFixture(t)
		var identitySamples atomic.Int64
		var buildSamples atomic.Int64
		leaderEntered := make(chan struct{})
		allowLeader := make(chan struct{})
		followerAdopted := make(chan struct{})
		allowFollowerJoin := make(chan struct{})
		request := fixture.request
		request.identitySampler = func(ctx context.Context, root string) (gitstate.DirtySnapshot, error) {
			identitySamples.Add(1)
			return gitstate.SampleDirty(ctx, root)
		}
		request.buildSampler = func(ctx context.Context, root string) (gitstate.DirtySnapshot, error) {
			if buildSamples.Add(1) == 1 {
				close(leaderEntered)
			}
			select {
			case <-allowLeader:
			case <-ctx.Done():
				return gitstate.DirtySnapshot{}, ctx.Err()
			}
			return gitstate.SampleDirty(ctx, root)
		}
		fixture.sparse.builder.beforePayloadFlightJoin = func(_ int64, adopted bool) {
			if !adopted {
				return
			}
			close(followerAdopted)
			<-allowFollowerJoin
		}

		leaderResult := make(chan sparseBuildFlightResult, 1)
		go func() {
			generationID, report, err := fixture.sparse.builder.BuildDirtyLayer(context.Background(), request)
			leaderResult <- sparseBuildFlightResult{generationID: generationID, report: report, err: err}
		}()
		select {
		case <-leaderEntered:
		case <-time.After(20 * time.Second):
			t.Fatal("dirty leader did not reach sampling")
		}

		followerResult := make(chan sparseBuildFlightResult, 1)
		go func() {
			generationID, report, err := fixture.sparse.builder.BuildDirtyLayer(context.Background(), request)
			followerResult <- sparseBuildFlightResult{generationID: generationID, report: report, err: err}
		}()
		select {
		case <-followerAdopted:
		case <-time.After(20 * time.Second):
			close(allowLeader)
			t.Fatal("dirty follower did not adopt the leader flight")
		}
		close(allowLeader)
		leader := <-leaderResult
		if leader.err != nil {
			close(allowFollowerJoin)
			t.Fatalf("dirty leader: %v", leader.err)
		}
		close(allowFollowerJoin)
		follower := <-followerResult
		if follower.err != nil {
			t.Fatalf("ready dirty follower: %v", follower.err)
		}
		if follower.generationID != leader.generationID {
			t.Fatalf("ready generation = %d, want %d", follower.generationID, leader.generationID)
		}
		if !follower.report.Coalesced {
			t.Fatal("ready follower report is not coalesced")
		}
		if got := identitySamples.Load(); got != 0 {
			t.Fatalf("preflight identity samples = %d, want 0", got)
		}
		if got := buildSamples.Load(); got != 1 {
			t.Fatalf("leader samples = %d, want 1", got)
		}
	})

	t.Run("legacy direct caller fallback", func(t *testing.T) {
		fixture := newRoutedDirtyBuildFixture(t)
		var identitySamples atomic.Int64
		var buildSamples atomic.Int64
		request := fixture.request
		request.Identity.TreeOID = ""
		request.Identity.ProvenanceCommitOID = ""
		request.Identity.LowerViewFingerprint = ""
		request.identitySampler = func(ctx context.Context, root string) (gitstate.DirtySnapshot, error) {
			identitySamples.Add(1)
			return gitstate.SampleDirty(ctx, root)
		}
		request.buildSampler = func(ctx context.Context, root string) (gitstate.DirtySnapshot, error) {
			buildSamples.Add(1)
			return gitstate.SampleDirty(ctx, root)
		}
		if _, _, err := fixture.sparse.builder.BuildDirtyLayer(context.Background(), request); err != nil {
			t.Fatalf("legacy dirty build: %v", err)
		}
		if got := identitySamples.Load(); got != 1 {
			t.Fatalf("preflight identity samples = %d, want 1", got)
		}
		if got := buildSamples.Load(); got != 1 {
			t.Fatalf("leader samples = %d, want 1", got)
		}
	})
}

func TestRoutedDirtyIdentityRejectsLeaderPreparationDrift(t *testing.T) {
	fixture := newRoutedDirtyBuildFixture(t)
	if err := os.WriteFile(
		filepath.Join(fixture.request.CheckoutRoot, "routed_dirty.go"),
		[]byte("package fixture\n\nfunc RoutedDirtyChangedBeforePreparation() {}\n"),
		0o600,
	); err != nil {
		t.Fatalf("change dirty fixture before leader preparation: %v", err)
	}
	changed, err := gitstate.SampleDirty(context.Background(), fixture.request.CheckoutRoot)
	if err != nil {
		t.Fatalf("sample changed dirty fixture: %v", err)
	}
	if changed.Fingerprint == fixture.sample.Fingerprint {
		t.Fatalf("changed fingerprint = original %q", changed.Fingerprint)
	}

	var identitySamples atomic.Int64
	var buildSamples atomic.Int64
	request := fixture.request
	request.identitySampler = func(ctx context.Context, root string) (gitstate.DirtySnapshot, error) {
		identitySamples.Add(1)
		return gitstate.SampleDirty(ctx, root)
	}
	request.buildSampler = func(ctx context.Context, root string) (gitstate.DirtySnapshot, error) {
		buildSamples.Add(1)
		return gitstate.SampleDirty(ctx, root)
	}

	assertDrift := func(attempt int, staleRequest DirtyLayerRequest) int64 {
		generationID, report, buildErr := fixture.sparse.builder.BuildDirtyLayer(context.Background(), staleRequest)
		if buildErr == nil {
			t.Fatalf("dirty build %d unexpectedly published the stale identity", attempt)
		}
		if !errors.Is(buildErr, ErrDirtySnapshotChanged) {
			t.Fatalf("dirty build %d error = %T %v, want ErrDirtySnapshotChanged in its chain", attempt, buildErr, buildErr)
		}
		if !isSparseBuildPreflightError(buildErr) {
			t.Fatalf("dirty build %d error = %T %v, want a retryable preflight outcome", attempt, buildErr, buildErr)
		}
		if generationID != 0 {
			t.Fatalf("dirty build %d returned generation = %d, want no publishable generation", attempt, generationID)
		}
		if report.GenerationID <= 0 {
			t.Fatalf("dirty build %d report generation = %d, want the abandoned flight generation", attempt, report.GenerationID)
		}
		if report.Coalesced {
			t.Fatalf("dirty build %d unexpectedly reused generation %d", attempt, report.GenerationID)
		}
		return report.GenerationID
	}

	firstGenerationID := assertDrift(1, request)
	if got := identitySamples.Load(); got != 0 {
		t.Fatalf("preflight identity samples after first drift = %d, want 0", got)
	}
	if got := buildSamples.Load(); got != 1 {
		t.Fatalf("leader preparation samples after first drift = %d, want 1", got)
	}

	retryRequest := request
	coordinator := &CheckoutCoordinator{
		checkoutID: request.Identity.CheckoutID,
		configHash: request.Identity.ConfigHash,
		extractors: request.Identity.ExtractorVersions,
	}
	retryRequest.Identity = coordinator.dirtyIdentity(
		request.Identity.GraphID,
		request.Identity.BaseGenerationID,
		changed,
	)
	generationID, report, err := fixture.sparse.builder.BuildDirtyLayer(context.Background(), retryRequest)
	if err != nil {
		t.Fatalf("retry changed dirty identity: %v", err)
	}
	if generationID <= 0 || report.GenerationID != generationID {
		t.Fatalf("retry generation = %d report=%d, want one published generation", generationID, report.GenerationID)
	}
	if generationID == firstGenerationID {
		t.Fatalf("retry reused abandoned generation %d", generationID)
	}
	if report.Coalesced {
		t.Fatalf("retry unexpectedly coalesced onto generation %d", generationID)
	}
	if got := identitySamples.Load(); got != 0 {
		t.Fatalf("preflight identity samples after retry = %d, want 0", got)
	}
	if got := buildSamples.Load(); got != 2 {
		t.Fatalf("leader preparation samples after retry = %d, want 2", got)
	}

	thirdGenerationID := assertDrift(3, request)
	if thirdGenerationID == firstGenerationID {
		t.Fatalf("stale identity reused abandoned generation %d", thirdGenerationID)
	}
	if thirdGenerationID == generationID {
		t.Fatalf("stale identity reused changed generation %d", thirdGenerationID)
	}
	if got := identitySamples.Load(); got != 0 {
		t.Fatalf("preflight identity samples = %d, want 0", got)
	}
	if got := buildSamples.Load(); got != 3 {
		t.Fatalf("leader preparation samples = %d, want 3", got)
	}
}

func BenchmarkRoutedDirtyIdentitySampling(b *testing.B) {
	for _, callers := range []int{1, 8, 64} {
		for _, routed := range []bool{false, true} {
			name := "legacy"
			if routed {
				name = "routed"
			}
			b.Run(fmt.Sprintf("%s_%d_callers", name, callers), func(b *testing.B) {
				benchmarkRoutedDirtyIdentitySampling(b, callers, routed)
			})
		}
	}
}

func benchmarkRoutedDirtyIdentitySampling(b *testing.B, callers int, routed bool) {
	fixture := newRoutedDirtyBuildFixture(b)
	var totalIdentitySamples int64
	var totalBuildSamples int64
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		content := fmt.Sprintf("package fixture\n\nfunc RoutedDirty%d() {}\n", iteration)
		if err := os.WriteFile(filepath.Join(fixture.request.CheckoutRoot, "routed_dirty.go"), []byte(content), 0o600); err != nil {
			b.Fatalf("write dirty iteration: %v", err)
		}
		sample, err := gitstate.SampleDirty(context.Background(), fixture.request.CheckoutRoot)
		if err != nil {
			b.Fatalf("sample dirty iteration: %v", err)
		}
		request := fixture.request
		request.Identity.TreeOID = sample.HeadTree
		request.Identity.ProvenanceCommitOID = sample.HeadCommit
		request.Identity.LowerViewFingerprint = sample.Fingerprint
		payloadIdentity := request.Identity
		if !routed {
			request.Identity.TreeOID = ""
			request.Identity.ProvenanceCommitOID = ""
			request.Identity.LowerViewFingerprint = ""
		}
		var identitySamples atomic.Int64
		var buildSamples atomic.Int64
		allIdentitySamples := make(chan struct{})
		if routed {
			close(allIdentitySamples)
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		request.identitySampler = func(ctx context.Context, root string) (gitstate.DirtySnapshot, error) {
			snapshot, err := gitstate.SampleDirty(ctx, root)
			if err == nil && identitySamples.Add(1) == int64(callers) {
				close(allIdentitySamples)
			}
			return snapshot, err
		}
		request.buildSampler = func(ctx context.Context, root string) (gitstate.DirtySnapshot, error) {
			buildSamples.Add(1)
			select {
			case <-allIdentitySamples:
			case <-ctx.Done():
				return gitstate.DirtySnapshot{}, ctx.Err()
			}
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return gitstate.DirtySnapshot{}, ctx.Err()
			}
			return gitstate.SampleDirty(ctx, root)
		}
		start := make(chan struct{})
		results := make(chan sparseBuildFlightResult, callers)
		b.StartTimer()
		for range callers {
			go func() {
				<-start
				generationID, report, err := fixture.sparse.builder.BuildDirtyLayer(context.Background(), request)
				results <- sparseBuildFlightResult{generationID: generationID, report: report, err: err}
			}()
		}
		close(start)
		select {
		case <-entered:
		case <-time.After(20 * time.Second):
			b.Fatal("dirty benchmark did not reach leader sampling")
		}
		generationID, _, adopted, err := fixture.sparse.store.BeginPayloadGenerationWithStatus(
			context.Background(),
			payloadRequestForBuild(BuildRequest{Identity: payloadIdentity}),
		)
		if err != nil || !adopted {
			close(release)
			b.Fatalf("observe dirty benchmark build: adopted=%t err=%v", adopted, err)
		}
		awaitSparseBuildFlightWaiters(b, fixture.sparse.store, generationID, int64(callers-1))
		close(release)
		physicalReports := 0
		for i := range callers {
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
		totalIdentitySamples += identitySamples.Load()
		totalBuildSamples += buildSamples.Load()
	}
	b.ReportMetric(float64(totalIdentitySamples)/float64(b.N), "preflight-samples/op")
	b.ReportMetric(float64(totalBuildSamples)/float64(b.N), "leader-samples/op")
	b.ReportMetric(1, "physical-builds/op")
}
