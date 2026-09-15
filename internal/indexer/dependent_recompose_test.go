package indexer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// A committed base that advances under a dependent worktree.
//
// The dependent's delta is `git diff-tree base..itsOwnTree`, so a base that
// moves changes what the delta has to contain: the same bytes are simply not
// valid over another base, and there is no key under which they would be.
//
// Whether the pair the dependent is ALREADY serving stays coherent depends on
// which base regime the family is in, and recomposeOverAdvancedBase's doc
// carries the full argument. Where the primary publishes a generation the old
// pair is coherent — the layer names an immutable ancestor, the materializer
// composes the ancestry the routed generation itself names, retirement refuses
// a generation anything still names — and pinning the dependent there is the
// unimplemented dependent-pin saving, not something these tests claim. Where
// it does not (the regime this fixture is in, and the one these tests drive)
// the delta names no immutable ancestor at all: BaseGenerationID is 0 and the
// layer composes over the shared indexed corpus, which is rewritten in place
// as the primary moves.
// TestARoutedDependentDeltaNamesNoImmutableBaseWithoutAPublishedGeneration
// pins that, because it is the premise everything below stands on.
//
// Either way a base advance here is a RECOMPOSITION: both layers are rebuilt
// off-route and installed together, and until that write lands the old pair
// keeps serving.
//
// These tests drive the production reconcile path and assert exactly that: one
// bounded rebuild per dependent per advance, the old pair routed for the whole
// of it, and never a new base under an old delta.

// advancePrimaryBase commits one new file on the primary checkout and records
// its tree as the family's committed base, which is what every dependent's
// graphBase reads. It returns the new base tree and the file's name.
//
// One file, deliberately: the dependent's recomposed delta is the difference
// between this tree and the dependent's own, so a one-file advance makes the
// bound observable — a delta that claimed more than that path would not be a
// delta.
func advancePrimaryBase(t *testing.T, f *coordinatorFixture) (tree, file string) {
	t.Helper()
	return advancePrimaryBaseWith(t, f, "advanced.go", "Advanced")
}

// advancePrimaryBaseWith is advancePrimaryBase with the file it adds named by
// the caller, so a test that has to advance the base twice adds a different
// path each time — committing the same bytes again is not a commit at all.
func advancePrimaryBaseWith(t *testing.T, f *coordinatorFixture, file, symbol string) (tree, name string) {
	t.Helper()
	contents := "package fixture\n\nfunc " + symbol + "() {\n\tHelper()\n}\n"
	if err := os.WriteFile(filepath.Join(f.primary, file), []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s in the primary: %v", file, err)
	}
	builderGit(t, f.primary, "add", "-A")
	builderGit(t, f.primary, "commit", "-m", "advance the base")
	tree = builderGit(t, f.primary, "rev-parse", "HEAD^{tree}")
	f.movePrimaryHead(t, tree)
	return tree, file
}

// claimedPaths lists the graph paths one generation's layer claims — the
// payload's own ownership masks, read through the same layer contract the
// composed reader applies.
//
// A path the generation declared read-only CONTEXT is deliberately not here:
// the generation states no claim over it and the layer below answers for it
// (graphview.GenerationLayer.FilePaths / ContextPaths). closurePaths is the
// other half, for a caller asking what the pass reached rather than what it
// speaks for.
func claimedPaths(t *testing.T, store *store_sqlite.Store, generationID int64) []string {
	t.Helper()
	return generationLayerPaths(t, store, generationID, false)
}

// closurePaths lists every path one generation's pass reached: the ones it
// claims plus the ones it declared read-only context. It is what "the closure
// did not leave this file out" has to be asked about — whether a file the
// closure pulled in ends up claimed or context is the ownership split's
// decision, and the closure's own question is whether the pass saw it at all.
func closurePaths(t *testing.T, store *store_sqlite.Store, generationID int64) []string {
	t.Helper()
	return generationLayerPaths(t, store, generationID, true)
}

func generationLayerPaths(t *testing.T, store *store_sqlite.Store, generationID int64, withContext bool) []string {
	t.Helper()
	layer, err := graphview.NewGenerationLayer(store.AtGeneration(generationID))
	if err != nil {
		t.Fatalf("open generation %d as a layer: %v", generationID, err)
	}
	out := layer.FilePaths()
	if withContext {
		out = append(out, layer.ContextPaths()...)
		sort.Strings(out)
	}
	return out
}

