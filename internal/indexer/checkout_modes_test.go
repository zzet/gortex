package indexer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/reconcile"
)

// The mode-change tests run against the lifecycle fixture's real store, real
// git repositories and real indexes. What they are about is which rows move in
// which order, and a stub anywhere between the checkout and the catalog would
// remove exactly the ordering the flows exist to get right.

// familyFixture is one primary repository, one linked worktree, and whatever
// the lifecycle made of them.
type familyFixture struct {
	*lifecycleFixture
	main         string
	worktree     string
	mainPrefix   string
	primaryGraph string
	familyID     string
	automatic    store_sqlite.Checkout
}

// newFamilyFixture tracks a primary and sweeps once, which is what mints the
// worktree beside it as an automatic checkout with a coordinator.
func newFamilyFixture(t *testing.T, name string) *familyFixture {
	t.Helper()
	f := newLifecycleFixture(t)
	ctx := context.Background()

	main := f.gitRepo(name + "-main")
	worktree := f.worktreeOf(main, name+"-wt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: name + "-main"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	report, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, report.Coordinators,
		"the dedicated primary and observed automatic worktree each own a coordinator")

	out := &familyFixture{
		lifecycleFixture: f,
		main:             main,
		worktree:         worktree,
		mainPrefix:       tracked.Prefix,
		primaryGraph:     tracked.GraphID,
		familyID:         tracked.FamilyID,
	}
	out.automatic = out.otherCheckout(tracked.CheckoutID)
	return out
}

// otherCheckout is the family's checkout that is not the given one.
func (f *familyFixture) otherCheckout(exclude string) store_sqlite.Checkout {
	f.t.Helper()
	checkouts, err := f.catalog.ListCheckouts(context.Background(), f.familyID)
	require.NoError(f.t, err)
	for i := range checkouts {
		if checkouts[i].CheckoutID != exclude {
			return checkouts[i]
		}
	}
	f.t.Fatalf("family %s holds no checkout besides %s", f.familyID, exclude)
	return store_sqlite.Checkout{}
}

// runCoordinator drives one checkout's coordinator through a cycle, so a test
// never waits on the quiet window.
//
// The loop is stopped first, and stays stopped. Its quiet window and its poll
// are wall-clock timers, so a cycle it fires on its own lands at a moment
// nothing in the test ordered — and over a working tree the test moved in
// between, that cycle installs a route the assertions never asked for.
// Stopping it leaves the coordinator in the registry, which is what the
// lifecycle still hands out, signals and drops. The lock is still taken,
// because a cycle already in flight runs to completion under it.
func (f *lifecycleFixture) runCoordinator(checkoutID string) CheckoutCycle {
	f.t.Helper()
	coordinator := lifecycleCoordinator(f.t, f.lc, checkoutID)
	require.NoError(f.t, coordinator.Close())
	coordinator.cycleMu.Lock()
	cycle := coordinator.reconcile(context.Background())
	coordinator.cycleMu.Unlock()
	require.NoError(f.t, cycle.Err)
	return cycle
}

// routeOf reads one checkout's route.
func (f *lifecycleFixture) routeOf(checkoutID string) (store_sqlite.CheckoutRoute, bool) {
	f.t.Helper()
	route, found, err := f.catalog.GetCheckoutRoute(context.Background(), checkoutID)
	require.NoError(f.t, err)
	return route, found
}

// materialize opens the view a checkout's queries land on. The caller closes it.
func (f *lifecycleFixture) materialize(checkoutID string) *graphview.RepoView {
	f.t.Helper()
	materializer := &graphview.Materializer{
		Store: f.store, Catalog: f.catalog, Leases: f.lc.ViewLeases(),
	}
	view, err := materializer.MaterializeCheckout(context.Background(), checkoutID)
	require.NoError(f.t, err)
	return view
}

// configLists reports whether the persisted tracked set holds a path, matched
// by filesystem identity: git spells a worktree root with its symlinks
// resolved, and a temporary directory is reached through one on some
// platforms.
func (f *lifecycleFixture) configLists(path string) bool {
	f.t.Helper()
	want, err := os.Stat(path)
	for _, tracked := range f.configPaths() {
		if tracked == path {
			return true
		}
		if err != nil {
			continue
		}
		if got, statErr := os.Stat(tracked); statErr == nil && os.SameFile(got, want) {
			return true
		}
	}
	return false
}

// contentIdentities is what one repository's nodes are called in a reader,
// with the repo prefix stripped off. The prefix is part of every node id and
// it is exactly the part a mode change is allowed to have moved, so stripping
// it is what lets one working tree be compared across a move from one corpus
// to another.
func contentIdentities(r graph.Reader, prefix string) []string {
	nodes := r.GetRepoNodes(prefix)
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}
		out = append(out, strings.TrimPrefix(node.ID, prefix+"/"))
	}
	slices.Sort(out)
	return out
}

// --- the naming rule ----------------------------------------------------

// TestRegisterNamesAWorktreeByItsAdminName pins the prefix rule at the seam
// the registration path goes through: a worktree that gets a corpus of its own
// is named for the family's base and the name git administers it under, not
// for the branch that happens to be checked out.
func TestRegisterNamesAWorktreeByItsAdminName(t *testing.T) {
	f := newFamilyFixture(t, "naming")
	defer f.close()
	ctx := context.Background()

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	adminName := f.automatic.AdminName
	require.NotEmpty(t, adminName)
	assert.Equal(t, f.mainPrefix+"@"+adminName, tracked.Prefix,
		"a new dedicated worktree corpus is named for the family base and the admin name")
	assert.NotContains(t, tracked.Prefix, filepath.Base(f.worktree)+"@",
		"the worktree's own basename is not the base of the name")

	// The branch moves; the prefix does not. That is the whole reason the
	// admin name is the tag.
	runGit(t, f.worktree, "checkout", "-q", "-b", "some-other-branch")
	again, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
	require.NoError(t, err)
	assert.Equal(t, tracked.Prefix, again.Prefix,
		"a re-track of an already-known prefix keeps it")
	assert.True(t, again.AlreadyTracked, "the corpus was not rebuilt")
}

