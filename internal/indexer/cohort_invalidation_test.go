package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// Who tells a cached dependency cohort that it has stopped being the current
// one.
//
// Both consumers — a checkout coordinator and a ref-view manager — CACHE the
// cohort description their generation identities are keyed on, because
// describing one costs a daemon-wide roster read lease plus a catalog read per
// in-scope member, and neither may pay that on an idle poll. The cache is what
// makes the poll affordable; the events pinned here are what keep it honest.
// Without them a certified revision is frozen for the life of the process (a
// freshness certificate that stops being true) and a transient refusal is
// permanent (a degraded identity that never recovers).
//
// These tests drive the PRODUCTION entry points — an explicit track, a registry
// teardown, a configuration reload — and assert the mark lands on the consumer.
// The primitive working in isolation is exactly the state this wave inherited:
// both InvalidateDependencyCohort methods existed with zero production callers,
// so a poll could never re-describe.

// cohortStale reads a coordinator's stale mark under the lock that guards it.
func cohortStale(c *CheckoutCoordinator) bool {
	c.revisionMu.RLock()
	defer c.revisionMu.RUnlock()
	return c.cohortStale
}

// cachedIdentityKeys counts a ref-view manager's memo entries.
func cachedIdentityKeys(m *RefViewManager) int {
	m.identityMu.Lock()
	defer m.identityMu.Unlock()
	return len(m.identityKeys)
}

// cohortWorkspaceFixture is a tracked repository whose automatic worktree has a
// live coordinator, plus the workspace both it and its siblings declare.
//
// The workspace is explicit because it is the boundary the fan-out is scoped
// by: a repository that declares none is its own workspace, and every
// invalidation would then be indistinguishable from a target-only one.
type cohortWorkspaceFixture struct {
	*lifecycleFixture
	workspace  string
	mainPath   string
	mainPrefix string
	familyID   string
	graphID    string
	automatic  store_sqlite.Checkout
}

const cohortTestWorkspace = "cohort-workspace"

func newCohortWorkspaceFixture(t *testing.T) *cohortWorkspaceFixture {
	t.Helper()
	f := newLifecycleFixture(t)
	ctx := context.Background()

	main := f.gitRepo("cohort-main")
	f.worktreeOf(main, "cohort-wt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{
		Path: main, Name: "cohort-main", Workspace: cohortTestWorkspace,
	}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	_, err = f.lc.Sweep(ctx)
	require.NoError(t, err)

	out := &cohortWorkspaceFixture{
		lifecycleFixture: f,
		workspace:        cohortTestWorkspace,
		mainPath:         main,
		mainPrefix:       tracked.Prefix,
		familyID:         tracked.FamilyID,
		graphID:          tracked.GraphID,
	}

	checkouts, err := f.catalog.ListCheckouts(ctx, tracked.FamilyID)
	require.NoError(t, err)
	for i := range checkouts {
		if checkouts[i].CheckoutID != tracked.CheckoutID {
			out.automatic = checkouts[i]
		}
	}
	require.NotEmpty(t, out.automatic.CheckoutID, "the linked worktree got no identity")
	f.activateAndWait(out.automatic.CheckoutID)
	return out
}

