package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

func TestImportPreparationSlotCancellationReleasesCapacity(t *testing.T) {
	gate := NewViewBuildGate()
	release, err := gate.acquireImportPreparation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := gate.acquireImportPreparation(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("occupied preparation slot returned %v", err)
	}
	release()
	release()
	release, err = gate.acquireImportPreparation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestRefusedImportReentersAndKeepsSparseFallbackInTheLane(t *testing.T) {
	for _, mode := range []string{"refused", "unconfirmable", "nil_gate"} {
		t.Run(mode, func(t *testing.T) {
			f := newCommittedBaseFixture(t)
			c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
			out := c.reconcile(t.Context())
			if out.Err != nil {
				t.Fatal(out.Err)
			}
			base, closeBase, err := c.generationLayerReader(t.Context(), out.CommitGenerationID)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBase()
			builderWriteFile(t, f.worktree, "fallback.fk", "one private import")
			target, err := source.NewFilesystemSource(f.worktree)
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			before, err := c.sampler.Sample(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			b := builderNewBuilder(f.store)
			ext := &toggleExtractor{fail: mode == "refused", funcs: []string{"RestoredAfterRefusal"}}
			b.Registry.Register(ext)
			gate := NewViewBuildGate()
			gate.Open()
			release, err := gate.Acquire(t.Context(), ViewBuildBackground)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { release() }()
			ctx, y := armBackgroundLaneYield(t.Context(), gate, 0)
			defer func() { y.close() }()
			detaches, prepublish := 0, 0
			lane := &importBuildLane{
				gate: gate,
				detach: func() bool {
					detaches++
					if !y.commit() {
						return false
					}
					release()
					release = func() {}
					return true
				},
				resume: func(ctx context.Context, yieldable bool) (context.Context, error) {
					next, err := gate.Acquire(ctx, ViewBuildBackground)
					if err != nil {
						return ctx, err
					}
					release = next
					ext.setFail(false)
					return ctx, nil
				},
				arm: func(ctx context.Context) error {
					var err error
					y, err = rearmBackgroundLaneYield(ctx, gate, y)
					return err
				},
			}
			if mode == "nil_gate" {
				lane.gate = nil
			}
			ctx = context.WithValue(ctx, importBuildLaneKey{}, lane)
			identity := StampDirtyLayerIdentity(c.dirtyIdentity(f.graphID, out.CommitGenerationID), before)
			generation, _, err := b.buildWorkingTreeLayer(ctx, BuildRequest{
				Identity: identity, Base: base, Target: target, RootPath: f.worktree,
				RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
				Changes:     []LayerPathChange{{Path: "fallback.fk", Kind: LayerPathAdded}},
				importBatch: true, importReadSetReady: func(context.Context) bool { return true },
				// A sparse fallback must ignore the off-lane short-proof protocol.
				prePublishRecheck: func(context.Context) (bool, error) { return false, nil },
				PrePublish: func(context.Context, int64) error {
					prepublish++
					wantActive := mode != "unconfirmable" || prepublish == 4
					if gate.Stats().Active != wantActive {
						return fmt.Errorf("fence %d lane active=%v, want %v", prepublish, gate.Stats().Active, wantActive)
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			wantDetaches, wantFences := 1, 1
			if mode == "unconfirmable" {
				wantDetaches, wantFences = 4, 4
			}
			if mode == "nil_gate" {
				wantDetaches = 0
			}
			if detaches != wantDetaches || prepublish != wantFences {
				t.Fatalf("%s detaches=%d fences=%d, want %d/%d", mode, detaches, prepublish, wantDetaches, wantFences)
			}
			row, found := f.generation(generation)
			if !found || row.State != store_sqlite.ViewGenerationReady {
				t.Fatalf("fallback not published: %+v", row)
			}

		})
	}
}

func TestDetachedImportRejectsMovedInputsAndReleasesOnStop(t *testing.T) {
	oldPaths := importInteractivePaths
	importInteractivePaths = 2
	t.Cleanup(func() { importInteractivePaths = oldPaths })
	for _, change := range []string{"file", "head", "stop"} {
		t.Run(change, func(t *testing.T) {
			f := newCommittedBaseFixture(t).coordinatorFixture
			gate := NewViewBuildGate()
			gate.Open()
			outcomes := make(chan CheckoutCycle, 64)
			entered, unblock := make(chan struct{}), make(chan struct{})
			var importing atomic.Bool
			var once sync.Once
			var cycleContext context.Context
			c := f.coordinator(t, CheckoutCoordinatorConfig{
				Gate: gate, cycleDone: func(out CheckoutCycle) { outcomes <- out },
				dirtyBarrier: func() {
					if importing.Load() {
						once.Do(func() {
							close(entered)
							select {
							case <-unblock:
							case <-cycleContext.Done():
							}
						})
					}
				},
			})
			c.cycleMu.Lock()
			c.cycleBarrier = func(ctx context.Context) { cycleContext = ctx }
			c.cycleMu.Unlock()
			c.compaction.mu.Lock()
			c.compaction.quiet = -1
			c.compaction.mu.Unlock()
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			await := func() CheckoutCycle {
				select {
				case out := <-outcomes:
					return out
				case <-ctx.Done():
					t.Fatal("coordinator did not report its cycle")
					return CheckoutCycle{}
				}
			}
			c.Signal("warmup")
			if out := await(); out.Err != nil || out.DirtyGenerationID == 0 {
				t.Fatalf("warmup: %+v", out)
			}
			for i := 0; i < 4; i++ {
				builderWriteFile(t, f.worktree, fmt.Sprintf("import_%d.go", i), fmt.Sprintf("package fixture\nfunc Import%d() {}\n", i))
			}
			importing.Store(true)
			c.Signal("import")
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("import never reached private payload")
			}
			defer close(unblock)
			if gate.Stats().Active {
				t.Fatal("private payload kept the physical build lane")
			}
			var unfinished []int64
			for _, row := range f.generations() {
				if row.GenerationKind == DirtyLayerGenerationKind && row.State == store_sqlite.ViewGenerationBuilding {
					unfinished = append(unfinished, row.GenerationID)
				}
			}
			if len(unfinished) != 1 {
				t.Fatalf("private building generations: %v", unfinished)
			}
			for _, routed := range routedStack(t, f, c) {
				if routed == unfinished[0] {
					t.Fatal("an unfinished payload became visible")
				}
			}
			release, err := gate.Acquire(ctx, ViewBuildInteractive)
			if err != nil {
				t.Fatal(err)
			}
			release()
			switch change {
			case "file":
				builderWriteFile(t, f.worktree, "import_0.go", "package fixture\nfunc ChangedAfterParse() {}\n")
			case "head":
				builderGit(t, f.worktree, "add", "import_0.go")
				builderGit(t, f.worktree, "commit", "-m", "move head while payload is private")
			case "stop":
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if change != "stop" {
				unblock <- struct{}{}
			}
			out := await()
			if out.DirtyBuilt || out.Err == nil && !out.Rescheduled {
				t.Fatalf("invalid private payload was accepted: %+v", out)
			}
			row, found := f.generation(unfinished[0])
			if !found || row.State == store_sqlite.ViewGenerationReady {
				t.Fatalf("invalid payload was published: %+v found=%v", row, found)
			}
			releaseSlot, err := gate.acquireImportPreparation(ctx)
			if err != nil {
				t.Fatalf("preparation capacity leaked: %v", err)
			}
			releaseSlot()
		})
	}
}

func TestImportLaneDetachDoesNotResurrectAnInteractiveCancellation(t *testing.T) {
	for _, fireFirst := range []bool{true, false} {
		gate := NewViewBuildGate()
		gate.Open()
		release, err := gate.Acquire(t.Context(), ViewBuildBackground)
		if err != nil {
			t.Fatal(err)
		}
		ctx, yield := armBackgroundLaneYield(t.Context(), gate, 0)
		if fireFirst {
			yield.fire()
		}
		detached := yield.commit()
		yield.fire()
		if fireFirst && (detached || ctx.Err() == nil) {
			t.Fatal("a canceled attempt was resurrected by the handoff")
		}
		if !fireFirst && (!detached || ctx.Err() != nil) {
			t.Fatal("detached preparation was canceled by late interactive demand")
		}
		release()
		yield.close()
	}
}

func TestImportPreparationRequiresCurrentImmutableAncestors(t *testing.T) {
	f := newCommittedBaseFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	out := c.reconcile(t.Context())
	if out.Err != nil || out.CommitGenerationID <= 0 {
		t.Fatalf("initial route: %+v", out)
	}
	base, closeBase, err := c.generationLayerReader(t.Context(), out.CommitGenerationID)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBase()
	b := builderNewBuilder(f.store)
	req := BuildRequest{
		Base: base, importBatch: true,
		Changes:            []LayerPathChange{{Path: "import.go", Kind: LayerPathAdded}},
		importReadSetReady: func(context.Context) bool { return true },
	}
	epochs, eligible, err := b.importPreparationEpochs(t.Context(), req)
	if err != nil || !eligible {
		t.Fatalf("current immutable ancestry was refused: eligible=%v err=%v", eligible, err)
	}
	stack := base.(commitLayerBase).stack
	generation := stack[0]
	rows := f.store.AtGeneration(generation).GetFileNodes(builderRepoPrefix + "/core.go")
	if len(rows) == 0 {
		t.Fatal("base file rows missing")
	}
	node := rows[0]
	if node.Meta == nil {
		node.Meta = map[string]any{}
	}
	node.Meta["import_epoch_fixture"] = "corrected"
	correction, err := f.store.BeginDerivedCorrection(t.Context(), store_sqlite.DerivedCorrectionRequest{
		GenerationID: generation, Pass: "import_epoch_fixture", ToVersion: 1,
		EdgeKinds: []graph.EdgeKind{graph.EdgeReadsEnv},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := correction.ReplaceSourceEdges(t.Context(), nil, nil, []*graph.Node{node}); err != nil {
		t.Fatal(err)
	}
	if _, err := correction.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := b.checkImportPreparationEpochs(epochs); !errors.Is(err, ErrDirtySnapshotChanged) {
		t.Fatalf("changed ancestry epoch was accepted: %v", err)
	}
	req.Base = commitLayerBase{Reader: f.store.AtGeneration(0)}
	if _, eligible, err := b.importPreparationEpochs(t.Context(), req); err != nil || eligible {
		t.Fatalf("mutable corpus escaped the lane: eligible=%v err=%v", eligible, err)
	}
	req.Base = base
	req.importReadSetReady = func(context.Context) bool { return false }
	if _, eligible, err := b.importPreparationEpochs(t.Context(), req); err != nil || eligible {
		t.Fatalf("unconfirmable filesystem escaped the lane: eligible=%v err=%v", eligible, err)
	}
}

// Clear/flip can leave a protected held lane. An ordinary fallback must renew
// cancellation in the SAME context still retained by its caller.
func TestImportLaneHeldFallbackRenewsTheOriginalContext(t *testing.T) {
	for _, commitFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit_first_%v", commitFirst), func(t *testing.T) {
			gate := NewViewBuildGate()
			gate.Open()
			release, err := gate.Acquire(t.Context(), ViewBuildBackground)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			ctx, original := armBackgroundLaneYield(t.Context(), gate, 0)
			defer original.close()
			current := original
			lane := &importBuildLane{arm: func(ctx context.Context) error {
				var err error
				current, err = rearmBackgroundLaneYield(ctx, gate, current)
				return err
			}}
			if !original.commit() {
				t.Fatal("initial protected operation failed")
			}
			returned, err := lane.reenter(ctx, true)
			if err != nil || returned != ctx || current == original {
				t.Fatalf("fallback failed to renew same context: %v", err)
			}
			if commitFirst {
				reachBuildCommitPoint(ctx)
			}
			current.fire()
			if commitFirst {
				if ctx.Err() != nil {
					t.Fatal("retained commit point did not protect newest yield")
				}
			} else if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal("ordinary fallback cancellation did not reach retained caller context")
			}
		})
	}
}

func TestImportPreparationWaitDoesNotHoldTheBuildLane(t *testing.T) {
	gate := NewViewBuildGate()
	gate.Open()
	first, err := gate.acquireImportPreparation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	release, err := gate.Acquire(t.Context(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	detached := make(chan struct{})
	lane := &importBuildLane{gate: gate, detach: func() bool { release(); close(detached); return true }}
	done := make(chan error, 1)
	go func() { _, err := lane.begin(ctx); done <- err }()
	<-detached
	foreground, err := gate.Acquire(t.Context(), ViewBuildInteractive)
	if err != nil {
		t.Fatal(err)
	}
	foreground()
	select {
	case err := <-done:
		t.Fatalf("occupied preparation slot admitted: %v", err)
	default:
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled slot wait: %v", err)
	}
	first()
	reused, err := gate.acquireImportPreparation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reused()
}
