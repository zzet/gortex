package indexer

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/reach"
)

// The reach topology writer under a sparse generation build.
//
// One process-global gate serialises every reach reader against every topology
// mutation in the daemon. It protects records that are built over the base
// corpus and stamped on its nodes, and every consumer reads them through the
// base store itself — a routed view answers about a checkout the base index
// never saw, so its reader is sent to a live walk instead.
//
// A generation-pinned pass writes rows at a derived view generation. No record
// covers them and no reach reader visits them, so holding the writer for the
// pass's whole duration buys nothing and costs everything: the warmup tail,
// reconciliation, ref-view builds and every other coordinator queue behind a
// pass that runs for as long as its payload takes. The base pass is the other
// half of the pin — it does mutate what the records describe, and must keep
// the writer.

// indexWalkStage is the first stage indexCtxRaw reports. It lands inside
// IndexCtx's topology region, so a pass parked there is parked holding
// whatever IndexCtx took.
const indexWalkStage = "walking files"

// topologyPassArrivalTimeout bounds setup and planning before IndexCtx emits
// its first walk tick. It is deliberately much larger than the gate probe:
// under a full or race-enabled package run, Git diffing, parsing, and SQLite
// planning can be delayed without saying anything about topology ownership.
const topologyPassArrivalTimeout = 30 * time.Second

// topologyGateProbeWindow is how long the negative assertion waits before
// concluding a held gate really is held.
const topologyGateProbeWindow = 75 * time.Millisecond

// passParkReporter parks an indexing pass on its first walk tick and lets
// every later tick through.
type passParkReporter struct {
	parked     chan struct{}
	release    chan struct{}
	parkOnce   sync.Once
	resumeOnce sync.Once
}

func newPassParkReporter() *passParkReporter {
	return &passParkReporter{parked: make(chan struct{}), release: make(chan struct{})}
}

func (r *passParkReporter) Report(stage string, _, _ int) {
	if stage != indexWalkStage {
		return
	}
	r.parkOnce.Do(func() {
		close(r.parked)
		<-r.release
	})
}

func (r *passParkReporter) resume() {
	r.resumeOnce.Do(func() { close(r.release) })
}

// waitParked blocks until the pass reaches its walk stage, while reporting a
// build that exits early instead of misdiagnosing it as a progress timeout.
func (r *passParkReporter) waitParked(t *testing.T, ctx context.Context, done <-chan error) {
	t.Helper()
	select {
	case <-r.parked:
	case err := <-done:
		if err != nil {
			t.Fatalf("the indexing pass failed before its walk stage: %v", err)
		}
		t.Fatal("the indexing pass completed without reaching its walk stage")
	case <-ctx.Done():
		t.Fatalf("the indexing pass did not reach its walk stage within %s: %v",
			topologyPassArrivalTimeout, ctx.Err())
	}
}

// gateProbe is a second topology writer racing the parked pass for the
// process-global gate.
type gateProbe struct {
	acquired chan func(bool)
	done     bool
}

func probeTopologyWriter(store *store_sqlite.Store) *gateProbe {
	probe := &gateProbe{acquired: make(chan func(bool), 1)}
	go func() { probe.acquired <- reach.BeginTopologyMutation(store) }()
	return probe
}

// take reports whether the probe owns the gate within window, releasing it
// again as soon as it does. A probe that timed out is still queued on the
// gate, so every caller must take it once more before the test ends or the
// next test in the package inherits an owned writer.
func (p *gateProbe) take(window time.Duration) bool {
	if p.done {
		return true
	}
	select {
	case finish := <-p.acquired:
		finish(false)
		p.done = true
		return true
	case <-time.After(window):
		return false
	}
}

// topologyGateFixture is one repository at two committed states, with the
// first already indexed into the base corpus.
type topologyGateFixture struct {
	store      *store_sqlite.Store
	dir        string
	baseTree   string
	targetTree string
	commitOID  string
}

func newTopologyGateFixture(t *testing.T) *topologyGateFixture {
	t.Helper()
	builderIsolateGit(t)
	dir := builderTempDir(t, "repo")
	builderGit(t, dir, "init", "--initial-branch=main")
	builderWriteTree(t, dir, builderTreeA())
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "A")
	baseTree := builderGit(t, dir, "rev-parse", "HEAD^{tree}")

	store := builderOpenStore(t, "base")
	builderIndex(t, store, dir)

	builderWriteTree(t, dir, builderTreeB())
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "B")

	return &topologyGateFixture{
		store:      store,
		dir:        dir,
		baseTree:   baseTree,
		targetTree: builderGit(t, dir, "rev-parse", "HEAD^{tree}"),
		commitOID:  builderGit(t, dir, "rev-parse", "HEAD"),
	}
}

func (f *topologyGateFixture) commitLayerRequest() CommitLayerRequest {
	return CommitLayerRequest{
		Identity: GenerationIdentity{
			OwnerKind:           "dedicated_graph",
			GraphID:             "graph-topology-gate",
			LayerID:             "layer-" + f.targetTree,
			CheckoutID:          "checkout-topology-gate",
			ProvenanceCommitOID: f.commitOID,
		},
		Base:          f.store,
		RepoDir:       f.dir,
		BaseTreeOID:   f.baseTree,
		TargetTreeOID: f.targetTree,
		RootPath:      f.dir,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	}
}

