package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The in-process half of working-tree-layer reuse.
//
// A coordinator that retires the layer its route just left re-indexes the
// working tree the moment the tree comes back to a state it has already
// described. Undo/redo is that case; so is a branch switch back onto a
// worktree whose uncommitted edits never moved. These tests drive the
// production reconcile path and assert that the retained layer is re-routed
// rather than rebuilt, that the re-route costs no payload write, and that the
// cache refuses anything it cannot still prove.
//
// The catalog-backed, survive-restart half is NOT implemented and is a
// declared limitation of the item: a daemon restart between two identical
// dirty states pays one rebuild. TestCoordinatorAdoptsAStoredCommitLayerAfterRestart
// is the commit-layer twin that does survive, for contrast.

// editWorktreeFile overwrites one file in the checkout and returns a func that
// puts the original bytes back. Restoring the bytes is what makes the working
// tree's content fingerprint equal to what it was: the fingerprint hashes the
// reported effective content over HEAD's tree, so a file whose bytes are back
// to HEAD's leaves git reporting nothing at all.
func editWorktreeFile(t *testing.T, root, name, content string) func() {
	t.Helper()
	path := filepath.Join(root, name)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return func() {
		t.Helper()
		if err := os.WriteFile(path, before, 0o644); err != nil {
			t.Fatalf("restore %s: %v", path, err)
		}
	}
}

const editedIsland = `package fixture

func Island() {
	Helper()
}
`

// TestUndoReusesTheRetainedDirtyLayer is the dirty-layer reuse acceptance case.
//
// Edit, reconcile, undo, reconcile. The second reconcile describes a working
// tree this coordinator has already indexed over the same commit layer, under
// the same configuration and the same cohort — the same build identity, by the
// same renderer the catalog and the commit-layer cache compare — so the layer
// is re-routed.
//
// Revert-red: delete the cachedDirty arm from reconcileDirtySlot and the undo
// reports DirtyBuilt; change releaseDirty back to offerRetire in
// reconcileDirtySlot and the layer the undo wants has been collected.
func TestUndoReusesTheRetainedDirtyLayer(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	clean := coordinatorReconcile(t, c)
	if !clean.DirtyBuilt || clean.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle did not build a working-tree layer: %+v", clean)
	}
	cleanDirty := clean.DirtyGenerationID

	restore := editWorktreeFile(t, f.worktree, "island.go", editedIsland)
	edited := coordinatorReconcile(t, c)
	if !edited.DirtyBuilt || edited.DirtyGenerationID == cleanDirty {
		t.Fatalf("the edit did not build a new working-tree layer: %+v", edited)
	}

	restore()
	undone := coordinatorReconcile(t, c)
	if undone.DirtyBuilt {
		t.Fatalf("the undo re-indexed the working tree: %+v", undone)
	}
	if !undone.DirtyReused {
		t.Fatalf("the undo did not report a working-tree reuse: %+v", undone)
	}
	if undone.DirtyGenerationID != cleanDirty {
		t.Fatalf("the undo routed generation %d, want the retained %d",
			undone.DirtyGenerationID, cleanDirty)
	}
	if got := f.route().DirtyGenerationID; got != cleanDirty {
		t.Fatalf("the route names working-tree generation %d, want %d", got, cleanDirty)
	}
	row, found := f.generation(cleanDirty)
	if !found || !servableGeneration(row.State) {
		t.Fatalf("the re-routed generation %d is not servable (found=%v state=%q)",
			cleanDirty, found, row.State)
	}

	// Two working-tree states visited three times is two working-tree layers.
	dirty := 0
	for _, row := range f.generations() {
		if row.GenerationKind == DirtyLayerGenerationKind {
			dirty++
		}
	}
	if dirty != 2 {
		t.Fatalf("%d working-tree generations exist for two states visited three times", dirty)
	}
}

