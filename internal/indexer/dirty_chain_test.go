package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
)

// A working-tree chain built by hand through the production builder: the
// coordinator routes C (commit) and D1 (built direct over C), then D2 is built
// over D1's composed view and routed in D1's place. Builds in the coordinator
// still go direct; these tests pin that every guard reads such a route as the
// coherent state it is, and refuses one that is not rooted at the route's
// commit generation.

const (
	chainHelperEdit = "package fixture\n\nfunc Helper() {\n\t// first working-tree edit\n}\n"
	chainCallerEdit = "package fixture\n\nfunc Run() {\n\tCompute(Options{})\n\t// second working-tree edit\n}\n"
	chainIslandEdit = "package fixture\n\nfunc Island() {\n\t// third working-tree edit\n}\n"
)

type dirtyChainFixture struct {
	f      *coordinatorFixture
	c      *CheckoutCoordinator
	commit int64
	d1     int64
	d2     int64
}

func newDirtyChainFixture(t *testing.T, cfg CheckoutCoordinatorConfig) *dirtyChainFixture {
	t.Helper()
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, cfg)
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	first := coordinatorReconcile(t, c)
	if !first.DirtyBuilt || first.CommitGenerationID <= 0 || first.DirtyGenerationID <= 0 {
		t.Fatalf("the first cycle did not route a stack: %+v", first)
	}
	builderWriteFile(t, f.worktree, "caller.go", chainCallerEdit)
	d2, _ := buildDirtyOver(t, f, c, c.builder, first.DirtyGenerationID, nil, true)
	routeDirtySlot(t, f, d2)
	return &dirtyChainFixture{f: f, c: c, commit: first.CommitGenerationID, d1: first.DirtyGenerationID, d2: d2}
}

// buildDirtyOver builds a working-tree generation of the checkout's current
// disk over parent (0 = the base corpus) through the production builder,
// with the coordinator's identity (optionally mutated). withManifest=false
// builds it the way a binary without manifests did.
func buildDirtyOver(
	t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator, b *SparseGenerationBuilder,
	parent int64, mutate func(*GenerationIdentity), withManifest bool,
) (int64, BuildReport) {
	t.Helper()
	ctx := context.Background()
	var base LayerBase = f.store
	if parent > 0 {
		row, found := f.generation(parent)
		if !found {
			t.Fatalf("parent generation %d does not exist", parent)
		}
		materializer := graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases, Logger: zap.NewNop()}
		view, err := materializer.MaterializeRefView(ctx, row.GraphID, parent)
		if err != nil {
			t.Fatalf("materialize parent %d: %v", parent, err)
		}
		defer view.Close()
		base = c.ancestryLayerBase(view)
	}
	identity := c.dirtyIdentity(f.graphID, parent)
	if mutate != nil {
		mutate(&identity)
	}
	req := DirtyLayerRequest{
		Identity: identity, Base: base, CheckoutRoot: f.worktree,
		RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
	}
	if withManifest {
		generationID, report, err := b.BuildDirtyLayer(ctx, req)
		if err != nil {
			t.Fatalf("build a working-tree generation over %d: %v", parent, err)
		}
		return generationID, report
	}
	snap, err := gitstate.SampleDirty(ctx, f.worktree)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	target, err := source.NewFilesystemSource(f.worktree)
	if err != nil {
		t.Fatalf("open checkout: %v", err)
	}
	defer target.Close()
	changes, err := dirtyLayerDiskTruthContext(ctx, dirtyLayerChanges(snap), target)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	generationID, report, err := b.Build(ctx, BuildRequest{
		Identity: StampDirtyLayerIdentity(req.Identity, snap), Base: base, Target: target, Changes: changes,
		RootPath: f.worktree, RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("build a manifest-less working-tree generation over %d: %v", parent, err)
	}
	return generationID, report
}

func routeDirtySlot(t *testing.T, f *coordinatorFixture, generationID int64) {
	t.Helper()
	route := f.route()
	err := f.catalog.FlipCheckoutRouteSlot(context.Background(), store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID: f.checkoutID, Slot: store_sqlite.RouteSlotDirty, GenerationID: generationID,
		ExpectedRouteEpoch: route.RouteEpoch, State: store_sqlite.RouteActive,
	})
	if err != nil {
		t.Fatalf("route generation %d: %v", generationID, err)
	}
}

func (x *dirtyChainFixture) sample(t *testing.T) gitstate.DirtySnapshot {
	t.Helper()
	sample, err := x.c.sampler.Sample(context.Background())
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	return sample
}