// TestARoutedDependentDeltaNamesNoImmutableBaseWithoutAPublishedGeneration
// pins the premise recomposeOverAdvancedBase's two-regime contract rests on,
// and the one it is easiest to get wrong by reading only the published-
// generation half of the code.
//
// The argument that an un-recomposed dependent keeps serving its own tree runs
// "the routed commit layer names an immutable ancestor, and the materializer
// composes the ancestry the ROUTED generation names rather than the family's
// current pointer". Both halves are true — of a family whose primary graph has
// published a generation. This family has not: graphBase's second arm resolves
// the base from the owner checkout's recorded committed tree and leaves
// primaryBase.generationID at zero, so commitIdentity stamps BaseGenerationID
// 0 and there is no ancestor to be immutable. generationAncestry terminates at
// zero rather than walking into it, and what the delta actually composes over
// is the shared indexed corpus — the one layer in the stack that carries no
// version identity and is rewritten in place as the primary moves.
//
// So in THIS regime "keep the dependent routed on the base it was built
// against" is not a saving that has been left on the table; there is nothing
// to stay on. That is why the recomposition below is a correctness path here
// and only a currency/availability path where a generation is published, and
// why settledWithoutBuild must not accept a superseded parent.
//
// Revert-red: drop the base generation from commitIdentity's stamp
// (BaseGenerationID: 0 unconditionally) and the published-regime half fails —
// that stamp is the only thing that makes an ancestor exist at all.
func TestARoutedDependentDeltaNamesNoImmutableBaseWithoutAPublishedGeneration(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	cycle := coordinatorReconcile(t, c)
	if cycle.CommitGenerationID == 0 || cycle.DirtyGenerationID == 0 {
		t.Fatalf("the coordinator did not bring both slots up: %+v", cycle)
	}

	// The regime: the primary graph publishes no generation, so the base the
	// cycle built against is a bare tree OID.
	base, err := c.primaryBase(context.Background())
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}
	if base.generationID != 0 {
		t.Fatalf("the fixture's primary published generation %d; this test is about the regime "+
			"where it publishes none", base.generationID)
	}
	if base.treeOID == "" {
		t.Fatal("the base names no committed tree, so there is no regime to be in")
	}

	// The consequence in the catalog: the routed delta names no ancestor.
	commit, found := f.generation(cycle.CommitGenerationID)
	if !found {
		t.Fatalf("the routed commit generation %d is not in the catalog", cycle.CommitGenerationID)
	}
	if commit.BaseGenerationID != 0 {
		t.Fatalf("the routed commit layer names ancestor %d; without a published base there is no "+
			"immutable generation for it to name", commit.BaseGenerationID)
	}
	if commit.LowerViewFingerprint != base.treeOID {
		t.Fatalf("the layer is stamped against %q, want the base tree %q — the diff's left-hand side "+
			"is the only record of what it was built over here",
			commit.LowerViewFingerprint, base.treeOID)
	}

	// The consequence at read time: the bottom of the composed stack is the
	// shared corpus, not a pinned ancestor.
	materializer := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
	view, err := materializer.MaterializeCheckout(context.Background(), f.checkoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	defer view.Close()
	if !view.PinsBaseCorpus() {
		t.Fatal("the view does not read generation zero, so the delta stands on something else — " +
			"recomposeOverAdvancedBase's second bullet no longer describes this regime")
	}
	for _, source := range view.GenerationSources() {
		if source.Generation == cycle.CommitGenerationID {
			continue
		}
		if source.Generation == cycle.DirtyGenerationID {
			continue
		}
		t.Fatalf("the stack carries generation %d beneath the routed pair; without a published base "+
			"the routed pair is the whole derived stack", source.Generation)
	}

	// And the other arm of the same stamp, so the published-generation half of
	// the contract is pinned by the same test that pins this one: where the
	// base IS a generation, commitIdentity records it, which is what gives the
	// materializer an immutable ancestor to walk.
	const published = int64(4242)
	identity := c.commitIdentity(primaryBase{
		graphID:      f.graphID,
		generationID: published,
		treeOID:      f.treeA,
	}, f.treeA)
	if identity.BaseGenerationID != published {
		t.Fatalf("commitIdentity stamped BaseGenerationID %d over a published base %d; the "+
			"published-generation regime's immutable ancestor is exactly this field",
			identity.BaseGenerationID, published)
	}
}