// TestDirtyLayerReuseWritesNothingBeyondTheRouteFlip measures the reuse path.
//
// Reuse that costs a write is not reuse. The audit covers view_generations and
// the payload tables; checkout_routes is deliberately outside it, because the
// route flip is the one write the adoption is allowed to make.
func TestDirtyLayerReuseWritesNothingBeyondTheRouteFlip(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	clean := coordinatorReconcile(t, c)
	if !clean.DirtyBuilt {
		t.Fatalf("the first cycle did not build a working-tree layer: %+v", clean)
	}
	restore := editWorktreeFile(t, f.worktree, "island.go", editedIsland)
	if edited := coordinatorReconcile(t, c); !edited.DirtyBuilt {
		t.Fatalf("the edit did not build a working-tree layer: %+v", edited)
	}
	restore()

	writes := installCheckoutLayerWriteAudit(t, f.storePath)
	undone := coordinatorReconcile(t, c)
	if !undone.DirtyReused || undone.DirtyBuilt {
		t.Fatalf("the measured cycle did not take the reuse path: %+v", undone)
	}
	if got := writes(t); got != 0 {
		t.Fatalf("the working-tree reuse made %d payload writes, want 0", got)
	}
}

// TestDirtyLayerSurvivesACommitSlotMoveAndIsReusedOnTheWayBack is the
// cached-tree-switch case, and the one that pins moveCommitSlot's release.
//
// The commit slot moving drops the working-tree slot in the same write,
// because a working-tree layer over a DIFFERENT commit layer is a state the
// checkout was never in. That is a reason to stop ROUTING the layer, not a
// reason to destroy it: the pair it forms with the commit layer it names is
// exactly what a switch back composes again. Here the checkout commits, then
// comes back to the tree and the working-tree state it had — and pays for
// neither.
//
// Revert-red: change releaseDirty back to offerRetire in moveCommitSlot and
// the layer is gone by the time the switch back asks for it.
func TestDirtyLayerSurvivesACommitSlotMoveAndIsReusedOnTheWayBack(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	commitA := builderGit(t, f.worktree, "rev-parse", "HEAD")

	if first := coordinatorReconcile(t, c); !first.CommitBuilt || !first.DirtyBuilt {
		t.Fatalf("the first cycle did not build the stack: %+v", first)
	}
	editWorktreeFile(t, f.worktree, "island.go", editedIsland)
	edited := coordinatorReconcile(t, c)
	if !edited.DirtyBuilt {
		t.Fatalf("the edit did not build a working-tree layer: %+v", edited)
	}
	editedDirty := edited.DirtyGenerationID
	commitGeneration := edited.CommitGenerationID

	// Commit it. The commit slot moves, which withdraws the working-tree slot.
	builderGit(t, f.worktree, "add", "-A")
	builderGit(t, f.worktree, "commit", "-m", "island calls helper")
	committed := coordinatorReconcile(t, c)
	if !committed.CommitBuilt || committed.CommitGenerationID == commitGeneration {
		t.Fatalf("the commit did not build a new commit layer: %+v", committed)
	}
	if committed.DirtyGenerationID == editedDirty {
		t.Fatalf("the commit left the old working-tree layer routed: %+v", committed)
	}
	// Released, not retired: the layer is still servable AND the coordinator
	// is not owing a collection for it. Both halves are asserted because a
	// retirement can end either way — collected outright, or refused onto the
	// backlog the janitor insists on — and "released" means neither happened.
	if row, found := f.generation(editedDirty); !found || !servableGeneration(row.State) {
		t.Fatalf("the commit-slot move destroyed working-tree generation %d (found=%v state=%q)",
			editedDirty, found, row.State)
	}
	c.mu.Lock()
	_, offered := c.backlog[editedDirty]
	c.mu.Unlock()
	if offered {
		t.Fatalf("the commit-slot move offered the retained working-tree layer %d for collection",
			editedDirty)
	}

	// Back to the tree the layer was built over, with the same edit on top.
	builderGit(t, f.worktree, "reset", "--hard", commitA)
	editWorktreeFile(t, f.worktree, "island.go", editedIsland)
	back := coordinatorReconcile(t, c)
	if !back.CommitReused || back.CommitGenerationID != commitGeneration {
		t.Fatalf("the switch back did not re-route the commit layer %d: %+v",
			commitGeneration, back)
	}
	if back.DirtyBuilt {
		t.Fatalf("the switch back re-indexed the working tree: %+v", back)
	}
	if !back.DirtyReused || back.DirtyGenerationID != editedDirty {
		t.Fatalf("the switch back routed working-tree generation %d, want the retained %d: %+v",
			back.DirtyGenerationID, editedDirty, back)
	}
	route := f.route()
	if route.CommitGenerationID != commitGeneration || route.DirtyGenerationID != editedDirty {
		t.Fatalf("the route names (%d, %d), want the retained pair (%d, %d)",
			route.CommitGenerationID, route.DirtyGenerationID, commitGeneration, editedDirty)
	}
}

