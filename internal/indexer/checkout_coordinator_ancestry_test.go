package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

type coordinatorAncestryFixture struct {
	c               *CheckoutCoordinator
	request         dedicatedBuilderFixtureRequest
	otherCheckoutID string
}

func newCoordinatorAncestryFixture(t testing.TB) coordinatorAncestryFixture {
	t.Helper()
	f := newCoordinatorFixture(t)
	request := dedicatedBuilderFixtureRequest{
		Identity: GenerationIdentity{OwnerKind: checkoutLayerOwnerKind, GraphID: f.graphID,
			CheckoutID: f.primaryID, GenerationKind: "dedicated", TreeOID: f.treeA,
			ConfigHash: "ancestry-config", ExtractorVersions: "ancestry-extractors", ResolverVersion: "ancestry-resolver"},
		RootPath: f.primary, RepoPrefix: builderRepoPrefix,
	}
	c := &CheckoutCoordinator{
		store: f.store, catalog: f.catalog,
		leases: f.leases, logger: zap.NewNop(),
	}
	return coordinatorAncestryFixture{c: c, request: request, otherCheckoutID: f.checkoutID}
}

func (f coordinatorAncestryFixture) node(name, file string) *graph.Node {
	return &graph.Node{ID: f.request.RepoPrefix + "/" + file + "::" + name,
		Name: name, Kind: graph.KindFunction, RepoPrefix: f.request.RepoPrefix,
		FilePath: f.request.RepoPrefix + "/" + file, Language: "go"}
}

func (f coordinatorAncestryFixture) generation(t testing.TB, parent int64, kind string, nodes []*graph.Node, masks []store_sqlite.FileMask, tombstones []string) int64 {
	t.Helper()
	ctx := context.Background()
	id, handle, err := f.c.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: f.request.Identity.OwnerKind, GraphID: f.request.Identity.GraphID,
		CheckoutID: f.request.Identity.CheckoutID, GenerationKind: kind,
		BaseGenerationID: parent, TreeOID: f.request.Identity.TreeOID,
		ConfigHash: f.request.Identity.ConfigHash, ExtractorVersions: f.request.Identity.ExtractorVersions,
		ResolverVersion: f.request.Identity.ResolverVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle.AddBatch(nodes, nil)
	if err := handle.SetFileMasks(masks); err != nil {
		t.Fatal(err)
	}
	if err := handle.SetNodeTombstones(tombstones); err != nil {
		t.Fatal(err)
	}
	if err := f.c.store.PublishPayloadGeneration(ctx, id, 100); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCoordinatorCommitReaderComposesAncestryAndReleases(t *testing.T) {
	f := newCoordinatorAncestryFixture(t)
	kept, old, deleted, tombstoned := f.node("Oldest", "oldest.go"), f.node("Old", "replace.go"), f.node("Deleted", "deleted.go"), f.node("Tombstoned", "tombstone.go")
	legacy := f.node("DirtyOnly", "legacy.go")
	f.c.store.AddBatch([]*graph.Node{legacy}, nil)
	root := f.generation(t, 0, "dedicated", []*graph.Node{kept, old, deleted, tombstoned}, nil, nil)
	replacement := f.node("Replacement", "replace.go")
	middle := f.generation(t, root, "dedicated", []*graph.Node{replacement}, []store_sqlite.FileMask{
		{RepoPrefix: f.request.RepoPrefix, FilePath: old.FilePath, Mode: store_sqlite.OwnershipReplace},
		{RepoPrefix: f.request.RepoPrefix, FilePath: deleted.FilePath, Mode: store_sqlite.OwnershipDelete},
	}, []string{tombstoned.ID})
	newest := f.node("Newest", "newest.go")
	top := f.generation(t, middle, "commit", []*graph.Node{newest}, []store_sqlite.FileMask{
		{RepoPrefix: f.request.RepoPrefix, FilePath: newest.FilePath, Mode: store_sqlite.OwnershipReplace},
	}, nil)
	if f.c.store.AtGeneration(top).GetNode(newest.ID) == nil {
		t.Fatal("newest generation payload missing before composition")
	}
	reader, release, err := f.c.commitLayerReader(context.Background(), top)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	for _, node := range []*graph.Node{kept, replacement, newest} {
		if reader.GetNode(node.ID) == nil {
			t.Errorf("missing ancestor payload %s", node.ID)
		}
	}
	for _, node := range []*graph.Node{old, deleted, tombstoned, legacy} {
		if reader.GetNode(node.ID) != nil {
			t.Errorf("masked or unrelated payload leaked: %s", node.ID)
		}
	}
	for _, id := range []int64{root, middle, top} {
		if !f.c.leases.InUse(id) {
			t.Errorf("generation %d not leased while reader is live", id)
		}
	}
	release()
	release() // The callback retains RepoView.Close's idempotence.
	for _, id := range []int64{root, middle, top} {
		if f.c.leases.InUse(id) {
			t.Errorf("generation %d lease leaked after release", id)
		}
	}
}

func TestCoordinatorCommitReaderPreservesLegacyBaseZero(t *testing.T) {
	f := newCoordinatorAncestryFixture(t)
	legacy, current := f.node("Legacy", "legacy.go"), f.node("Current", "current.go")
	f.c.store.AddBatch([]*graph.Node{legacy}, nil)
	top := f.generation(t, 0, "commit", []*graph.Node{current}, []store_sqlite.FileMask{
		{RepoPrefix: f.request.RepoPrefix, FilePath: current.FilePath, Mode: store_sqlite.OwnershipReplace},
	}, nil)
	if f.c.store.AtGeneration(top).GetNode(current.ID) == nil {
		t.Fatal("current generation payload missing before composition")
	}
	reader, release, err := f.c.commitLayerReader(context.Background(), top)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, node := range []*graph.Node{legacy, current} {
		if reader.GetNode(node.ID) == nil {
			t.Errorf("legacy composition lost %s", node.ID)
		}
	}
}

func TestCoordinatorCommitReaderRejectsUnavailableAncestry(t *testing.T) {
	f := newCoordinatorAncestryFixture(t)
	root := f.generation(t, 0, "dedicated", []*graph.Node{f.node("Root", "root.go")}, nil, nil)
	top := f.generation(t, root, "commit", nil, nil, nil)
	if err := f.c.catalog.SetViewGenerationState(context.Background(), root, store_sqlite.ViewGenerationRetiring); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{top, top + 1000} {
		reader, release, err := f.c.commitLayerReader(context.Background(), id)
		if release != nil {
			release()
		}
		if err == nil || reader != nil {
			t.Errorf("unavailable generation %d accepted: reader=%v err=%v", id, reader, err)
		}
	}
	for _, id := range []int64{root, top} {
		if f.c.leases.InUse(id) {
			t.Errorf("error path leaked generation %d lease", id)
		}
	}
}

func TestCoordinatorCommitReaderRejectsMismatchedDedicatedOwner(t *testing.T) {
	f := newCoordinatorAncestryFixture(t)
	ctx := context.Background()
	id, _, err := f.c.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: checkoutLayerOwnerKind, GraphID: f.request.Identity.GraphID,
		CheckoutID: f.otherCheckoutID, GenerationKind: "dedicated", TreeOID: f.request.Identity.TreeOID,
	})
	if err != nil {
		t.Fatalf("malformed ownership fixture allocation: %v", err)
	}
	if err := f.c.store.PublishPayloadGeneration(ctx, id, 100); err != nil {
		t.Fatal(err)
	}
	reader, release, err := f.c.commitLayerReader(ctx, id)
	if release != nil {
		release()
	}
	if err == nil || reader != nil {
		t.Errorf("dedicated root owned by a different checkout accepted: reader=%v err=%v", reader, err)
	}
	if f.c.leases.InUse(id) {
		t.Errorf("malformed dedicated root leaked lease %d", id)
	}
}