// TestADependentOnGenerationZeroRecomposesOntoTheFirstPublishedBase pins the
// transition the committed-base consumer gate makes ordinary.
//
// Publication is now DEFERRED until something can read the base, and the thing
// that makes a family publishable is a dependent appearing. So the sequence
// below is the product's normal one, not an edge case: a dependent is routed
// in the legacy regime over generation 0 (BaseGenerationID 0, composing over
// the shared corpus), the family's first committed base then lands underneath
// it, and the dependent has to end up routed over an immutable ancestor.
//
// That is the promise the deferral rests on. If the first published base left
// a routed gen-0 delta where it was, the deferral would be trading a write now
// for a permanently un-pinned dependent — the exact staleness
// recomposeOverAdvancedBase's second bullet describes, since the corpus under
// that delta is re-indexed in place as the primary moves.
//
// What is asserted is the END STATE, not which of the two paths reached it:
// whether the cycle takes the bounded recomposition or the ordinary
// slot-by-slot flip is a decision recomposableStack owns and the other tests in
// this file pin. What must be true either way is that the route names a new
// commit layer and that the layer names the published base.
func TestADependentOnGenerationZeroRecomposesOntoTheFirstPublishedBase(t *testing.T) {
	f := newUnpublishedCommittedBaseFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	legacy := coordinatorReconcile(t, c)
	if legacy.CommitGenerationID == 0 || legacy.DirtyGenerationID == 0 {
		t.Fatalf("the coordinator did not bring both slots up: %+v", legacy)
	}
	before, found := f.generation(legacy.CommitGenerationID)
	if !found {
		t.Fatalf("the routed commit generation %d is not in the catalog", legacy.CommitGenerationID)
	}
	if before.BaseGenerationID != 0 {
		t.Fatalf("the premise is the legacy regime; the routed layer already names ancestor %d",
			before.BaseGenerationID)
	}

	// The family gains its first committed base, exactly as a consumer's
	// demand would make it: the same publisher, the same claim, the same
	// adoption.
	baseGeneration := f.publishBase(t)
	if baseGeneration <= 0 {
		t.Fatalf("the fixture published no base: %d", baseGeneration)
	}

	recomposed := coordinatorReconcile(t, c)
	if recomposed.CommitGenerationID == legacy.CommitGenerationID {
		t.Fatalf("the dependent is still routed over the generation-0 layer %d after the family "+
			"published base %d; it composes over a corpus that is now re-indexed under it",
			legacy.CommitGenerationID, baseGeneration)
	}
	after, found := f.generation(recomposed.CommitGenerationID)
	if !found {
		t.Fatalf("the recomposed commit generation %d is not in the catalog", recomposed.CommitGenerationID)
	}
	if after.BaseGenerationID != baseGeneration {
		t.Fatalf("the recomposed layer names ancestor %d, want the published base %d",
			after.BaseGenerationID, baseGeneration)
	}
	route := f.route()
	if route.CommitGenerationID != recomposed.CommitGenerationID ||
		route.DirtyGenerationID != recomposed.DirtyGenerationID ||
		route.State != store_sqlite.RouteActive {
		t.Fatalf("the route does not name the recomposed pair: %+v vs %+v", route, recomposed)
	}
	// No new base under an old delta: the layer the route names must be the
	// one stamped against the base the family is on now.
	if after.LowerViewFingerprint == "" {
		t.Fatalf("the recomposed layer carries no lower fingerprint: %+v", after)
	}
}