// TestDirtyLayerCacheRefusesWhatItCannotStillProve pins the fail-closed half
// of the cache, the way cachedCommit's re-check is pinned for the commit half.
//
// A cache entry is a claim about a row, and the row can be retired, superseded
// out of servability or re-keyed under the entry. Every one of those has to
// end in a rebuild rather than in a route flip onto a payload that is not what
// the entry said it was.
func TestDirtyLayerCacheRefusesWhatItCannotStillProve(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()

	first := coordinatorReconcile(t, c)
	if first.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle routed no working-tree layer: %+v", first)
	}

	// A generation that does not exist.
	c.retainDirty(ctx, "ghost", 4242)
	if id, ok := c.cachedDirty(ctx, "ghost"); ok {
		t.Fatalf("the cache offered generation %d for an id no row holds", id)
	}
	for _, entry := range c.retainedDirty {
		if entry.key == "ghost" {
			t.Fatalf("the cache kept an entry it could not prove: %+v", entry)
		}
	}

	// A real, servable generation filed under a key its row does not render.
	c.retainDirty(ctx, "not-the-identity", first.DirtyGenerationID)
	if id, ok := c.cachedDirty(ctx, "not-the-identity"); ok {
		t.Fatalf("the cache offered generation %d under a foreign key", id)
	}
	for _, entry := range c.retainedDirty {
		if entry.key == "not-the-identity" {
			t.Fatalf("the cache kept a foreign-key entry: %+v", entry)
		}
	}

	// An empty key is not an identity. What this binds is the RENDERER: an
	// unborn sample has no tree and no fingerprint, and dirtySampleKey has to
	// refuse it rather than render the identity with those fields blank —
	// which is a perfectly good-looking key that two unrelated unborn states
	// would share. The two early returns in retainDirty and cachedDirty are
	// knowingly redundant with the row re-key check above (no row renders the
	// empty key, so a lookup fails closed there anyway) and are documented as
	// such at the cache; the assertion below is the end-to-end statement that
	// the cache never answers one, however it got there.
	if key := c.dirtySampleKey(f.graphID, first.CommitGenerationID, gitstate.DirtySnapshot{}); key != "" {
		t.Fatalf("an unborn sample rendered the key %q", key)
	}
	c.retainDirty(ctx, "", first.DirtyGenerationID)
	if _, ok := c.cachedDirty(ctx, ""); ok {
		t.Fatalf("the cache answered an empty key")
	}
}