func BenchmarkCoordinatorCommitLayerReader(b *testing.B) {
	for _, depth := range []int{1, 3} {
		b.Run(fmt.Sprintf("depth_%d", depth), func(b *testing.B) {
			f := newCoordinatorAncestryFixture(b)
			kept := f.node("Oldest", "oldest.go")
			top := f.generation(b, 0, "dedicated", []*graph.Node{kept}, nil, nil)
			for level := 1; level < depth; level++ {
				top = f.generation(b, top, "dedicated", nil, nil, nil)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				reader, release, err := f.c.commitLayerReader(context.Background(), top)
				if err != nil {
					b.Fatal(err)
				}
				found := reader.GetNode(kept.ID) != nil
				release()
				if !found {
					b.Fatal("oldest ancestor missing")
				}
			}
		})
	}
}

// coordinatorParsedAncestry uses the actual full inventory and public physical
// builder to seed a small committed root. Later empty deltas intentionally do
// not repeat its nodes or incoming edges.
func coordinatorParsedAncestry(t *testing.T) (*coordinatorFixture, *CheckoutCoordinator, []int64, string) {
	t.Helper()
	f := newCoordinatorFixture(t)
	builderWriteFile(t, f.worktree, "ancestry_target.go", "package fixture\n\nfunc AncestryTarget() {}\n")
	builderWriteFile(t, f.worktree, "ancestry_caller.go", "package fixture\n\nfunc AncestryCaller() { AncestryTarget() }\n")
	builderGit(t, f.worktree, "add", "-A")
	builderGit(t, f.worktree, "commit", "-m", "ancestry root")
	tree := builderGit(t, f.worktree, "rev-parse", "HEAD^{tree}")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	target, err := source.NewGitTreeSource(ctx, f.worktree, tree)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	plan, _, err := planDedicatedSnapshot(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	changes := make([]LayerPathChange, 0, len(plan.indexed))
	for _, path := range plan.indexed {
		changes = append(changes, LayerPathChange{Path: path, Kind: LayerPathAdded})
	}
	identity := GenerationIdentity{OwnerKind: checkoutLayerOwnerKind, GraphID: f.graphID,
		CheckoutID: f.primaryID, GenerationKind: "dedicated", TreeOID: tree,
		ConfigHash: "ancestry-config", ExtractorVersions: "ancestry-extractors", ResolverVersion: "ancestry-resolver"}
	builder := builderNewBuilder(f.store)
	root, report, err := builder.Build(ctx, BuildRequest{Identity: identity, Base: graph.New(), Target: target,
		Changes: changes, RootPath: f.worktree, RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix})
	if err != nil || root <= 0 || report.NodeCount == 0 {
		t.Fatalf("real root: id=%d report=%+v err=%v", root, report, err)
	}
	callerID := builderRepoPrefix + "/ancestry_caller.go::AncestryCaller"
	if f.store.AtGeneration(root).GetNode(callerID) == nil {
		t.Fatal("real root did not parse the unchanged caller")
	}
	ids := []int64{root}
	for range 2 {
		id, _, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
			OwnerKind: checkoutLayerOwnerKind, GraphID: f.graphID, CheckoutID: f.primaryID,
			GenerationKind: "dedicated", BaseGenerationID: ids[len(ids)-1], TreeOID: tree,
			ConfigHash: identity.ConfigHash, ExtractorVersions: identity.ExtractorVersions, ResolverVersion: identity.ResolverVersion})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.PublishPayloadGeneration(ctx, id, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return f, f.inertCoordinator(t, CheckoutCoordinatorConfig{}), ids, tree
}

func TestCoordinatorCommitBuildUsesOldestAncestorClosure(t *testing.T) {
	f, c, ids, tree := coordinatorParsedAncestry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base, err := c.primaryBase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base.generationID, base.treeOID = ids[len(ids)-1], tree
	callerID := builderRepoPrefix + "/ancestry_caller.go::AncestryCaller"
	if f.store.AtGeneration(base.generationID).GetNode(callerID) != nil {
		t.Fatal("top delta unexpectedly repeats caller; closure oracle is ineffective")
	}
	builderWriteFile(t, f.worktree, "ancestry_target.go", "package fixture\n\nfunc AncestryReplacement() {}\n")
	builderGit(t, f.worktree, "add", "-A")
	builderGit(t, f.worktree, "commit", "-m", "remove oldest callee")
	targetTree := builderGit(t, f.worktree, "rev-parse", "HEAD^{tree}")
	id, _, err := c.resolveCommitLayer(ctx, base, targetTree)
	if err != nil || id <= 0 {
		t.Fatalf("real commit build: id=%d err=%v", id, err)
	}
	if f.store.AtGeneration(id).GetNode(callerID) == nil {
		t.Error("actual commit payload omitted the incoming caller held only in the oldest ancestor")
	}
	if f.store.AtGeneration(id).GetNode(builderRepoPrefix+"/ancestry_target.go::AncestryReplacement") == nil {
		t.Error("actual commit payload did not parse the changed target")
	}
	for _, ancestor := range ids {
		if f.leases.InUse(ancestor) {
			t.Errorf("commit build leaked lower lease %d", ancestor)
		}
	}
}

func TestCoordinatorDirtyRetriesRetainAndReleaseWholeAncestry(t *testing.T) {
	f, c, ids, tree := coordinatorParsedAncestry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base, err := c.primaryBase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base.generationID, base.treeOID = ids[len(ids)-1], tree
	commit, _, err := c.resolveCommitLayer(ctx, base, tree)
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, commit)
	route, err := c.ensureRoute(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	builds := 0
	c.dirtyBarrier = func() {
		builds++
		for _, id := range ids {
			if !f.leases.InUse(id) {
				t.Errorf("attempt %d did not retain ancestor %d", builds, id)
			}
		}
		// The existing producer barrier is before its final snapshot check.
		// Change the real untracked input on every attempt, forcing both retries.
		body := fmt.Sprintf("package fixture\n\nfunc DirtyAttempt%d() {}\n", builds)
		if err := os.WriteFile(filepath.Join(f.worktree, "ancestry_dirty.go"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var cycle CheckoutCycle
	if err := c.reconcileDirtySlot(ctx, commit, tree, &route, &cycle); err != nil {
		t.Fatal(err)
	}
	if builds != 2 || cycle.DirtyGenerationID != 0 {
		t.Errorf("retry result: attempts=%d cycle=%+v", builds, cycle)
	}
	for _, id := range ids {
		if f.leases.InUse(id) {
			t.Errorf("dirty retries leaked ancestor %d", id)
		}
	}
}