// TestBaseAdvanceRecomposesTheStackWithoutDroppingTheRoute is gate 5's
// "old coherent routes remain available until replacement routes are ready".
//
// The barrier runs inside the working-tree build, after its payload is written
// and before it is published — which is after the new commit layer has been
// built. The route read there is what a reader materializing mid-rebuild would
// get, and it has to be the pair the checkout was already serving. The
// slot-at-a-time path cannot pass it: moveCommitSlot flips the commit slot and
// clears the working-tree slot before the working-tree layer is rebuilt, so
// the route observed at the barrier would name the new commit layer and no
// dirty layer at all.
//
// Revert-red: delete the recomposeOverAdvancedBase call from reconcile and the
// barrier observes the new commit generation with the dirty slot cleared.
func TestBaseAdvanceRecomposesTheStackWithoutDroppingTheRoute(t *testing.T) {
	var (
		mu       sync.Mutex
		armed    bool
		observed store_sqlite.CheckoutRoute
		seen     bool
	)
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{dirtyBarrier: func() {
		mu.Lock()
		defer mu.Unlock()
		if !armed || seen {
			return
		}
		observed, seen = f.route(), true
	}})

	first := coordinatorReconcile(t, c)
	if !first.CommitBuilt || !first.DirtyBuilt {
		t.Fatalf("the first cycle did not build the stack: %+v", first)
	}
	before := f.route()
	replacedDirty, found := f.generation(before.DirtyGenerationID)
	if !found {
		t.Fatalf("the routed working-tree generation %d does not exist", before.DirtyGenerationID)
	}
	replacedDirtyKey := generationRowKey(replacedDirty)

	newBaseTree, _ := advancePrimaryBase(t, f)
	mu.Lock()
	armed = true
	mu.Unlock()

	out := coordinatorReconcile(t, c)
	if !out.Recomposed {
		t.Fatalf("the base advance did not recompose the dependent: %+v", out)
	}
	if !out.CommitBuilt || !out.DirtyBuilt {
		t.Fatalf("the recomposition did not rebuild both layers: %+v", out)
	}

	mu.Lock()
	observedRoute, observedSeen := observed, seen
	mu.Unlock()
	if !observedSeen {
		t.Fatalf("the working-tree build never reached the barrier")
	}
	if observedRoute.CommitGenerationID != before.CommitGenerationID ||
		observedRoute.DirtyGenerationID != before.DirtyGenerationID {
		t.Fatalf("mid-recomposition the route named (%d, %d), want the old pair (%d, %d)",
			observedRoute.CommitGenerationID, observedRoute.DirtyGenerationID,
			before.CommitGenerationID, before.DirtyGenerationID)
	}

	after := f.route()
	if after.CommitGenerationID == before.CommitGenerationID ||
		after.DirtyGenerationID == before.DirtyGenerationID {
		t.Fatalf("the recomposition left a layer of the old pair routed: %+v", after)
	}
	if after.State != store_sqlite.RouteActive {
		t.Fatalf("the recomposed route is %q, want active", after.State)
	}

	// The composed identity pair: a new delta over the NEW base, describing
	// the dependent's own unchanged tree. Never the new base under the old
	// delta, and never the old base under the new one.
	commitRow, found := f.generation(after.CommitGenerationID)
	if !found {
		t.Fatalf("the recomposed commit generation %d does not exist", after.CommitGenerationID)
	}
	if commitRow.LowerViewFingerprint != newBaseTree {
		t.Fatalf("the recomposed delta names base tree %q, want the advanced %q",
			commitRow.LowerViewFingerprint, newBaseTree)
	}
	if commitRow.TreeOID != f.treeA {
		t.Fatalf("the recomposed delta describes tree %q, want the dependent's own %q",
			commitRow.TreeOID, f.treeA)
	}
	oldCommit, found := f.generation(before.CommitGenerationID)
	if !found || oldCommit.LowerViewFingerprint == newBaseTree {
		t.Fatalf("the old delta was rewritten onto the new base: found=%v %+v", found, oldCommit)
	}
	dirtyRow, found := f.generation(after.DirtyGenerationID)
	if !found || dirtyRow.BaseGenerationID != after.CommitGenerationID {
		t.Fatalf("the recomposed working-tree layer %d sits on generation %d, want %d",
			after.DirtyGenerationID, dirtyRow.BaseGenerationID, after.CommitGenerationID)
	}

	// The pair the recomposition replaced is RELEASED into the reuse caches,
	// not retired. The old working-tree layer names the old commit layer as
	// its base, so it is re-routable exactly when that base is routed again —
	// a revert of the advance, or a rehome that lands back where it started —
	// and retiring it would make that case pay a full working-tree index.
	// cachedDirty fails closed on a collected generation, so a hit here is
	// proof the payload survived.
	ctx := context.Background()
	if _, ok := c.cachedDirty(ctx, replacedDirtyKey); !ok {
		t.Fatalf("the replaced working-tree layer %d was not released into the reuse cache",
			before.DirtyGenerationID)
	}
	// And the recomposed one is filed too, under the key its own row renders.
	if cached, ok := c.cachedDirty(ctx, generationRowKey(dirtyRow)); !ok || cached != after.DirtyGenerationID {
		t.Fatalf("the recomposed working-tree layer is not re-routable: cached=%d ok=%v", cached, ok)
	}

	// Exactly once. The next cycle finds the route already describing the
	// state the checkout is in.
	settled := coordinatorReconcile(t, c)
	if settled.CommitBuilt || settled.DirtyBuilt || settled.Recomposed {
		t.Fatalf("a second cycle rebuilt after the recomposition: %+v", settled)
	}
	if now := f.route(); now != after {
		t.Fatalf("the settled cycle moved the route: %+v, want %+v", now, after)
	}
}

// buildSeconds reads how many builds one slot has observed. The duration
// series' COUNT is the committed-base build counter: resolveCommitLayer
// observes one sample per commit-layer build and buildDirtyLayerOver one
// per working-tree build attempt, so a delta of 1 is one build.
func buildSeconds(s viewmetrics.Snapshot, slot string) int64 {
	return s.Durations[viewmetrics.CoordinatorBuildSeconds+"{"+viewmetrics.LabelSlot+"="+slot+"}"].Count
}

// generationCensus counts the store's generations by kind.
func generationCensus(f *coordinatorFixture) map[string]int {
	out := map[string]int{}
	for _, row := range f.generations() {
		out[row.GenerationKind]++
	}
	return out
}

// dirtyGraphPaths is the change set a working-tree build of this checkout
// would be given: the checkout's own dirty diff against its HEAD, spelled in
// graph paths. It is read through the same two functions the builder uses
// (gitstate.SampleDirty, dirtyLayerChanges), so it is the build's own input
// set rather than a restatement of it.
func dirtyGraphPaths(t *testing.T, root string) []string {
	t.Helper()
	snap, err := gitstate.SampleDirty(context.Background(), root)
	if err != nil {
		t.Fatalf("sample %s: %v", root, err)
	}
	var out []string
	for _, change := range dirtyLayerChanges(snap) {
		out = append(out, builderGraphPath(builderRepoPrefix, change.Path))
	}
	sort.Strings(out)
	return out
}