// TestDedicatedWorktreePrefixCollisionIsReproducible states the half of the
// rule a daemon is not needed for: two checkouts that would take the same name
// are separated by a digest of the path alone, so the name a checkout will
// take can be worked out offline.
func TestDedicatedWorktreePrefixCollisionIsReproducible(t *testing.T) {
	held := "/srv/one/review"
	taken := func(prefix string) (string, bool) {
		if prefix == "app@review" {
			return held, true
		}
		return "", false
	}

	first := DedicatedWorktreePrefix("app", "review", held, taken)
	assert.Equal(t, "app@review", first, "the holder keeps the plain name")

	other := "/srv/two/review"
	loser := DedicatedWorktreePrefix("app", "review", other, taken)
	assert.Equal(t, "app@review-"+shortPathHash(other), loser)
	assert.Equal(t, loser, DedicatedWorktreePrefix("app", "review", other, taken),
		"the same inputs name the same prefix every time")
	assert.Equal(t, "app@review", DedicatedWorktreePrefix("app", "review", other, nil),
		"with nothing registered there is nothing to collide with")
}

// --- promotion ----------------------------------------------------------

// TestPromoteGivesAnAutomaticCheckoutItsOwnCorpus walks the whole flow: the
// journalled intent, the new corpus under the admin-name prefix, the mode
// flip, and the dedicated route and coordinator replacing the automatic pair.
func TestPromoteGivesAnAutomaticCheckoutItsOwnCorpus(t *testing.T) {
	f := newFamilyFixture(t, "promote")
	defer f.close()
	ctx := context.Background()

	f.runCoordinator(f.automatic.CheckoutID)
	before, found := f.routeOf(f.automatic.CheckoutID)
	require.True(t, found)
	require.NotZero(t, before.CommitGenerationID, "the automatic view is not serving yet")

	result, err := f.lc.PromoteCheckout(ctx, f.automatic.CheckoutID, TrackSourceMCP)
	require.NoError(t, err)
	assert.Equal(t, f.automatic.CheckoutID, result.CheckoutID, "a promotion keeps the identity")
	assert.Equal(t, f.mainPrefix+"@"+f.automatic.AdminName, result.Prefix)
	require.NotNil(t, result.Index, "the worktree was indexed as its own base corpus")
	assert.Zero(t, result.Resampled)

	promoted, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, store_sqlite.CheckoutModeDedicated, promoted.EffectiveMode)
	assert.Equal(t, store_sqlite.CheckoutModeDedicated, promoted.DesiredMode)

	graph, found, err := f.catalog.GetDedicatedGraph(ctx, result.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	assert.False(t, graph.IsPrimaryBase, "a promotion never moves the family's base")
	assert.Equal(t, f.familyID, graph.FamilyID)

	route, routed := f.routeOf(f.automatic.CheckoutID)
	require.True(t, routed, "the dedicated route replaces the automatic route")
	assert.Equal(t, result.GraphID, route.GraphID)
	assert.NotZero(t, route.CommitGenerationID)
	assert.NotZero(t, route.DirtyGenerationID)
	assert.True(t, f.lc.SignalCheckout(f.automatic.CheckoutID, "test"),
		"a dedicated checkout retains its own coordinator")
	assert.NotNil(t, f.mi.GetMetadata(result.Prefix), "the new corpus is served")
	assert.True(t, f.configLists(f.worktree), "the promotion is persisted")
	assert.False(t, f.watcher.isAttached(result.Prefix),
		"the route-owned immutable corpus is not watched as a live filesystem base")

	intents, err := f.catalog.ListTrackingIntents(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.Len(t, intents, 1)
	assert.Equal(t, TrackSourceMCP, intents[0].SourceKind)
	_, pending, err := f.catalog.GetIntentTransition(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	assert.False(t, pending, "a promotion that landed releases its journal slot")
}

// TestPromoteRollsBackWhenTheCheckoutMovesUnderIt is the failure half.
//
// The corpus a promotion builds has to describe the checkout it is built for.
// A working tree that commits under the index produced one that describes
// neither state, so the build is taken again — and a checkout that will not
// hold still for two of them fails the promotion outright, with the automatic
// view still serving exactly what it served before.
func TestPromoteRollsBackWhenTheCheckoutMovesUnderIt(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("rollback-main")
	worktree := f.worktreeOf(main, "rollback-wt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "rollback-main"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	// Every index of the worktree is followed by a commit in it, so no corpus
	// can ever describe the state it was sampled at.
	moves := 0
	f.lc.indexBarrier = func() {
		moves++
		writeFile(t, filepath.Join(worktree, "moved.go"),
			"package a\n\nfunc Moved"+string(rune('A'+moves))+"() {}\n")
		require.NoError(t, exec.Command("git", "-C", worktree, "add", "moved.go").Run())
		require.NoError(t, exec.Command(
			"git", "-C", worktree, "commit", "-m", "move-"+strconv.Itoa(moves),
		).Run())
	}

	report, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, report.Coordinators,
		"the dedicated primary and observed automatic worktree each own a coordinator")
	automatic := store_sqlite.Checkout{}
	checkouts, err := f.catalog.ListCheckouts(ctx, tracked.FamilyID)
	require.NoError(t, err)
	for i := range checkouts {
		if checkouts[i].CheckoutID != tracked.CheckoutID {
			automatic = checkouts[i]
		}
	}
	require.NotEmpty(t, automatic.CheckoutID)

	cycle := f.runCoordinator(automatic.CheckoutID)
	served := f.materialize(automatic.CheckoutID)
	before := contentIdentities(served.Reader, tracked.Prefix)
	served.Close()

	prefix := tracked.Prefix + "@" + automatic.AdminName
	result, err := f.lc.PromoteCheckout(ctx, automatic.CheckoutID, TrackSourceCLI)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCheckoutMoved), "got %v", err)
	assert.Equal(t, 2, result.Resampled,
		"both unstable samples are rejected before the retry budget is exhausted")
	assert.True(t, result.Retryable, "nothing moved, so the same call is the whole of the recovery")

	still, found, err := f.catalog.GetCheckout(ctx, automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, store_sqlite.CheckoutModeAutomatic, still.EffectiveMode,
		"a failed promotion leaves the checkout where it was")
	route, routed := f.routeOf(automatic.CheckoutID)
	require.True(t, routed, "the automatic route is still installed")
	assert.Equal(t, cycle.CommitGenerationID, route.CommitGenerationID)
	assert.Equal(t, cycle.DirtyGenerationID, route.DirtyGenerationID)
	assert.Nil(t, f.mi.GetMetadata(prefix), "the half-built corpus was evicted")
	_, bound, err := f.catalog.GetDedicatedGraph(ctx, GraphIDFor(prefix))
	require.NoError(t, err)
	assert.False(t, bound, "no graph row survives a rolled-back promotion")
	assert.False(t, f.configLists(worktree), "nothing was added to the tracked set")

	after := f.materialize(automatic.CheckoutID)
	defer after.Close()
	assert.Equal(t, before, contentIdentities(after.Reader, tracked.Prefix),
		"the automatic view still serves")

	transition, pending, err := f.catalog.GetIntentTransition(ctx, automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, pending, "the journalled intent stands, so the call can be retried")
	assert.Equal(t, store_sqlite.IntentTransitionPending, transition.State)
	assert.NotEmpty(t, transition.LastError)
}