// TestRetainedDirtyLayersAreDrainedOnTeardown is the leak guard.
//
// The retained layers are the only record of a payload once the route row is
// withdrawn: nothing else in the catalog names a checkout's generations. A
// drain that forgot them would leave a payload in the database with no id
// anything could offer for collection.
func TestRetainedDirtyLayersAreDrainedOnTeardown(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	clean := coordinatorReconcile(t, c)
	restore := editWorktreeFile(t, f.worktree, "island.go", editedIsland)
	edited := coordinatorReconcile(t, c)
	restore()
	if clean.DirtyGenerationID == 0 || edited.DirtyGenerationID == 0 {
		t.Fatalf("the fixture built no pair of working-tree layers: %+v / %+v", clean, edited)
	}

	drained := map[int64]bool{}
	for _, generationID := range c.DrainRetirements() {
		drained[generationID] = true
	}
	for _, generationID := range []int64{clean.DirtyGenerationID, edited.DirtyGenerationID} {
		if !drained[generationID] {
			t.Fatalf("the drain forgot working-tree generation %d: %v", generationID, drained)
		}
	}
	if len(c.retainedDirty) != 0 {
		t.Fatalf("the drain left %d retained working-tree layers", len(c.retainedDirty))
	}
}

// --- the ref-fact hint scope --------------------------------------------

// TestCommitLayerReaderComposesRefFactHintsOverTheAncestry pins what a dirty
// build's base reads its reference-fact hints from.
//
// ref_facts rows carry a view_gen column and a store handle answers with one
// generation's rows alone, so a base handed a single generation handle asks
// its fact question of one layer of a stack the structural reads compose in
// full. Every fact in the database today was written by the legacy indexer at
// generation zero — the sparse generation builder writes none — so a handle
// pinned to a published committed base answers "no facts" for every file in
// the repository, and the closure silently narrows.
//
// Revert-red: put `corpus: c.store.AtGeneration(row.BaseGenerationID)` back in
// commitLayerReader and the composed handle list is empty.
func TestCommitLayerReaderComposesRefFactHintsOverTheAncestry(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()

	first := coordinatorReconcile(t, c)
	if first.CommitGenerationID == 0 {
		t.Fatalf("the first cycle routed no commit layer: %+v", first)
	}

	corpusFacts := seedCorpusRefFacts(t, f)
	// The blindness the composition fixes: the generation's own handle holds
	// none of them.
	generationFacts, err := f.store.AtGeneration(first.CommitGenerationID).
		LoadRefFactsByFiles(builderRepoPrefix, nil)
	if err != nil {
		t.Fatalf("read generation %d's facts: %v", first.CommitGenerationID, err)
	}
	if len(generationFacts) != 0 {
		t.Fatalf("generation %d holds %d facts of its own; the single-handle scope is no longer blind and this test needs rewriting",
			first.CommitGenerationID, len(generationFacts))
	}

	base, release, err := c.commitLayerReader(ctx, first.CommitGenerationID)
	if err != nil {
		t.Fatalf("commitLayerReader: %v", err)
	}
	defer release()
	layer, ok := base.(commitLayerBase)
	if !ok {
		t.Fatalf("the dirty build's base is a %T, not a composed commit-layer base", base)
	}
	if len(layer.facts.handles) == 0 {
		t.Fatalf("the dirty build's base reads its fact hints from a single generation handle")
	}
	reader, ok := base.(graph.RefFactsReader)
	if !ok {
		t.Fatalf("the dirty build's base is not a RefFactsReader")
	}
	composed, err := reader.LoadRefFactsByFiles(builderRepoPrefix, nil)
	if err != nil {
		t.Fatalf("read the composed facts: %v", err)
	}
	if len(composed) < len(corpusFacts) {
		t.Fatalf("the composed hints hold %d facts, fewer than the %d the corpus under them holds",
			len(composed), len(corpusFacts))
	}
}

