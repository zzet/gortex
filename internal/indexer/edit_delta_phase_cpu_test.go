package indexer

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graphview"
)

// Every delta phase carries the process CPU and the store readers' wait it
// spanned, beside its wall time, so a phase's work is told from its waits.
func TestEveryDeltaPhaseRecordsProcessCPUAndReaderWait(t *testing.T) {
	fx := newDirtyChainFixture(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()
	row, found := fx.f.generation(fx.d1)
	if !found {
		t.Fatal("no parent generation")
	}
	materializer := graphview.Materializer{Store: fx.f.store, Catalog: fx.f.catalog, Leases: fx.f.leases, Logger: zap.NewNop()}
	view, err := materializer.MaterializeRefView(ctx, row.GraphID, fx.d1)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	builderWriteFile(t, fx.f.worktree, "island.go", chainIslandEdit)
	recordLastEditDelta(nil)
	if _, _, err := fx.c.builder.BuildDirtyLayer(ctx, DirtyLayerRequest{
		Identity: fx.c.dirtyIdentity(fx.f.graphID, fx.d1), Base: commitLayerBase{Reader: view.Reader, stack: []int64{9500001, 9500002}},
		CheckoutRoot: fx.f.worktree, RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
	}); err != nil {
		t.Fatal(err)
	}
	rep := LastEditDeltaReport()
	if rep == nil || len(rep.Phases) == 0 {
		t.Fatalf("no delta phases: %+v", rep)
	}
	var cpu time.Duration
	for _, phase := range rep.Phases {
		c, okCPU := rep.PhaseCPU[phase.Name]
		_, okWait := rep.PhaseReaderWait[phase.Name]
		_, okStore := rep.PhaseStoreWaits[phase.Name]
		_, okSched := rep.PhaseSchedWait[phase.Name]
		_, okSchedN := rep.PhaseSchedWaits[phase.Name]
		if !okCPU || !okWait || !okStore || !okSched || !okSchedN {
			t.Fatalf("phase %q has no process CPU or reader wait (cpu %v, wait %v)", phase.Name, rep.PhaseCPU, rep.PhaseReaderWait)
		}
		cpu += c
	}
	// The fixture's store installed the sleep gauge (it is on unless
	// GORTEX_SQLITE_SLEEP_GAUGE=0), and the delta reports it.
	if os.Getenv("GORTEX_SQLITE_SLEEP_GAUGE") == "" && !rep.SleepGaugeInstalled {
		t.Fatal("the delta does not report the sleep gauge installed")
	}
	if runtime.GOOS != "windows" && cpu <= 0 {
		t.Fatalf("the delta's phases spanned no process CPU: %v", rep.PhaseCPU)
	}
}

// The declaration-diff measurement runs inside a delta only when asked for.
func TestTheDeclarationDiffMeasurementIsOptIn(t *testing.T) {
	previous := editDeltaMeasureDeclarations
	t.Cleanup(func() { editDeltaMeasureDeclarations = previous })
	for _, on := range []bool{false, true} {
		editDeltaMeasureDeclarations = on
		fx := newDirtyChainFixture(t, CheckoutCoordinatorConfig{})
		ctx := context.Background()
		row, found := fx.f.generation(fx.d1)
		if !found {
			t.Fatal("no parent generation")
		}
		materializer := graphview.Materializer{Store: fx.f.store, Catalog: fx.f.catalog, Leases: fx.f.leases, Logger: zap.NewNop()}
		view, err := materializer.MaterializeRefView(ctx, row.GraphID, fx.d1)
		if err != nil {
			t.Fatal(err)
		}
		builderWriteFile(t, fx.f.worktree, "island.go", chainIslandEdit)
		recordLastEditDelta(nil)
		if _, _, err := fx.c.builder.BuildDirtyLayer(ctx, DirtyLayerRequest{
			Identity: fx.c.dirtyIdentity(fx.f.graphID, fx.d1), Base: commitLayerBase{Reader: view.Reader, stack: []int64{9500001, 9500002}},
			CheckoutRoot: fx.f.worktree, RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
		}); err != nil {
			t.Fatal(err)
		}
		view.Close()
		rep := LastEditDeltaReport()
		if rep == nil {
			t.Fatal("no delta report")
		}
		measured := false
		for _, phase := range rep.Phases {
			measured = measured || phase.Name == "declaration_diff"
		}
		if measured != on || (rep.DeclarationDiff.Files > 0) != on {
			t.Fatalf("measurement on=%v: phase %v, diff %+v", on, measured, rep.DeclarationDiff)
		}
	}
}