// TestPromoteCapturesASettledDirtyChange is the other side of the same rule:
// a dirty-only move does not invalidate the immutable HEAD base, and is instead
// captured by the dirty layer built over it.
func TestPromoteCapturesASettledDirtyChange(t *testing.T) {
	f := newFamilyFixture(t, "resample")
	defer f.close()
	ctx := context.Background()

	moved := false
	f.lc.indexBarrier = func() {
		if moved {
			return
		}
		moved = true
		writeFile(t, filepath.Join(f.worktree, "late.go"), "package a\n\nfunc Late() {}\n")
	}

	result, err := f.lc.PromoteCheckout(ctx, f.automatic.CheckoutID, TrackSourceCLI)
	require.NoError(t, err)
	assert.Zero(t, result.Resampled,
		"a dirty-only change does not invalidate the captured HEAD tree")
	assert.NotNil(t, f.mi.GetMetadata(result.Prefix))
	assert.Equal(t, store_sqlite.CheckoutModeDedicated,
		f.checkoutOf(result.Prefix).EffectiveMode)
	view := f.materialize(f.automatic.CheckoutID)
	defer view.Close()
	identities := contentIdentities(view.Reader, result.Prefix)
	assert.Contains(t, identities, "late.go")
	assert.Contains(t, identities, "late.go::Late")
}

// --- demotion -----------------------------------------------------------

// TestUntrackDemotesADedicatedWorktree is the untrack path for a working copy
// the family can still serve.
//
// The checkout keeps its identity and its view; what it loses is the corpus of
// its own. The composed stack it flips onto has to carry the same content the
// dedicated corpus did — the working tree did not move, only where it is read
// from.
func TestUntrackDemotesADedicatedWorktree(t *testing.T) {
	f := newFamilyFixture(t, "demote")
	defer f.close()
	ctx := context.Background()

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	require.Equal(t, f.automatic.CheckoutID, tracked.CheckoutID)
	dedicatedView := f.materialize(tracked.CheckoutID)
	dedicatedContent := contentIdentities(dedicatedView.Reader, tracked.Prefix)
	dedicatedView.Close()
	require.NotEmpty(t, dedicatedContent)

	preview, err := f.lc.PreviewUntrack(ctx, f.worktree)
	require.NoError(t, err)
	assert.Equal(t, UntrackPlanDemote, preview.Plan)
	assert.False(t, preview.IsPrimary)
	require.NotEmpty(t, preview.Closure)
	assert.Equal(t, reconcile.DependentGraph, preview.Closure[0].Kind)

	result, err := f.lc.Untrack(ctx, f.worktree)
	require.NoError(t, err)
	assert.True(t, result.Demoted)
	assert.Equal(t, UntrackPlanDemote, result.Plan)
	assert.Equal(t, []string{string(TrackSourceCLI)}, result.Revoked)

	demoted, found, err := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, found, "a demotion keeps the checkout id")
	assert.Equal(t, store_sqlite.CheckoutModeAutomatic, demoted.EffectiveMode)
	assert.Equal(t, f.automatic.Incarnation, demoted.Incarnation, "and its incarnation")

	route, routed := f.routeOf(tracked.CheckoutID)
	require.True(t, routed)
	assert.Equal(t, f.primaryGraph, route.GraphID, "it is served from the family primary now")
	assert.NotZero(t, route.CommitGenerationID)
	assert.NotZero(t, route.DirtyGenerationID)
	assert.Equal(t, store_sqlite.RouteActive, route.State)

	_, bound, err := f.catalog.GetDedicatedGraph(ctx, GraphIDFor(tracked.Prefix))
	require.NoError(t, err)
	assert.False(t, bound, "the corpus it left is retired")
	assert.Nil(t, f.mi.GetMetadata(tracked.Prefix))
	assert.False(t, f.configLists(f.worktree), "the demoted worktree left the tracked set")
	assert.True(t, f.lc.SignalCheckout(tracked.CheckoutID, "test"),
		"an automatic checkout has a coordinator listening")

	view := f.materialize(tracked.CheckoutID)
	defer view.Close()
	assert.Equal(t, dedicatedContent, contentIdentities(view.Reader, f.mainPrefix),
		"the composed stack carries what the dedicated corpus carried")
}

// TestUntrackIsBlockedWithoutAnotherReadyPrimary states the rule that stops an
// untrack from silently deleting the only copy of something.
//
// A dedicated checkout whose family has no other ready primary cannot be
// demoted — there is nothing to serve it from — and an untrack must not
// quietly turn into a forget because of it. The refusal names both ways out.
func TestUntrackIsBlockedWithoutAnotherReadyPrimary(t *testing.T) {
	f := newFamilyFixture(t, "blocked")
	defer f.close()
	ctx := context.Background()

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	primary, found, err := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	require.NoError(t, err)
	require.True(t, found)
	primary.State = "graph_degraded"
	require.NoError(t, f.catalog.UpsertDedicatedGraph(ctx, primary))

	preview, err := f.lc.PreviewUntrack(ctx, f.worktree)
	require.NoError(t, err)
	assert.Equal(t, UntrackPlanBlocked, preview.Plan)
	require.Len(t, preview.Blockers, 3)
	assert.Contains(t, strings.Join(preview.Blockers, " "), "set another checkout's graph as the family primary")
	assert.Contains(t, strings.Join(preview.Blockers, " "), "preview a forget")

	_, err = f.lc.Untrack(ctx, f.worktree)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUntrackBlocked), "got %v", err)

	assert.NotNil(t, f.mi.GetMetadata(tracked.Prefix), "nothing was torn down")
	intents, err := f.catalog.ListTrackingIntents(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.Len(t, intents, 1)
	assert.True(t, intents[0].Active, "a blocked untrack revokes nothing")
}