// seedCorpusRefFacts writes reference facts into the base corpus and returns
// them.
//
// They are written rather than assumed: the sidecar is populated by the legacy
// indexer's own incremental and full paths (ref_facts.go
// persistRefFactsForFiles), which this fixture's direct index does not run, and
// a test that asserted on whatever the fixture happened to persist would assert
// on nothing at all. What the rows say does not matter here — the question is
// which generation handle can see them.
//
// A published generation cannot be seeded: its payload is sealed
// (payload_generation.go refuseSealedPayloadWrite), which is exactly why the
// ancestry has to be composed rather than re-keyed.
func seedCorpusRefFacts(t *testing.T, f *coordinatorFixture) []graph.RefFact {
	t.Helper()
	facts := []graph.RefFact{
		{
			FromID: builderRepoPrefix + "caller.go::Run", ToID: builderRepoPrefix + "core.go::Compute",
			Kind: "calls", RefName: "Compute", Line: 4, Origin: "ast_resolved", Tier: "resolved",
			FilePath: "caller.go", Lang: "go",
		},
		{
			FromID: builderRepoPrefix + "core.go::Compute", ToID: builderRepoPrefix + "helper.go::Helper",
			Kind: "calls", RefName: "Helper", Line: 6, Origin: "ast_resolved", Tier: "resolved",
			FilePath: "core.go", Lang: "go",
		},
	}
	if err := f.store.AtGeneration(0).BulkSetRefFacts(builderRepoPrefix, facts); err != nil {
		t.Fatalf("seed the corpus reference facts: %v", err)
	}
	stored, err := f.store.AtGeneration(0).LoadRefFactsByFiles(builderRepoPrefix, nil)
	if err != nil {
		t.Fatalf("read the corpus facts back: %v", err)
	}
	if len(stored) != len(facts) {
		t.Fatalf("the corpus holds %d facts after seeding %d", len(stored), len(facts))
	}
	return stored
}

// TestAncestryRefFactsUnionsWithoutDoublingARow pins the composition itself.
//
// Two generations of one stack can hold a row for the same reference — one
// that re-derived it, one that was there first — and the closure must see one
// target, not two. Origin, tier and candidates are provenance rather than
// identity, so they do not separate two rows that name the same reference at
// the same line of the same file.
func TestAncestryRefFactsUnionsWithoutDoublingARow(t *testing.T) {
	f := newCoordinatorFixture(t)
	facts := seedCorpusRefFacts(t, f)
	corpus := f.store.AtGeneration(0)

	composed := ancestryRefFacts{handles: []*store_sqlite.Store{corpus, corpus}}
	forward, err := composed.LoadRefFactsByFiles(builderRepoPrefix, nil)
	if err != nil {
		t.Fatalf("compose the forward facts: %v", err)
	}
	if len(forward) != len(facts) {
		t.Fatalf("composing one generation twice returned %d forward facts, want %d",
			len(forward), len(facts))
	}

	var targets []string
	for _, fact := range facts {
		if fact.ToID != "" {
			targets = append(targets, fact.ToID)
		}
	}
	if len(targets) == 0 {
		t.Fatalf("the seeded facts name no targets")
	}
	single, err := corpus.LoadRefFactsByTargets(builderRepoPrefix, targets)
	if err != nil {
		t.Fatalf("read the corpus reverse facts: %v", err)
	}
	reverse, err := composed.LoadRefFactsByTargets(builderRepoPrefix, targets)
	if err != nil {
		t.Fatalf("compose the reverse facts: %v", err)
	}
	for file, rows := range single {
		if len(reverse[file]) != len(rows) {
			t.Fatalf("composing one generation twice returned %d reverse facts for %s, want %d",
				len(reverse[file]), file, len(rows))
		}
	}
	if len(reverse) != len(single) {
		t.Fatalf("the composed reverse facts cover %d files, want %d", len(reverse), len(single))
	}
}

// --- the inherited layer, and the key a build is filed under -------------

const skewedIsland = `package fixture

func Island() {
	Helper()
	Helper()
}
`

