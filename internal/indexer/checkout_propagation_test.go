package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Primary-to-worktree propagation by content (checkout_propagation.go).

// TestPropagationScopeClassifiesEveryPathByContent pins the per-path rule on
// its own: coverage and content identity decide the action, and nothing else.
func TestPropagationScopeClassifiesEveryPathByContent(t *testing.T) {
	const (
		oldBlob = "1111111111111111111111111111111111111111"
		newBlob = "2222222222222222222222222222222222222222"
		ownBlob = "3333333333333333333333333333333333333333"
	)
	delta := BaseDelta{
		FromTree: "from", ToTree: "to",
		Paths: []BaseDeltaPath{
			{Path: "covered_same.go", OldBlob: oldBlob, NewBlob: newBlob},
			{Path: "covered_own.go", OldBlob: oldBlob, NewBlob: newBlob},
			{Path: "shares_change.go", OldBlob: oldBlob, NewBlob: newBlob},
			{Path: "main_moved.go", OldBlob: oldBlob, NewBlob: newBlob},
			{Path: "added_by_main.go", NewBlob: newBlob},
			{Path: "deleted_by_main.go", OldBlob: oldBlob},
			{Path: "deleted_by_both.go", OldBlob: oldBlob},
		},
	}
	worktree := map[string]string{
		"covered_same.go":  newBlob,
		"covered_own.go":   ownBlob,
		"shares_change.go": newBlob,
		"main_moved.go":    oldBlob,
		// added_by_main.go: absent in the worktree -> re-assert its absence.
		"deleted_by_main.go": oldBlob,
		// deleted_by_both.go: absent on both sides -> untouched.
	}
	covered := map[string]struct{}{"covered_same.go": {}, "covered_own.go": {}}
	scope, err := scopeBaseDelta(delta, propagationWorktree{
		Covered: covered,
		Content: func(p, like string) (string, bool, error) {
			if len(like) != 40 {
				t.Errorf("content identity asked for %s with a like-blob of length %d", p, len(like))
			}
			blob, ok := worktree[p]
			return blob, ok, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]PropagationAction{
		"covered_same.go":    PropagationDropCoverage,
		"covered_own.go":     PropagationKeep,
		"shares_change.go":   PropagationUntouched,
		"main_moved.go":      PropagationReassert,
		"added_by_main.go":   PropagationReassert,
		"deleted_by_main.go": PropagationReassert,
		"deleted_by_both.go": PropagationUntouched,
	}
	for p, action := range want {
		if got := scope.Actions[p]; got != action {
			t.Errorf("%s: action %q, want %q", p, got, action)
		}
	}
	if got := scope.Parses(); got != 3 {
		t.Errorf("the scope parses %d files, want the 3 re-asserted ones", got)
	}
	if scope.Empty() {
		t.Error("a scope with re-asserted paths reports empty")
	}
	if len(scope.Rebind) != 0 {
		t.Errorf("a committed-base advance re-binds %v; the routed stack already served the worktree's own bytes", scope.Rebind)
	}

	// A worktree that shares every change and covers none of the paths is
	// untouched: an empty scope.
	shared := BaseDelta{Paths: []BaseDeltaPath{{Path: "a.go", OldBlob: oldBlob, NewBlob: newBlob}}}
	empty, err := scopeBaseDelta(shared, propagationWorktree{
		Covered: map[string]struct{}{},
		Content: func(string, string) (string, bool, error) { return newBlob, true, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Empty() || empty.Parses() != 0 {
		t.Errorf("a worktree that shares the change has scope %+v, want empty", empty)
	}

	// A base rewritten in place served the base's new bytes at paths the
	// worktree did not cover: re-asserting one changes what the worktree's
	// own covered referrers are bound to, and only those re-bind.
	inPlace := BaseDelta{
		Paths:               []BaseDeltaPath{{Path: "decl.go", OldBlob: oldBlob, NewBlob: newBlob}},
		ChangedDeclarations: []string{"repo/decl.go::Changed", "repo/other.go::Elsewhere"},
		ServedNewBytes:      true,
	}
	var asked []string
	rebound, err := scopeBaseDelta(inPlace, propagationWorktree{
		Covered: map[string]struct{}{"caller.go": {}},
		Content: func(string, string) (string, bool, error) { return oldBlob, true, nil },
		Referrers: func(ids, paths []string) ([]string, error) {
			asked = append(asked, ids...)
			return []string{"caller.go", "uncovered.go"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{"repo/decl.go::Changed"}) {
		t.Errorf("referrers asked for %v, want only the re-asserted path's changed declaration", asked)
	}
	if !slices.Equal(rebound.Rebind, []string{"caller.go"}) {
		t.Errorf("re-bind %v, want only the covered referrer", rebound.Rebind)
	}
}

// TestGitBlobIDMatchesGit pins the content identity against git itself, in
// both object formats' shape.
func TestGitBlobIDMatchesGit(t *testing.T) {
	dir := t.TempDir()
	builderIsolateGit(t)
	builderGit(t, dir, "init", "--initial-branch=main")
	body := "package fixture\n\nfunc F() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	want := builderGit(t, dir, "hash-object", "f.go")
	got, present, err := workingBlobID(dir, "f.go", want)
	if err != nil || !present || got != want {
		t.Fatalf("workingBlobID = %q present=%v err=%v, want git's %q", got, present, err, want)
	}
	if _, present, err := workingBlobID(dir, "absent.go", want); err != nil || present {
		t.Fatalf("an absent file reads present=%v err=%v", present, err)
	}
	if got := gitBlobID([]byte(body), true); len(got) != 64 {
		t.Fatalf("a SHA-256 blob id has length %d", len(got))
	}
}

// propagationCoordinator is a coordinator for one more linked worktree of the
// fixture's family, built exactly like the fixture's own (same configuration,
// so the generation identities agree), with chaining and the delta model on.
func propagationCoordinator(t *testing.T, f *coordinatorFixture, adminName string) (*CheckoutCoordinator, string) {
	t.Helper()
	root := filepath.Join(filepath.Dir(f.worktree), adminName)
	builderGit(t, f.primary, "worktree", "add", "-b", adminName, root)
	now := time.Now().Unix()
	checkout := store_sqlite.Checkout{
		CheckoutID:     "checkout-" + adminName,
		Incarnation:    "incarnation-" + adminName,
		FamilyID:       f.familyID,
		RootPath:       root,
		GitDir:         filepath.Join(f.primary, ".git", "worktrees", adminName),
		AdminName:      adminName,
		State:          store_sqlite.CheckoutStateReady,
		DesiredMode:    store_sqlite.CheckoutModeAutomatic,
		EffectiveMode:  store_sqlite.CheckoutModeAutomatic,
		HeadRef:        "refs/heads/" + adminName,
		HeadTree:       f.treeA,
		LastAccessible: now,
		LastSeen:       now,
	}
	if err := f.catalog.AllocateCheckout(context.Background(), checkout); err != nil {
		t.Fatalf("allocate %s: %v", adminName, err)
	}
	c, err := NewCheckoutCoordinator(CheckoutCoordinatorConfig{
		CheckoutID:     checkout.CheckoutID,
		CheckoutRoot:   root,
		FamilyID:       f.familyID,
		RepoPrefix:     builderRepoPrefix,
		WorkspaceID:    builderRepoPrefix,
		ProjectID:      builderRepoPrefix,
		Store:          f.store,
		Builder:        builderNewBuilder(f.store),
		Leases:         f.leases,
		Config:         config.Default().Index,
		ConfigSections: dedicatedBaseConfigSections(config.Default()),
		Logger:         zap.NewNop(),
		PollInterval:   -1,
	})
	if err != nil {
		t.Fatalf("NewCheckoutCoordinator for %s: %v", adminName, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Close(); err != nil {
		t.Fatalf("stop the %s coordinator loop: %v", adminName, err)
	}
	c.compaction.quiet = -1
	return c, root
}

// routedStack lists what a checkout's route composes over the base corpus,
// oldest first: its commit generation, then its working-tree chain.
func routedStack(t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator) []int64 {
	t.Helper()
	route, found, err := f.catalog.GetCheckoutRoute(context.Background(), c.checkoutID)
	if err != nil || !found {
		t.Fatalf("route of %s: found=%v err=%v", c.checkoutID, found, err)
	}
	members := c.dirtyChainMembers(context.Background(), route.DirtyGenerationID)
	slices.Reverse(members)
	return append([]int64{route.CommitGenerationID}, members...)
}

// generationNodeRows renders one generation's own node rows, content only.
func generationNodeRows(t *testing.T, f *coordinatorFixture, generationID int64) []string {
	t.Helper()
	return builderRenderNodes(f.store.AtGeneration(generationID).GetRepoNodes(builderRepoPrefix))
}

const (
	// The primary's body edit: same declarations, another body.
	propagationCoreC2 = "package fixture\n\ntype Options struct{}\n\nfunc Compute(o Options) {\n\t_ = o\n\tHelper()\n}\n"
	// A worktree's own version of the same file.
	propagationCoreOwn = "package fixture\n\ntype Options struct{}\n\nfunc Compute(o Options) {\n\tHelper()\n\tIsland()\n}\n"
)

// TestPrimaryBodyEditPropagatesByContentToEachWorktree is the three-worktree
// shape of a primary body edit (the primary commits a new body of core.go):
//
//   - "same" already holds the primary's new bytes in its working tree;
//   - "own" holds a version of core.go of its own;
//   - "unrelated" edits other files and still holds core.go's old bytes, as
//     does its HEAD — the typical worktree: main moved, it did not.
//
// What must hold:
//
//   - the advance costs an unused worktree nothing: every cycle before a use
//     defers, routes stay, no generation is written;
//   - each worktree's scope is decided by content: "same" drops coverage,
//     "own" keeps its version, "unrelated" re-asserts its own core.go;
//   - applying it parses no working-tree file anywhere: each chain is carried
//     over by copy, row for row, and the only parse is the commit layer's
//     one changed path (the re-assertion of the committed core.go, which in
//     "same" and "own" the working tree masks);
//   - every worktree serves exactly a clean index of its working tree.
func TestPrimaryBodyEditPropagatesByContentToEachWorktree(t *testing.T) {
	f := newCoordinatorFixture(t)
	ctx := context.Background()
	same := chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
	own, ownRoot := propagationCoordinator(t, f, "own")
	unrelated, unrelatedRoot := propagationCoordinator(t, f, "unrelated")
	worktrees := []struct {
		name string
		c    *CheckoutCoordinator
		root string
		want PropagationAction
	}{
		{"same", same, f.worktree, PropagationDropCoverage},
		{"own", own, ownRoot, PropagationKeep},
		{"unrelated", unrelated, unrelatedRoot, PropagationReassert},
	}

	builderWriteFile(t, f.worktree, "core.go", propagationCoreC2)
	builderWriteFile(t, ownRoot, "core.go", propagationCoreOwn)
	builderWriteFile(t, unrelatedRoot, "island.go", "package fixture\n\nfunc Island() {\n\tHelper()\n}\n")
	for _, w := range worktrees {
		if out := coordinatorReconcile(t, w.c); out.CommitGenerationID == 0 || out.DirtyGenerationID == 0 {
			t.Fatalf("%s: the first cycle routed no stack: %+v", w.name, out)
		}
	}
	// A second edit in "unrelated" makes its working tree a two-member chain,
	// so the carry-over is exercised on a chain and not only on one layer.
	builderWriteFile(t, unrelatedRoot, "extra.go", "package fixture\n\nfunc Extra() {\n\tIsland()\n}\n")
	if out := coordinatorReconcile(t, unrelated); out.DirtyChainDepth != 2 {
		t.Fatalf("unrelated: the second edit did not chain: %+v", out)
	}

	// The primary commits the body edit, and the corpus follows it in place
	// (the regime where the primary publishes no generation).
	builderWriteFile(t, f.primary, "core.go", propagationCoreC2)
	builderGit(t, f.primary, "add", "-A")
	builderGit(t, f.primary, "commit", "-m", "body edit")
	newTree := builderGit(t, f.primary, "rev-parse", "HEAD^{tree}")
	builderIndex(t, f.store, f.primary)
	f.movePrimaryHead(t, newTree)

	delta, err := baseTreeDelta(ctx, f.primary, f.treeA, newTree)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Paths) != 1 || delta.Paths[0].Path != "core.go" || delta.Paths[0].OldBlob == "" || delta.Paths[0].NewBlob == "" {
		t.Fatalf("the base delta is %+v, want core.go modified", delta.Paths)
	}

	// Nobody uses any worktree yet: every cycle defers and writes nothing.
	before := generationCensus(f)
	stacks := map[string][]int64{}
	for _, w := range worktrees {
		stacks[w.name] = routedStack(t, f, w.c)
		out := coordinatorReconcile(t, w.c)
		if !out.RebaseDeferred || out.CommitBuilt || out.DirtyBuilt || out.Recomposed {
			t.Fatalf("%s: an unused worktree acted on the base advance: %+v", w.name, out)
		}
		if got := routedStack(t, f, w.c); !slices.Equal(got, stacks[w.name]) {
			t.Fatalf("%s: the deferral moved the route %v -> %v", w.name, stacks[w.name], got)
		}
		if !w.c.PropagationStats().Pending {
			t.Fatalf("%s: the deferred advance is not pending", w.name)
		}
	}
	if after := generationCensus(f); !mapsEqual(after, before) {
		t.Fatalf("the deferred advance wrote generations: %v -> %v", before, after)
	}

	// Each worktree is used (a selection), and its next cycle applies it.
	for _, w := range worktrees {
		oldStack := stacks[w.name]
		w.c.PrioritizeSelection()
		applied := time.Now()
		out := coordinatorReconcile(t, w.c)
		t.Logf("%s: rebase applied in %s (commit layer built=%v, chain carried over=%v, scope %v)",
			w.name, time.Since(applied), out.CommitBuilt, out.DirtyReparented, propagationActions(out.Propagation))
		if !out.Recomposed || !out.DirtyReparented || out.DirtyBuilt || out.DirtyWork != nil {
			t.Fatalf("%s: the used worktree was not recomposed by carrying its chain over: %+v", w.name, out)
		}
		if out.Propagation == nil {
			t.Fatalf("%s: the cycle computed no scope", w.name)
		}
		if got := out.Propagation.Actions["core.go"]; got != w.want {
			t.Errorf("%s: core.go scoped %q, want %q", w.name, got, w.want)
		}
		if w.c.PropagationStats().Pending {
			t.Errorf("%s: the advance is still pending after it was applied", w.name)
		}
		newStack := routedStack(t, f, w.c)
		if len(newStack) != len(oldStack) {
			t.Fatalf("%s: the carried-over stack %v does not mirror %v", w.name, newStack, oldStack)
		}
		// The commit layer parsed only the path the two committed trees
		// differ by.
		if claimed := claimedPaths(t, f.store, newStack[0]); !slices.Equal(claimed, []string{builderRepoPrefix + "/core.go"}) {
			t.Errorf("%s: the recomposed commit layer claims %v, want core.go only", w.name, claimed)
		}
		// Every working-tree generation is a row-for-row copy of the one it
		// replaces, over the new commit layer.
		for i := 1; i < len(newStack); i++ {
			if newStack[i] == oldStack[i] {
				t.Fatalf("%s: chain member %d was not carried over", w.name, oldStack[i])
			}
			if got, want := generationNodeRows(t, f, newStack[i]), generationNodeRows(t, f, oldStack[i]); !slices.Equal(got, want) {
				t.Errorf("%s: carried-over generation %d differs from %d", w.name, newStack[i], oldStack[i])
			}
			row, found := f.generation(newStack[i])
			if !found || row.BaseGenerationID != newStack[i-1] || row.State != store_sqlite.ViewGenerationReady {
				t.Errorf("%s: carried-over generation %d sits on %d in state %s, want %d ready",
					w.name, newStack[i], row.BaseGenerationID, row.State, newStack[i-1])
			}
		}
		if result := assertPropagationParity(t, f.store, newStack, w.root, "propagation-"+w.name); !result.ok() {
			t.Errorf("%s: the propagated view differs from a clean index of its working tree: %v", w.name, result.Diffs)
		}
		// Settled: the next cycle does nothing.
		if next := coordinatorReconcile(t, w.c); next.CommitBuilt || next.DirtyBuilt || next.Recomposed || next.RebaseDeferred {
			t.Errorf("%s: the cycle after the rebase acted again: %+v", w.name, next)
		}
	}
}

func propagationActions(s *PropagationScope) map[string]PropagationAction {
	if s == nil {
		return nil
	}
	return s.Actions
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestBaseAdvanceIsNotedWithoutACycleAndAppliedOnUse pins the lazy half on the
// committed-base event path: the lifecycle's fan-out notes the advance on
// every coordinator of the family without waking any of them, and a
// selection or a refresh ticket is what asks for the cycle.
func TestBaseAdvanceIsNotedWithoutACycleAndAppliedOnUse(t *testing.T) {
	f := newCoordinatorFixture(t)
	delta := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	other, _ := f.secondWorktree(t, "other", nil)
	drain := func(c *CheckoutCoordinator) bool {
		select {
		case <-c.signal:
			return true
		default:
			return false
		}
	}
	drain(delta)
	drain(other)

	l := &CheckoutLifecycle{logger: zap.NewNop(), coordinators: map[string]*CheckoutCoordinator{
		delta.checkoutID: delta,
		other.checkoutID: other,
	}}
	if signalled := l.PropagateBaseAdvance(f.familyID, "", 42, "tree-42", "test advance"); signalled != 0 {
		t.Fatalf("the fan-out signalled %d coordinators, want none", signalled)
	}
	if drain(delta) || drain(other) {
		t.Fatal("a coordinator was woken by the base advance")
	}
	for _, c := range []*CheckoutCoordinator{delta, other} {
		if stats := c.PropagationStats(); stats.Noted != 1 || !stats.Pending {
			t.Fatalf("the advance was not noted on %s: %+v", c.checkoutID, stats)
		}
	}

	// A selection is a use: one signal, however often it is repeated.
	delta.PrioritizeSelection()
	if !drain(delta) {
		t.Fatal("a selection did not ask for the pending rebase")
	}
	delta.PrioritizeSelection()
	if drain(delta) {
		t.Fatal("a repeated selection signalled again")
	}
	if !delta.rebaseWantedNow() {
		t.Fatal("the used checkout does not want its rebase")
	}
	delta.rebaseApplied()
	if delta.rebaseWantedNow() || delta.PropagationStats().Pending {
		t.Fatal("an applied rebase is still wanted or pending")
	}

	// Nothing pending: a selection costs nothing and marks nothing, so the
	// next advance is as lazy as the first.
	delta.PrioritizeSelection()
	if drain(delta) {
		t.Fatal("a selection with nothing pending signalled a cycle")
	}
	delta.NoteBaseAdvance(43, "tree-43", "second advance")
	if drain(delta) || delta.rebaseWantedNow() {
		t.Fatal("a checkout used once before an advance became eager for it")
	}
	// A refresh ticket is a use too, and it runs its own cycle: it marks the
	// rebase wanted without a second wake.
	delta.wantRebase("refresh ticket", false)
	if drain(delta) || !delta.rebaseWantedNow() {
		t.Fatal("a ticket did not mark the pending rebase wanted, or signalled a second wake")
	}
}

// viewGenerationCount counts every generation the catalog holds, whatever its
// state: a restart that rebuilt anything would add rows.
func viewGenerationCount(t *testing.T, store *store_sqlite.Store) int {
	t.Helper()
	var n int
	if err := parityOpenRaw(t, store).QueryRow(`SELECT COUNT(*) FROM view_generations`).Scan(&n); err != nil {
		t.Fatalf("count generations: %v", err)
	}
	return n
}

// TestRestartLeavesRoutedWorktreesDormantAndRebuildsNothing pins the restart
// rule under the delta model: a route is a durable overlay, so a restart
// rebuilds nothing and a routed checkout nobody selects stays dormant — its
// coordinator starts when something selects it, and even then a working tree
// that did not move while the daemon was down builds nothing. Outside the
// delta model the routed checkout still resumes on sight, as before.
func TestRestartLeavesRoutedWorktreesDormantAndRebuildsNothing(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("restart-main")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "restart-main"}, TrackSourceCLI)
	if err != nil || tracked.CatalogErr != nil {
		t.Fatalf("register: %v / %v", err, tracked.CatalogErr)
	}
	if _, err := f.lc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	wt := f.worktreeOf(main, "restart-wt")
	if report, err := f.lc.Sweep(ctx); err != nil || report.Coordinators != 1 {
		t.Fatalf("the runtime worktree did not get its coordinator: %+v %v", report, err)
	}
	id := f.automaticCheckoutID(tracked.FamilyID, "restart-wt")
	writeFile(t, filepath.Join(wt, "overlay.go"), "package a\n\nfunc Overlay() {}\n")
	if out := f.runCoordinator(id); out.DirtyGenerationID == 0 {
		t.Fatalf("the worktree routed no working-tree layer: %+v", out)
	}
	route, found := f.routeOf(id)
	if !found {
		t.Fatal("the worktree has no route")
	}
	generations := viewGenerationCount(t, f.store)

	// A daemon start: the repository is tracked again and the lifecycle
	// seeds itself from the catalog, which is where a routed checkout would
	// be resumed.
	boot := func() {
		t.Helper()
		f.restart()
		if _, err := f.mi.TrackRepoCtx(ctx, config.RepoEntry{Path: main, Name: "restart-main"}); err != nil {
			t.Fatalf("track after restart: %v", err)
		}
		if err := f.lc.Seed(ctx); err != nil {
			t.Fatalf("seed after restart: %v", err)
		}
	}

	// Nothing starts, nothing is written.
	boot()
	if live := f.lc.LiveCoordinators(""); live != 0 {
		t.Fatalf("a restart started %d coordinators for a routed, unselected worktree", live)
	}
	if now, _ := f.routeOf(id); now != route {
		t.Fatalf("the restart moved the route: %+v -> %+v", route, now)
	}
	if n := viewGenerationCount(t, f.store); n != generations {
		t.Fatalf("the restart wrote generations: %d -> %d", generations, n)
	}

	// Selected, the checkout wakes, and its unchanged working tree builds
	// nothing: the overlay it was routed to is still exact.
	f.activateAndWait(id)
	out := f.runCoordinator(id)
	if out.CommitBuilt || out.DirtyBuilt || out.Recomposed {
		t.Fatalf("the woken worktree rebuilt its unchanged overlay: %+v", out)
	}
	if now, _ := f.routeOf(id); now.CommitGenerationID != route.CommitGenerationID || now.DirtyGenerationID != route.DirtyGenerationID {
		t.Fatalf("the woken worktree re-routed: %+v -> %+v", route, now)
	}
	if n := viewGenerationCount(t, f.store); n != generations {
		t.Fatalf("waking the worktree wrote generations: %d -> %d", generations, n)
	}
}

// renderChain renders every node and edge a stack of generations composes to
// over the base corpus, content only.
func renderChain(t *testing.T, f *coordinatorFixture, stack []int64) ([]string, []string) {
	t.Helper()
	composed := dirtyChainComposed(t, f.store, stack)
	return builderRenderNodes(composed.AllNodes()), builderRenderEdges(composed.AllEdges())
}

// TestOverlayFoldByCopyServesTheSameView pins the copy-based compactor as the
// fold of a large overlay: a working-tree chain whose members claim more paths
// than the fold threshold is folded into ONE generation over the commit
// generation by copying rows — no working-tree file parsed — and the folded
// stack serves exactly what the chain served, which is exactly a clean index
// of the working tree. The chain carries every kind of claim a working tree
// makes: a body edit, an added file, a deleted file, a signature change its
// callers re-bind to, and an undo back to the committed bytes.
func TestOverlayFoldByCopyServesTheSameView(t *testing.T) {
	old := dirtyChainFoldPaths
	dirtyChainFoldPaths = 1
	t.Cleanup(func() { dirtyChainFoldPaths = old })

	f := newCoordinatorFixture(t)
	ctx := context.Background()
	c := chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
	if out := coordinatorReconcile(t, c); out.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle routed no working-tree layer: %+v", out)
	}
	edits := []func(){
		func() { builderWriteFile(t, f.worktree, "core.go", propagationCoreC2) },
		func() {
			builderWriteFile(t, f.worktree, "extra.go", "package fixture\n\nfunc Extra() {\n\tHelper()\n\tIsland()\n}\n")
		},
		func() {
			if err := os.Remove(filepath.Join(f.worktree, "gone.go")); err != nil {
				t.Fatal(err)
			}
		},
		func() {
			builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() int {\n\treturn 1\n}\n")
		},
		func() {
			builderWriteFile(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {\n\tHelper()\n}\n")
		},
		// Undo the island edit: back to the committed bytes.
		func() { builderWriteFile(t, f.worktree, "island.go", builderTreeA()["island.go"]) },
	}
	var last CheckoutCycle
	for i, edit := range edits {
		edit()
		last = coordinatorReconcile(t, c)
		if !last.DirtyBuilt {
			t.Fatalf("edit %d built nothing: %+v", i, last)
		}
		if last.CompactionScheduled {
			break
		}
	}
	if !last.CompactionScheduled || last.DirtyChainDepth >= dirtyChainCompactionDepth {
		t.Fatalf("the overlay was not due for a fold before the depth bound: %+v", last)
	}
	before := routedStack(t, f, c)
	wantNodes, wantEdges := renderChain(t, f, before)
	db := parityOpenRaw(t, f.store)
	for _, id := range before[1:] {
		var files, context, identities, sources int
		_ = db.QueryRow(`SELECT COUNT(*) FROM generation_file_masks WHERE view_gen = ? AND ownership_mode <> 'context'`, id).Scan(&files)
		_ = db.QueryRow(`SELECT COUNT(*) FROM generation_file_masks WHERE view_gen = ? AND ownership_mode = 'context'`, id).Scan(&context)
		_ = db.QueryRow(`SELECT COUNT(*) FROM generation_node_tombstones WHERE view_gen = ?`, id).Scan(&identities)
		_ = db.QueryRow(`SELECT COUNT(*) FROM generation_edge_sources WHERE view_gen = ?`, id).Scan(&sources)
		t.Logf("chain member %d: %d file claims, %d context marks, %d identity masks, %d edge-source marks",
			id, files, context, identities, sources)
	}

	report := c.compactDirtyChain(ctx, last)
	if report.Outcome != dirtyChainCompactionFlipped || report.Err != nil {
		t.Fatalf("the fold did not flip: %+v", report)
	}
	t.Logf("fold of %d generations: %s (copy + verify + publish %s)", len(before)-1, report.Duration, report.BuildDuration)
	after := routedStack(t, f, c)
	if len(after) != 2 || after[0] != before[0] {
		t.Fatalf("the folded stack is %v, want the commit generation %d under one working-tree generation", after, before[0])
	}
	row, found := f.generation(after[1])
	if !found || row.BaseGenerationID != before[0] {
		t.Fatalf("the folded generation sits on %d, want the commit generation %d", row.BaseGenerationID, before[0])
	}
	meta, _, found, err := f.store.AtGeneration(after[1]).InputManifest(ctx)
	if err != nil || !found || !meta.IsFull {
		t.Fatalf("the folded generation carries no full manifest: %+v found=%v err=%v", meta, found, err)
	}
	gotNodes, gotEdges := renderChain(t, f, after)
	if !slices.Equal(gotNodes, wantNodes) {
		t.Errorf("the fold serves other nodes than the chain:\n%v", parityDiff("nodes", gotNodes, wantNodes))
	}
	if !slices.Equal(gotEdges, wantEdges) {
		t.Errorf("the fold serves other edges than the chain:\n%v", parityDiff("edges", gotEdges, wantEdges))
	}
	if result := assertPropagationParity(t, f.store, after, f.worktree, "fold"); !result.ok() {
		t.Errorf("the folded stack differs from a clean index of the working tree: %v", result.Diffs)
	}

	// The next edit chains over the fold like over any direct generation.
	builderWriteFile(t, f.worktree, "extra.go", "package fixture\n\nfunc Extra() {\n\tHelper()\n}\n")
	next := coordinatorReconcile(t, c)
	if next.DirtyParentGenerationID != after[1] || next.DirtyChainDepth != 2 {
		t.Fatalf("the edit after the fold did not chain over it: %+v", next)
	}
	if result := assertPropagationParity(t, f.store, routedStack(t, f, c), f.worktree, "after-fold"); !result.ok() {
		t.Errorf("the edit over the fold differs from a clean index: %v", result.Diffs)
	}
}

// TestOverlayFoldByCopyKeepsReboundCallersAndRemovedIdentities is the fold
// over a chain whose members carry the finer claims: a renamed declaration
// whose untouched callers are re-bound (edge-source marks) and removed
// identities. The fold must still serve exactly the chain.
func TestOverlayFoldByCopyKeepsReboundCallersAndRemovedIdentities(t *testing.T) {
	old := dirtyChainFoldPaths
	dirtyChainFoldPaths = 1
	t.Cleanup(func() { dirtyChainFoldPaths = old })

	f := newCoordinatorFixture(t)
	ctx := context.Background()
	c := chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
	if out := coordinatorReconcile(t, c); out.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle routed no working-tree layer: %+v", out)
	}
	edits := []func(){
		// caller.go gains a call to Helper: its edges live in this member.
		func() {
			builderWriteFile(t, f.worktree, "caller.go", "package fixture\n\nfunc Run() {\n\tCompute(Options{})\n\tHelper()\n}\n")
		},
		// Helper is renamed: core.go (untouched) and caller.go (edited in
		// the member below) call it, so their calls are re-bound by a member
		// that claims neither file.
		func() { builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Assist() {\n}\n") },
		// Compute moves out of core.go into a new file, which also calls
		// into the standard library (a pathless external stub).
		func() {
			builderWriteFile(t, f.worktree, "core.go", "package fixture\n\ntype Options struct{}\n")
			builderWriteFile(t, f.worktree, "compute.go",
				"package fixture\n\nimport \"strings\"\n\nfunc Compute(o Options) string {\n\tAssist()\n\treturn strings.ToUpper(\"x\")\n}\n")
		},
		// A second external call from another new file.
		func() {
			builderWriteFile(t, f.worktree, "extern.go",
				"package fixture\n\nimport \"strings\"\n\nfunc Lower() string {\n\treturn strings.ToLower(\"X\")\n}\n")
		},
	}
	var last CheckoutCycle
	for i, edit := range edits {
		edit()
		last = coordinatorReconcile(t, c)
		if !last.DirtyBuilt {
			t.Fatalf("edit %d built nothing: %+v", i, last)
		}
	}
	if !last.CompactionScheduled {
		t.Fatalf("the overlay was not due for a fold: %+v", last)
	}
	before := routedStack(t, f, c)
	if len(before) < 3 {
		t.Fatalf("the edits did not chain: %v", before)
	}
	db := parityOpenRaw(t, f.store)
	var identities, sources int
	for _, id := range before[1:] {
		var n, m int
		_ = db.QueryRow(`SELECT COUNT(*) FROM generation_node_tombstones WHERE view_gen = ?`, id).Scan(&n)
		_ = db.QueryRow(`SELECT COUNT(*) FROM generation_edge_sources WHERE view_gen = ?`, id).Scan(&m)
		identities, sources = identities+n, sources+m
	}
	t.Logf("chain %v carries %d identity masks and %d edge-source marks", before[1:], identities, sources)
	chainParity := assertPropagationParity(t, f.store, before, f.worktree, "fold-rebound-chain")
	if !chainParity.ok() {
		// What the edit builds produced is the edit path's to answer for;
		// what the fold owes is to serve exactly the chain (asserted below).
		t.Logf("the chain itself differs from a clean index before any fold (edit-path divergence): %v", chainParity.Diffs)
	}
	wantNodes, wantEdges := renderChain(t, f, before)

	report := c.compactDirtyChain(ctx, last)
	if report.Outcome != dirtyChainCompactionFlipped || report.Err != nil {
		t.Fatalf("the fold did not flip: %+v", report)
	}
	after := routedStack(t, f, c)
	if len(after) != 2 {
		t.Fatalf("the folded stack is %v", after)
	}
	gotNodes, gotEdges := renderChain(t, f, after)
	if !slices.Equal(gotNodes, wantNodes) {
		t.Errorf("the fold serves other nodes than the chain:\n%v", parityDiff("nodes", gotNodes, wantNodes))
	}
	if !slices.Equal(gotEdges, wantEdges) {
		t.Errorf("the fold serves other edges than the chain:\n%v", parityDiff("edges", gotEdges, wantEdges))
	}
	if result := assertPropagationParity(t, f.store, after, f.worktree, "fold-rebound"); chainParity.ok() && !result.ok() {
		t.Errorf("the folded stack differs from a clean index of the working tree: %v", result.Diffs)
	}
}

// TestALargeWorkingTreeChangeIsImportedFileByFileAndYieldsToAnInteractiveBuild
// pins the import that replaces the batched builds of a large working-tree
// change under the delta model. A `git checkout <branch> -- .` lands a dozen
// files in the worktree at once; the coordinator imports them one file per
// cycle, each a chained generation, through the real admission path and the
// shared build lane, folding the chain by copy whenever it reaches the
// compaction depth. Three controlled interactive interruptions arrive after a
// real file payload is written and before it is published. Each must get the
// lane promptly, and the interrupted file must then publish before the next
// interruption. Nothing is re-parsed after it was imported, and the end state
// is exactly a clean index. This finite schedule does not establish progress
// under uninterrupted interactive demand: cancellation of unpublished work
// can still starve an import under sustained 40 ms probes (a known NO-GO).
func TestALargeWorkingTreeChangeIsImportedFileByFileAndYieldsToAnInteractiveBuild(t *testing.T) {
	oldPaths := importInteractivePaths
	importInteractivePaths = 4
	t.Cleanup(func() { importInteractivePaths = oldPaths })

	f := newCoordinatorFixture(t)
	gate := NewViewBuildGate()
	gate.Open()
	outcomes := make(chan CheckoutCycle, 512)
	interrupt := make(chan struct{}, 1)
	atPayload := make(chan context.Context, 1)
	var cycleContext context.Context // used only by the coordinator's cycle goroutine
	// The coordinator's own loop drives the import, exactly as in the daemon:
	// each file's cycle signals the next one through the quiet window.
	c := f.coordinator(t, CheckoutCoordinatorConfig{
		Gate:      gate,
		cycleDone: func(out CheckoutCycle) { outcomes <- out },
		dirtyBarrier: func() {
			select {
			case <-interrupt:
				atPayload <- cycleContext
				// The interactive waiter cancels this context. Coordinator
				// cleanup also cancels it if an assertion fails here.
				<-cycleContext.Done()
			default:
			}
		},
	})
	// Install the context capture before signaling any cycle, under the same
	// lock that guards cycleBarrier's invocation.
	c.cycleMu.Lock()
	c.cycleBarrier = func(ctx context.Context) { cycleContext = ctx }
	c.cycleMu.Unlock()
	c.compaction.mu.Lock()
	c.compaction.quiet = -1
	c.compaction.mu.Unlock()
	await := func(ctx context.Context) (CheckoutCycle, bool) {
		select {
		case out := <-outcomes:
			return out, true
		case <-ctx.Done():
			return CheckoutCycle{}, false
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	c.Signal("first build")
	if out, ok := await(ctx); !ok || out.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle routed no working-tree layer: %+v", out)
	}

	const imported = 12
	builderGit(t, f.primary, "checkout", "-q", "-b", "import-src")
	for i := 0; i < imported; i++ {
		builderWriteFile(t, f.primary, fmt.Sprintf("imported_%02d.go", i),
			fmt.Sprintf("package fixture\n\nfunc Imported%02d() int {\n\treturn %d\n}\n", i, i))
	}
	builderGit(t, f.primary, "add", "-A")
	builderGit(t, f.primary, "commit", "-q", "-m", "import source")
	builderGit(t, f.primary, "checkout", "-q", "main")
	builderGit(t, f.worktree, "checkout", "import-src", "--", ".")

	type importCycle struct {
		out   CheckoutCycle
		files []string
	}
	var cycles []importCycle
	complete := func(out CheckoutCycle) bool {
		return out.DirtyBuilt && out.DirtyBatchRemaining == 0 && !out.Rescheduled
	}
	record := func(out CheckoutCycle) {
		// The files a link imported are the imported paths its own generation
		// claims; a fold claims everything before it and is counted separately.
		var files []string
		if out.DirtyBuilt && !out.ImportFolded && out.DirtyGenerationID > 0 {
			for _, p := range claimedPaths(t, f.store, out.DirtyGenerationID) {
				if rel := strings.TrimPrefix(p, builderRepoPrefix+"/"); strings.HasPrefix(rel, "imported_") {
					files = append(files, rel)
				}
			}
		}
		cycles = append(cycles, importCycle{out: out, files: files})
		if out.Err != nil {
			t.Fatalf("import cycle failed: %+v", out)
		}
		if len(cycles) > 400 {
			t.Fatal("the finite import exceeded its cycle bound")
		}
	}
	routedFiles := func() map[string]struct{} {
		files := map[string]struct{}{}
		for _, id := range routedStack(t, f, c)[1:] {
			for _, p := range claimedPaths(t, f.store, id) {
				if rel := strings.TrimPrefix(p, builderRepoPrefix+"/"); strings.HasPrefix(rel, "imported_") {
					files[rel] = struct{}{}
				}
			}
		}
		return files
	}

	// Interrupt three distinct in-flight file payloads, then let each retry
	// make durable progress before introducing another interactive waiter.
	var waits []time.Duration
	interruptedFiles := map[string]struct{}{}
	for n := 0; n < 3; n++ {
		before := routedFiles()
		interrupt <- struct{}{}
		if n == 0 {
			c.Signal("git checkout")
		}
		var payloadContext context.Context
		for payloadContext == nil {
			select {
			case payloadContext = <-atPayload:
			case out := <-outcomes:
				record(out)
				if complete(out) {
					t.Fatal("the import completed before all three controlled interruptions")
				}
			case <-ctx.Done():
				t.Fatal("the import never reached its payload barrier")
			}
		}
		// A building generation and its claimed file prove this interruption
		// reached actual payload work, rather than a pre-admission refusal.
		var inFlight []string
		for _, row := range f.generations() {
			if row.GenerationKind != DirtyLayerGenerationKind || row.State != store_sqlite.ViewGenerationBuilding {
				continue
			}
			for _, p := range claimedPaths(t, f.store, row.GenerationID) {
				if rel := strings.TrimPrefix(p, builderRepoPrefix+"/"); strings.HasPrefix(rel, "imported_") {
					inFlight = append(inFlight, rel)
				}
			}
		}
		if len(inFlight) != 1 {
			t.Fatalf("the admitted payload claims %v, want one imported file", inFlight)
		}
		file := inFlight[0]
		if _, repeated := interruptedFiles[file]; repeated {
			t.Fatalf("interrupted %s twice instead of progressing across files", file)
		}
		interruptedFiles[file] = struct{}{}
		asked := time.Now()
		actx, acancel := context.WithTimeout(ctx, 5*time.Second)
		release, err := gate.Acquire(actx, ViewBuildInteractive)
		acancel()
		if err != nil {
			t.Fatalf("an interactive build was not admitted during the import: %v", err)
		}
		waits = append(waits, time.Since(asked))
		release()
		if payloadContext.Err() == nil {
			t.Fatal("interactive admission did not cancel the in-flight payload")
		}
		yielded := false
		for {
			out, ok := await(ctx)
			if !ok {
				t.Fatal("the interrupted import never resumed")
			}
			record(out)
			if out.YieldedTo == laneYieldedToInteractive {
				yielded = true
			}
			// An earlier published cycle may already be queued when the
			// payload barrier wins the select. Its outcome precedes this
			// yield and cannot prove that the interrupted file resumed.
			if !out.DirtyBuilt || !yielded {
				continue
			}
			after := routedFiles()
			if _, published := after[file]; !published || len(after) <= len(before) {
				t.Fatalf("interruption of %s made no durable progress: yielded=%v, before=%v, after=%v", file, yielded, before, after)
			}
			break
		}
	}
	for !complete(cycles[len(cycles)-1].out) {
		out, ok := await(ctx)
		if !ok {
			t.Fatal("the finite import never completed")
		}
		record(out)
	}
	if len(cycles) == 0 {
		t.Fatal("the import ran no cycle")
	}
	last := cycles[len(cycles)-1].out
	if last.Err != nil || !last.DirtyBuilt || last.DirtyBatchRemaining != 0 {
		t.Fatalf("the import did not complete: %+v", last)
	}

	seen := map[string]int{}
	folds, links, yields := 0, 0, 0
	for i, cy := range cycles {
		if cy.out.YieldedTo != "" {
			yields++
			continue
		}
		if !cy.out.DirtyBuilt {
			continue
		}
		links++
		if len(cy.files) > 1 {
			t.Errorf("import cycle %d imported %d files %v, want one", i, len(cy.files), cy.files)
		}
		for _, p := range cy.files {
			seen[p]++
		}
		if cy.out.ImportFolded {
			folds++
		}
		if cy.out.DirtyChainReason != "" && links > 1 {
			// The first file stands direct on the commit generation (the
			// clean layer it replaces is no smaller a parent); every later
			// one must chain.
			t.Errorf("import cycle %d fell back (%s): %+v", i, cy.out.DirtyChainReason, cy.out)
		}
	}
	// A link that was folded right after it was built claims its file in
	// the fold, not in its own generation, so the distinct count is checked
	// against the imported files the routed stack claims at the end.
	for p, n := range seen {
		if n > 1 {
			t.Errorf("%s was imported by %d links; a completed file is never imported again", p, n)
		}
	}
	if folds == 0 {
		t.Errorf("the import never folded its chain (%d links)", links)
	}
	routedImported := map[string]struct{}{}
	for _, id := range routedStack(t, f, c)[1:] {
		for _, p := range claimedPaths(t, f.store, id) {
			if rel := strings.TrimPrefix(p, builderRepoPrefix+"/"); strings.HasPrefix(rel, "imported_") {
				routedImported[rel] = struct{}{}
			}
		}
	}
	if len(routedImported) != imported {
		t.Errorf("the imported working tree claims %d imported files, want %d", len(routedImported), imported)
	}
	var worst time.Duration
	for _, w := range waits {
		worst = max(worst, w)
	}
	t.Logf("import: %d cycles, %d file links, %d folds, %d yields; %d interactive probes, worst wait %s (all: %v)",
		len(cycles), links, folds, yields, len(waits), worst, waits)
	if worst > 100*time.Millisecond {
		t.Errorf("an interactive build waited %s for the import, want ≤ 100ms", worst)
	}
	if result := assertPropagationParity(t, f.store, routedStack(t, f, c), f.worktree, "import"); !result.ok() {
		t.Errorf("the imported working tree differs from a clean index: %v", result.Diffs)
	}
}

// TestCommittedBaseAdvancePropagatesLazilyAndReleasesByCopy is the committed
// regime, the one the daemon runs once a dependent exists: worktrees compose
// over the primary's published base generation.
//
//   - A per-save in the primary's working copy rewrites generation 0 only;
//     no dependent composes over it, so every dependent is untouched: no
//     cycle work, no build, and its view is still exactly its own tree.
//   - A primary commit publishes a new base. The dependent is told without a
//     cycle; when it next runs it stays pinned to the base it was built over
//     (its view is exact there) and builds nothing.
//   - When the retirement sweep asks for the old base back, the dependent is
//     recomposed over the new base with its working-tree chain carried over
//     by copy — the commit layer re-asserts the one path the primary changed
//     and the dependent did not — and its view is still exactly its own tree.
func TestCommittedBaseAdvancePropagatesLazilyAndReleasesByCopy(t *testing.T) {
	f := newCommittedBaseFixture(t)
	ctx := context.Background()
	c, root := f.pinDependent(t, "alpha")
	c.compaction.quiet = -1

	if first := c.reconcile(ctx); first.Err != nil || !first.CommitBuilt {
		t.Fatalf("the first cycle did not build the stack: %+v", first)
	}
	builderWriteFile(t, root, "alpha_dirty.go", "package fixture\n\nfunc AlphaDirty() {\n\tHelper()\n}\n")
	if out := c.reconcile(ctx); out.Err != nil || !out.DirtyBuilt {
		t.Fatalf("the working-tree edit was not built: %+v", out)
	}
	builderWriteFile(t, root, "alpha_more.go", "package fixture\n\nfunc AlphaMore() {\n\tAlphaDirty()\n}\n")
	if out := c.reconcile(ctx); out.Err != nil || out.DirtyChainDepth < 2 {
		t.Fatalf("the second edit did not chain: %+v", out)
	}
	before := f.routeOf(t, c.checkoutID)
	commit, _ := f.generation(before.CommitGenerationID)
	oldBase := commit.BaseGenerationID

	// A per-save in the primary: its working copy, generation 0, moves.
	builderWriteFile(t, f.primary, "core.go", propagationCoreC2)
	builderIndex(t, f.store, f.primary)
	if out := c.reconcile(ctx); out.Err != nil || out.CommitBuilt || out.DirtyBuilt || out.Recomposed || out.RebaseDeferred {
		t.Fatalf("a primary per-save moved a dependent composed over the committed base: %+v", out)
	}
	if now := f.routeOf(t, c.checkoutID); now != before {
		t.Fatalf("a primary per-save moved the dependent's route: %+v -> %+v", before, now)
	}
	assertNeutralViewIsItsOwnTree(t, f, c.checkoutID, root)

	// The primary commits the body edit and publishes it as the new base.
	builderGit(t, f.primary, "add", "-A")
	builderGit(t, f.primary, "commit", "-m", "body edit")
	newBase := f.publishBase(t)
	newTree := builderGit(t, f.primary, "rev-parse", "HEAD^{tree}")
	c.NoteBaseAdvance(newBase, newTree, "committed base advanced")
	if stats := c.PropagationStats(); stats.Noted != 1 || !stats.Pending {
		t.Fatalf("the advance was not noted: %+v", stats)
	}
	written := viewGenerationCount(t, f.store)
	pinned := c.reconcile(ctx)
	if pinned.Err != nil || !pinned.BasePinned || pinned.CommitBuilt || pinned.DirtyBuilt || pinned.Recomposed {
		t.Fatalf("the dependent acted on the advance instead of staying pinned: %+v", pinned)
	}
	if n := viewGenerationCount(t, f.store); n != written {
		t.Fatalf("the pinned cycle wrote %d generations", n-written)
	}
	assertNeutralViewIsItsOwnTree(t, f, c.checkoutID, root)

	// The sweep asks for the old base back: the recomposition carries the
	// chain over by copy.
	if !c.RequestBaseRelease(oldBase, "the sweep wants the base back") {
		t.Fatal("the release was not accepted")
	}
	out := c.reconcile(ctx)
	if out.Err != nil || !out.Recomposed || !out.DirtyReparented || out.DirtyBuilt || out.DirtyWork != nil {
		t.Fatalf("the released dependent was not recomposed by carrying its chain over: %+v", out)
	}
	if got := propagationActions(out.Propagation)["core.go"]; got != PropagationReassert {
		t.Errorf("core.go scoped %q, want %q (the primary moved it, the dependent did not)", got, PropagationReassert)
	}
	after := f.routeOf(t, c.checkoutID)
	recomposed, _ := f.generation(after.CommitGenerationID)
	if recomposed.BaseGenerationID != newBase {
		t.Fatalf("the recomposed commit layer sits on %d, want the new base %d", recomposed.BaseGenerationID, newBase)
	}
	if claimed := claimedPaths(t, f.store, after.CommitGenerationID); !slices.Contains(claimed, builderRepoPrefix+"/core.go") {
		t.Errorf("the recomposed commit layer does not re-assert core.go: %v", claimed)
	}
	assertNeutralViewIsItsOwnTree(t, f, c.checkoutID, root)
	if stats := c.PropagationStats(); stats.Pending || stats.Applied != 1 {
		t.Errorf("the applied rebase is still pending or uncounted: %+v", stats)
	}
}

// TestConsecutiveBodyEditsOfOneFileAreChainedDeltas pins that an edit is a
// delta over the previous working-tree generation, not a direct rebuild: ten
// consecutive body edits of one file — a one-file dirty set, the shape a
// planner that compared the delta with the dirty set refused every time —
// produce ten generations, each built over the one before (a chain the
// copy-based compactor folds when it is due), each parsing only the edited
// file, and the view is a clean index of the working tree after every one.
func TestConsecutiveBodyEditsOfOneFileAreChainedDeltas(t *testing.T) {
	f := newCoordinatorFixture(t)
	ctx := context.Background()
	c := chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
	if first := coordinatorReconcile(t, c); first.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle routed no working-tree layer: %+v", first)
	}
	// The worktree is already being worked on: one dirty file, built. (The
	// first edit of a clean tree has no parent worth reusing and goes direct.)
	builderWriteFile(t, f.worktree, "island.go", "package fixture\n\nfunc Island() int {\n\treturn 0\n}\n")
	previous := coordinatorReconcile(t, c)
	if !previous.DirtyBuilt {
		t.Fatalf("the working tree's first edit was not built: %+v", previous)
	}
	folds := 0
	for k := 1; k <= 10; k++ {
		builderWriteFile(t, f.worktree, "island.go",
			fmt.Sprintf("package fixture\n\nfunc Island() int {\n\treturn %d\n}\n", k))
		out := coordinatorReconcile(t, c)
		if !out.DirtyBuilt || out.DirtyChainReason != "" || out.DirtyParentGenerationID != previous.DirtyGenerationID {
			t.Fatalf("edit %d was not a delta over generation %d: %+v", k, previous.DirtyGenerationID, out)
		}
		// The delta claims every changed path on every edit: the edited
		// file alone.
		if claimed := claimedPaths(t, f.store, out.DirtyGenerationID); !slices.Equal(claimed, []string{builderRepoPrefix + "/island.go"}) {
			t.Errorf("edit %d claims %v, want the edited file alone", k, claimed)
		}
		if out.CompactionScheduled {
			report := c.compactDirtyChain(ctx, out)
			switch {
			case report.Outcome != dirtyChainCompactionFlipped:
				t.Fatalf("edit %d: the due compaction did not fold by copy: %+v", k, report)
			case report.Outcome == dirtyChainCompactionFlipped:
				folds++
				out.DirtyGenerationID = report.GenerationID
			}
		}
		result := assertPropagationParity(t, f.store, routedStack(t, f, c), f.worktree, fmt.Sprintf("edit-%d", k))
		if !result.ok() {
			t.Errorf("after edit %d the view differs from a clean index: %v", k, result.Diffs)
		}
		previous = out
	}
	t.Logf("10 edits: 10 chained deltas, %d folds", folds)
}

// assertPropagationParity is the clean-index oracle for these tests. The
// working-tree builds stamp per-file extraction
// fingerprints (source_*_fingerprint) on file nodes — bookkeeping a whole
// index never writes — so nodes and edges are compared through the
// fingerprint-neutral reader, strictly, and every other surface (files, symbol
// FTS, semantic binding types, constant values) through the strict harness.
func assertPropagationParity(t *testing.T, store *store_sqlite.Store, chain []int64, repoDir, label string) cleanParityResult {
	t.Helper()
	clean := builderOpenStore(t, "neutral-"+label)
	builderIndex(t, clean, repoDir)
	composed := fingerprintNeutralReader{dirtyChainComposed(t, store, chain)}
	flat := fingerprintNeutralReader{clean.AtGeneration(0)}
	result := assertCleanIndexParityChain(t, store, chain, repoDir, label, false)
	if !result.FilesEqual || !result.SymbolFTSEqual || !result.SemanticEqual || !result.ConstantsEqual || !result.MasksValid {
		return result
	}
	composedNodes, cleanNodes := builderRenderNodes(composed.AllNodes()), builderRenderNodes(flat.AllNodes())
	composedEdges, cleanEdges := builderRenderEdges(composed.AllEdges()), builderRenderEdges(flat.AllEdges())
	neutral := cleanParityResult{
		NodesEqual: slices.Equal(composedNodes, cleanNodes), EdgesEqual: slices.Equal(composedEdges, cleanEdges),
		FilesEqual: true, SymbolFTSEqual: true, SemanticEqual: true, ConstantsEqual: true, SearchProbesEqual: true,
		MasksValid: true, ReaderSurfaceAgree: true,
	}
	if !neutral.NodesEqual {
		neutral.Diffs = append(neutral.Diffs, parityDiff("nodes", composedNodes, cleanNodes)...)
	}
	if !neutral.EdgesEqual {
		neutral.Diffs = append(neutral.Diffs, parityDiff("edges", composedEdges, cleanEdges)...)
	}
	return neutral
}

// assertNeutralViewIsItsOwnTree is assertViewIsItsOwnTree through the
// fingerprint-neutral reader: the working-tree builds
// stamp extraction fingerprints on file nodes that a whole index never writes.
func assertNeutralViewIsItsOwnTree(t *testing.T, f *committedBaseFixture, checkoutID, root string) {
	t.Helper()
	materializer := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
	view, err := materializer.MaterializeCheckout(context.Background(), checkoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout(%s): %v", checkoutID, err)
	}
	defer view.Close()
	flatStore := builderOpenStore(t, "neutral-oracle-"+filepath.Base(root))
	builderIndex(t, flatStore, root)
	builderAssertReadersAgree(t, fingerprintNeutralReader{view.Reader}, fingerprintNeutralReader{flatStore.AtGeneration(0)})
}