// TestDemoteRefusesAStaleIncarnation covers the interleaving the guard exists
// for: the checkout was re-keyed while the demotion was building its layers,
// so the identity the flip names is not the one the catalog holds.
//
// The route the rebuild installed describes a checkout that is still dedicated
// and is therefore withdrawn again — a failure before the flip leaves the
// dedicated state exactly as it was.
func TestDemoteRefusesAStaleIncarnation(t *testing.T) {
	f := newFamilyFixture(t, "stale")
	defer f.close()
	ctx := context.Background()

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	checkout, found, err := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	owned, primary, err := f.lc.familyGraphsFor(ctx, checkout)
	require.NoError(t, err)
	require.NotNil(t, owned)
	require.NotNil(t, primary)

	family, found, err := f.catalog.GetRepositoryFamily(ctx, checkout.FamilyID)
	require.NoError(t, err)
	require.True(t, found)
	authorization, err := f.lc.Reconciler().AuthorizeDemotion(
		ctx, checkout, owned.GraphID, primary.GraphID, family.PrimaryEpoch)
	require.NoError(t, err)

	stale := checkout
	stale.Incarnation = "incarnation-that-was-replaced"
	err = f.lc.demote(ctx, stale, owned, authorization)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store_sqlite.ErrCatalogStaleGuard), "got %v", err)

	held, found, err := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, store_sqlite.CheckoutModeDedicated, held.EffectiveMode)
	_, routed := f.routeOf(tracked.CheckoutID)
	assert.False(t, routed, "the route the rebuild installed was withdrawn again")
	assert.NotNil(t, f.mi.GetMetadata(tracked.Prefix), "its corpus is untouched")
	assert.True(t, f.lc.SignalCheckout(tracked.CheckoutID, "test"),
		"rollback did not preserve the coordinator for the still-dedicated checkout")
}

func modeTransitionRunFor(t *testing.T, lifecycle *CheckoutLifecycle, transitionID string) *modeTransitionRun {
	t.Helper()
	lifecycle.transitionMu.Lock()
	run := lifecycle.transitionRuns[transitionID]
	lifecycle.transitionMu.Unlock()
	require.NotNil(t, run, "transition %s has no lifecycle worker", transitionID)
	return run
}

func awaitModeTransition(t *testing.T, lifecycle *CheckoutLifecycle, transitionID string) modeTransitionOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	outcome, err := waitModeTransition(ctx, modeTransitionRunFor(t, lifecycle, transitionID))
	require.NoError(t, err)
	return outcome
}

func TestPromotionWorkerCoalescesOneDurableTransition(t *testing.T) {
	f := newFamilyFixture(t, "promote-coalesce")
	defer f.close()

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	f.lc.indexBarrier = func() {
		enteredOnce.Do(func() { close(entered) })
		<-release
	}

	first, err := f.lc.StartPromoteCheckout(context.Background(), f.automatic.CheckoutID, TrackSourceMCP)
	require.NoError(t, err)
	require.True(t, first.Pending)
	require.NotEmpty(t, first.TransitionID)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("promotion worker did not reach the index barrier")
	}
	second, err := f.lc.StartPromoteCheckout(context.Background(), f.automatic.CheckoutID, TrackSourceMCP)
	require.NoError(t, err)
	assert.Equal(t, first.TransitionID, second.TransitionID)
	assert.Same(t, modeTransitionRunFor(t, f.lc, first.TransitionID),
		modeTransitionRunFor(t, f.lc, second.TransitionID))

	releaseOnce.Do(func() { close(release) })
	outcome := awaitModeTransition(t, f.lc, first.TransitionID)
	require.NoError(t, outcome.err)
	assert.Equal(t, first.TransitionID, outcome.promotion.TransitionID)

	require.NoError(t, f.lc.resumeModeTransitions(context.Background()))
	f.lc.transitionMu.Lock()
	_, retained := f.lc.transitionRuns[first.TransitionID]
	f.lc.transitionMu.Unlock()
	assert.False(t, retained, "the authoritative empty journal prunes a completed run")
}

func TestSeedResumesDurablePromotion(t *testing.T) {
	f := newFamilyFixture(t, "promote-restart")
	defer f.close()
	ctx := context.Background()

	transition, err := f.lc.beginModeChange(ctx, f.automatic,
		store_sqlite.CheckoutModeDedicated, promotionTransitionCause, &store_sqlite.TrackingIntent{
			IntentID: "promote-restart-intent", CheckoutID: f.automatic.CheckoutID,
			SourceKind: TrackSourceMCP, SourceLocator: f.automatic.RootPath,
			Active: true, CreatedAt: f.clock.Now().Unix(),
		})
	require.NoError(t, err)
	f.restart()
	require.NoError(t, f.lc.Seed(ctx))

	outcome := awaitModeTransition(t, f.lc, transition.TransitionID)
	require.NoError(t, outcome.err)
	checkout, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, store_sqlite.CheckoutModeDedicated, checkout.EffectiveMode)
	_, pending, err := f.catalog.GetIntentTransition(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	assert.False(t, pending)
}