// TestAnInheritedWorkingTreeLayerIsFiledBeforeItIsReplaced pins the arm that
// files whatever the route already names.
//
// A coordinator is not the only thing that outlives a route: a daemon restart,
// a rehome or a configuration reload all produce a NEW coordinator over a
// checkout that is already being served, and its reuse cache is empty. The
// only record it has of the layer on the route is the route row itself, so the
// first cycle that replaces that layer has to file it before it lets go of it
// — otherwise the very first undo after a restart retires the payload it is
// about to ask for.
//
// Revert-red: skip the retainDirty on the route-preserving read in
// reconcileDirtySlot and the undo re-indexes, because the layer the fresh
// coordinator inherited was released into a cache that never held it and was
// therefore collected.
func TestAnInheritedWorkingTreeLayerIsFiledBeforeItIsReplaced(t *testing.T) {
	f := newCoordinatorFixture(t)
	first := f.inertCoordinator(t, CheckoutCoordinatorConfig{})

	clean := coordinatorReconcile(t, first)
	if !clean.DirtyBuilt || clean.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle did not build a working-tree layer: %+v", clean)
	}

	// The second coordinator over the same checkout, with everything the first
	// one learned left behind in the first one.
	fresh := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	if len(fresh.retainedDirty) != 0 {
		t.Fatalf("a fresh coordinator started with %d retained working-tree layers", len(fresh.retainedDirty))
	}

	restore := editWorktreeFile(t, f.worktree, "island.go", editedIsland)
	edited := coordinatorReconcile(t, fresh)
	if !edited.DirtyBuilt || edited.DirtyGenerationID == clean.DirtyGenerationID {
		t.Fatalf("the edit did not build a new working-tree layer: %+v", edited)
	}

	restore()
	undone := coordinatorReconcile(t, fresh)
	if undone.DirtyBuilt {
		t.Fatalf("the undo re-indexed a working tree the inherited layer already described: %+v", undone)
	}
	if !undone.DirtyReused || undone.DirtyGenerationID != clean.DirtyGenerationID {
		t.Fatalf("the undo routed working-tree generation %d, want the inherited %d: %+v",
			undone.DirtyGenerationID, clean.DirtyGenerationID, undone)
	}
}

// TestTheBuiltWorkingTreeLayerIsFiledUnderTheKeyItStamped closes the skew
// between the cycle's sample and the build's.
//
// reconcileDirtySlot renders its reuse key from the sample it took to decide
// whether to build at all; BuildDirtyLayer stamps the generation's identity
// from a sample of its OWN, taken later. A checkout that moves between the two
// — the editor saving again while the build runs — makes the two disagree, and
// filing the build under the cycle's key would put an entry in a bounded cache
// that no lookup can ever hit: the row does not render that key, so cachedDirty
// fails closed on it, and until it is asked for it sits in a slot a real entry
// would have used.
//
// The barrier moves the tree after the first attempt's payload is written,
// which is exactly the window confirmDirtySnapshot refuses in: the attempt is
// torn, and the retry samples — and stamps — the state the barrier left.
//
// Revert-red: file the build under the cycle's own key and the lookup by the
// row's key misses.
func TestTheBuiltWorkingTreeLayerIsFiledUnderTheKeyItStamped(t *testing.T) {
	var (
		mu    sync.Mutex
		armed bool
		moved bool
	)
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{dirtyBarrier: func() {
		mu.Lock()
		defer mu.Unlock()
		if !armed {
			return
		}
		armed = false
		if err := os.WriteFile(filepath.Join(f.worktree, "island.go"), []byte(skewedIsland), 0o644); err != nil {
			t.Errorf("move the working tree under the build: %v", err)
			return
		}
		moved = true
	}})
	ctx := context.Background()

	clean := coordinatorReconcile(t, c)
	if clean.CommitGenerationID == 0 {
		t.Fatalf("the first cycle routed no commit layer: %+v", clean)
	}

	editWorktreeFile(t, f.worktree, "island.go", editedIsland)
	sample, err := c.sampler.Sample(ctx)
	if err != nil {
		t.Fatalf("sample the edited checkout: %v", err)
	}
	cycleKey := c.dirtySampleKey(f.graphID, clean.CommitGenerationID, sample)
	if cycleKey == "" {
		t.Fatalf("the edited sample rendered no key")
	}

	mu.Lock()
	armed = true
	mu.Unlock()
	out := coordinatorReconcile(t, c)
	if !out.DirtyBuilt || out.DirtyGenerationID == 0 {
		t.Fatalf("the cycle did not build a working-tree layer: %+v", out)
	}
	mu.Lock()
	observedMove := moved
	mu.Unlock()
	if !observedMove {
		t.Fatalf("the barrier never moved the working tree; the skew was never created")
	}

	row, found := f.generation(out.DirtyGenerationID)
	if !found {
		t.Fatalf("the built working-tree generation %d does not exist", out.DirtyGenerationID)
	}
	builtKey := generationRowKey(row)
	if builtKey == cycleKey {
		t.Fatalf("the build stamped the cycle's own sample; the test proves nothing")
	}
	if cached, ok := c.cachedDirty(ctx, builtKey); !ok || cached != out.DirtyGenerationID {
		t.Fatalf("the built working-tree layer %d is not filed under the key its own row renders (cached=%d ok=%v)",
			out.DirtyGenerationID, cached, ok)
	}
	if _, ok := c.cachedDirty(ctx, cycleKey); ok {
		t.Fatalf("the cache answered the cycle's key with a layer built for another state")
	}
}