// TestBaseAdvanceRecomposesTwoDependentsOnceEachWithABoundedDelta is the
// multi-dependent shape, and the one that measures what a recomposition costs.
//
// Ten is the workload the gate names; two is what makes the same statement
// testable in a fixture. What it pins is the whole of gate 5's "while reusing
// valid payload" as this path delivers it (recomposeOverAdvancedBase's doc
// comment states why bounded recomposition is the available reading and a pin
// on the old base is not):
//
//   - EXACTLY two commit-delta builds and two working-tree builds for one
//     advance across two dependents — counted twice over, by the
//     committed-base build counters and by the generations that appeared in
//     the store;
//   - each commit delta claims exactly the paths its own two committed trees
//     differ by — not an index of its tree;
//   - each working-tree layer claims exactly the paths its own dirty set
//     names, and the two dependents' dirty sets are deliberately different, so
//     a layer that widened to the other's file or to the tree would be caught;
//   - the pass after it writes nothing at all.
//
// Revert-red: widen either build to a full index — build the delta from the
// empty tree, or hand the working-tree build the whole tree as its change set
// — and the claimed-path assertions fail.
func TestBaseAdvanceRecomposesTwoDependentsOnceEachWithABoundedDelta(t *testing.T) {
	f := newCoordinatorFixture(t)
	ctx := context.Background()
	first := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	second, secondRoot := f.secondWorktree(t, "second", nil)

	dependents := []struct {
		name string
		c    *CheckoutCoordinator
		root string
		// dirtyFile is this dependent's OWN uncommitted change. The two differ
		// so that a working-tree layer which claimed more than its own set
		// would name a path this dependent has never had.
		dirtyFile, dirtySymbol string
	}{
		{"feature", first, f.worktree, "feature_only.go", "FeatureOnly"},
		{"second", second, secondRoot, "second_only.go", "SecondOnly"},
	}
	initial := map[string]CheckoutCycle{}
	for _, dependent := range dependents {
		// An untracked file that references nothing and that nothing
		// references: the dirty change set is exactly one path, and the
		// closure has no edge to widen it along.
		builderWriteFile(t, dependent.root, dependent.dirtyFile,
			"package fixture\n\nfunc "+dependent.dirtySymbol+"() {}\n")
		out := coordinatorReconcile(t, dependent.c)
		if !out.CommitBuilt || !out.DirtyBuilt {
			t.Fatalf("%s did not build its stack: %+v", dependent.name, out)
		}
		initial[dependent.name] = out
	}

	newBaseTree, advancedFile := advancePrimaryBase(t, f)

	beforeMetrics := viewmetrics.Read()
	beforeCensus := generationCensus(f)
	for _, dependent := range dependents {
		out := coordinatorReconcile(t, dependent.c)
		if !out.Recomposed || !out.CommitBuilt || !out.DirtyBuilt {
			t.Fatalf("%s did not recompose over the advanced base: %+v", dependent.name, out)
		}
		if out.CommitGenerationID == initial[dependent.name].CommitGenerationID {
			t.Fatalf("%s kept its old delta over the advanced base: %+v", dependent.name, out)
		}

		// Bounded: the recomposed delta claims the paths the two committed
		// trees differ by, and nothing else. Here that is the one file the
		// base advance added and the dependent does not have.
		changes, err := diffTreeChanges(ctx, dependent.root, newBaseTree, f.treeA)
		if err != nil {
			t.Fatalf("%s: diff the advanced base against its own tree: %v", dependent.name, err)
		}
		if len(changes) != 1 || !strings.HasSuffix(changes[0].Path, advancedFile) {
			t.Fatalf("%s: the base advance is not the one-file difference this test needs: %+v",
				dependent.name, changes)
		}
		claimed := claimedPaths(t, f.store, out.CommitGenerationID)
		if len(claimed) != len(changes) {
			t.Fatalf("%s: the recomposed delta claims %d paths %v, want the %d the trees differ by",
				dependent.name, len(claimed), claimed, len(changes))
		}
		for _, path := range claimed {
			if !strings.HasSuffix(path, advancedFile) {
				t.Fatalf("%s: the recomposed delta claims %q, which the two trees do not differ by",
					dependent.name, path)
			}
		}

		// And the working-tree layer over it is bounded by this dependent's
		// OWN dirty set, which is one untracked file.
		want := dirtyGraphPaths(t, dependent.root)
		if len(want) != 1 || !strings.HasSuffix(want[0], dependent.dirtyFile) {
			t.Fatalf("%s: the dirty set is not the one-file change this test needs: %v",
				dependent.name, want)
		}
		got := claimedPaths(t, f.store, out.DirtyGenerationID)
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Fatalf("%s: the recomposed working-tree layer claims %v, want its own dirty set %v",
				dependent.name, got, want)
		}
		// And it did not merely decline to CLAIM what it indexed: the file
		// set the pass reached — claims plus read-only context — is the same
		// one set. An untracked file nothing references drags nothing in.
		reached := closurePaths(t, f.store, out.DirtyGenerationID)
		if !slices.Equal(reached, want) {
			t.Fatalf("%s: the recomposed working-tree layer indexed %v, want its own dirty set %v",
				dependent.name, reached, want)
		}
	}

	// Two dependents, one advance: two commit-delta builds and two
	// working-tree builds. Not four, and not one apiece plus a retry.
	afterMetrics := viewmetrics.Read()
	for slot, want := range map[string]int64{viewmetrics.SlotCommit: 2, viewmetrics.SlotDirty: 2} {
		if got := buildSeconds(afterMetrics, slot) - buildSeconds(beforeMetrics, slot); got != want {
			t.Fatalf("the advance ran %d %s-slot builds, want %d", got, slot, want)
		}
	}
	// The cycle-outcome counters are recorded by cycle(), which these tests
	// deliberately do not drive — reconcile is called directly so the
	// decisions are asserted without a clock. What the build-slot durations
	// above count is the builds themselves, inside resolveCommitLayer and
	// buildDirtyLayerOver, which is the measure this bound needs.
	afterCensus := generationCensus(f)
	for kind, want := range map[string]int{CommitLayerGenerationKind: 2, DirtyLayerGenerationKind: 2} {
		if got := afterCensus[kind] - beforeCensus[kind]; got != want {
			t.Fatalf("the advance left %d new %s generations, want %d", got, kind, want)
		}
	}

	// Once each: a second pass over both dependents builds nothing, and writes
	// nothing. The audit covers the payload tables and view_generations;
	// checkout_routes is outside it, and the settled pass does not touch that
	// either.
	writes := installCheckoutLayerWriteAudit(t, f.storePath)
	for _, dependent := range dependents {
		out := coordinatorReconcile(t, dependent.c)
		if out.CommitBuilt || out.DirtyBuilt || out.Recomposed {
			t.Fatalf("%s rebuilt on a second cycle after the advance: %+v", dependent.name, out)
		}
	}
	if counted := writes(t); counted != 0 {
		t.Fatalf("the settled pass after the recomposition made %d payload/generation writes, want none", counted)
	}
}