func TestSeedResumesDemotionWithoutRestoringRevokedConfigIntent(t *testing.T) {
	f := newFamilyFixture(t, "demote-restart")
	defer f.close()
	ctx := context.Background()

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	checkout := f.checkoutOf(tracked.Prefix)
	owned, primary, err := f.lc.familyGraphsFor(ctx, checkout)
	require.NoError(t, err)
	require.NotNil(t, owned)
	require.NotNil(t, primary)
	preview, err := f.lc.PreviewUntrack(ctx, f.worktree)
	require.NoError(t, err)
	authorization, err := f.lc.rec.AuthorizeDemotion(ctx, checkout,
		owned.GraphID, primary.GraphID, preview.PrimaryEpoch)
	require.NoError(t, err)
	require.True(t, f.configLists(f.worktree), "cleanup has not removed the stale config entry yet")

	f.restart()
	gate := NewViewBuildGate()
	f.lc.SetBuildGate(gate)
	require.NoError(t, f.lc.Seed(ctx))
	run := modeTransitionRunFor(t, f.lc, authorization.Transition.TransitionID)
	require.Never(t, func() bool {
		select {
		case <-run.done:
			return true
		default:
			return false
		}
	}, 100*time.Millisecond, 10*time.Millisecond, "worker crossed warmup gate")

	restored, err := f.lc.Register(ctx,
		config.RepoEntry{Path: f.main, Name: f.mainPrefix}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, restored.CatalogErr)
	require.False(t, restored.Pending, "a ready dedicated route requires no replacement build")
	require.Empty(t, restored.TransitionID)
	require.NotNil(t, f.mi.GetMetadata(restored.Prefix),
		"register did not restore the route-owned repository shell after restart")
	require.True(t, f.lc.hasCoordinator(restored.CheckoutID),
		"register did not restore the ready dedicated coordinator after restart")
	gate.Open()

	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	outcome, err := waitModeTransition(waitCtx, run)
	require.NoError(t, err)
	require.NoError(t, outcome.err)
	require.True(t, outcome.demoted)

	demoted, found, err := f.catalog.GetCheckout(ctx, checkout.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, store_sqlite.CheckoutModeAutomatic, demoted.EffectiveMode)
	intents, err := f.catalog.ListTrackingIntents(ctx, checkout.CheckoutID)
	require.NoError(t, err)
	for _, intent := range intents {
		assert.False(t, intent.Active, "restart reactivated revoked intent %+v", intent)
	}
	require.False(t, f.configLists(f.worktree),
		"durable demotion replay must remove the persisted explicit worktree entry")
}

func TestCloseCancelsAndJoinsModeTransitionWorker(t *testing.T) {
	f := newFamilyFixture(t, "promote-close")
	defer f.close()

	entered := make(chan struct{})
	var once sync.Once
	f.lc.indexBarrier = func() {
		once.Do(func() { close(entered) })
		<-f.lc.transitionCtx.Done()
	}
	started, err := f.lc.StartPromoteCheckout(context.Background(), f.automatic.CheckoutID, TrackSourceMCP)
	require.NoError(t, err)
	run := modeTransitionRunFor(t, f.lc, started.TransitionID)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("promotion worker did not reach the index barrier")
	}
	require.NoError(t, f.lc.Close())
	select {
	case <-run.done:
	case <-time.After(time.Second):
		t.Fatal("Close returned before the transition worker joined")
	}
	assert.Error(t, run.outcome.err)
}

// --- set-primary --------------------------------------------------------

// TestSetPrimaryRebuildsEveryDependentBeforeItFlips is the transition test.
//
// Moving a family's base invalidates every automatic checkout's stack at once.
// What must never happen is a window in which a checkout's route names the new
// base with no layers over it: a query landing there would answer from the new
// primary's working tree while claiming to be the checkout's. So the stack is
// rebuilt off-route and installed in one write, and a reader crossing the
// transition sees the whole of one state or the whole of the other.
func TestSetPrimaryRebuildsEveryDependentBeforeItFlips(t *testing.T) {
	f := newFamilyFixture(t, "setprimary")
	defer f.close()
	ctx := context.Background()

	// A second worktree with a corpus of its own — the candidate primary. Its
	// working tree is committed first: a base corpus holding uncommitted files
	// shows them through every layer built over it, because no tree-to-tree
	// diff can mention a path that is in neither tree.
	candidate := f.worktreeOf(f.main, "setprimary-alt")
	runGit(t, candidate, "add", "-A")
	runGit(t, candidate, "commit", "-q", "-m", "alt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: candidate}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	require.NotEqual(t, f.primaryGraph, tracked.GraphID)

	f.runCoordinator(f.automatic.CheckoutID)
	beforeRoute, found := f.routeOf(f.automatic.CheckoutID)
	require.True(t, found)
	before := f.materialize(f.automatic.CheckoutID)
	beforeIDs := contentIdentities(before.Reader, f.mainPrefix)
	before.Close()

	preview, err := f.lc.PreviewSetPrimary(ctx, tracked.GraphID)
	require.NoError(t, err)
	assert.True(t, preview.Ready, "blockers: %v", preview.Blockers)
	assert.Equal(t, f.primaryGraph, preview.CurrentGraphID)
	require.Len(t, preview.Dependents, 1, "one automatic checkout rebuilds")
	assert.Equal(t, f.automatic.CheckoutID, preview.Dependents[0].ID)
	assert.Contains(t, preview.Dependents[0].Detail, "a coordinator is live for it")

	// Materialize continuously across the move. Every view has to be one whole
	// stack: the one the transition started from, or the one it installed.
	var (
		wg      sync.WaitGroup
		stop    = make(chan struct{})
		mu      sync.Mutex
		torn    []string
		failed  []error
		reached bool
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		materializer := &graphview.Materializer{
			Store: f.store, Catalog: f.catalog, Leases: f.lc.ViewLeases(),
		}
		for {
			select {
			case <-stop:
				return
			default:
			}
			view, err := materializer.MaterializeCheckout(ctx, f.automatic.CheckoutID)
			if err != nil {
				mu.Lock()
				failed = append(failed, err)
				mu.Unlock()
				continue
			}
			// The route names the graph the view is read over, so the same
			// content is spelled with one prefix before the move and the
			// other after it.
			old := contentIdentities(view.Reader, f.mainPrefix)
			moved := contentIdentities(view.Reader, tracked.Prefix)
			view.Close()
			mu.Lock()
			switch {
			case slices.Equal(old, beforeIDs), slices.Equal(moved, beforeIDs):
				reached = true
			default:
				torn = append(torn, strings.Join(old, ",")+" / "+strings.Join(moved, ","))
			}
			mu.Unlock()
		}
	}()

	result, err := f.lc.SetPrimary(ctx, tracked.GraphID)
	close(stop)
	wg.Wait()
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.Equal(t, []string{f.automatic.CheckoutID}, result.Rebuilt)
	assert.Empty(t, result.Stale)

	mu.Lock()
	assert.Empty(t, failed, "a materialization crossed a route with no stack behind it")
	assert.Empty(t, torn, "a materialization saw neither the old stack nor the new one")
	assert.True(t, reached, "the concurrent reader never got a view at all")
	mu.Unlock()

	moved, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, moved.IsPrimaryBase)
	incumbent, found, err := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	require.NoError(t, err)
	require.True(t, found)
	assert.False(t, incumbent.IsPrimaryBase, "one primary per family")

	afterRoute, found := f.routeOf(f.automatic.CheckoutID)
	require.True(t, found)
	assert.Equal(t, tracked.GraphID, afterRoute.GraphID)
	assert.Equal(t, beforeRoute.RouteEpoch+1, afterRoute.RouteEpoch,
		"the graph and both slots moved in one compare-and-set")
	assert.NotZero(t, afterRoute.CommitGenerationID)
	assert.NotZero(t, afterRoute.DirtyGenerationID)

	after := f.materialize(f.automatic.CheckoutID)
	defer after.Close()
	assert.Equal(t, beforeIDs, contentIdentities(after.Reader, tracked.Prefix),
		"the same working tree, read over a different base")
}

