package indexer

import (
	"context"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graphview"
)

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