// TestSparseGenerationBuildDoesNotHoldTheTopologyWriter is the work bound: a
// build pinned to a derived generation must leave the process-global reach
// topology writer free for the whole of its pass.
func TestSparseGenerationBuildDoesNotHoldTheTopologyWriter(t *testing.T) {
	fixture := newTopologyGateFixture(t)
	reporter := newPassParkReporter()
	defer reporter.resume()

	buildCtx, cancelBuild := context.WithTimeout(context.Background(), topologyPassArrivalTimeout)
	defer cancelBuild()
	buildErr := make(chan error, 1)
	go func() {
		_, _, err := builderNewBuilder(fixture.store).BuildCommitLayer(
			progress.WithReporter(buildCtx, reporter), fixture.commitLayerRequest())
		buildErr <- err
	}()
	reporter.waitParked(t, buildCtx, buildErr)

	probe := probeTopologyWriter(fixture.store)
	defer probe.take(batchTransitionTestTimeout)
	free := probe.take(batchTransitionTestTimeout)
	reporter.resume()

	if !free {
		t.Error("a sparse generation build held the reach topology writer across its whole pass")
	}
	select {
	case err := <-buildErr:
		if err != nil {
			t.Fatalf("BuildCommitLayer: %v", err)
		}
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("the sparse build did not finish after its pass was released")
	}
}

// TestSparseGenerationBuildDoesNotInvalidateBaseReach is the other half of the
// same property. Records describe the base corpus; a build that writes none of
// it must not retire them, and now that it runs outside the writer it must not
// bump the generation under a reader's feet either.
func TestSparseGenerationBuildDoesNotInvalidateBaseReach(t *testing.T) {
	for _, strategy := range []struct {
		name           string
		shadowMaxFiles string
		wantShadow     bool
	}{
		{name: "direct_sqlite", shadowMaxFiles: "0", wantShadow: false},
		{name: "bounded_shadow", shadowMaxFiles: "1000000", wantShadow: true},
	} {
		t.Run(strategy.name, func(t *testing.T) {
			t.Setenv("GORTEX_SHADOW_MAX_FILES", strategy.shadowMaxFiles)
			t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1073741824")
			fixture := newTopologyGateFixture(t)
			base := fixture.store.AtGeneration(0)
			baseNodes, baseEdges := base.NodeCount(), base.EdgeCount()
			if stats := reach.BuildIndex(base); stats.NodesIndexed == 0 {
				t.Fatal("base fixture produced no persisted reachability records")
			}

			core, logs := observer.New(zapcore.InfoLevel)
			builder := builderNewBuilder(fixture.store)
			builder.Logger = zap.New(core)
			before := reach.BuildCounter()
			generationID, report, err := builder.BuildCommitLayer(
				context.Background(), fixture.commitLayerRequest())
			if err != nil {
				t.Fatalf("BuildCommitLayer: %v", err)
			}
			if taken := observedShadowDecision(t, logs); taken != strategy.wantShadow {
				t.Fatalf("shadow_taken = %t, want %t", taken, strategy.wantShadow)
			}
			if after := reach.BuildCounter(); after != before {
				t.Errorf("a sparse generation build moved the reach build counter from %d to %d",
					before, after)
			}
			if nodes, edges := base.NodeCount(), base.EdgeCount(); nodes != baseNodes || edges != baseEdges {
				t.Errorf("base topology changed from %d/%d nodes/edges to %d/%d",
					baseNodes, baseEdges, nodes, edges)
			}

			generation := fixture.store.AtGeneration(generationID)
			if got := generation.NodeCount(); got == 0 || got != report.NodeCount {
				t.Errorf("generation nodes = %d, report = %d", got, report.NodeCount)
			}
			if got := generation.EdgeCount(); got == 0 || got != report.EdgeCount {
				t.Errorf("generation edges = %d, report = %d", got, report.EdgeCount)
			}
		})
	}
}

// TestBaseIndexHoldsTheTopologyWriter pins the half that must not move. A pass
// over the base corpus rewrites exactly what the reach records describe, so a
// second topology writer must not get in while it runs.
func TestBaseIndexHoldsTheTopologyWriter(t *testing.T) {
	fixture := newTopologyGateFixture(t)
	reporter := newPassParkReporter()
	defer reporter.resume()

	idx := New(fixture.store, builderRegistry(), config.Default().Index, zap.NewNop())
	defer idx.Close()
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)

	indexCtx, cancelIndex := context.WithTimeout(context.Background(), topologyPassArrivalTimeout)
	defer cancelIndex()
	indexErr := make(chan error, 1)
	go func() {
		_, err := idx.IndexCtx(progress.WithReporter(indexCtx, reporter), fixture.dir)
		indexErr <- err
	}()
	reporter.waitParked(t, indexCtx, indexErr)

	probe := probeTopologyWriter(fixture.store)
	defer probe.take(batchTransitionTestTimeout)
	crossed := probe.take(topologyGateProbeWindow)
	reporter.resume()

	if crossed {
		t.Error("a second topology writer crossed a live base index pass")
	}
	select {
	case err := <-indexErr:
		if err != nil {
			t.Fatalf("IndexCtx: %v", err)
		}
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("the base index did not finish after its pass was released")
	}
}