// TestSetPrimaryReportsACheckoutThatCannotRebuild covers the other outcome: a
// dependent that cannot be rebuilt keeps the route it has, which is a stale
// view of a real state rather than a broken one.
func TestSetPrimaryReportsACheckoutThatCannotRebuild(t *testing.T) {
	f := newFamilyFixture(t, "stalerebuild")
	defer f.close()
	ctx := context.Background()

	candidate := f.worktreeOf(f.main, "stalerebuild-alt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: candidate}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	f.runCoordinator(f.automatic.CheckoutID)
	before, found := f.routeOf(f.automatic.CheckoutID)
	require.True(t, found)

	// The dependent's working tree goes away between the preview and the
	// rebuild, so its stack cannot be built over anything.
	require.NoError(t, os.RemoveAll(f.worktree))

	result, err := f.lc.SetPrimary(ctx, tracked.GraphID)
	require.NoError(t, err)
	assert.Equal(t, []string{f.automatic.CheckoutID}, result.Stale)
	assert.Empty(t, result.Rebuilt)
	require.Len(t, result.Errors, 1)

	after, found := f.routeOf(f.automatic.CheckoutID)
	require.True(t, found)
	assert.Equal(t, before, after, "a checkout that could not rebuild keeps its route exactly")
}

// --- the primary's own untrack ------------------------------------------

// TestPreviewUntrackListsThePrimaryClosure is the preview a primary's untrack
// has to show before it is allowed to happen: everything that stops being
// servable, and everything that does not.
func TestPreviewUntrackListsThePrimaryClosure(t *testing.T) {
	f := newFamilyFixture(t, "closure")
	defer f.close()
	ctx := context.Background()

	sibling := f.worktreeOf(f.main, "closure-sib")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: sibling}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	f.runCoordinator(f.automatic.CheckoutID)

	preview, err := f.lc.PreviewUntrack(ctx, f.main)
	require.NoError(t, err)
	assert.Equal(t, UntrackPlanPrimaryClosure, preview.Plan)
	assert.True(t, preview.IsPrimary)
	assert.Equal(t, f.primaryGraph, preview.GraphID)

	kinds := map[reconcile.DependentKind][]string{}
	for _, dependent := range preview.Closure {
		kinds[dependent.Kind] = append(kinds[dependent.Kind], dependent.ID)
	}
	assert.Equal(t, []string{f.primaryGraph}, kinds[reconcile.DependentGraph],
		"the primary graph itself is in the closure")
	owner := f.checkoutOf(f.mainPrefix)
	assert.ElementsMatch(t, []string{f.automatic.CheckoutID, owner.CheckoutID},
		kinds[reconcile.DependentCheckout],
		"the automatic checkout and owner identity the saga forgets; the dedicated sibling is not one")
	assert.ElementsMatch(t, []string{f.automatic.CheckoutID, owner.CheckoutID},
		kinds[reconcile.DependentRoute],
		"both live routes owned by the primary closure are named")
	automaticRoute, routed := f.routeOf(f.automatic.CheckoutID)
	require.True(t, routed)
	ownerRoute, routed := f.routeOf(owner.CheckoutID)
	require.True(t, routed)
	primary, found, err := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	require.NoError(t, err)
	require.True(t, found)
	assert.ElementsMatch(t, []string{
		layerID(primary.ActiveGenerationID),
		layerID(automaticRoute.CommitGenerationID),
		layerID(automaticRoute.DirtyGenerationID),
		layerID(ownerRoute.CommitGenerationID),
		layerID(ownerRoute.DirtyGenerationID),
	}, kinds[reconcile.DependentLayer], "the immutable base and every routed layer are named")

	require.Len(t, preview.Preserved, 1)
	assert.Equal(t, tracked.GraphID, preview.Preserved[0].ID,
		"an independent sibling survives the primary")
	assert.False(t, preview.SolePrimary, "something in the family is left to serve from")

	// The preview is a read. Nothing moved.
	assert.NotNil(t, f.mi.GetMetadata(f.mainPrefix))
	assert.NotNil(t, f.mi.GetMetadata(tracked.Prefix))
}

// TestUntrackAPrimaryRetiresItsClosure confirms what the preview described,
// and leaves the independent sibling alone.
func TestUntrackAPrimaryRetiresItsClosure(t *testing.T) {
	f := newFamilyFixture(t, "retire")
	defer f.close()
	ctx := context.Background()

	sibling := f.worktreeOf(f.main, "retire-sib")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: sibling}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	f.runCoordinator(f.automatic.CheckoutID)

	result, err := f.lc.Untrack(ctx, f.main)
	require.NoError(t, err)
	assert.Equal(t, UntrackPlanPrimaryClosure, result.Plan)
	assert.Equal(t, []string{string(TrackSourceCLI)}, result.Revoked)

	assert.Nil(t, f.mi.GetMetadata(f.mainPrefix), "the primary corpus is gone")
	_, found, err := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	require.NoError(t, err)
	assert.False(t, found)
	_, found, err = f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	assert.False(t, found, "the dependent automatic checkout went with it")

	surviving, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, found, "the independent sibling is preserved")
	assert.NotNil(t, f.mi.GetMetadata(tracked.Prefix))
	assert.Equal(t, f.familyID, surviving.FamilyID, "and its family survives with it")
}