// TestCommitLayerBuildTakesItsFactHintsFromTheWholeAncestry is the ref-fact
// hint scope at the commit half's production entrypoint.
//
// resolveCommitLayer hands the build the primary base to compute its affected
// closure against. Scoped to the base generation ALONE that base answers the
// closure's durable-hint question from one layer of a stack the structural
// reads compose in full — and since no sparse generation writes reference
// facts, from a layer that holds none. The closure then adds nothing for the
// hint, and a file that references the changed one is left out of the
// generation: a NARROWER affected set, which is the one direction the closure
// is not allowed to be wrong in.
//
// The hint here is the only thing that can pull helper.go in. island.go
// references nothing — that is what it is named for — so the structural walk
// over the changed file's edges reaches no other file, and the fact is the
// whole difference between the two scopes.
//
// Revert-red: scope the base's facts to c.store.AtGeneration(base.generationID)
// and the built generation stops claiming helper.go.
func TestCommitLayerBuildTakesItsFactHintsFromTheWholeAncestry(t *testing.T) {
	f, c, ids, tree := coordinatorParsedAncestry(t)
	ctx := context.Background()

	base, err := c.primaryBase(ctx)
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}
	base.generationID, base.treeOID = ids[len(ids)-1], tree
	if base.generationID <= 0 {
		t.Fatalf("the ancestry fixture published no generation: %v", ids)
	}

	islandPath := builderGraphPath(builderRepoPrefix, "island.go")
	helperPath := builderGraphPath(builderRepoPrefix, "helper.go")
	helperID := helperPath + "::Helper"
	if f.store.AtGeneration(ids[0]).GetNode(helperID) == nil {
		t.Fatalf("the ancestry root does not hold %s; the hint would name nothing", helperID)
	}
	// The top of the ancestry — the generation a single-handle scope would
	// read — holds no facts of its own, which is the blindness this pins.
	topFacts, err := f.store.AtGeneration(base.generationID).LoadRefFactsByFiles(builderRepoPrefix, nil)
	if err != nil {
		t.Fatalf("read generation %d's facts: %v", base.generationID, err)
	}
	if len(topFacts) != 0 {
		t.Fatalf("generation %d holds %d facts of its own; the single-handle scope is no longer blind and this test needs rewriting",
			base.generationID, len(topFacts))
	}

	// A durable hint at generation zero, where every fact in the database is.
	hint := graph.RefFact{
		FromID: islandPath + "::Island", ToID: helperID,
		Kind: "calls", RefName: "Helper", Line: 4, Origin: "ast_resolved", Tier: "resolved",
		FilePath: islandPath, Lang: "go",
	}
	if err := f.store.AtGeneration(0).BulkSetRefFacts(builderRepoPrefix, []graph.RefFact{hint}); err != nil {
		t.Fatalf("seed the corpus hint: %v", err)
	}

	// Change the one file the hint is about, and nothing else. The body it
	// gets references nothing, so the only route from island.go to helper.go
	// is the hint.
	builderWriteFile(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\tvar unused int\n\t_ = unused\n}\n")
	builderGit(t, f.worktree, "add", "-A")
	builderGit(t, f.worktree, "commit", "-m", "touch the island")
	targetTree := builderGit(t, f.worktree, "rev-parse", "HEAD^{tree}")

	generationID, reused, err := c.resolveCommitLayer(ctx, base, targetTree)
	if err != nil || generationID <= 0 {
		t.Fatalf("resolveCommitLayer: id=%d reused=%v err=%v", generationID, reused, err)
	}

	claimed := map[string]bool{}
	for _, path := range claimedPaths(t, f.store, generationID) {
		claimed[path] = true
	}
	if !claimed[islandPath] {
		t.Fatalf("the commit layer does not claim the changed file %s: %v", islandPath, claimed)
	}
	// Whether a file the closure pulled in is CLAIMED or declared read-only
	// context is the ownership split's decision (graphview.GenerationLayer
	// FilePaths / ContextPaths); the closure's own question is whether the
	// pass reached it at all, so that is what is asked here.
	reached := map[string]bool{}
	for _, path := range closurePaths(t, f.store, generationID) {
		reached[path] = true
	}
	if !reached[helperPath] {
		t.Fatalf("the commit layer left %s out of its closure: the build read its fact hints from one generation, not from the ancestry (reached %v)",
			helperPath, reached)
	}
}