func (x *dirtyChainFixture) commitRow(t *testing.T) store_sqlite.ViewGeneration {
	t.Helper()
	row, found := x.f.generation(x.commit)
	if !found {
		t.Fatalf("commit generation %d does not exist", x.commit)
	}
	return row
}

func TestDirtyChainRootWalksToTheRoutedCommit(t *testing.T) {
	x := newDirtyChainFixture(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()
	d2, _ := x.f.generation(x.d2)
	if d2.BaseGenerationID != x.d1 {
		t.Fatalf("the hand-built child sits on %d, want %d", d2.BaseGenerationID, x.d1)
	}
	chain, ok, reason, err := x.c.dirtyChainRoot(ctx, x.d2, x.commitRow(t), maxDirtyChainDepth)
	if err != nil || !ok || reason != "" {
		t.Fatalf("rooted chain refused: ok=%v reason=%q err=%v", ok, reason, err)
	}
	if len(chain) != 2 || chain[0].GenerationID != x.d2 || chain[1].GenerationID != x.d1 {
		t.Fatalf("chain=%+v", chain)
	}
	if _, ok, reason, _ := x.c.dirtyChainRoot(ctx, x.d2, x.commitRow(t), 1); ok || reason != dirtyChainFallbackChainDepthExhausted {
		t.Fatalf("a chain past its bound: ok=%v reason=%q", ok, reason)
	}
	other := x.commitRow(t)
	other.GenerationID = x.commit + 1000
	if _, ok, reason, _ := x.c.dirtyChainRoot(ctx, x.d2, other, maxDirtyChainDepth); ok || reason != dirtyChainFallbackHeadOrBaseMoved {
		t.Fatalf("a chain rooted at another commit: ok=%v reason=%q", ok, reason)
	}
}

func TestSettledWithoutBuildAcceptsRootedChain(t *testing.T) {
	x := newDirtyChainFixture(t, CheckoutCoordinatorConfig{})
	out, ok := x.c.settledWithoutBuild(context.Background())
	if !ok || out.CommitGenerationID != x.commit || out.DirtyGenerationID != x.d2 {
		t.Fatalf("a rooted chain was not settled: ok=%v out=%+v", ok, out)
	}
	before := x.f.route()
	cycle := coordinatorReconcile(t, x.c)
	if cycle.DirtyBuilt || cycle.CommitBuilt || cycle.DirtyGenerationID != x.d2 {
		t.Fatalf("a cycle over a settled chain rebuilt: %+v", cycle)
	}
	if after := x.f.route(); after != before {
		t.Fatalf("a settled chain's route moved: %+v -> %+v", before, after)
	}
}

func TestReconcileDirtySlotKeepsRootedChainRouted(t *testing.T) {
	var (
		mu       sync.Mutex
		armed    bool
		observed store_sqlite.CheckoutRoute
		seen     bool
		f        *coordinatorFixture
	)
	x := newDirtyChainFixture(t, CheckoutCoordinatorConfig{dirtyBarrier: func() {
		mu.Lock()
		defer mu.Unlock()
		if armed && !seen {
			observed, seen = f.route(), true
		}
	}})
	f = x.f
	before := f.route()
	builderWriteFile(t, f.worktree, "island.go", chainIslandEdit)
	mu.Lock()
	armed = true
	mu.Unlock()

	out := coordinatorReconcile(t, x.c)
	if !out.DirtyBuilt || out.DirtyGenerationID == x.d2 {
		t.Fatalf("the edit was not built: %+v", out)
	}
	mu.Lock()
	gotObserved, gotSeen := observed, seen
	mu.Unlock()
	if !gotSeen || gotObserved.DirtyGenerationID != x.d2 || gotObserved.State != store_sqlite.RouteActive {
		t.Fatalf("mid-build the route was %+v (seen=%v), want the chain %d still routed", gotObserved, gotSeen, x.d2)
	}
	after := f.route()
	if after.RouteEpoch != before.RouteEpoch+1 {
		t.Fatalf("route epoch %d -> %d: the dirty slot was withdrawn before the rebuild", before.RouteEpoch, after.RouteEpoch)
	}
	built, _ := f.generation(out.DirtyGenerationID)
	if built.BaseGenerationID != x.d2 {
		t.Fatalf("the build went over %d, want a delta over the routed top %d", built.BaseGenerationID, x.d2)
	}
	if out.DirtyParentCandidate != x.d2 || out.DirtyParentGenerationID != x.d2 || out.DirtyChainReason != "" {
		t.Fatalf("parent %d (candidate %d) reason %q, want %d", out.DirtyParentGenerationID, out.DirtyParentCandidate, out.DirtyChainReason, x.d2)
	}
}

func TestCheckRoutedSnapshotAcceptsRootedChain(t *testing.T) {
	x := newDirtyChainFixture(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()
	if err := x.c.checkRoutedSnapshot(ctx, x.f.route(), x.sample(t)); err != nil {
		t.Fatalf("a rooted chain was refused for edits: %v", err)
	}
	if err := x.c.refuseStaleAdmission(ctx, x.f.route().RouteEpoch); err != nil {
		t.Fatalf("the lock-free admission check refused a rooted chain: %v", err)
	}
}

func TestCheckRoutedSnapshotRefusesForeignChain(t *testing.T) {
	x := newDirtyChainFixture(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()
	f, c := x.f, x.c

	sibling := f.siblingCheckout("sibling")
	foreignHop, _ := buildDirtyOver(t, f, c, c.builder, x.commit, func(id *GenerationIdentity) {
		id.CheckoutID, id.LayerID = sibling, dirtyLayerID(sibling)
	}, true)
	overForeign, _ := buildDirtyOver(t, f, c, c.builder, foreignHop, nil, true)
	otherConfig, _ := buildDirtyOver(t, f, c, c.builder, x.d1, func(id *GenerationIdentity) {
		id.ConfigHash = "a-different-index-configuration"
	}, true)
	overCorpus, _ := buildDirtyOver(t, f, c, c.builder, 0, nil, true)
	overOtherRoot, _ := buildDirtyOver(t, f, c, c.builder, overCorpus, nil, true)

	sample := x.sample(t)
	for _, tc := range []struct {
		name   string
		top    int64
		reason string
	}{
		{"a hop from another checkout", overForeign, dirtyChainFallbackNoParent},
		{"a different config hash", otherConfig, dirtyChainFallbackNoParent},
		{"a terminal other than the route's commit", overOtherRoot, dirtyChainFallbackHeadOrBaseMoved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := f.route()
			route.DirtyGenerationID = tc.top
			err := c.checkRoutedSnapshot(ctx, route, sample)
			if !errors.Is(err, ErrCheckoutMutationStale) {
				t.Fatalf("a foreign chain was admitted for edits: %v", err)
			}
			if _, ok, reason, err := c.dirtyChainRoot(ctx, tc.top, x.commitRow(t), maxDirtyChainDepth); err != nil || ok || reason != tc.reason {
				t.Fatalf("predicate ok=%v reason=%q err=%v, want reason %q", ok, reason, err, tc.reason)
			}
		})
	}
}

func TestCheckoutRefreshCompletesOnRootedChain(t *testing.T) {
	x := newDirtyChainFixture(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()
	c := x.c
	// The fixture's coordinator loop is stopped; completion only needs a live
	// lifetime to tell a finished cycle from a shutdown.
	c.lifetime = context.Background()
	checkout, found, err := x.f.catalog.GetCheckout(ctx, x.f.checkoutID)
	if err != nil || !found {
		t.Fatalf("read checkout: found=%v err=%v", found, err)
	}
	rootInfo, err := checkoutRootFileInfo(checkout.RootPath)
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	sample := x.sample(t)
	done := make(chan MutationResult, 1)
	request := &checkoutRefreshRequest{
		checkout: checkout, rootInfo: rootInfo,
		headRef: sample.HeadRef, headCommit: sample.HeadCommit, headTree: sample.HeadTree,
		fingerprint: sample.Fingerprint, done: done,
		ticket: &CheckoutRefreshTicket{Ticket: &MutationTicket{Path: checkout.RootPath, Done: done, Generation: 1}},
	}
	c.refreshMu.Lock()
	c.refreshWaiters = map[uint64]*checkoutRefreshRequest{1: request}
	c.refreshMu.Unlock()

	c.completeCheckoutRefreshTickets(ctx, 1, CheckoutCycle{CommitGenerationID: x.commit, DirtyGenerationID: x.d2})
	select {
	case result := <-done:
		if result.Err != nil || result.AppliedGeneration != uint64(x.d2) {
			t.Fatalf("refresh completed with %+v, want generation %d", result, x.d2)
		}
	default:
		t.Fatal("a refresh over a rooted chain was left open")
	}
}

func TestRecomposeOverAdvancedBaseCarriesTheChainOver(t *testing.T) {
	x := newDirtyChainFixture(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()
	advancePrimaryBase(t, x.f)
	advanced, err := x.c.primaryBase(ctx)
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}
	if _, _, ok, err := x.c.recomposableStack(ctx, advanced, x.sample(t), x.f.route()); err != nil || !ok {
		t.Fatalf("a rooted chain was refused by the recomposition path: ok=%v err=%v", ok, err)
	}
	useCheckout(x.c)
	out := coordinatorReconcile(t, x.c)
	if !out.Recomposed || out.DirtyBuilt || !out.DirtyReparented {
		t.Fatalf("the base advance did not recompose the chained checkout by copy: %+v", out)
	}
	after := x.f.route()
	if after.CommitGenerationID == x.commit || after.DirtyGenerationID == x.d2 {
		t.Fatalf("the recomposition left the old pair routed: %+v", after)
	}
	// The chain is carried over member by member: its root sits on the new
	// commit generation, and it keeps its depth.
	members := x.c.dirtyChainMembers(ctx, after.DirtyGenerationID)
	if len(members) != 2 {
		t.Fatalf("the carried-over chain has %d members %v, want the routed chain's 2", len(members), members)
	}
	root, _ := x.f.generation(members[len(members)-1])
	if root.BaseGenerationID != after.CommitGenerationID {
		t.Fatalf("the carried-over chain's root sits on %d, want the new commit %d",
			root.BaseGenerationID, after.CommitGenerationID)
	}
}

func TestDirtyUndoCacheHitsChainedGeneration(t *testing.T) {
	x := newDirtyChainFixture(t, CheckoutCoordinatorConfig{})
	builderWriteFile(t, x.f.worktree, "island.go", chainIslandEdit)
	edited := coordinatorReconcile(t, x.c)
	if !edited.DirtyBuilt {
		t.Fatalf("the edit was not built: %+v", edited)
	}
	// Undo: the working tree returns to exactly the chained generation's state.
	builderWriteFile(t, x.f.worktree, "island.go", builderTreeA()["island.go"])
	undone := coordinatorReconcile(t, x.c)
	if undone.DirtyBuilt || !undone.DirtyReused || undone.DirtyGenerationID != x.d2 {
		t.Fatalf("the undo did not re-route the chained generation %d: %+v", x.d2, undone)
	}
	if route := x.f.route(); route.DirtyGenerationID != x.d2 {
		t.Fatalf("route names %d, want %d", route.DirtyGenerationID, x.d2)
	}
}

func TestSelectDirtyParentReasonCodes(t *testing.T) {
	x := newDirtyChainFixture(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()
	f, c := x.f, x.c
	commit := x.commitRow(t)
	route := f.route()
	seen := map[string]bool{}
	expect := func(t *testing.T, name string, got dirtyParentSelection, reason string) {
		t.Helper()
		seen[reason] = true
		if got.Reason != reason {
			t.Fatalf("%s: reason %q want %q (%s)", name, got.Reason, reason, got)
		}
		if reason != "" && got.Parent != 0 {
			t.Fatalf("%s: a fallback named parent %d", name, got.Parent)
		}
	}

	// A usable parent: one more edit over the routed chain.
	builderWriteFile(t, f.worktree, "island.go", chainIslandEdit)
	ok := c.selectDirtyParentDetail(ctx, route, commit, x.sample(t), maxDirtyChainDepth)
	expect(t, "usable parent", ok, "")
	if ok.Parent != x.d2 || ok.Depth != 2 || ok.PreviewChanges != 1 {
		t.Fatalf("usable parent: %s", ok)
	}
	if parent, manifest, reason := c.selectDirtyParent(ctx, route, commit, x.sample(t)); parent != x.d2 || reason != "" || manifest.dirtyCount() != 2 {
		t.Fatalf("selectDirtyParent=%d %q dirty=%d", parent, reason, manifest.dirtyCount())
	}
	builderWriteFile(t, f.worktree, "island.go", builderTreeA()["island.go"])

	expect(t, "clean", c.selectDirtyParentDetail(ctx, route, commit, gitstate.DirtySnapshot{}, maxDirtyChainDepth), dirtyChainFallbackCleanCheckout)

	noDirty := route
	noDirty.DirtyGenerationID = 0
	expect(t, "no parent", c.selectDirtyParentDetail(ctx, noDirty, commit, x.sample(t), maxDirtyChainDepth), dirtyChainFallbackNoParent)

	overCorpus, _ := buildDirtyOver(t, f, c, c.builder, 0, nil, true)
	overOtherRoot, _ := buildDirtyOver(t, f, c, c.builder, overCorpus, nil, true)
	elsewhere := route
	elsewhere.DirtyGenerationID = overOtherRoot
	expect(t, "rooted elsewhere", c.selectDirtyParentDetail(ctx, elsewhere, commit, x.sample(t), maxDirtyChainDepth), dirtyChainFallbackHeadOrBaseMoved)

	expect(t, "depth bound", c.selectDirtyParentDetail(ctx, route, commit, x.sample(t), 2), dirtyChainFallbackChainDepthExhausted)

	savedConfig := c.configHash
	c.configHash = "a-different-index-configuration"
	expect(t, "policy", c.selectDirtyParentDetail(ctx, route, commit, x.sample(t), maxDirtyChainDepth), dirtyChainFallbackPolicyChanged)
	c.configHash = savedConfig

	// A state of its own, so the build is not coalesced onto the routed child.
	builderWriteFile(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\t// built without a manifest\n}\n")
	manifestless, _ := buildDirtyOver(t, f, c, c.builder, x.d1, nil, false)
	builderWriteFile(t, f.worktree, "island.go", builderTreeA()["island.go"])
	missing := route
	missing.DirtyGenerationID = manifestless
	expect(t, "manifest missing", c.selectDirtyParentDetail(ctx, missing, commit, x.sample(t), maxDirtyChainDepth), dirtyChainFallbackParentManifestMissing)

	truncating := builderNewBuilder(f.store)
	truncating.Config.AffectedByReresolveMax = 1
	// core.go is dirty for this build and now calls into two other files, so
	// its forward closure (helper.go, island.go) exceeds the one-file cap.
	// A manifest arrives with it, so the state is built by the sparse
	// closure builder — the one builder whose closure can truncate.
	builderWriteFile(t, f.worktree, "helper.go", builderTreeA()["helper.go"])
	builderWriteFile(t, f.worktree, "caller.go", builderTreeA()["caller.go"])
	builderWriteFile(t, f.worktree, "core.go", "package fixture\n\ntype Options struct{}\n\nfunc Compute(o Options) {\n\tHelper()\n\tIsland()\n}\n")
	builderWriteFile(t, f.worktree, "tsconfig.json", "{}\n")
	truncated, report := buildDirtyOver(t, f, c, truncating, x.d1, nil, true)
	if !report.ClosureTruncated {
		t.Fatalf("the capped build did not truncate its closure: closure=%v cap=%d", report.ClosurePaths, report.ClosureCap)
	}
	if err := os.Remove(filepath.Join(f.worktree, "tsconfig.json")); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "core.go", builderTreeA()["core.go"])
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	builderWriteFile(t, f.worktree, "caller.go", chainCallerEdit)
	cut := route
	cut.DirtyGenerationID = truncated
	expect(t, "truncated parent", c.selectDirtyParentDetail(ctx, cut, commit, x.sample(t), maxDirtyChainDepth), dirtyChainFallbackClosureTruncatedParent)

	gomod := filepath.Join(f.worktree, "go.mod")
	builderWriteFile(t, f.worktree, "go.mod", "module example.com/fixture\n\ngo 1.22\n")
	expect(t, "manifest", c.selectDirtyParentDetail(ctx, route, commit, x.sample(t), maxDirtyChainDepth), dirtyChainFallbackDependencyManifestChanged)
	if err := os.Remove(gomod); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(f.worktree, "link.go")
	if err := os.Symlink("helper.go", link); err != nil {
		t.Fatal(err)
	}
	expect(t, "symlink", c.selectDirtyParentDetail(ctx, route, commit, x.sample(t), maxDirtyChainDepth), dirtyChainFallbackSymlinkOrSubmoduleChanged)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	// Revert both chained edits and make one new one: three paths move for a
	// one-path dirty set. A delta that small still chains; past the small-delta
	// bound (lowered here so three paths are "large") it goes direct.
	builderWriteFile(t, f.worktree, "helper.go", builderTreeA()["helper.go"])
	builderWriteFile(t, f.worktree, "caller.go", builderTreeA()["caller.go"])
	builderWriteFile(t, f.worktree, "island.go", chainIslandEdit)
	if small := c.selectDirtyParentDetail(ctx, route, commit, x.sample(t), maxDirtyChainDepth); small.Parent == 0 {
		t.Fatalf("a three-path delta was refused (%s); a small delta always chains", small.Reason)
	}
	savedSmall := dirtyChainSmallDelta
	dirtyChainSmallDelta = 2
	expect(t, "delta not smaller", c.selectDirtyParentDetail(ctx, route, commit, x.sample(t), maxDirtyChainDepth), dirtyChainFallbackDeltaNotSmaller)
	dirtyChainSmallDelta = savedSmall

	for _, reason := range dirtyChainFallbackReasons {
		if !seen[reason] {
			t.Errorf("reason %q was never produced", reason)
		}
	}
}