// TestUntrackASolePrimaryForgetsTheFamily is the closure with nothing left
// underneath it: no dedicated graph survives the retirement, so it carries on
// into a family teardown — and the preview says so before the confirm does it.
func TestUntrackASolePrimaryForgetsTheFamily(t *testing.T) {
	f := newFamilyFixture(t, "sole")
	defer f.close()
	ctx := context.Background()

	preview, err := f.lc.PreviewUntrack(ctx, f.main)
	require.NoError(t, err)
	assert.Equal(t, UntrackPlanPrimaryClosure, preview.Plan)
	assert.True(t, preview.SolePrimary, "nothing in the family survives this")
	assert.Empty(t, preview.Preserved)

	family := []string{}
	for _, dependent := range preview.Closure {
		if dependent.Kind == reconcile.DependentFamily {
			family = append(family, dependent.ID)
		}
	}
	assert.Equal(t, []string{f.familyID}, family,
		"the family row the teardown continues into is enumerated, not left to the flag")

	_, err = f.lc.Untrack(ctx, f.main)
	require.NoError(t, err)

	_, found, err := f.catalog.GetRepositoryFamily(ctx, f.familyID)
	require.NoError(t, err)
	assert.False(t, found, "the family row went with its last graph")
	checkouts, err := f.catalog.ListCheckouts(ctx, f.familyID)
	require.NoError(t, err)
	assert.Empty(t, checkouts)
}

// TestPrimaryClosureRefusesAStaleEpoch covers the interleaving between a
// preview and its confirm: the family's primary moved in between, so the
// closure the caller is holding describes a graph that is no longer the base.
func TestPrimaryClosureRefusesAStaleEpoch(t *testing.T) {
	f := newFamilyFixture(t, "epoch")
	defer f.close()
	ctx := context.Background()

	candidate := f.worktreeOf(f.main, "epoch-alt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: candidate}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	preview, err := f.lc.PreviewUntrack(ctx, f.main)
	require.NoError(t, err)
	require.Equal(t, UntrackPlanPrimaryClosure, preview.Plan)

	_, err = f.lc.SetPrimary(ctx, tracked.GraphID)
	require.NoError(t, err)

	err = f.lc.Reconciler().RetirePrimaryClosure(ctx, preview.GraphID, preview.PrimaryEpoch)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store_sqlite.ErrCatalogStaleGuard), "got %v", err)

	_, found, err := f.catalog.GetDedicatedGraph(ctx, preview.GraphID)
	require.NoError(t, err)
	assert.True(t, found, "a refused closure removes nothing")
}

// layerID renders a generation id the way a closure names it.
func layerID(generationID int64) string {
	return strconv.FormatInt(generationID, 10)
}

// liveCoordinatorOrNil peeks the registry without insisting on a hit, so a test
// can ask whether a cycle could still be handed out at a given moment.
func liveCoordinatorOrNil(l *CheckoutLifecycle, checkoutID string) *CheckoutCoordinator {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	return l.coordinators[checkoutID]
}

// TestPromotionDoesNotUseTheLegacyRouteWithdrawal proves the dedicated route,
// graph marker, and mode flip are one publication. A failure injected at the
// retired automatic-route cleanup seam cannot split that transaction.
func TestPromotionDoesNotUseTheLegacyRouteWithdrawal(t *testing.T) {
	f := newFamilyFixture(t, "routefail")
	defer f.close()
	ctx := context.Background()

	f.runCoordinator(f.automatic.CheckoutID)
	before, routed := f.routeOf(f.automatic.CheckoutID)
	require.True(t, routed, "the automatic view is not serving yet")

	f.lc.routeBarrier = func(context.Context, string) error {
		return errors.New("the catalog is refusing writes")
	}

	result, err := f.lc.PromoteCheckout(ctx, f.automatic.CheckoutID, TrackSourceCLI)
	require.NoError(t, err, "the flip landed, so the promotion happened")
	assert.False(t, result.Retryable)
	assert.Equal(t, f.mainPrefix+"@"+f.automatic.AdminName, result.Prefix)

	promoted, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, store_sqlite.CheckoutModeDedicated, promoted.EffectiveMode)
	assert.NotNil(t, f.mi.GetMetadata(result.Prefix), "the corpus the mode points at is served")
	_, bound, err := f.catalog.GetDedicatedGraph(ctx, result.GraphID)
	require.NoError(t, err)
	assert.True(t, bound, "the graph row binding that corpus survives")
	_, pending, err := f.catalog.GetIntentTransition(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	assert.False(t, pending, "a promotion that happened journals no retry")

	dedicated, routed := f.routeOf(f.automatic.CheckoutID)
	require.True(t, routed, "the atomic publication installs the dedicated route")
	assert.NotEqual(t, before.GraphID, dedicated.GraphID)
	assert.Equal(t, result.GraphID, dedicated.GraphID)
	assert.NotZero(t, dedicated.CommitGenerationID)
	assert.NotZero(t, dedicated.DirtyGenerationID)

	f.lc.routeBarrier = nil
	_, err = f.lc.Sweep(ctx)
	require.NoError(t, err)
	stillDedicated, routed := f.routeOf(f.automatic.CheckoutID)
	require.True(t, routed, "reconciliation retains the dedicated route")
	assert.Equal(t, dedicated, stillDedicated)
	assert.Equal(t, store_sqlite.CheckoutModeDedicated,
		f.checkoutOf(result.Prefix).EffectiveMode)
}