// TestRecompositionRefusesAnythingButABaseAdvance pins the narrowness of the
// path.
//
// Recomposing is only sound because the checkout's own state did not move: the
// pair it is serving still describes its tree, so keeping it routed through the
// rebuild is truthful. Everything else — a checkout that moved its own HEAD, a
// working tree that moved, an identity that differs by configuration or cohort
// rather than by its base — has to fall through to the ordinary path, which
// withdraws what no longer describes anything before it rebuilds.
//
// Every refusal but the first is posed over the ADVANCED base, deliberately.
// Over the un-advanced base the settled-identity clause refuses everything on
// its own, so a case posed there proves nothing about the clause it is named
// after: the guard it claims to pin could be deleted and the case would still
// pass. Only a base that really has moved reaches the clauses that decide
// whether THIS checkout also moved.
func TestRecompositionRefusesAnythingButABaseAdvance(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()

	first := coordinatorReconcile(t, c)
	if first.CommitGenerationID == 0 || first.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle routed no pair: %+v", first)
	}
	settledBase, err := c.primaryBase(ctx)
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}
	sample, err := c.sampler.Sample(ctx)
	if err != nil {
		t.Fatalf("sample the checkout: %v", err)
	}
	route := f.route()

	// Nothing moved at all: the ordinary path recognises this in one read.
	if _, _, ok, err := c.recomposableStack(ctx, settledBase, sample, route); err != nil || ok {
		t.Fatalf("a settled checkout was offered to the recomposition path (ok=%v err=%v)", ok, err)
	}

	advancePrimaryBase(t, f)
	advanced, err := c.primaryBase(ctx)
	if err != nil {
		t.Fatalf("primaryBase after the advance: %v", err)
	}
	if advanced == settledBase {
		t.Fatalf("the base did not advance: still %+v", advanced)
	}

	// The base advance alone is the whole of the path's remit.
	if _, _, ok, err := c.recomposableStack(ctx, advanced, sample, route); err != nil || !ok {
		t.Fatalf("a pure base advance was refused by the recomposition path (ok=%v err=%v)", ok, err)
	}

	// The checkout's own tree moved under the advance: its delta describes a
	// tree it has left, and keeping the pair routed through a rebuild would be
	// untruthful.
	movedHead := sample
	movedHead.HeadTree = "0000000000000000000000000000000000000000"
	if _, _, ok, _ := c.recomposableStack(ctx, advanced, movedHead, route); ok {
		t.Fatalf("a checkout that moved its own HEAD over an advanced base was offered to the recomposition path")
	}

	// The working tree moved under the advance: the routed working-tree layer
	// no longer describes what is on disk, so there is nothing to keep
	// serving and the ordinary path has to withdraw it.
	movedTree := sample
	movedTree.Fingerprint += "-moved"
	if _, _, ok, _ := c.recomposableStack(ctx, advanced, movedTree, route); ok {
		t.Fatalf("a moved working tree over an advanced base was offered to the recomposition path")
	}

	// The identity moved by something other than the base. A configuration
	// change re-keys every layer, and a layer built under other rules is not a
	// recomposition candidate — it is a rebuild.
	c.configHash += "-changed"
	if _, _, ok, _ := c.recomposableStack(ctx, advanced, sample, route); ok {
		t.Fatalf("a configuration change was offered to the recomposition path")
	}
}

