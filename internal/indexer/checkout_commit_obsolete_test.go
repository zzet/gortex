package indexer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graphview"
)

func TestCommitTargetCheckReusesEvidenceAndRechecksRefChanges(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()
	target := builderGit(t, f.worktree, "rev-parse", "HEAD^{tree}")
	trace := filepath.Join(t.TempDir(), "git-trace.json")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	// Some platforms and Git layouts cannot provide stable file evidence.
	// They must keep asking Git rather than taking the metadata fast path.
	metadataUsable := c.sampler.CaptureHeadEvidence().Usable()
	check := c.commitTargetCheck(target)
	if err := check(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(trace)
	if err != nil || len(before) == 0 {
		t.Fatalf("initial check did not run Git: %v", err)
	}
	for range 64 {
		if err := check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if metadataUsable && !bytes.Equal(before, after) {
		t.Fatal("unchanged checks spawned Git subprocesses despite usable evidence")
	}
	if !metadataUsable && bytes.Equal(before, after) {
		t.Fatal("unsupported metadata evidence did not fall back to Git")
	}
	for _, args := range [][]string{{"pack-refs", "--all"}, {"commit", "--allow-empty", "-m", "same tree"}} {
		builderGit(t, f.worktree, args...)
		if err := check(ctx); err != nil {
			t.Fatalf("same-tree metadata change aborted build: %v", err)
		}
		// After revalidation, supported evidence must regain the fast path.
		// Unsupported evidence must still take the authoritative Git path.
		before, err = os.ReadFile(trace)
		if err != nil {
			t.Fatal(err)
		}
		if err := check(ctx); err != nil {
			t.Fatal(err)
		}
		after, err = os.ReadFile(trace)
		if err != nil {
			t.Fatal(err)
		}
		if metadataUsable != bytes.Equal(before, after) {
			t.Fatalf("ref revalidation did not preserve evidence behavior (usable=%v)", metadataUsable)
		}
	}
	// An unavailable metadata source must ask Git on every call, rather
	// than turning the first successful read into unconditional permission.
	c.sampler = nil
	uncached := c.commitTargetCheck(target)
	if err := uncached(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ = os.ReadFile(trace)
	if err := uncached(ctx); err != nil {
		t.Fatal(err)
	}
	after, err = os.ReadFile(trace)
	if err != nil || bytes.Equal(before, after) {
		t.Fatalf("unusable evidence did not fall back to Git: %v", err)
	}
	builderWriteFile(t, f.worktree, "new.go", "package fixture\nfunc NewTree() {}\n")
	builderGit(t, f.worktree, "add", "-A")
	builderGit(t, f.worktree, "commit", "-m", "new tree")
	if err := check(ctx); !errors.Is(err, errCommitTargetMoved) {
		t.Fatalf("changed tree was not detected: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := c.commitTargetCheck(target)(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("check ignored cancellation: %v", err)
	}
}

// Switch a real checkout at the first index-start boundary. The obsolete
// committed source remains readable, but it must not drain an entire payload
// before the coordinator starts satisfying demand for the new HEAD.
func TestCoordinatorSkipsObsoleteCommitBeforeDrain(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	initial := builderGit(t, f.worktree, "rev-parse", "HEAD")
	initialTree := builderGit(t, f.worktree, "rev-parse", "HEAD^{tree}")
	builderWriteFile(t, f.worktree, "after.go", "package fixture\nfunc AfterSwitch() {}\n")
	builderGit(t, f.worktree, "add", "-A")
	builderGit(t, f.worktree, "commit", "-m", "final target")
	final := builderGit(t, f.worktree, "rev-parse", "HEAD")
	finalTree := builderGit(t, f.worktree, "rev-parse", "HEAD^{tree}")
	builderGit(t, f.worktree, "checkout", "--detach", initial)

	core, logs := observer.New(zap.InfoLevel)
	var switched sync.Once
	builder := builderNewBuilder(f.store)
	builder.Logger = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Message == "indexer: full index started" {
			switched.Do(func() { builderGit(t, f.worktree, "checkout", "--detach", final) })
		}
		return nil
	}))
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{Builder: builder})
	out := c.reconcile(context.Background())
	if out.Err != nil || !out.Rescheduled || out.rescheduledBy != stallReasonHeadMoved || out.CommitBuilt || out.DirtyBuilt {
		t.Fatalf("obsolete build outcome: %+v", out)
	}
	if logs.FilterMessage("indexer: drain start (shadow → disk)").Len() != 0 {
		t.Fatal("obsolete commit drained before rescheduling")
	}
	if logs.FilterMessage("indexer: resolve pass").Len() != 0 {
		t.Fatal("obsolete commit resolved references before rescheduling")
	}
	for _, row := range f.generations() {
		if row.TreeOID == initialTree {
			if servableGeneration(row.State) || f.store.AtGeneration(row.GenerationID).NodeCount() != 0 {
				t.Fatalf("obsolete generation has a published or persisted payload: %+v", row)
			}
		}
	}
	if graphview.RouteReady(f.route()) || len(c.signal) == 0 {
		t.Fatal("obsolete cycle became ready or failed to schedule the current HEAD")
	}
	settled := coordinatorReconcile(t, c)
	row, found := f.generation(settled.CommitGenerationID)
	if !found || row.TreeOID != finalTree || !graphview.RouteReady(f.route()) {
		t.Fatalf("next cycle did not serve the final tree: %+v %+v", settled, row)
	}
}

