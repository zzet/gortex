package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestImportPreambleKeepsLegacyAndUnsupportedEvidenceInTheLane(t *testing.T) {
	oldPaths := importInteractivePaths
	importInteractivePaths = 2
	t.Cleanup(func() { importInteractivePaths = oldPaths })
	for _, mode := range []string{"legacy base", "unsupported HEAD proof"} {
		t.Run(mode, func(t *testing.T) {
			var f *coordinatorFixture
			if mode == "legacy base" {
				f = newCoordinatorFixture(t)
			} else {
				f = newCommittedBaseFixture(t).coordinatorFixture
			}
			c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
			if out := c.reconcile(t.Context()); out.Err != nil {
				t.Fatal(out.Err)
			}
			if mode == "unsupported HEAD proof" {
				if err := os.Mkdir(filepath.Join(f.primary, ".git", "reftable"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 4; i++ {
				builderWriteFile(t, f.worktree, fmt.Sprintf("fallback_%d.go", i), fmt.Sprintf("package fixture\nfunc Fallback%d() {}\n", i))
			}
			sample, err := c.sampler.Sample(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			gate := NewViewBuildGate()
			gate.Open()
			release, err := gate.Acquire(t.Context(), ViewBuildBackground)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			detached := false
			lane := &importBuildLane{gate: gate, detach: func() bool { detached = true; return true }}
			ctx := context.WithValue(t.Context(), importBuildLaneKey{}, lane)
			finish, err := c.prepareImportPreamble(ctx, sample, f.route())
			if err != nil {
				t.Fatal(err)
			}
			finish()
			if detached || !gate.Stats().Active || lane.preparationRelease != nil {
				t.Fatal("unproven preamble escaped ordinary yieldable admission")
			}
		})
	}
}

func TestImportPreambleRetainsAncestryAndRefusesMovedInputs(t *testing.T) {
	oldPaths := importInteractivePaths
	importInteractivePaths = 2
	t.Cleanup(func() { importInteractivePaths = oldPaths })
	for _, change := range []string{"route", "correction", "HEAD", "stop"} {
		t.Run(change, func(t *testing.T) {
			f := newCommittedBaseFixture(t)
			c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
			if out := c.reconcile(t.Context()); out.Err != nil {
				t.Fatal(out.Err)
			}
			requireImportReadSetProof(t, c)
			for i := 0; i < 4; i++ {
				builderWriteFile(t, f.worktree, fmt.Sprintf("preamble_%d.go", i), fmt.Sprintf("package fixture\nfunc Preamble%d() {}\n", i))
			}
			sample, err := c.sampler.Sample(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			route := f.route()
			generationsBefore := len(f.generations())
			heldBefore := c.leases.Held()
			gate := NewViewBuildGate()
			gate.Open()
			release, err := gate.Acquire(t.Context(), ViewBuildBackground)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { release() }()
			lane := &importBuildLane{
				gate:   gate,
				detach: func() bool { release(); release = func() {}; return true },
				resume: func(ctx context.Context, _ bool) (context.Context, error) {
					next, err := gate.Acquire(ctx, ViewBuildBackground)
					if err == nil {
						release = next
					}
					return ctx, err
				},
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = context.WithValue(ctx, importBuildLaneKey{}, lane)
			finish, err := c.prepareImportPreamble(ctx, sample, route)
			if err != nil {
				t.Fatal(err)
			}
			defer finish()
			if !lane.detached || gate.Stats().Active {
				t.Fatal("read-only preamble did not release the physical lane")
			}
			if f.route() != route || len(f.generations()) != generationsBefore {
				t.Fatal("read-only preamble changed the catalog or published payload")
			}
			commit, found := f.generation(route.CommitGenerationID)
			if !found || !c.leases.InUse(commit.GenerationID) || !c.leases.InUse(commit.BaseGenerationID) {
				t.Fatal("read-only preamble did not retain the complete immutable ancestry")
			}
			foreground, err := gate.Acquire(t.Context(), ViewBuildInteractive)
			if err != nil {
				t.Fatal(err)
			}
			foreground()
			want := ErrDirtySnapshotChanged
			switch change {
			case "route":
				want = errRouteMoved
				if err := f.catalog.FlipCheckoutRoute(t.Context(), store_sqlite.FlipCheckoutRouteRequest{
					CheckoutID: f.checkoutID, ExpectedRouteEpoch: route.RouteEpoch, GraphID: route.GraphID,
					CommitGenerationID: route.CommitGenerationID, DirtyGenerationID: route.DirtyGenerationID, State: route.State,
				}); err != nil {
					t.Fatal(err)
				}
			case "correction":
				rows := f.store.AtGeneration(commit.BaseGenerationID).GetFileNodes(builderRepoPrefix + "/core.go")
				if len(rows) == 0 {
					t.Fatal("immutable ancestor fixture has no actual file nodes")
				}
				if rows[0].Meta == nil {
					rows[0].Meta = map[string]any{}
				}
				rows[0].Meta["preamble_epoch_fixture"] = "corrected"
				epochBefore := f.store.GenerationCorrectionEpoch(commit.BaseGenerationID)
				correction, err := f.store.BeginDerivedCorrection(t.Context(), store_sqlite.DerivedCorrectionRequest{
					GenerationID: commit.BaseGenerationID, Pass: "preamble_epoch_fixture", ToVersion: 1,
					EdgeKinds: []graph.EdgeKind{graph.EdgeReadsEnv},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := correction.ReplaceSourceEdges(t.Context(), nil, nil, []*graph.Node{rows[0]}); err != nil {
					t.Fatal(err)
				}
				if _, err := correction.Finish(t.Context()); err != nil {
					t.Fatal(err)
				}
				if f.store.GenerationCorrectionEpoch(commit.BaseGenerationID) == epochBefore {
					t.Fatal("fixture performed no actual ancestor correction")
				}
			case "HEAD":
				builderGit(t, f.worktree, "add", "preamble_0.go")
				builderGit(t, f.worktree, "commit", "-m", "move HEAD during preamble")
			case "stop":
				want = context.Canceled
				cancel()
			}
			afterChange := f.route()
			if _, err := lane.reenter(ctx, false); !errors.Is(err, want) {
				t.Fatalf("%s reentry = %v, want refusal %v", change, err, want)
			}
			if f.route() != afterChange || len(f.generations()) != generationsBefore {
				t.Fatal("refused preamble mutated the route or published a generation")
			}
			finish()
			finish()
			if c.leases.Held() != heldBefore {
				t.Fatalf("refused preamble leaked ancestry: before=%d after=%d", heldBefore, c.leases.Held())
			}
			slotCtx, stopSlot := context.WithTimeout(t.Context(), time.Second)
			defer stopSlot()
			slot, err := gate.acquireImportPreparation(slotCtx)
			if err != nil {
				t.Fatal(err)
			}
			slot()
		})
	}
}