// TestRecompositionRefusesToInstallOverABaseThatMovedUnderIt pins the
// post-build base-move guard.
//
// A recomposition reads the family's base, builds a delta against it and a
// working-tree layer over that delta, and only then writes the route. The
// family is free to advance again inside that window — the primary commits
// while the dependent is being rebuilt — and installing afterwards would route
// a delta over a base the family has already left: exactly the splice this
// path exists to avoid, and one the ordinary slot path makes its own
// equivalent guard against (reconcileCommitSlot's errBaseMoved).
//
// The window is driven with the seam the build already has. dirtyBarrier fires
// inside BuildDirtyLayer, after the commit delta is built and before
// recomposeOverAdvancedBase re-reads the base, so a barrier that advances the
// primary a second time puts the cycle exactly in it.
//
// Revert-red: make baseMovedUnderCycle's answer in recomposeOverAdvancedBase
// unconditionally false and the stack is installed over the base the family
// left — the route moves, and this test fails on it.
func TestRecompositionRefusesToInstallOverABaseThatMovedUnderIt(t *testing.T) {
	var (
		mu    sync.Mutex
		armed bool
		fired int
	)
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{dirtyBarrier: func() {
		mu.Lock()
		defer mu.Unlock()
		if !armed || fired > 0 {
			return
		}
		fired++
		// The family advances again while this cycle's pair is being built.
		advancePrimaryBaseWith(t, f, "advanced_again.go", "AdvancedAgain")
	}})

	first := coordinatorReconcile(t, c)
	if !first.CommitBuilt || !first.DirtyBuilt {
		t.Fatalf("the first cycle did not build the stack: %+v", first)
	}
	before := f.route()

	overtakenTree, _ := advancePrimaryBase(t, f)
	mu.Lock()
	armed = true
	mu.Unlock()

	beforeMetrics := viewmetrics.Read()
	out := coordinatorReconcile(t, c)

	mu.Lock()
	barrierFired := fired
	mu.Unlock()
	if barrierFired != 1 {
		t.Fatalf("the working-tree build reached the barrier %d times; the window was never opened", barrierFired)
	}
	if out.Recomposed {
		t.Fatalf("the cycle installed a stack over a base the family had left: %+v", out)
	}
	if !out.Rescheduled {
		t.Fatalf("the refusal did not ask for another window: %+v", out)
	}
	if now := f.route(); now != before {
		t.Fatalf("the refused recomposition moved the route: %+v, was %+v", now, before)
	}

	// Nothing built against the base that was overtaken is left servable: both
	// halves of the abandoned pair are superseded and offered.
	for _, row := range f.generations() {
		if row.GenerationKind != CommitLayerGenerationKind || row.GenerationID == before.CommitGenerationID {
			continue
		}
		if row.LowerViewFingerprint == overtakenTree && servableGeneration(row.State) {
			t.Fatalf("a delta built over the base the family left is still servable: %+v", row)
		}
	}
	after := viewmetrics.Read()
	if got := counterDelta(beforeMetrics, after, cycleKey(viewmetrics.OutcomeRescheduled)); got != 1 {
		t.Fatalf("the refusal counted %d rescheduled cycles, want 1", got)
	}
	if len(c.signal) != 1 {
		t.Fatal("the coordinator did not signal itself for another window")
	}
}