// TestWithdrawingTheWorkingTreeSlotReleasesTheLayer is the other withdrawal.
//
// clearDirtySlot is what a route left naming a working-tree layer over a
// commit layer it no longer names gets: the slot goes before the rebuild
// starts, because the pair would otherwise be served for the whole of it. That
// is a reason to stop routing the layer and not a reason to destroy it — the
// payload still describes a real working tree over the commit generation it
// names, and the checkout may be routed back onto exactly that pair.
//
// Revert-red: change releaseDirty back to offerRetire in clearDirtySlot and
// the withdrawn layer is collected.
func TestWithdrawingTheWorkingTreeSlotReleasesTheLayer(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()

	first := coordinatorReconcile(t, c)
	if first.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle routed no working-tree layer: %+v", first)
	}
	withdrawn := first.DirtyGenerationID
	row, found := f.generation(withdrawn)
	if !found {
		t.Fatalf("the routed working-tree generation %d does not exist", withdrawn)
	}

	route := f.route()
	if err := c.clearDirtySlot(ctx, &route); err != nil {
		t.Fatalf("clearDirtySlot: %v", err)
	}
	if got := f.route().DirtyGenerationID; got != 0 {
		t.Fatalf("the working-tree slot still names generation %d", got)
	}

	if now, found := f.generation(withdrawn); !found || !servableGeneration(now.State) {
		t.Fatalf("the withdrawal destroyed working-tree generation %d (found=%v state=%q)",
			withdrawn, found, now.State)
	}
	c.mu.Lock()
	_, offered := c.backlog[withdrawn]
	c.mu.Unlock()
	if offered {
		t.Fatalf("the withdrawal offered the retained working-tree layer %d for collection", withdrawn)
	}
	if cached, ok := c.cachedDirty(ctx, generationRowKey(row)); !ok || cached != withdrawn {
		t.Fatalf("the withdrawn working-tree layer is not re-routable: cached=%d ok=%v", cached, ok)
	}
}