// sibling tracks a second repository into one workspace, through the same
// explicit-track entry point a user reaches.
func (f *cohortWorkspaceFixture) sibling(t *testing.T, name, workspace string) (path, prefix string) {
	t.Helper()
	path = f.gitRepo(name)
	tracked, err := f.lc.Register(context.Background(), config.RepoEntry{
		Path: path, Name: name, Workspace: workspace,
	}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	return path, tracked.Prefix
}

// settledCoordinator stops one checkout's build loop and leaves its cohort
// cache described and settled.
//
// The loop is stopped for the reason runCoordinator stops it: its poll is a
// wall-clock timer, and a cycle it fires on its own describes the cohort at a
// moment the test did not order — which is exactly the mark being asserted on.
// Describing once afterwards is what a build does, and it is what makes "the
// event marked it stale" a statement about the event.
func settledCoordinator(t *testing.T, l *CheckoutLifecycle, checkoutID string) *CheckoutCoordinator {
	t.Helper()
	coordinator := lifecycleCoordinator(t, l, checkoutID)
	require.NoError(t, coordinator.Close())
	coordinator.describeDependencyCohort(context.Background())
	require.False(t, cohortStale(coordinator),
		"the coordinator's cohort was stale before the test did anything to it")
	return coordinator
}

// TestTrackingARepositoryInvalidatesItsWorkspaceCohorts is the registration
// half of the wiring, driven through Register — the one path behind every
// explicit track, whichever surface asked.
//
// A repository joining the workspace changes which repositories are inputs at
// all, and registering its owner is the moment a member that refused every
// description (tracked, no registered owner) starts being describable. A
// consumer that keeps its pre-registration answer is keyed on a cohort that no
// longer exists.
func TestTrackingARepositoryInvalidatesItsWorkspaceCohorts(t *testing.T) {
	f := newCohortWorkspaceFixture(t)
	defer f.close()

	coordinator := settledCoordinator(t, f.lc, f.automatic.CheckoutID)
	require.Equal(t, f.workspace, coordinator.workspaceID,
		"the coordinator did not take the repository's declared workspace")

	// A ref-view manager over the same repository, with a memo to lose.
	idx := f.mi.GetIndexer(f.mainPrefix)
	require.NotNil(t, idx)
	manager, err := f.lc.refViewManager(f.mainPrefix, idx)
	require.NoError(t, err)
	manager.identityKeysFor(context.Background(), RefViewRequest{
		RepoPrefix:  f.mainPrefix,
		WorkspaceID: idx.WorkspaceID(),
		ProjectID:   idx.ProjectID(),
	})
	memoized := cachedIdentityKeys(manager)

	f.sibling(t, "cohort-sibling", f.workspace)

	require.True(t, cohortStale(coordinator),
		"a repository tracked into the workspace left the coordinator's cohort settled; "+
			"its next poll compares identities against a roster that has moved")
	if memoized > 0 {
		require.Zero(t, cachedIdentityKeys(manager),
			"a repository tracked into the workspace left the ref view's memoized cohort in place")
	}
}

// TestTrackingARepositoryOutsideTheWorkspaceInvalidatesNothing is the other
// half, and the reason the fan-out is not simply "mark everything".
//
// Re-describing a cohort costs a roster lease and a catalog read per in-scope
// member. A repository in an unrelated workspace can never be consulted by this
// target's resolution and is not one of its inputs, so charging every
// coordinator in the daemon for it is precisely the amplification the scoped
// cohort exists to remove.
func TestTrackingARepositoryOutsideTheWorkspaceInvalidatesNothing(t *testing.T) {
	f := newCohortWorkspaceFixture(t)
	defer f.close()

	coordinator := settledCoordinator(t, f.lc, f.automatic.CheckoutID)
	f.sibling(t, "cohort-stranger", "another-workspace")

	require.False(t, cohortStale(coordinator),
		"a repository tracked into an UNRELATED workspace made this coordinator re-describe "+
			"a cohort it cannot be an input to")
}

// TestTearingDownARepositoryInvalidatesItsWorkspaceCohorts is the close half.
//
// The registry entry going away removes a member the cohort named the bytes of,
// and a description taken while the admission was closing was a refusal. Both
// leave every in-scope consumer holding an answer that is no longer current —
// and the refusal is the worse of the two, because nothing would ever clear it.
func TestTearingDownARepositoryInvalidatesItsWorkspaceCohorts(t *testing.T) {
	f := newCohortWorkspaceFixture(t)
	defer f.close()
	ctx := context.Background()

	siblingPath, siblingPrefix := f.sibling(t, "cohort-doomed", f.workspace)
	require.NotEmpty(t, siblingPrefix)
	coordinator := settledCoordinator(t, f.lc, f.automatic.CheckoutID)

	out, err := f.lc.Untrack(ctx, siblingPath)
	require.NoError(t, err)
	require.False(t, out.Pending, "the untrack did not finish, so this test proves nothing")
	require.Nil(t, f.mi.GetMetadata(siblingPrefix),
		"the sibling is still served, so nothing was torn down")

	require.True(t, cohortStale(coordinator),
		"a repository torn out of the workspace left the coordinator's cohort settled")
}

// TestReloadingTheConfigurationInvalidatesEveryCohort is the configuration
// half.
//
// A refreshed repository configuration moves the sections the cohort digests —
// artifacts, semantic/LSP, workspace/project, source-selection — and the
// reload does not report WHICH repository's sections moved, so every live
// consumer re-describes once. That is bounded by the number of live consumers,
// and it happens only on an actual reload.
func TestReloadingTheConfigurationInvalidatesEveryCohort(t *testing.T) {
	f := newCohortWorkspaceFixture(t)
	defer f.close()

	coordinator := settledCoordinator(t, f.lc, f.automatic.CheckoutID)

	out, err := f.lc.ApplyReload(context.Background())
	require.NoError(t, err)
	require.Positive(t, out.Refreshed,
		"the reload refreshed no repository configuration, so this test proves nothing")
	require.True(t, cohortStale(coordinator),
		"a configuration reload left the coordinator keyed on the sections it digested before it")
}

// TestACheckoutWhoseConfigurationCannotBeFrozenReportsAHealthReason closes the
// gap the constructor's refusal opened.
//
// NewCheckoutCoordinator used to degrade on a configuration it could not freeze
// (a unique digest, so nothing was reused); it now refuses. Nothing upstream
// receives that error — every path that starts a coordinator is a background
// reconciliation — so without a stated reason the checkout simply has no view
// and no explanation, which is strictly worse than the degraded coordinator it
// replaced. buildCoordinator refuses for the same input rather than warning and
// handing the constructor a configuration it is about to reject anyway.
func TestACheckoutWhoseConfigurationCannotBeFrozenReportsAHealthReason(t *testing.T) {
	f := newFamilyFixture(t, "unfreezable")
	defer f.close()
	ctx := context.Background()

	checkout, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	f.lc.dropCoordinator(f.automatic.CheckoutID)

	f.lc.configSnapshot = func(
		config.IndexConfig, string, string, string,
	) (config.IndexConfig, string, error) {
		return config.IndexConfig{}, "", errors.New("encode dedicated base config: unencodable value")
	}
	f.lc.ensureCoordinator(ctx, f.primaryGraph, checkout)

	f.lc.coordMu.Lock()
	live := f.lc.coordinators[f.automatic.CheckoutID]
	f.lc.coordMu.Unlock()
	require.Nil(t, live,
		"a coordinator was installed over a configuration that could not be frozen; "+
			"its builder holds the ConfigManager's own nested values")

	failures := f.lc.CoordinatorStartFailures()
	require.Len(t, failures, 1,
		"the checkout lost its build loop and nothing says why")
	require.Equal(t, f.automatic.CheckoutID, failures[0].CheckoutID)
	require.Contains(t, failures[0].Reason, "freeze the index configuration",
		"the stated reason does not name what failed")

	// The reason is retracted once the checkout has a build loop again, or a
	// transient failure would be reported for the life of the daemon.
	f.lc.configSnapshot = nil
	f.lc.ensureCoordinator(ctx, f.primaryGraph, checkout)
	require.Empty(t, f.lc.CoordinatorStartFailures(),
		"a checkout that recovered its build loop still reports why it had none")
}

// TestTheRefViewServiceCarriesACertifiedCohort is the ref-view wiring.
//
// CheckoutLifecycle.refViewManager is the only constructor of a production ref
// view manager, and it passed neither Leases nor ConfigSections. Leases is the
// one that moves the output here: without it dependencyCohortSource.describe
// refuses before it reads anything, so EVERY ref-view generation carried the
// degraded revision. ConfigSections is defence in depth against the fallback
// link going missing, and what it buys is pinned on the output in
// TestAProductionRefViewDigestDoesNotDependOnTheBuilderLink rather than
// claimed here.
func TestTheRefViewServiceCarriesACertifiedCohort(t *testing.T) {
	f := newCohortWorkspaceFixture(t)
	defer f.close()
	ctx := context.Background()

	idx := f.mi.GetIndexer(f.mainPrefix)
	require.NotNil(t, idx)
	manager, err := f.lc.refViewManager(f.mainPrefix, idx)
	require.NoError(t, err)

	require.NotNil(t, manager.leases,
		"a production ref view manager has no roster to describe its cohort with; "+
			"every generation it keys carries the degraded revision")
	require.NotEmpty(t, manager.configSections,
		"a production ref view manager names no configuration sections; "+
			"its widened config digest is the narrow index-configuration one")
	require.Same(t, f.lc.leases, manager.leases,
		"the manager was handed a roster that is not the daemon's")

	req := RefViewRequest{
		RepoPrefix:  f.mainPrefix,
		WorkspaceID: idx.WorkspaceID(),
		ProjectID:   idx.ProjectID(),
	}
	keys := manager.identityKeysFor(ctx, req)
	require.True(t, strings.HasPrefix(keys.dependencyRevision, DependencyRevisionEncodingVersion+":"),
		"a production ref view's dependency revision is %q, not a certificate", keys.dependencyRevision)
}

// TestARefViewRefusalIsNotMemoized is the de-poisoning the ref-view half was
// missing.
//
// Unlike a coordinator, which re-describes the cohort on every build path, a
// manager derives its identity keys once and answers from the memo forever
// after. Caching a refusal therefore does not save a description — it replaces
// every future certificate with the degraded value one transient moment
// produced, for the life of the manager.
func TestARefViewRefusalIsNotMemoized(t *testing.T) {
	f := newRefViewFixture(t)
	ctx := context.Background()
	leases := graphview.NewLeaseManager()
	manager := f.managerTuned(t, nil, func(cfg *RefViewManagerConfig) {
		cfg.Leases = leases
		cfg.ConfigSections = dedicatedBaseConfigSections(config.Default())
	})
	req := f.request("refs/heads/main")

	// The transient: the repository is not in the registered roster yet, so no
	// description can name its bytes.
	refused := manager.identityKeysFor(ctx, req)
	require.True(t, strings.HasPrefix(refused.dependencyRevision, DependencyRevisionDegradedPrefix+":"),
		"a cohort with an empty roster produced %q rather than a refusal", refused.dependencyRevision)
	require.Zero(t, cachedIdentityKeys(manager),
		"a transient refusal was memoized; nothing in a ref view manager ever re-describes")

	// The transient clears.
	require.NoError(t, leases.RegisterRepositoryOwner(graphview.RepositoryOwner{
		GraphID:     f.graphID,
		CheckoutID:  f.checkoutID,
		Incarnation: "incarnation-refview",
		RepoPrefix:  builderRepoPrefix,
	}))
	certified := manager.identityKeysFor(ctx, req)
	require.NotEqual(t, refused.dependencyRevision, certified.dependencyRevision,
		"the cleared transient did not move the revision; the refusal is permanent")
	require.True(t, strings.HasPrefix(certified.dependencyRevision, DependencyRevisionEncodingVersion+":"),
		"the revision after the transient cleared is %q, not a certificate",
		certified.dependencyRevision)
	require.Equal(t, 1, cachedIdentityKeys(manager),
		"a certified description was not memoized, so every selection pays for one")
}

// TestARefViewMemoFollowsTheWorkspaceTopology is the self-observed half of the
// refresh.
//
// An event source can only tell the manager about what it performs. A
// repository tracked into the workspace by some other path changes which
// repositories are inputs at all, and the manager can see that for itself: the
// topology token is a map walk under the MultiIndexer's own read lock — no
// roster lease, no catalog read — so a selection that answers "already current"
// can re-check it every time.
func TestARefViewMemoFollowsTheWorkspaceTopology(t *testing.T) {
	f := newRefViewFixture(t)
	ctx := context.Background()
	leases := graphview.NewLeaseManager()
	require.NoError(t, leases.RegisterRepositoryOwner(graphview.RepositoryOwner{
		GraphID:     f.graphID,
		CheckoutID:  f.checkoutID,
		Incarnation: "incarnation-refview",
		RepoPrefix:  builderRepoPrefix,
	}))
	// A second registered repository, OUTSIDE the workspace to begin with: the
	// scope excludes it, so it is not an input and the first description
	// certifies without naming it.
	require.NoError(t, leases.RegisterRepositoryOwner(graphview.RepositoryOwner{
		GraphID:     GraphIDFor("late-member"),
		CheckoutID:  "checkout-late-member",
		Incarnation: "incarnation-late-member",
		RepoPrefix:  "late-member",
	}))

	// The production link: the builder's Admissions handle is a live Indexer
	// whose MultiIndexer answers "which repositories share this workspace".
	mi := &MultiIndexer{
		repos:    map[string]*RepoMetadata{builderRepoPrefix: {}},
		indexers: map[string]*Indexer{},
	}
	idx := &Indexer{repositoryMutationOwner: mi}
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(cohortTestWorkspace)
	mi.indexers[builderRepoPrefix] = idx

	builder := builderNewBuilder(f.store)
	builder.Admissions = idx
	manager, err := NewRefViewManager(RefViewManagerConfig{
		Store:          f.store,
		Builder:        builder,
		Config:         config.Default().Index,
		ConfigSections: dedicatedBaseConfigSections(config.Default()),
		Leases:         leases,
		Logger:         zap.NewNop(),
	})
	require.NoError(t, err)

	req := f.request("refs/heads/main")
	req.WorkspaceID = cohortTestWorkspace
	first := manager.identityKeysFor(ctx, req)
	require.True(t, strings.HasPrefix(first.dependencyRevision, DependencyRevisionEncodingVersion+":"),
		"the first description refused: %q", first.dependencyRevision)
	require.Equal(t, 1, cachedIdentityKeys(manager))

	// The registered repository joins the workspace, with nothing telling the
	// manager about it. It is an input now, and its corpus is not in the
	// catalog — so the only honest answers are a different certificate or a
	// refusal. Serving the old one is the freshness claim that stopped being
	// true.
	joined := &Indexer{repositoryMutationOwner: mi}
	joined.SetRepoPrefix("late-member")
	joined.SetWorkspaceID(cohortTestWorkspace)
	mi.mu.Lock()
	mi.repos["late-member"] = &RepoMetadata{}
	mi.indexers["late-member"] = joined
	mi.mu.Unlock()

	second := manager.identityKeysFor(ctx, req)
	require.NotEqual(t, first.dependencyRevision, second.dependencyRevision,
		"a repository joined the workspace and the ref view kept serving the revision "+
			"it certified before it")
}

// TestTheCohortScopeFallsBackToTheRepositoryWhenMembershipOmitsTheTarget pins a
// documented guard that nothing else does.
//
// dependencyCohortSource.scope says that a workspace answer which does not
// contain the target narrows the cohort to the target repository alone AND says
// so in the declared scope. Without the membership condition a coordinator
// mid-registration — one whose ReposInWorkspace answer does not yet carry its
// own prefix — would declare scope "workspace:<id>" while sourceIdentities and
// DependencyRevisionRosterScoped both exclude the target's OWN corpus: a
// certificate over a cohort that omits the repository it certifies, or a
// refusal when no sibling is in the map either.
func TestTheCohortScopeFallsBackToTheRepositoryWhenMembershipOmitsTheTarget(t *testing.T) {
	target := dependencyCohortSource{
		Target:           DependencyRevisionTarget{RepoPrefix: builderRepoPrefix, WorkspaceID: "shared"},
		WorkspaceMembers: workspaceOf(builderRepoPrefix, "sibling"),
	}
	label, member := target.scope()
	require.Equal(t, DependencyRevisionScopeWorkspace+"shared", label)
	require.True(t, member("sibling"), "a workspace-scoped cohort excluded a workspace member")

	// The same source whose workspace answer does not name the target.
	orphan := target
	orphan.WorkspaceMembers = workspaceOf("sibling", "another")
	label, member = orphan.scope()
	require.Equal(t, DependencyRevisionScopeRepository+builderRepoPrefix, label,
		"a cohort whose workspace answer omits its own target still declared the workspace scope; "+
			"its certificate would name every member EXCEPT the repository it certifies")
	require.True(t, member(builderRepoPrefix),
		"the fallback scope excluded the target's own repository")
	require.False(t, member("sibling"),
		"the fallback scope admitted a repository the declared scope does not name")

	// And the two claims are distinguishable, which is what makes the fallback
	// honest rather than silent.
	require.NotEqual(t, DependencyRevisionScopeWorkspace+"shared",
		DependencyRevisionScopeRepository+builderRepoPrefix)
}

// TestAnInvalidationDuringADescriptionIsNotSwallowed closes the race the cache
// opens.
//
// describeDependencyCohort used to clear the stale mark unconditionally, so an
// event that arrived while a description was in flight — naming inputs that
// description never read, including the very change that would let the next one
// succeed — was lost. Nothing retries afterwards: a poll re-describes only when
// the mark or the topology token has moved.
func TestAnInvalidationDuringADescriptionIsNotSwallowed(t *testing.T) {
	f := newCoordinatorFixture(t)
	cfg := cohortCoordinatorConfig()

	var c *CheckoutCoordinator
	raced := false
	cfg.WorkspaceMembers = func() map[string]bool {
		// Fires INSIDE the description, once, after the coordinator exists.
		if c != nil && !raced {
			raced = true
			c.InvalidateDependencyCohort("test: an event landed mid-description")
		}
		return map[string]bool{builderRepoPrefix: true}
	}
	c = f.inertCoordinator(t, cfg)

	require.True(t, c.describeDependencyCohort(context.Background()),
		"the fixture's cohort could not be described at all")
	require.True(t, raced, "the mid-description invalidation never fired")
	require.True(t, cohortStale(c),
		"an invalidation that landed while the cohort was being described was swallowed; "+
			"no poll will ever retry it")

	// The rule it must not break: a description nobody invalidated still
	// settles, or a degraded poll would pay a roster lease every fifteen
	// seconds for the whole of a transient.
	require.True(t, c.describeDependencyCohort(context.Background()))
	require.False(t, cohortStale(c),
		"an uncontested description left the cohort stale; every poll now re-describes")
}

// TestACohortDeferredCycleIsCountedDeferred states what the cycle counter means.
//
// A cycle held back for want of a describable cohort read nothing, wrote
// nothing and still owes its demand — the same outcome as one held back by
// warmup or by a saturated admission queue, both of which count themselves.
// Falling through to "skipped" labels it as a cycle that found nothing to do,
// which is the opposite fact.
func TestACohortDeferredCycleIsCountedDeferred(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	cfg := cohortCoordinatorConfig()
	cfg.WorkspaceMembers = workspaceOf(builderRepoPrefix, "never-indexed")
	c := f.inertCoordinator(t, cfg)

	// The first cycle certifies; the transient then arrives.
	coordinatorReconcile(t, c)
	registerCohortSibling(t, f, "never-indexed", "")
	worktreeWrite(t, f.worktree, "during-transient.go", "package fixture\n\nfunc T() {}\n")

	before := viewmetrics.Read()
	c.cycle(context.Background())
	after := viewmetrics.Read()

	if got := counterDelta(before, after, cycleKey(viewmetrics.OutcomeDeferred)); got != 1 {
		t.Fatalf("a cohort-deferred cycle counted deferred = %d, want 1", got)
	}
	if got := counterDelta(before, after, cycleKey(viewmetrics.OutcomeSkipped)); got != 0 {
		t.Fatalf("a cohort-deferred cycle also counted as skipped (%d); "+
			"skipped is the label for a cycle that found nothing to do", got)
	}
	if got := counterDelta(before, after, cycleKey(viewmetrics.OutcomeBuiltCommit)); got != 0 {
		t.Fatalf("a deferred cycle built a commit layer (%d)", got)
	}
}

// TestReRegisteringARepositoryOwnerInvalidatesItsWorkspaceCohorts pins the
// event source the first round left unexercised: the binding-reuse path.
//
// bindDedicatedGraph has two outcomes, and only one of them creates a graph
// row. The other finds the row this checkout already owns and registers its
// owner against the live roster — which on a process that restarted over an
// existing catalog is the FIRST registration of that repository in this
// process, the transition that turns a member every description refused into a
// describable one. The invalidation lives inside reuseOwner rather than at its
// call sites because the UpsertDedicatedGraph error and retry arms take the
// same closure and returned without it.
func TestReRegisteringARepositoryOwnerInvalidatesItsWorkspaceCohorts(t *testing.T) {
	f := newCohortWorkspaceFixture(t)
	defer f.close()
	ctx := context.Background()

	coordinator := settledCoordinator(t, f.lc, f.automatic.CheckoutID)

	// A memo entry for the target requested with NO workspace: the entry an
	// event keyed on a workspace alone can never name, and whose topology token
	// is the constant repository-scope string, so nothing self-observed moves
	// it either.
	idx := f.mi.GetIndexer(f.mainPrefix)
	require.NotNil(t, idx)
	manager, err := f.lc.refViewManager(f.mainPrefix, idx)
	require.NoError(t, err)
	manager.identityKeysFor(ctx, RefViewRequest{RepoPrefix: f.mainPrefix})
	require.Equal(t, 1, cachedIdentityKeys(manager),
		"the workspaceless target was not memoized, so this test proves nothing")

	// The production entry point: tracking the SAME repository again finds the
	// binding it already owns.
	tracked, err := f.lc.Register(ctx, config.RepoEntry{
		Path: f.mainPath, Name: "cohort-main", Workspace: f.workspace,
	}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	require.Equal(t, f.graphID, tracked.GraphID,
		"the second track minted a new graph, so it did not take the reuse path")

	require.True(t, cohortStale(coordinator),
		"a repository owner re-registered through the binding-reuse path left the "+
			"coordinator's cohort settled")
	require.Zero(t, cachedIdentityKeys(manager),
		"an event about this repository left a memo entry that names no workspace in place; "+
			"no workspace-keyed event can ever drop it and its topology token cannot move")
}

// TestEvictingARepositoryWithNoCheckoutIdentityInvalidatesItsWorkspaceCohorts
// pins the second teardown path.
//
// A repository with a dedicated graph leaves through the forget saga's
// ReleaseGraph hook. One without a catalog identity — a directory git does not
// administer — has no saga at all: applyUntrack evicts it directly, and
// evictRepoChecked is the only place that teardown passes. Both remove a member
// whose bytes every in-scope cohort named.
func TestEvictingARepositoryWithNoCheckoutIdentityInvalidatesItsWorkspaceCohorts(t *testing.T) {
	f := newCohortWorkspaceFixture(t)
	defer f.close()
	ctx := context.Background()

	plain := filepath.Join(f.dir, "cohort-plain")
	require.NoError(t, os.MkdirAll(plain, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(plain, "plain.go"), []byte("package plain\n"), 0o644))
	tracked, err := f.lc.Register(ctx, config.RepoEntry{
		Path: plain, Name: "cohort-plain", Workspace: f.workspace,
	}, TrackSourceCLI)
	require.NoError(t, err)
	require.NotEmpty(t, tracked.Prefix)
	require.NotNil(t, f.mi.GetMetadata(tracked.Prefix), "the plain directory is not served")

	preview, err := f.lc.PreviewUntrack(ctx, plain)
	require.NoError(t, err)
	require.Equal(t, UntrackPlanEvict, preview.Plan,
		"the plain directory has a catalog identity, so this test does not reach evictRepoChecked")

	coordinator := settledCoordinator(t, f.lc, f.automatic.CheckoutID)
	out, err := f.lc.Untrack(ctx, plain)
	require.NoError(t, err)
	require.False(t, out.Pending)
	require.Nil(t, f.mi.GetMetadata(tracked.Prefix), "the plain directory is still served")

	require.True(t, cohortStale(coordinator),
		"a repository evicted without a catalog identity left the coordinator's cohort settled; "+
			"its next poll compares identities against a roster that has lost a member")
}

// TestASweepStatesWhyACheckoutHasNoBuildLoop carries the stated reason onto a
// production return value.
//
// The janitor pass is what tries to start the loops, so it is the pass that
// knows which checkouts have none and why. A reason held only in a map with no
// caller is the same defect this item exists to close on the cohort side: the
// primitive works and nothing reaches it.
func TestASweepStatesWhyACheckoutHasNoBuildLoop(t *testing.T) {
	f := newFamilyFixture(t, "unfreezable-sweep")
	defer f.close()
	ctx := context.Background()

	checkout, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	f.lc.dropCoordinator(f.automatic.CheckoutID)
	f.lc.configSnapshot = func(
		config.IndexConfig, string, string, string,
	) (config.IndexConfig, string, error) {
		return config.IndexConfig{}, "", errors.New("encode dedicated base config: unencodable value")
	}
	// The same call the janitor pass makes for every ready automatic checkout
	// it finds without a loop (applyCoordinators), and the selection path makes
	// for a dormant one.
	f.lc.ensureCoordinator(ctx, f.primaryGraph, checkout)

	report, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.Len(t, report.CoordinatorStartFailures, 1,
		"a checkout has no build loop and the janitor's report says nothing about why")
	require.Equal(t, f.automatic.CheckoutID, report.CoordinatorStartFailures[0].CheckoutID)
	require.Contains(t, report.CoordinatorStartFailures[0].Reason, "freeze the index configuration")
	require.Equal(t, checkout.RootPath, report.CoordinatorStartFailures[0].RootPath,
		"the stated reason does not name the working copy that has no view")

	// And a pass that starts the loop again reports nothing, so the reason is
	// about the daemon's present rather than about its history.
	f.lc.configSnapshot = nil
	f.lc.ensureCoordinator(ctx, f.primaryGraph, checkout)
	report, err = f.lc.Sweep(ctx)
	require.NoError(t, err)
	require.Empty(t, report.CoordinatorStartFailures,
		"a checkout that recovered its build loop is still reported as having none")
}

// TestAForgottenCheckoutStopsStatingWhyItHasNoBuildLoop bounds the ledger.
//
// A reason is a fact about a checkout that is being served and has no loop. A
// checkout that has been forgotten is not being served, so keeping its reason
// would report a working copy that no longer exists for the life of the
// process — and the entry could never be retracted, because only a successful
// install retracts one and nothing will ever install a loop for it again.
func TestAForgottenCheckoutStopsStatingWhyItHasNoBuildLoop(t *testing.T) {
	f := newFamilyFixture(t, "unfreezable-forgotten")
	defer f.close()
	ctx := context.Background()

	f.lc.dropCoordinator(f.automatic.CheckoutID)
	f.lc.configSnapshot = func(
		config.IndexConfig, string, string, string,
	) (config.IndexConfig, string, error) {
		return config.IndexConfig{}, "", errors.New("encode dedicated base config: unencodable value")
	}
	checkout, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	f.lc.ensureCoordinator(ctx, f.primaryGraph, checkout)
	require.Len(t, f.lc.CoordinatorStartFailures(), 1,
		"the checkout lost its build loop and nothing says why")

	out, err := f.lc.Untrack(ctx, f.main)
	require.NoError(t, err)
	require.False(t, out.Pending, "the untrack did not finish, so this test proves nothing")

	require.Empty(t, f.lc.CoordinatorStartFailures(),
		"a checkout that was untracked still states why it has no build loop")
}

// TestARefViewHoldsOneDegradedIdentityWhileTheRefusalLasts is the other half of
// not memoizing a refusal.
//
// Re-describing on every selection is what lets a cleared transient be noticed.
// What must NOT follow from it is a moving answer: a degraded revision carries
// the refusal's reason, so a transient whose class flaps — no registered owner
// on one selection, admissions stopping on the next — would hand out a
// different identity per selection. A ref view's identity moving is a BUILD, so
// that turns a read-only refusal into write amplification, which is the exact
// failure the memo used to hide.
func TestARefViewHoldsOneDegradedIdentityWhileTheRefusalLasts(t *testing.T) {
	f := newRefViewFixture(t)
	ctx := context.Background()
	leases := graphview.NewLeaseManager()
	build := func() *RefViewManager {
		return f.managerTuned(t, nil, func(cfg *RefViewManagerConfig) {
			cfg.Leases = leases
			cfg.ConfigSections = dedicatedBaseConfigSections(config.Default())
		})
	}
	manager := build()
	req := f.request("refs/heads/main")

	// Transient one: no registered owner, so nothing can name this
	// repository's bytes.
	first := manager.identityKeysFor(ctx, req)
	require.True(t, strings.HasPrefix(first.dependencyRevision, DependencyRevisionDegradedPrefix+":"),
		"the first description certified (%q), so there is no refusal to hold",
		first.dependencyRevision)

	// Transient two, a different class: the daemon stops admitting
	// repositories at all.
	<-leases.ShutdownRepositoryAdmissions()

	second := manager.identityKeysFor(ctx, req)
	require.Equal(t, first.dependencyRevision, second.dependencyRevision,
		"the refusal class changed and the ref view's dependency revision moved with it; "+
			"the next selection re-keys the view's fingerprint and starts a build")
	require.Zero(t, cachedIdentityKeys(manager),
		"a refusal was memoized; a cleared transient would never be noticed")

	// The hold is what did that, not the two refusals being identical: a
	// manager that never saw the first transient names the second one.
	fresh := build().identityKeysFor(ctx, req)
	require.True(t, strings.HasPrefix(fresh.dependencyRevision, DependencyRevisionDegradedPrefix+":"))
	require.NotEqual(t, first.dependencyRevision, fresh.dependencyRevision,
		"the two transients produce the same degraded revision, so this test cannot "+
			"tell a held identity from a recomputed one")
}

// TestAProductionRefViewDigestDoesNotDependOnTheBuilderLink is the output-level
// half of passing ConfigSections explicitly.
//
// identityKeysFor falls back to builderConfigSections, which walks the
// builder's Admissions handle to a live Indexer's MultiIndexer and asks ITS
// ConfigManager. On a daemon wired end to end that walk answers with the same
// sections the lifecycle holds, so the explicit pass moves nothing — which is
// exactly why it has to be pinned on the case where the walk answers with
// NOTHING. An empty section list collapses the widened configuration digest
// back onto config.IndexConfig alone, and a stored generation then stays
// reusable across an artifacts / semantic / LSP / workspace / project change.
func TestAProductionRefViewDigestDoesNotDependOnTheBuilderLink(t *testing.T) {
	f := newCohortWorkspaceFixture(t)
	defer f.close()
	ctx := context.Background()

	// The production constructor, handed an Indexer with no mutation owner —
	// the shape whose builder link derives no sections at all.
	const detached = "cohort-detached"
	manager, err := f.lc.refViewManager(detached, &Indexer{})
	require.NoError(t, err)
	require.Nil(t, builderConfigSections(manager.builder, detached),
		"the builder link still answers, so this test does not reach the collapse")

	req := RefViewRequest{RepoPrefix: detached, WorkspaceID: f.workspace, ProjectID: detached}
	keys := manager.identityKeysFor(ctx, req)

	_, fingerprint, err := snapshotDedicatedBaseConfig(
		manager.config, req.RepoPrefix, req.WorkspaceID, req.ProjectID)
	require.NoError(t, err)
	sections := dedicatedBaseConfigSections(f.cm.GetRepoConfig(detached))
	require.NotEmpty(t, sections)
	require.Equal(t, checkoutConfigHash(fingerprint, sections), keys.configHash,
		"a production ref view's widened configuration digest is not the one the "+
			"lifecycle's own configuration renders")
	require.NotEqual(t, checkoutConfigHash(fingerprint, nil), keys.configHash,
		"a production ref view's configuration digest collapsed onto config.IndexConfig "+
			"alone; an artifacts / semantic / LSP / workspace change re-keys nothing")
}

// TestAReleaseWhoseSubjectCannotBeReadInvalidatesEveryCohort closes the last
// silent path out of the teardown event source.
//
// The forget saga's ReleaseGraph hook reads the released graph's row BEFORE the
// teardown to learn which repository — and which workspace — just lost a
// member. That read is a catalog call, and it used to fold a FAILURE into the
// same answer as "this graph names no served repository": the invalidation was
// skipped and nothing was logged, so every in-scope consumer kept a cohort
// certifying a repository that had just been released, with no trace anywhere.
//
// A failure is not that answer. The subject is unknown, not absent, so the
// consumers that moved cannot be named — and of the two available mistakes,
// making every live consumer describe once more is bounded and self-clearing
// while a frozen false certificate is neither.
func TestAReleaseWhoseSubjectCannotBeReadInvalidatesEveryCohort(t *testing.T) {
	f := newCohortWorkspaceFixture(t)
	defer f.close()
	ctx := context.Background()

	siblingPath, siblingPrefix := f.sibling(t, "cohort-unreadable", f.workspace)
	require.NotEmpty(t, siblingPrefix)
	coordinator := settledCoordinator(t, f.lc, f.automatic.CheckoutID)

	// The transient: the catalog cannot answer which repository the graph
	// being released holds. A real store does not fail a primary-key read, so
	// the branch has no other way to be reached.
	logs, restore := observedLifecycleLogs(f.lc)
	defer restore()
	f.lc.cohortGraphSubject = func(
		context.Context, string,
	) (store_sqlite.DedicatedGraph, bool, error) {
		return store_sqlite.DedicatedGraph{}, false, errors.New("catalog: database is locked")
	}

	out, err := f.lc.Untrack(ctx, siblingPath)
	require.NoError(t, err)
	require.False(t, out.Pending, "the untrack did not finish, so this test proves nothing")
	require.Nil(t, f.mi.GetMetadata(siblingPrefix),
		"the sibling is still served, so nothing was torn down")

	require.True(t, cohortStale(coordinator),
		"the released graph's subject could not be read and the teardown marked nothing; "+
			"every in-scope consumer still certifies a repository that has just been released")
	require.NotEmpty(t,
		logs.FilterMessageSnippet("could not read which repository a released graph held").All(),
		"a catalog failure on the teardown's subject read was swallowed silently")
}

// observedLifecycleLogs redirects one lifecycle's logger into an observer and
// returns what it recorded, plus the undo.
//
// Installed before the call under test and read after it, so the only writer is
// the goroutine doing the installing — the coordinators took their logger by
// value when they were built, and anything the call starts is started after the
// swap.
func observedLifecycleLogs(l *CheckoutLifecycle) (*observer.ObservedLogs, func()) {
	core, logs := observer.New(zapcore.WarnLevel)
	previous := l.logger
	l.logger = zap.New(core)
	return logs, func() { l.logger = previous }
}

// TestARefViewDropsAHeldRefusalWhenTheWorkspaceMembershipMoves pins the gate on
// the held degraded identity.
//
// A refusal is re-described on every selection, and the identity it yields is
// held steady so a flapping refusal CLASS does not re-key the view and start a
// build. What must not be held steady is an identity the inputs have moved out
// from under: a degraded revision carries the cohort's declared SCOPE, and the
// scope is a function of workspace membership — a target that leaves the
// workspace is repository-scoped, and serving the workspace-scoped identity it
// was refused under names a cohort it no longer has. The hold is therefore
// gated on the same cheap topology observation the memo is.
func TestARefViewDropsAHeldRefusalWhenTheWorkspaceMembershipMoves(t *testing.T) {
	f := newRefViewFixture(t)
	ctx := context.Background()
	// An empty roster: every description in this test refuses, so the identity
	// under test is a held one throughout.
	leases := graphview.NewLeaseManager()

	// The production link: the builder's Admissions handle is a live Indexer
	// whose MultiIndexer answers "which repositories share this workspace".
	mi := &MultiIndexer{
		repos: map[string]*RepoMetadata{
			builderRepoPrefix: {},
			"cohort-member":   {},
		},
		indexers: map[string]*Indexer{},
	}
	for _, prefix := range []string{builderRepoPrefix, "cohort-member"} {
		idx := &Indexer{repositoryMutationOwner: mi}
		idx.SetRepoPrefix(prefix)
		idx.SetWorkspaceID(cohortTestWorkspace)
		mi.indexers[prefix] = idx
	}
	builder := builderNewBuilder(f.store)
	builder.Admissions = mi.indexers[builderRepoPrefix]

	build := func() *RefViewManager {
		manager, err := NewRefViewManager(RefViewManagerConfig{
			Store:          f.store,
			Builder:        builder,
			Config:         config.Default().Index,
			ConfigSections: dedicatedBaseConfigSections(config.Default()),
			Leases:         leases,
			Logger:         zap.NewNop(),
		})
		require.NoError(t, err)
		return manager
	}
	manager := build()
	req := f.request("refs/heads/main")
	req.WorkspaceID = cohortTestWorkspace

	first := manager.identityKeysFor(ctx, req)
	require.True(t, strings.HasPrefix(first.dependencyRevision, DependencyRevisionDegradedPrefix+":"),
		"the first description certified (%q), so there is no refusal to hold",
		first.dependencyRevision)
	require.Equal(t, first.dependencyRevision,
		manager.identityKeysFor(ctx, req).dependencyRevision,
		"the identity moved while nothing about the cohort did; the hold is not working "+
			"and this test cannot tell a held identity from a stale one")

	// The membership moves: the target itself leaves the workspace, so its
	// cohort narrows from every member's bytes to its own repository — a
	// different declared scope, and therefore a different honest answer.
	mi.indexers[builderRepoPrefix].SetWorkspaceID("cohort-elsewhere")

	moved := manager.identityKeysFor(ctx, req)
	require.NotEqual(t, first.dependencyRevision, moved.dependencyRevision,
		"the workspace membership moved and the ref view kept serving the degraded identity "+
			"it was refused under before it; that identity declares a scope this cohort no "+
			"longer has")
	require.Equal(t, build().identityKeysFor(ctx, req).dependencyRevision, moved.dependencyRevision,
		"the identity served after the membership moved is not the one a manager that held "+
			"nothing derives, so it is neither the held answer nor the honest one")
	require.Zero(t, cachedIdentityKeys(manager),
		"a refusal was memoized; a cleared transient would never be noticed")
}

// TestAConfigurationChangeReKeysAProductionRefViewDigest is what makes passing
// the configuration sections load-bearing rather than decorative.
//
// A ref-view manager is cached per repository for the life of the daemon, and a
// configuration reload swaps that repository's whole config.Config underneath
// it. A section list frozen when the manager was built therefore keeps keying
// generations on the artifacts / semantic / LSP / workspace / project domains
// as they stood at some arbitrary earlier selection, and a view built after the
// reload reuses a generation produced under rules that no longer apply. The
// lifecycle passes the sections as a SOURCE, so a description taken after the
// reload reads the configuration the reload installed.
func TestAConfigurationChangeReKeysAProductionRefViewDigest(t *testing.T) {
	f := newCohortWorkspaceFixture(t)
	defer f.close()
	ctx := context.Background()

	idx := f.mi.GetIndexer(f.mainPrefix)
	require.NotNil(t, idx)
	manager, err := f.lc.refViewManager(f.mainPrefix, idx)
	require.NoError(t, err)
	req := RefViewRequest{
		RepoPrefix:  f.mainPrefix,
		WorkspaceID: idx.WorkspaceID(),
		ProjectID:   idx.ProjectID(),
	}
	before := manager.identityKeysFor(ctx, req)
	require.NotEmpty(t, before.configHash)

	// One of the six domains the widened digest covers, changed the way a user
	// changes it: a repository-level declaration that lives OUTSIDE
	// config.IndexConfig, so the narrow index-configuration digest — which the
	// manager freezes deliberately, because its builder was handed it — does
	// not move with it.
	require.NoError(t, os.WriteFile(filepath.Join(f.mainPath, ".gortex.yaml"),
		[]byte("project: cohort-reloaded\n"), 0o644))
	out, err := f.lc.ApplyReload(ctx)
	require.NoError(t, err)
	require.Positive(t, out.Refreshed,
		"the reload refreshed no repository configuration, so this test proves nothing")
	require.NotNil(t, f.mi.GetMetadata(f.mainPrefix),
		"the reload retired the repository under test, so this test proves nothing")

	after := manager.identityKeysFor(ctx, req)
	require.NotEqual(t, before.configHash, after.configHash,
		"a configuration reload left a cached ref-view manager keying its generations on "+
			"the configuration domains it was constructed with; every view it serves after "+
			"the change reuses payload built under the rules before it")

	_, fingerprint, err := snapshotDedicatedBaseConfig(
		manager.config, req.RepoPrefix, req.WorkspaceID, req.ProjectID)
	require.NoError(t, err)
	require.Equal(t,
		checkoutConfigHash(fingerprint, dedicatedBaseConfigSections(f.cm.GetRepoConfig(f.mainPrefix))),
		after.configHash,
		"the digest after the reload is not the one the lifecycle's own configuration renders")
}