func TestCoordinatorKeepsCommitBuildAcrossSameTreeHEADChange(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	core, logs := observer.New(zap.InfoLevel)
	var switched sync.Once
	builder := builderNewBuilder(f.store)
	builder.Logger = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Message == "indexer: full index started" {
			switched.Do(func() { builderGit(t, f.worktree, "commit", "--allow-empty", "-m", "same tree") })
		}
		return nil
	}))
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{Builder: builder})
	out := coordinatorReconcile(t, c)
	if out.Rescheduled || !out.CommitBuilt || !out.DirtyBuilt || !graphview.RouteReady(f.route()) {
		t.Fatalf("same-tree commit unnecessarily abandoned the build: %+v", out)
	}
	if logs.FilterMessage("indexer: drain start (shadow → disk)").Len() != 1 {
		t.Fatal("same-tree commit did not persist its payload exactly once")
	}
}

func TestCommitBuildForInactiveTreeStillPublishes(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	targetTree := builderGit(t, f.worktree, "rev-parse", "HEAD^{tree}")
	builderGit(t, f.worktree, "checkout", "--detach", "HEAD~1")
	// A caller explicitly requesting an immutable snapshot does not opt in
	// to the coordinator's demand check, even when RootPath names a checkout.
	id, report, err := builderNewBuilder(f.store).BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: f.graphID,
			LayerID: "inactive-target", CheckoutID: f.checkoutID},
		Base: f.store, RepoDir: f.worktree, RootPath: f.worktree,
		BaseTreeOID: f.treeA, TargetTreeOID: targetTree,
		RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	row, found := f.generation(id)
	if !found || !servableGeneration(row.State) || row.TreeOID != targetTree || report.NodeCount == 0 {
		t.Fatalf("inactive snapshot was not published: %+v %+v", row, report)
	}
}

func TestCommitDispatchBoundsGitFallbackWithoutHeadEvidence(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	c.sampler = nil // Windows and unsupported Git layouts have the same evidence.
	target := builderGit(t, f.worktree, "rev-parse", "HEAD^{tree}")
	trace := filepath.Join(t.TempDir(), "git-trace.json")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	check := c.commitTargetCheck(target)
	now := time.Now()
	dispatch := rateLimitedDispatchCheck(check, func() time.Time { return now })
	if err := dispatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(trace)
	if err != nil || len(before) == 0 {
		t.Fatalf("fallback did not sample Git: %v", err)
	}
	for range 2048 {
		if err := dispatch(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(trace)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("dispatch repeated Git within one second: %v", err)
	}
	now = now.Add(time.Second)
	if err := dispatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err = os.ReadFile(trace)
	if err != nil || bytes.Equal(before, after) {
		t.Fatalf("dispatch never refreshed fallback: %v", err)
	}
	builderWriteFile(t, f.worktree, "new.go", "package fixture\nfunc NewTree() {}\n")
	builderGit(t, f.worktree, "add", "-A")
	builderGit(t, f.worktree, "commit", "-m", "new tree")
	// An immediate phase boundary bypasses the dispatch throttle.
	if err := check(t.Context()); !errors.Is(err, errCommitTargetMoved) {
		t.Fatalf("phase check missed HEAD move: %v", err)
	}
	now = now.Add(time.Second)
	if err := dispatch(t.Context()); !errors.Is(err, errCommitTargetMoved) {
		t.Fatalf("next dispatch missed HEAD move: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := dispatch(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("throttle ignored cancellation: %v", err)
	}
}