// TestRecompositionStopsWhenTheCheckoutCommitsUnderIt pins the other
// post-build guard: the checkout's own HEAD moving between the sample the
// cycle planned against and the one taken after the commit delta is built.
//
// A commit layer takes as long as the tree it spans, and a checkout is free to
// commit while one is built for it. The working-tree layer that would go on
// top describes the tree of a HEAD the layer beneath knows nothing about, so
// the cycle stops before it builds one and the next cycle rebuilds for the
// head the checkout is really at — the same decision, and the same counter,
// reconcileDirtySlot makes for itself.
//
// The window has no seam of its own: it closes between the commit build and
// the working-tree build, and dirtyBarrier fires inside the latter. So the
// entry point is called directly with the sample the cycle would have carried,
// which is what a cycle overtaken by a commit really holds. The call from
// reconcile is pinned separately, by the recomposition tests above.
//
// Revert-red: delete the sample.HeadTree != head.HeadTree arm and the cycle
// builds a working-tree layer for tree B over a delta describing tree A, and
// routes the pair.
func TestRecompositionStopsWhenTheCheckoutCommitsUnderIt(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()

	first := coordinatorReconcile(t, c)
	if !first.CommitBuilt || !first.DirtyBuilt {
		t.Fatalf("the first cycle did not build the stack: %+v", first)
	}
	before := f.route()

	newBaseTree, _ := advancePrimaryBase(t, f)
	advanced, err := c.primaryBase(ctx)
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}
	// The sample this cycle plans against: the checkout as it is now.
	head, err := c.sampler.Sample(ctx)
	if err != nil {
		t.Fatalf("sample the checkout: %v", err)
	}
	// ... and the checkout commits before the working-tree layer is built.
	treeB := f.commitTreeB()
	if treeB == head.HeadTree {
		t.Fatalf("the checkout did not move off %s", head.HeadTree)
	}

	route := before
	var out CheckoutCycle
	beforeMetrics := viewmetrics.Read()
	handled, err := c.recomposeOverAdvancedBase(ctx, advanced, head, &route, &out)
	if err != nil {
		t.Fatalf("recomposeOverAdvancedBase: %v", err)
	}
	if !handled {
		t.Fatalf("the recomposition path declined a pure base advance: %+v", out)
	}
	if out.Recomposed {
		t.Fatalf("the cycle routed a working-tree layer over a delta the checkout had left: %+v", out)
	}
	if !out.Rescheduled {
		t.Fatalf("the refusal did not ask for another window: %+v", out)
	}
	if now := f.route(); now != before {
		t.Fatalf("the refused recomposition moved the route: %+v, was %+v", now, before)
	}
	if got := counterDelta(beforeMetrics, viewmetrics.Read(), cycleKey(viewmetrics.OutcomeHeadMoved)); got != 1 {
		t.Fatalf("the refusal counted %d head_moved cycles, want 1", got)
	}
	// The delta built for the tree the checkout has left is not servable.
	for _, row := range f.generations() {
		if row.GenerationKind != CommitLayerGenerationKind || row.GenerationID == before.CommitGenerationID {
			continue
		}
		if row.LowerViewFingerprint == newBaseTree && row.TreeOID == head.HeadTree && servableGeneration(row.State) {
			t.Fatalf("the abandoned delta is still servable: %+v", row)
		}
	}
}

// TestRecompositionRefusesAWorkingTreeLayerOverAnotherCommitLayer pins the
// clause that says the routed pair has to BE a pair.
//
// A working-tree layer names the commit layer it was built over as its base.
// Recomposing keeps that layer serving for the whole of the rebuild, which is
// only truthful while it really sits on the commit layer the route names
// beside it; a layer over some other commit generation is one branch's
// uncommitted edits over another branch's tree, which is the state
// moveCommitSlot clears the dirty slot to avoid ever routing.
//
// moveCommitSlot's clearing is why production cannot reach this shape, and
// why it has to be posed directly: the pair is assembled from two real
// generations this coordinator built, one of them the working-tree layer an
// earlier recomposition replaced.
//
// Revert-red: delete dirtyRow.BaseGenerationID != commitRow.GenerationID from
// recomposableStack and the mismatched pair is admitted.
func TestRecompositionRefusesAWorkingTreeLayerOverAnotherCommitLayer(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()

	first := coordinatorReconcile(t, c)
	if first.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle routed no working-tree layer: %+v", first)
	}
	// The layer the recomposition below replaces: it sits on the FIRST commit
	// generation, and it describes the same (clean) working tree, so the only
	// thing separating it from the routed one is the commit layer it names.
	strandedDirty := first.DirtyGenerationID

	advancePrimaryBase(t, f)
	recomposed := coordinatorReconcile(t, c)
	if !recomposed.Recomposed {
		t.Fatalf("the base advance did not recompose: %+v", recomposed)
	}
	if _, found := f.generation(strandedDirty); !found {
		t.Fatalf("the replaced working-tree layer %d was collected; this test needs it", strandedDirty)
	}

	// A second advance, so the routed pair is a recomposition candidate again.
	advancePrimaryBaseWith(t, f, "advanced_again.go", "AdvancedAgain")
	advanced, err := c.primaryBase(ctx)
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}
	sample, err := c.sampler.Sample(ctx)
	if err != nil {
		t.Fatalf("sample the checkout: %v", err)
	}
	route := f.route()

	// The control: the real pair IS admitted, so what the refusal below
	// separates is the mismatched base and nothing else about the fixture.
	if _, _, ok, err := c.recomposableStack(ctx, advanced, sample, route); err != nil || !ok {
		t.Fatalf("the routed pair was refused by the recomposition path (ok=%v err=%v)", ok, err)
	}

	mismatched := route
	mismatched.DirtyGenerationID = strandedDirty
	if _, _, ok, err := c.recomposableStack(ctx, advanced, sample, mismatched); err != nil || ok {
		t.Fatalf("a working-tree layer over another commit layer was offered to the recomposition path (ok=%v err=%v)",
			ok, err)
	}
}
