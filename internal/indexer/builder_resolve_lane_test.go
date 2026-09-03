package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// One checkout's build against another's.
//
// Every sparse generation build resolves, infers overrides, detects clones and
// synthesises capability, test and external-call edges, and each of those
// passes takes the resolver-coordination lane of the graph it runs over. When
// that lane belonged to the whole database, one checkout's commit layer held
// it for the length of its own O(graph) passes and every other checkout —
// including a dirty refresh polling every fifteen seconds, which is editor
// latency — waited for the whole thing.
//
// A build reads and writes only the generation its handle is pinned to, so the
// lane is per generation. These cases pin the three properties that follow: a
// dirty layer never queues behind another checkout's commit lane, the commit
// build it overtook still completes, and the base corpus keeps the one lane
// every base mutation still serialises on.

// resolveLaneBuildTimeout is a ceiling, not a schedule: a build that has to
// wait for another checkout's lane never finishes at all, so any value that
// clears a real build on a loaded machine separates the two outcomes.
const resolveLaneBuildTimeout = 90 * time.Second

// resolveLaneFixture is one repository indexed at tree A, a second checkout of
// the same content whose working tree has diverged, and the store both build
// families write their generations into.
type resolveLaneFixture struct {
	*topologyGateFixture

	// dirtyRoot is the second checkout: tree A committed, working tree moved.
	dirtyRoot string
}

func newResolveLaneFixture(t *testing.T) *resolveLaneFixture {
	t.Helper()
	base := newTopologyGateFixture(t)

	dirtyRoot := builderTempDir(t, "checkout-b")
	builderGit(t, dirtyRoot, "init", "--initial-branch=main")
	builderWriteTree(t, dirtyRoot, builderTreeA())
	builderGit(t, dirtyRoot, "add", "-A")
	builderGit(t, dirtyRoot, "commit", "-m", "A")
	builderWriteFile(t, dirtyRoot, "helper.go", `package fixture

func Helper() {
	// reworked in the working tree
}
`)
	if err := os.Remove(filepath.Join(dirtyRoot, "gone.go")); err != nil {
		t.Fatalf("remove gone.go: %v", err)
	}
	return &resolveLaneFixture{topologyGateFixture: base, dirtyRoot: dirtyRoot}
}

func (f *resolveLaneFixture) dirtyLayerRequest() DirtyLayerRequest {
	return DirtyLayerRequest{
		Identity: GenerationIdentity{
			OwnerKind:  "dedicated_graph",
			GraphID:    "graph-checkout-b",
			LayerID:    "layer-checkout-b-worktree",
			CheckoutID: "checkout-b",
		},
		Base:         f.store,
		CheckoutRoot: f.dirtyRoot,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
	}
}

// holdCommitLane mints the generation another checkout's commit build would be
// filling and takes its resolver lane, which is what that build holds across
// each of its whole-graph passes. Its identity shares nothing with any build
// these cases run, so no build can adopt it and finish it out from under the
// test.
func holdCommitLane(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	generationID, handle, err := store.BeginPayloadGeneration(
		context.Background(), store_sqlite.PayloadGenerationRequest{
			OwnerKind:      "dedicated_graph",
			GraphID:        "graph-checkout-a",
			LayerID:        "layer-checkout-a-commit",
			CheckoutID:     "checkout-a",
			GenerationKind: "commit",
			TreeOID:        "tree-checkout-a",
			ConfigHash:     "config-checkout-a",
		})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	lane := handle.ResolveMutex()
	lane.Lock()
	t.Cleanup(func() {
		lane.Unlock()
		_ = store.Catalog().SetViewGenerationState(context.Background(), generationID,
			store_sqlite.ViewGenerationFailed, store_sqlite.ViewGenerationBuilding)
	})
}

// TestDirtyLayerBuildDoesNotWaitForAnotherCheckoutsCommitLane is the work
// bound. A dirty refresh is editor latency; it must not be priced at another
// checkout's commit build.
func TestDirtyLayerBuildDoesNotWaitForAnotherCheckoutsCommitLane(t *testing.T) {
	fixture := newResolveLaneFixture(t)
	holdCommitLane(t, fixture.store)

	built := make(chan error, 1)
	go func() {
		_, _, err := builderNewBuilder(fixture.store).BuildDirtyLayer(
			context.Background(), fixture.dirtyLayerRequest())
		built <- err
	}()

	select {
	case err := <-built:
		if err != nil {
			t.Fatalf("BuildDirtyLayer: %v", err)
		}
	case <-time.After(resolveLaneBuildTimeout):
		t.Fatal("a dirty layer build waited behind another checkout's commit lane")
	}
}

// TestOvertakenCommitLayerBuildStillCompletes is the other half: overtaking
// must not cost the build that was overtaken its generation. Both builds run
// while a third checkout's commit lane is held, so neither can be finishing
// because the contention went away.
func TestOvertakenCommitLayerBuildStillCompletes(t *testing.T) {
	fixture := newResolveLaneFixture(t)
	holdCommitLane(t, fixture.store)

	type commitOutcome struct {
		generationID int64
		report       BuildReport
		err          error
	}
	commit := make(chan commitOutcome, 1)
	go func() {
		generationID, report, err := builderNewBuilder(fixture.store).BuildCommitLayer(
			context.Background(), fixture.commitLayerRequest())
		commit <- commitOutcome{generationID, report, err}
	}()

	dirty := make(chan error, 1)
	go func() {
		_, _, err := builderNewBuilder(fixture.store).BuildDirtyLayer(
			context.Background(), fixture.dirtyLayerRequest())
		dirty <- err
	}()
	select {
	case err := <-dirty:
		if err != nil {
			t.Fatalf("BuildDirtyLayer: %v", err)
		}
	case <-time.After(resolveLaneBuildTimeout):
		t.Fatal("a dirty layer build waited behind another checkout's commit lane")
	}

	select {
	case out := <-commit:
		switch {
		case out.err != nil:
			t.Fatalf("BuildCommitLayer: %v", out.err)
		case out.generationID <= 0:
			t.Fatalf("the overtaken commit build published no generation: %d", out.generationID)
		case out.report.NodeCount == 0:
			t.Fatal("the overtaken commit build published an empty payload")
		}
	case <-time.After(resolveLaneBuildTimeout):
		t.Fatal("the overtaken commit layer build never completed")
	}
}

// TestSparseBuildRunsWhileTheBaseResolveLaneIsHeld pins the contract that must
// not move. The base corpus keeps one lane — a watcher reindex and a resolver
// pass over it still exclude each other — and a build over a derived
// generation neither waits for it nor takes it.
func TestSparseBuildRunsWhileTheBaseResolveLaneIsHeld(t *testing.T) {
	fixture := newResolveLaneFixture(t)

	fixture.store.ResolveMutex().Lock()
	defer fixture.store.ResolveMutex().Unlock()

	built := make(chan error, 1)
	go func() {
		_, _, err := builderNewBuilder(fixture.store).BuildCommitLayer(
			context.Background(), fixture.commitLayerRequest())
		built <- err
	}()

	select {
	case err := <-built:
		if err != nil {
			t.Fatalf("BuildCommitLayer: %v", err)
		}
	case <-time.After(resolveLaneBuildTimeout):
		t.Fatal("a sparse generation build waited for the base corpus resolve lane")
	}
	if fixture.store.ResolveMutex().TryLock() {
		t.Fatal("the base corpus resolve lane stopped excluding a concurrent base mutation")
	}
}