// TestApplyUntrackRefusesARekeyedCheckout is the identity half of the
// preview-and-confirm contract.
//
// The plan a caller confirmed was decided against one incarnation of the path.
// A checkout re-keyed in between — a root removed and recreated — is a
// different identity under the same name, and demoting or forgetting it would
// carry out a decision nobody was shown.
func TestApplyUntrackRefusesARekeyedCheckout(t *testing.T) {
	f := newFamilyFixture(t, "rekeyed")
	defer f.close()
	ctx := context.Background()

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	preview, err := f.lc.PreviewUntrack(ctx, f.worktree)
	require.NoError(t, err)
	require.Equal(t, UntrackPlanDemote, preview.Plan)
	require.NotEmpty(t, preview.Incarnation)

	rekeyed, found, err := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	rekeyed.Incarnation = "incarnation-minted-after-the-preview"
	require.NoError(t, f.catalog.UpsertCheckout(ctx, rekeyed))

	_, err = f.lc.ApplyUntrack(ctx, preview)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store_sqlite.ErrCatalogStaleGuard), "got %v", err)

	held, found, err := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, store_sqlite.CheckoutModeDedicated, held.EffectiveMode, "nothing was demoted")
	assert.NotNil(t, f.mi.GetMetadata(tracked.Prefix), "its corpus is untouched")
	intents, err := f.catalog.ListTrackingIntents(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.Len(t, intents, 1)
	assert.True(t, intents[0].Active, "a refused confirm revokes nothing")
}

// TestApplyUntrackRefusesADemoteWhoseGraphBecameThePrimary re-asks the demote
// plan's own precondition at confirm time.
//
// A demote means "this checkout's corpus goes and the family's primary serves
// it instead". A SetPrimary landing between the preview and the confirm makes
// the checkout's own graph that base, and running the plan anyway rehomes the
// checkout onto its own corpus, flips it to automatic, and then cannot retire
// the graph it is being served from — a family whose base is owned by an
// automatic checkout.
func TestApplyUntrackRefusesADemoteWhoseGraphBecameThePrimary(t *testing.T) {
	f := newFamilyFixture(t, "demotestale")
	defer f.close()
	ctx := context.Background()

	// The worktree is committed first: a base corpus holding uncommitted files
	// shows them through every layer built over it.
	runGit(t, f.worktree, "add", "-A")
	runGit(t, f.worktree, "commit", "-q", "-m", "wt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	preview, err := f.lc.PreviewUntrack(ctx, f.worktree)
	require.NoError(t, err)
	require.Equal(t, UntrackPlanDemote, preview.Plan)

	_, err = f.lc.SetPrimary(ctx, tracked.GraphID)
	require.NoError(t, err)

	_, err = f.lc.ApplyUntrack(ctx, preview)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUntrackBlocked), "got %v", err)

	held, found, err := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, store_sqlite.CheckoutModeDedicated, held.EffectiveMode)
	base, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, base.IsPrimaryBase, "the family's base is left where SetPrimary put it")
	assert.NotNil(t, f.mi.GetMetadata(tracked.Prefix), "and its corpus with it")
	intents, err := f.catalog.ListTrackingIntents(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.Len(t, intents, 1)
	assert.True(t, intents[0].Active, "a refused confirm revokes nothing")
}

// TestSetPrimaryDropsDependentCoordinatorsBeforeTheEpochMoves closes the window
// the off-route rebuild exists for.
//
// The registry is the only thing that can hand a coordinator a cycle, and a
// cycle taken once the epoch has moved is an ordinary one: it repoints the
// route at the new base and clears both slots for the length of a full build,
// which drops every query landing there onto the base corpus. So no dependent
// may still be in the registry between the compare-and-set and its own rebuild.
//
// Two automatic checkouts are what make that window observable. Dropping each
// coordinator when its own turn comes leaves the second one registered for the
// whole of the first one's rebuild — a full index — with the epoch already
// moved and its route still naming the old base.
func TestSetPrimaryDropsDependentCoordinatorsBeforeTheEpochMoves(t *testing.T) {
	f := newFamilyFixture(t, "coorddrop")
	defer f.close()
	ctx := context.Background()

	f.worktreeOf(f.main, "coorddrop-second")
	report, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, report.Coordinators,
		"the family needs a dedicated primary plus two automatic checkout coordinators")

	candidate := f.worktreeOf(f.main, "coorddrop-alt")
	runGit(t, candidate, "add", "-A")
	runGit(t, candidate, "commit", "-q", "-m", "alt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: candidate}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	preview, err := f.lc.PreviewSetPrimary(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, preview.Ready, "blockers: %v", preview.Blockers)
	dependents := make([]string, 0, len(preview.Dependents))
	for _, dependent := range preview.Dependents {
		dependents = append(dependents, dependent.ID)
		f.runCoordinator(dependent.ID)
	}
	require.Len(t, dependents, 2)

	// Watch the base and the registry together from before the move. A
	// dependent is inside the window when the base has moved and its route has
	// not; a coordinator the registry still holds there is one an ordinary
	// cycle can reach.
	var (
		wg        sync.WaitGroup
		stop      = make(chan struct{})
		mu        sync.Mutex
		inside    bool
		reachable []string
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			moved, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
			if err != nil || !found || !moved.IsPrimaryBase {
				runtime.Gosched()
				continue
			}
			for _, checkoutID := range dependents {
				route, routed, err := f.catalog.GetCheckoutRoute(ctx, checkoutID)
				if err != nil || (routed && route.GraphID == tracked.GraphID) {
					continue // the rebuild landed; this one's window is shut
				}
				mu.Lock()
				inside = true
				if liveCoordinatorOrNil(f.lc, checkoutID) != nil &&
					!slices.Contains(reachable, checkoutID) {
					reachable = append(reachable, checkoutID)
				}
				mu.Unlock()
			}
			runtime.Gosched()
		}
	}()

	result, err := f.lc.SetPrimary(ctx, tracked.GraphID)
	close(stop)
	wg.Wait()
	require.NoError(t, err)
	assert.Empty(t, result.Errors)
	assert.ElementsMatch(t, dependents, result.Rebuilt)

	mu.Lock()
	assert.True(t, inside, "the watcher never reached the window it is watching")
	assert.Empty(t, reachable, "a dependent's coordinator outlived the epoch move")
	mu.Unlock()

	for _, checkoutID := range dependents {
		assert.True(t, f.lc.hasCoordinator(checkoutID),
			"a rebuilt dependent gets a coordinator back")
		route, routed := f.routeOf(checkoutID)
		require.True(t, routed)
		assert.Equal(t, tracked.GraphID, route.GraphID)
		assert.NotZero(t, route.CommitGenerationID)
		assert.NotZero(t, route.DirtyGenerationID)
	}
}
