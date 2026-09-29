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

// TestALargeWorkingTreeChangeIsImportedFileByFileAndYieldsToAnInteractiveBuild
// pins the import that replaces the batched builds of a large working-tree
// change under the delta model. A `git checkout <branch> -- .` lands a dozen
// files in the worktree at once; the coordinator imports them one file per
// cycle, each a chained generation, through the real admission path and the
// shared build lane, folding the chain by copy whenever it reaches the
// compaction depth. Interactive builds of another checkout probe the lane all
// the while: each is admitted within 100 ms, because the import holds the lane
// for one file at a time and yields a file in flight. Nothing is re-parsed
// after it was imported, and the end state is exactly a clean index.
func TestALargeWorkingTreeChangeIsImportedFileByFileAndYieldsToAnInteractiveBuild(t *testing.T) {
	oldPaths := importInteractivePaths
	importInteractivePaths = 4
	t.Cleanup(func() { importInteractivePaths = oldPaths })

	f := newCoordinatorFixture(t)
	gate := NewViewBuildGate()
	gate.Open()
	outcomes := make(chan CheckoutCycle, 512)
	// The coordinator's own loop drives the import, exactly as in the daemon:
	// each file's cycle signals the next one through the quiet window.
	c := f.coordinator(t, CheckoutCoordinatorConfig{
		Gate:      gate,
		cycleDone: func(out CheckoutCycle) { outcomes <- out },
	})
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
	importDone := make(chan struct{})
	go func() {
		defer close(importDone)
		c.Signal("git checkout")
		for i := 0; i < 400; i++ {
			out, ok := await(ctx)
			if !ok {
				return
			}
			// The files a link imported are the imported paths its own
			// generation claims (whichever builder produced it); a fold
			// claims everything before it and is counted separately.
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
				return
			}
			if out.DirtyBuilt && out.DirtyBatchRemaining == 0 && !out.Rescheduled {
				return
			}
		}
	}()

	// Interactive builds of another checkout, while the import runs.
	var waits []time.Duration
	probe := time.NewTicker(40 * time.Millisecond)
	defer probe.Stop()
probing:
	for {
		select {
		case <-importDone:
			break probing
		case <-probe.C:
			asked := time.Now()
			actx, acancel := context.WithTimeout(ctx, 5*time.Second)
			release, err := gate.Acquire(actx, ViewBuildInteractive)
			acancel()
			if err != nil {
				t.Fatalf("an interactive build was not admitted during the import: %v", err)
			}
			waits = append(waits, time.Since(asked))
			release()
		}
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
