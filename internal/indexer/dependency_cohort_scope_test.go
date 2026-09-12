package indexer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
)

// The cohort's SCOPE, its COST and its REFUSAL semantics.
//
// The revision itself (what it digests, that it reaches the stored row) is
// pinned by checkout_identity_revision_test.go. What is pinned here is the
// three things that decide whether the revision is affordable and safe:
//
//  1. scope — a repository the target's resolution can never consult is not an
//     input. Digesting every tracked repository's tree made a commit in ANY of
//     them re-key and rebuild EVERY checkout's layers, which is the write
//     amplification this branch exists to remove.
//  2. cost — describing a cohort takes a daemon-wide roster read lease and one
//     catalog read per in-scope member. An idle poll must describe nothing.
//  3. refusal — a transient (a sibling tracked but not yet indexed) must not
//     stamp stored layers with a revision nothing can ever match again.

// cohortSibling registers a second repository: a roster owner, a checkout row
// naming a committed tree, and the dedicated graph that binds them. It is what
// the reconciler records for a tracked repository that has no published
// generation yet, which is the cheapest real roster member a test can make.
type cohortSibling struct {
	prefix     string
	graphID    string
	checkoutID string
}

func registerCohortSibling(t *testing.T, f *coordinatorFixture, prefix, headTree string) cohortSibling {
	t.Helper()
	sibling := cohortSibling{
		prefix:     prefix,
		graphID:    GraphIDFor(prefix),
		checkoutID: "checkout-" + prefix,
	}
	err := f.leases.RegisterRepositoryOwner(graphview.RepositoryOwner{
		GraphID:     sibling.graphID,
		CheckoutID:  sibling.checkoutID,
		Incarnation: "incarnation-" + prefix,
		RepoPrefix:  prefix,
	})
	if err != nil {
		t.Fatalf("register the %s repository owner: %v", prefix, err)
	}
	if headTree != "" {
		bindCohortSiblingCorpus(t, f, sibling, headTree)
	}
	return sibling
}

// bindCohortSiblingCorpus gives a registered sibling the catalog rows that name
// its committed corpus. Until they exist the repository is tracked but not
// indexed, which is one of the transients the refusal path has to survive.
func bindCohortSiblingCorpus(t *testing.T, f *coordinatorFixture, sibling cohortSibling, headTree string) {
	t.Helper()
	ctx := context.Background()
	err := f.catalog.AllocateCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    sibling.checkoutID,
		Incarnation:   "incarnation-" + sibling.prefix,
		FamilyID:      f.familyID,
		RootPath:      f.primary + "-" + sibling.prefix,
		GitDir:        f.primary + "-" + sibling.prefix + "/.git",
		AdminName:     "@" + sibling.prefix,
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		HeadRef:       "refs/heads/main",
		HeadTree:      headTree,
	})
	if err != nil {
		t.Fatalf("allocate the %s checkout: %v", sibling.prefix, err)
	}
	err = f.catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:         sibling.graphID,
		OwnerCheckoutID: sibling.checkoutID,
		RepoPrefix:      sibling.prefix,
		FamilyID:        f.familyID,
		State:           reconcile.GraphStateReady,
	})
	if err != nil {
		t.Fatalf("bind the %s dedicated graph: %v", sibling.prefix, err)
	}
}

// moveCohortSiblingTree re-points a sibling's committed tree, which is what a
// commit in that repository does to the cohort's view of it.
func moveCohortSiblingTree(t *testing.T, f *coordinatorFixture, sibling cohortSibling, headTree string) {
	t.Helper()
	moveCheckoutTree(t, f, sibling.checkoutID, sibling.prefix, headTree)
}

// moveTargetCorpus re-points the TARGET repository's own committed corpus.
//
// graphBase resolves a dedicated member's corpus from its published generation,
// or from its owning checkout's head tree when it has none, so moving the
// primary checkout's head tree is exactly what a commit in the target does to
// the cohort's view of ITSELF — the input that used to re-key the target on
// every one of its own commits.
func moveTargetCorpus(t *testing.T, f *coordinatorFixture, headTree string) {
	t.Helper()
	moveCheckoutTree(t, f, f.primaryID, builderRepoPrefix, headTree)
}

func moveCheckoutTree(t *testing.T, f *coordinatorFixture, checkoutID, label, headTree string) {
	t.Helper()
	ctx := context.Background()
	row, found, err := f.catalog.GetCheckout(ctx, checkoutID)
	if err != nil || !found {
		t.Fatalf("read the %s checkout: %v (found=%v)", label, err, found)
	}
	err = f.catalog.UpdateCheckoutObservation(ctx, store_sqlite.UpdateCheckoutObservationRequest{
		CheckoutID:  row.CheckoutID,
		Incarnation: row.Incarnation,
		State:       row.State,
		RootPath:    row.RootPath,
		GitDir:      row.GitDir,
		HeadRef:     row.HeadRef,
		HeadCommit:  row.HeadCommit,
		HeadTree:    headTree,
	})
	if err != nil {
		t.Fatalf("move the %s checkout's tree: %v", label, err)
	}
}

// workspaceOf is the WorkspaceMembers seam in its production shape: a set of
// repo prefixes, answered fresh on every call.
func workspaceOf(prefixes ...string) func() map[string]bool {
	return func() map[string]bool {
		out := make(map[string]bool, len(prefixes))
		for _, prefix := range prefixes {
			out[prefix] = true
		}
		return out
	}
}

// TestCheckoutCohortIsScopedToTheWorkspace is the amplification fix.
//
// Before it, rosterSourceIdentities named EVERY registered repository's
// committed tree and DependencyRevisionRoster refused a cohort that omitted
// one, so a commit in any tracked repository re-keyed the commit and
// working-tree layers of every checkout in the daemon — on a branch whose whole
// purpose is bounding incremental write amplification.
func TestCheckoutCohortIsScopedToTheWorkspace(t *testing.T) {
	t.Run("a repository outside the workspace is not an input", func(t *testing.T) {
		f := newCoordinatorFixture(t)
		cfg := cohortCoordinatorConfig()
		cfg.WorkspaceMembers = workspaceOf(builderRepoPrefix)
		c := f.inertCoordinator(t, cfg)

		baseline := c.dependencyRevision()
		if !strings.HasPrefix(baseline, DependencyRevisionEncodingVersion+":") {
			t.Fatalf("the production cohort was refused: %q", baseline)
		}

		// A repository in another workspace joins the daemon, is indexed, and
		// then commits. None of that is an input to this checkout's payload.
		outside := registerCohortSibling(t, f, "other-workspace-repo", "tree-outside-1")
		if !c.describeDependencyCohort(context.Background()) {
			t.Fatal("registering a repository outside the workspace made the cohort undescribable")
		}
		if got := c.dependencyRevision(); got != baseline {
			t.Fatalf("a repository outside the workspace joined and re-keyed this checkout:\n"+
				" before %q\n after  %q", baseline, got)
		}

		moveCohortSiblingTree(t, f, outside, "tree-outside-2")
		if !c.describeDependencyCohort(context.Background()) {
			t.Fatal("a commit outside the workspace made the cohort undescribable")
		}
		if got := c.dependencyRevision(); got != baseline {
			t.Fatalf("a commit in a repository outside the workspace rebuilt this checkout:\n"+
				" before %q\n after  %q", baseline, got)
		}
	})

	t.Run("a repository inside the workspace is an input", func(t *testing.T) {
		f := newCoordinatorFixture(t)
		cfg := cohortCoordinatorConfig()
		cfg.WorkspaceMembers = workspaceOf(builderRepoPrefix, "workspace-sibling")
		c := f.inertCoordinator(t, cfg)

		baseline := c.dependencyRevision()
		sibling := registerCohortSibling(t, f, "workspace-sibling", "tree-sibling-1")
		if !c.describeDependencyCohort(context.Background()) {
			t.Fatal("a workspace sibling made the cohort undescribable")
		}
		joined := c.dependencyRevision()
		if joined == baseline {
			t.Fatal("a repository joined the workspace roster and the cohort did not move")
		}

		moveCohortSiblingTree(t, f, sibling, "tree-sibling-2")
		if !c.describeDependencyCohort(context.Background()) {
			t.Fatal("a workspace sibling's commit made the cohort undescribable")
		}
		if got := c.dependencyRevision(); got == joined {
			t.Fatal("a workspace sibling committed and the cohort did not move")
		}
	})

	t.Run("the scope it used is part of the claim", func(t *testing.T) {
		f := newCoordinatorFixture(t)
		cfg := cohortCoordinatorConfig()
		cfg.WorkspaceMembers = workspaceOf(builderRepoPrefix)
		c := f.inertCoordinator(t, cfg)

		lease, err := f.leases.AcquireRepositoryRoster()
		if err != nil {
			t.Fatalf("acquire the roster: %v", err)
		}
		defer lease.Release()
		inputs, err := c.cohort.inputs(context.Background(), lease)
		if err != nil {
			t.Fatalf("assemble the production cohort: %v", err)
		}
		if want := DependencyRevisionScopeWorkspace + builderRepoPrefix; inputs.RosterScope != want {
			t.Fatalf("the cohort declared scope %q, want %q", inputs.RosterScope, want)
		}
		scoped, err := ComputeDependencyRevision(inputs)
		if err != nil {
			t.Fatalf("digest the scoped cohort: %v", err)
		}
		// The same members under a different claim are a different certificate:
		// without the scope in the digest a reader cannot tell a complete
		// daemon roster from a workspace that happened to match it.
		unscoped := inputs
		unscoped.RosterScope = ""
		whole, err := ComputeDependencyRevision(unscoped)
		if err != nil {
			t.Fatalf("digest the unscoped cohort: %v", err)
		}
		if scoped == whole {
			t.Fatal("the declared roster scope is not part of the revision")
		}
	})

	// With no workspace topology to read, the cohort narrows to the target
	// repository alone and says so, rather than silently digesting every
	// repository in the daemon.
	t.Run("no topology narrows to the target repository", func(t *testing.T) {
		f := newCoordinatorFixture(t)
		c := f.inertCoordinator(t, cohortCoordinatorConfig())
		baseline := c.dependencyRevision()

		lease, err := f.leases.AcquireRepositoryRoster()
		if err != nil {
			t.Fatalf("acquire the roster: %v", err)
		}
		inputs, err := c.cohort.inputs(context.Background(), lease)
		lease.Release()
		if err != nil {
			t.Fatalf("assemble the production cohort: %v", err)
		}
		if want := DependencyRevisionScopeRepository + builderRepoPrefix; inputs.RosterScope != want {
			t.Fatalf("the cohort declared scope %q, want %q", inputs.RosterScope, want)
		}

		registerCohortSibling(t, f, "unrelated-repo", "tree-unrelated")
		if !c.describeDependencyCohort(context.Background()) {
			t.Fatal("an unrelated repository made the cohort undescribable")
		}
		if got := c.dependencyRevision(); got != baseline {
			t.Fatalf("an unrelated repository re-keyed a repository-scoped cohort:\n"+
				" before %q\n after  %q", baseline, got)
		}
	})
}

// TestBuilderWorkspaceMembersReachesTheRepositoryTopology is the production
// trace for the scope: a coordinator the lifecycle built is handed the live
// per-repository Indexer as its builder's Admissions handle, and every Indexer
// a MultiIndexer creates carries the link back to it. That link is what makes
// the coordinator's cohort scope agree with the workspace boundary the request
// surface uses.
func TestBuilderWorkspaceMembersReachesTheRepositoryTopology(t *testing.T) {
	mi := &MultiIndexer{
		repos:    map[string]*RepoMetadata{"api": {}, "web": {}, "infra": {}},
		indexers: map[string]*Indexer{},
	}
	for prefix, workspace := range map[string]string{
		"api": "product", "web": "product", "infra": "platform",
	} {
		idx := &Indexer{repositoryMutationOwner: mi}
		idx.SetRepoPrefix(prefix)
		idx.SetWorkspaceID(workspace)
		mi.indexers[prefix] = idx
	}

	members := builderWorkspaceMembers(&SparseGenerationBuilder{Admissions: mi.indexers["api"]}, "product")
	if members == nil {
		t.Fatal("a builder whose Admissions indexer belongs to a MultiIndexer reported no topology")
	}
	got := members()
	if !got["api"] || !got["web"] {
		t.Fatalf("the workspace lost one of its own repositories: %v", got)
	}
	if got["infra"] {
		t.Fatalf("the workspace admitted a repository from another workspace: %v", got)
	}

	// And the two shapes that have no topology to read fall back rather than
	// reaching for a nil map.
	if builderWorkspaceMembers(&SparseGenerationBuilder{}, "product") != nil {
		t.Fatal("a builder with no admissions handle claimed a workspace topology")
	}
	standalone := &Indexer{}
	if builderWorkspaceMembers(&SparseGenerationBuilder{Admissions: standalone}, "product") != nil {
		t.Fatal("a standalone indexer claimed a workspace topology")
	}
}

// TestAnIdlePollDescribesNoCohort is the cost fix.
//
// Before it, settledWithoutBuild described the cohort on every poll: one
// daemon-wide roster read lease plus one catalog read per roster member, every
// 15 seconds, per checkout — a lease that every repository registration and
// every admission close has to wait behind, for an answer that moves only when
// the topology does.
func TestAnIdlePollDescribesNoCohort(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	c := f.inertCoordinator(t, cohortCoordinatorConfig())
	if cycle := coordinatorReconcile(t, c); cycle.CommitGenerationID == 0 {
		t.Fatalf("the cycle did not route the layers: %+v", cycle)
	}

	rosterBefore := c.cohortCost.RosterAcquisitions.Load()
	readsBefore := c.cohortCost.MemberSourceReads.Load()
	for i := 0; i < 5; i++ {
		if _, settled := c.settledWithoutBuild(context.Background()); !settled {
			t.Fatalf("poll %d did not settle on the layers the cycle had just routed", i)
		}
	}
	if got := c.cohortCost.RosterAcquisitions.Load() - rosterBefore; got != 0 {
		t.Fatalf("five idle polls took %d roster read leases; an idle poll must take none", got)
	}
	if got := c.cohortCost.MemberSourceReads.Load() - readsBefore; got != 0 {
		t.Fatalf("five idle polls read %d roster members' sources; an idle poll must read none", got)
	}

	// An event — and only an event — makes the next poll describe it, once.
	c.InvalidateDependencyCohort("test: the topology moved")
	if _, settled := c.settledWithoutBuild(context.Background()); !settled {
		t.Fatal("the poll after an invalidation stopped settling on unchanged inputs")
	}
	if got := c.cohortCost.RosterAcquisitions.Load() - rosterBefore; got != 1 {
		t.Fatalf("an invalidated cohort was described %d times, want exactly 1", got)
	}
	for i := 0; i < 3; i++ {
		if _, settled := c.settledWithoutBuild(context.Background()); !settled {
			t.Fatalf("poll %d after the refresh stopped settling", i)
		}
	}
	if got := c.cohortCost.RosterAcquisitions.Load() - rosterBefore; got != 1 {
		t.Fatalf("the refreshed cohort was described again by an idle poll (%d descriptions)", got)
	}

	// A build always describes it afresh: what a layer is STAMPED with names
	// inputs that were validated when the build started.
	beforeBuild := c.cohortCost.RosterAcquisitions.Load()
	worktreeWrite(t, f.worktree, "poll-cost.go", "package fixture\n\nfunc PollCost() {}\n")
	if cycle := coordinatorReconcile(t, c); !cycle.DirtyBuilt {
		t.Fatalf("the cycle did not rebuild the working-tree layer: %+v", cycle)
	}
	if got := c.cohortCost.RosterAcquisitions.Load() - beforeBuild; got != 1 {
		t.Fatalf("a build described the cohort %d times, want exactly 1", got)
	}
}

// TestAnUndescribableCohortDefersRatherThanPoisoningTheLayers is the refusal
// fix.
//
// Before it, any transient that made the cohort undescribable — a repository
// closing, a raw repository mid-mutation, a sibling tracked but not yet indexed
// — stamped the built layers with `cohort-refused:<nanoseconds>`, a value no
// stored row can ever match. Every checkout in the daemon then built layers
// that were permanently unreusable and had to be rebuilt on the next successful
// cycle.
//
// The cycle now DEFERS instead, bounded, and the layers it already built keep
// their certified revision — so when the transient clears the poll settles on
// them without a rebuild.
func TestAnUndescribableCohortDefersRatherThanPoisoningTheLayers(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	cfg := cohortCoordinatorConfig()
	cfg.WorkspaceMembers = workspaceOf(builderRepoPrefix, "late-sibling")
	c := f.inertCoordinator(t, cfg)

	cycle := coordinatorReconcile(t, c)
	if cycle.CommitGenerationID == 0 || cycle.DirtyGenerationID == 0 {
		t.Fatalf("the cycle did not route both layers: %+v", cycle)
	}
	certified := c.dependencyRevision()
	if !strings.HasPrefix(certified, DependencyRevisionEncodingVersion+":") {
		t.Fatalf("the first cycle did not certify its cohort: %q", certified)
	}
	routed := f.route()
	generationsBefore := len(f.generations())

	// The transient: a repository joins this workspace's roster and is not
	// indexed yet, so its corpus cannot be named.
	sibling := registerCohortSibling(t, f, "late-sibling", "")

	// Every deferral leaves the route exactly as it found it, allocates no
	// generation and stamps nothing.
	worktreeWrite(t, f.worktree, "during-transient.go", "package fixture\n\nfunc Transient() {}\n")
	for i := 1; i <= maxCohortBuildDeferrals; i++ {
		out := c.reconcile(context.Background())
		if !out.Deferred {
			t.Fatalf("cycle %d built under an undescribable cohort instead of deferring: %+v", i, out)
		}
		if out.CommitBuilt || out.DirtyBuilt {
			t.Fatalf("a deferred cycle %d built something: %+v", i, out)
		}
	}
	if got := f.route(); got.CommitGenerationID != routed.CommitGenerationID ||
		got.DirtyGenerationID != routed.DirtyGenerationID {
		t.Fatalf("a deferred cycle moved the route: %+v then %+v", routed, got)
	}
	if got := len(f.generations()); got != generationsBefore {
		t.Fatalf("deferred cycles allocated %d generations", got-generationsBefore)
	}
	for _, id := range []int64{routed.CommitGenerationID, routed.DirtyGenerationID} {
		row, found := f.generation(id)
		if !found {
			t.Fatalf("routed generation %d vanished", id)
		}
		if row.DependencyRevision != certified {
			t.Fatalf("a deferral re-stamped generation %d: %q, want the certified %q",
				id, row.DependencyRevision, certified)
		}
	}

	// The transient clears. Nothing was poisoned, so the layers are still the
	// ones this cohort describes and the poll settles without a rebuild.
	bindCohortSiblingCorpus(t, f, sibling, "tree-late-sibling")
	if !c.describeDependencyCohort(context.Background()) {
		t.Fatal("the cohort stayed undescribable after the sibling was indexed")
	}
	if got := c.dependencyRevision(); got == certified {
		t.Fatal("a repository joined the workspace and the cohort did not move")
	}
}

// TestABoundedTransientStopsDeferringAndDegradesStably is the other half: a
// transient that never clears must not freeze a checkout's view forever, and
// the revision it then builds under must be STABLE — a unique per-refusal value
// can never be matched again, so every following cycle would rebuild.
func TestABoundedTransientStopsDeferringAndDegradesStably(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	cfg := cohortCoordinatorConfig()
	cfg.WorkspaceMembers = workspaceOf(builderRepoPrefix, "never-indexed")
	c := f.inertCoordinator(t, cfg)

	// A certified cycle first: deferring is a response to a transient, and a
	// transient is a departure from a state that worked.
	if first := coordinatorReconcile(t, c); first.CommitGenerationID == 0 {
		t.Fatalf("the first cycle did not route the layers: %+v", first)
	}
	if !c.dependencyCohortDescribable() {
		t.Fatal("the first cycle did not certify its cohort")
	}
	registerCohortSibling(t, f, "never-indexed", "")
	worktreeWrite(t, f.worktree, "degraded.go", "package fixture\n\nfunc Degraded() {}\n")

	var built CheckoutCycle
	for i := 0; i <= maxCohortBuildDeferrals; i++ {
		built = c.reconcile(context.Background())
		if built.Err != nil {
			t.Fatalf("cycle %d: %v", i, built.Err)
		}
		if i < maxCohortBuildDeferrals && !built.Deferred {
			t.Fatalf("cycle %d built before the deferral budget ran out: %+v", i, built)
		}
	}
	if built.Deferred || built.DirtyGenerationID == 0 {
		t.Fatalf("the cycle after the deferral budget did not build: %+v", built)
	}

	degraded := c.dependencyRevision()
	switch {
	case degraded == "":
		t.Fatal("an undescribable cohort fell back to the empty (legacy) revision, " +
			"which every stored generation matches")
	case strings.HasPrefix(degraded, DependencyRevisionEncodingVersion+":"):
		t.Fatalf("an undescribable cohort produced a certificate: %q", degraded)
	case !strings.HasPrefix(degraded, DependencyRevisionDegradedPrefix+":"):
		t.Fatalf("an undescribable cohort produced %q, which names neither vocabulary", degraded)
	}
	row, found := f.generation(built.DirtyGenerationID)
	if !found || row.DependencyRevision != degraded {
		t.Fatalf("the degraded cycle stored %q, want %q (found=%v)",
			row.DependencyRevision, degraded, found)
	}

	// Stable: the next cycle over the same transient reuses instead of
	// rebuilding, which a per-refusal unique value would never do.
	if _, settled := c.settledWithoutBuild(context.Background()); !settled {
		t.Fatal("the poll after a degraded build did not settle; the degraded revision is not stable")
	}
	if again := c.reconcile(context.Background()); again.CommitBuilt || again.DirtyBuilt {
		t.Fatalf("a second degraded cycle rebuilt what the first one built: %+v", again)
	}
	if c.dependencyRevision() != degraded {
		t.Fatalf("the degraded revision re-minted itself: %q then %q", degraded, c.dependencyRevision())
	}
}

// TestACoordinatorWithNoDescribableCohortBuildsImmediately pins the other side
// of the deferral policy. Deferring is a response to a TRANSIENT — something
// that was describable and stopped being so — and it costs the checkout a
// slightly stale view for a cycle or two. A coordinator that has NEVER
// described a cohort has no view to keep serving, so deferring would leave the
// checkout with nothing at all; it builds under the stable degraded revision
// straight away and logs why.
func TestACoordinatorWithNoDescribableCohortBuildsImmediately(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()

	// A coordinator with no configuration sections: the producer refuses the
	// cohort for want of the configuration domains outside config.IndexConfig,
	// so it is undescribable from its very first cycle.
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{
		ConfigSections: []DependencyRevisionConfigSection{},
	})
	if c.dependencyCohortDescribable() {
		t.Fatal("a coordinator with no configuration sections certified a cohort")
	}

	out := c.reconcile(context.Background())
	if out.Err != nil {
		t.Fatalf("reconcile: %v", out.Err)
	}
	if out.Deferred {
		t.Fatal("a coordinator that has never described a cohort deferred its first build; " +
			"the checkout is left with no view at all")
	}
	if out.CommitGenerationID == 0 || out.DirtyGenerationID == 0 {
		t.Fatalf("the first cycle did not route both layers: %+v", out)
	}
	row, found := f.generation(out.CommitGenerationID)
	if !found || !strings.HasPrefix(row.DependencyRevision, DependencyRevisionDegradedPrefix+":") {
		t.Fatalf("the first cycle stored %q, want a degraded revision (found=%v)",
			row.DependencyRevision, found)
	}
}

// TestTheDegradedRevisionIsStableAcrossCoordinatorsAndMovesWithItsInputs pins
// the value's two halves at the unit level: two producers over the same
// undescribable inputs agree (so a restart does not rebuild), and a change in
// anything the producer CAN name still re-keys (so the degradation does not
// swallow a real configuration change).
func TestTheDegradedRevisionIsStableAcrossCoordinatorsAndMovesWithItsInputs(t *testing.T) {
	base := dependencyCohortSource{
		Target:            DependencyRevisionTarget{RepoPrefix: "repo", WorkspaceID: "ws", ProjectID: "proj"},
		Config:            config.Default().Index,
		ConfigSections:    dedicatedBaseConfigSections(config.Default()),
		Producers:         cohortProducerPolicy(config.Default().Index, false),
		Capabilities:      cohortCapabilityVocabulary(),
		ExtractorVersions: extractorVersionsFingerprint(),
	}
	reason := dependencyCohortReasonIncomplete
	baseline := base.degradedRevision(reason)

	if again := base.degradedRevision(reason); again != baseline {
		t.Fatalf("two producers over identical inputs disagree: %q vs %q", baseline, again)
	}
	if !strings.Contains(baseline, reason) {
		t.Fatalf("the degraded revision %q does not name the reason it was produced for", baseline)
	}
	if other := base.degradedRevision(dependencyCohortReasonClosing); other == baseline {
		t.Fatal("two different refusals produced one revision")
	}

	for _, tc := range []struct {
		name   string
		mutate func(*dependencyCohortSource)
	}{
		{"the target moves", func(s *dependencyCohortSource) { s.Target.ProjectID = "other" }},
		{"the index configuration moves", func(s *dependencyCohortSource) { s.Config.MaxFileSize++ }},
		{"a named configuration domain moves", func(s *dependencyCohortSource) {
			s.ConfigSections = append([]DependencyRevisionConfigSection(nil), s.ConfigSections...)
			s.ConfigSections[0].Digest += "-changed"
		}},
		{"the producer policy moves", func(s *dependencyCohortSource) {
			s.Producers = cohortProducerPolicy(config.Default().Index, true)
		}},
		{"the capability vocabulary moves", func(s *dependencyCohortSource) {
			s.Capabilities = s.Capabilities[:len(s.Capabilities)-1]
		}},
		{"an extractor version moves", func(s *dependencyCohortSource) { s.ExtractorVersions += "+" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := base
			tc.mutate(&mutated)
			if got := mutated.degradedRevision(reason); got == baseline {
				t.Fatalf("%s left the degraded revision at %q", tc.name, got)
			}
		})
	}
}

// TestCheckoutLayerIdentitiesCarryTheDerivedResolverVersion is the wiring claim
// for the resolver contract's fingerprint: the identity column used to be the
// compile-time literal "1" in two copies, which only ever invalidated when a
// human remembered to raise it.
func TestCheckoutLayerIdentitiesCarryTheDerivedResolverVersion(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	worktreeWrite(t, f.worktree, "resolver-version.go", "package fixture\n\nfunc Version() {}\n")
	c := f.inertCoordinator(t, cohortCoordinatorConfig())
	cycle := coordinatorReconcile(t, c)
	if cycle.CommitGenerationID == 0 || cycle.DirtyGenerationID == 0 {
		t.Fatalf("the cycle did not route both layers: %+v", cycle)
	}
	want := resolver.Version()
	if want == "1" || want == "" {
		t.Fatalf("the resolver contract fingerprint is %q; it is not derived", want)
	}
	for _, named := range []struct {
		what         string
		generationID int64
	}{
		{"commit layer", cycle.CommitGenerationID},
		{"working-tree layer", cycle.DirtyGenerationID},
	} {
		row, found := f.generation(named.generationID)
		if !found {
			t.Fatalf("%s generation %d is not in the catalog", named.what, named.generationID)
		}
		if row.ResolverVersion != want {
			t.Errorf("%s stored resolver_version %q, the derived contract is %q",
				named.what, row.ResolverVersion, want)
		}
	}
}

// TestCohortProducerPolicyMirrorsTheDeclaredProducers pins the mirror.
//
// cohortProducerPolicy states what a build's producer policy WILL be, from the
// configuration alone; SparseGenerationBuilder.declareProducers is what the
// build actually writes. The two are separate functions — a cohort digests
// inputs, and declareProducers also narrows rows by build outcome — so nothing
// but this test stops them drifting. It reads a real build's stored rows back
// and compares them row for row.
func TestCohortProducerPolicyMirrorsTheDeclaredProducers(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	c := f.inertCoordinator(t, cohortCoordinatorConfig())
	cycle := coordinatorReconcile(t, c)
	if cycle.CommitGenerationID == 0 {
		t.Fatalf("the cycle did not route a commit layer: %+v", cycle)
	}
	declared, err := f.store.AtGeneration(cycle.CommitGenerationID).ProducerStates()
	if err != nil {
		t.Fatalf("read the built generation's producer states: %v", err)
	}
	byProducer := make(map[string]store_sqlite.ProducerCompleteness, len(declared))
	for _, row := range declared {
		byProducer[row.Producer] = row
	}
	for _, row := range cohortProducerPolicy(c.config, c.builder.Embedder != nil) {
		actual, found := byProducer[row.Producer]
		if !found {
			t.Errorf("the cohort digests producer %q, which the build never declares", row.Producer)
			continue
		}
		if string(actual.State) != row.State || actual.Reason != row.Reason {
			t.Errorf("the cohort and the build disagree about %q:\n cohort %s/%q\n build  %s/%q",
				row.Producer, row.State, row.Reason, actual.State, actual.Reason)
		}
	}
}

// TestCoordinatorRefusesAnUnfreezableConfiguration pins the fail-closed
// direction of the configuration freeze.
//
// The constructor used to log a Warn and carry on with the caller's value. That
// keeps the IDENTITY honest — it mints a unique config digest, so nothing is
// mis-reused — but it leaves the BUILDER holding the ConfigManager's own nested
// maps and slices, which the next configuration reload can change underneath a
// build that has already stamped an identity naming the configuration it
// started with. That is the exact mis-description the freeze exists to prevent,
// so it is a refusal to construct.
func TestCoordinatorRefusesAnUnfreezableConfiguration(t *testing.T) {
	f := newCoordinatorFixture(t)

	c, err := NewCheckoutCoordinator(CheckoutCoordinatorConfig{
		CheckoutID:     f.checkoutID,
		CheckoutRoot:   f.worktree,
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
		// The seam is per-construction, so this substitution is private to
		// this coordinator and cannot race another test's.
		snapshotConfig: func(
			cfg config.IndexConfig, repoPrefix, workspaceID, projectID string,
		) (config.IndexConfig, string, error) {
			return config.IndexConfig{}, "",
				errors.New("encode dedicated base config: unencodable value")
		},
	})
	if err == nil {
		_ = c.Close()
		t.Fatal("a coordinator was built over a configuration that could not be frozen; " +
			"its builder would hold the ConfigManager's own values")
	}
	if c != nil {
		_ = c.Close()
		t.Fatal("the refused constructor returned a coordinator")
	}
	if !strings.Contains(err.Error(), "freeze the index configuration") {
		t.Fatalf("the refusal does not name what failed: %v", err)
	}
}

// TestADegradedPollDescribesNoCohortEither is the cost half of the refusal
// path. A coordinator whose cohort cannot be described must not retry the
// description on every poll: the retry is the expensive operation (a
// daemon-wide roster read lease plus a catalog read per in-scope member), and a
// transient that lasts minutes would then pay it every fifteen seconds, per
// checkout, for the whole of it. Retrying is what an event and a build do.
func TestADegradedPollDescribesNoCohortEither(t *testing.T) {
	f := newCoordinatorFixture(t)
	cfg := cohortCoordinatorConfig()
	cfg.WorkspaceMembers = workspaceOf(builderRepoPrefix, "never-indexed")
	registerCohortSibling(t, f, "never-indexed", "")
	c := f.inertCoordinator(t, cfg)

	if c.dependencyCohortDescribable() {
		t.Fatal("a cohort naming a repository with no corpus reported itself describable")
	}
	before := c.cohortCost.RosterAcquisitions.Load()
	for i := 0; i < 5; i++ {
		c.settledWithoutBuild(context.Background())
	}
	if got := c.cohortCost.RosterAcquisitions.Load() - before; got != 0 {
		t.Fatalf("five polls under a degraded cohort took %d roster read leases", got)
	}
	// And the degraded revision every identity carries is one value, not one
	// per read: it digests the whole frozen configuration.
	first := c.dependencyRevision()
	if second := c.dependencyRevision(); second != first {
		t.Fatalf("the degraded revision re-minted itself between reads: %q then %q", first, second)
	}
}

// TestTheRevisionIsNeverTheLegacyEmptyValue pins the one value a checkout
// layer's identity may never carry. generationIdentityKey renders an empty
// dependency revision byte-for-byte as the pre-cohort key, which every stored
// generation matches — so an empty revision is not "no claim", it is the claim
// that any payload will do.
func TestTheRevisionIsNeverTheLegacyEmptyValue(t *testing.T) {
	f := newCoordinatorFixture(t)

	for _, tc := range []struct {
		name string
		of   func() *CheckoutCoordinator
	}{
		{"a certified coordinator", func() *CheckoutCoordinator {
			return f.inertCoordinator(t, cohortCoordinatorConfig())
		}},
		{"an undescribable coordinator", func() *CheckoutCoordinator {
			return f.inertCoordinator(t, CheckoutCoordinatorConfig{
				ConfigSections: []DependencyRevisionConfigSection{},
			})
		}},
		{"a hand-assembled coordinator", func() *CheckoutCoordinator {
			return &CheckoutCoordinator{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.of()
			if got := c.dependencyRevision(); got == "" {
				t.Fatal("the coordinator's identities would carry the legacy empty revision, " +
					"which every stored generation matches")
			}
		})
	}
}

// --- the cost and refusal guards the production claims rest on -------------

// mutableWorkspace is the WorkspaceMembers seam with a set the test can move.
//
// It is the shape production has: MultiIndexer.ReposInWorkspace answers from
// the live repository topology, so tracking a repository into this workspace —
// or untracking one out of it — changes what the coordinator's next call sees,
// with nothing having to tell the coordinator about it.
type mutableWorkspace struct {
	mu      sync.Mutex
	members map[string]bool
}

func newMutableWorkspace(prefixes ...string) *mutableWorkspace {
	w := &mutableWorkspace{members: map[string]bool{}}
	for _, prefix := range prefixes {
		w.members[prefix] = true
	}
	return w
}

func (w *mutableWorkspace) add(prefix string) {
	w.mu.Lock()
	w.members[prefix] = true
	w.mu.Unlock()
}

// seam renders it the way ReposInWorkspace does: a fresh map per call.
func (w *mutableWorkspace) seam() func() map[string]bool {
	return func() map[string]bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		out := make(map[string]bool, len(w.members))
		for prefix, in := range w.members {
			out[prefix] = in
		}
		return out
	}
}

// TestTheCohortReadsOnlyInScopeRepositories pins the scope filter where the
// per-cycle COST is actually paid: dependencyCohortSource.sourceIdentities.
//
// DependencyRevisionRosterScoped's own filter decides which repositories are
// MEMBERS. This one decides which repositories are READ — a catalog row and a
// primary-base resolution for a dedicated member, a bounded source witness for
// a raw one. Deleting it leaves the revision correct and silently restores the
// daemon-wide cost the whole change exists to remove, and makes this checkout's
// cohort undescribable whenever ANY tracked repository in the daemon cannot
// name its corpus — a repository this checkout's resolution can never consult.
func TestTheCohortReadsOnlyInScopeRepositories(t *testing.T) {
	f := newCoordinatorFixture(t)
	cfg := cohortCoordinatorConfig()
	cfg.WorkspaceMembers = workspaceOf(builderRepoPrefix)
	c := f.inertCoordinator(t, cfg)

	reads := func(during func()) int64 {
		before := c.cohortCost.MemberSourceReads.Load()
		during()
		return c.cohortCost.MemberSourceReads.Load() - before
	}

	// The floor: a one-repository workspace reads NOTHING. The only member is
	// the target, and a repository's own corpus is not one of its dependencies
	// — see DependencyRevisionTargetSourceIdentity.
	if got := reads(func() {
		if !c.describeDependencyCohort(context.Background()) {
			t.Fatal("the production cohort was refused")
		}
	}); got != 0 {
		t.Fatalf("a one-repository workspace read %d roster members' sources, want 0: "+
			"the only member is the target, whose corpus is the target rather than an input", got)
	}

	// Three indexed repositories in other workspaces. They are roster members
	// of the daemon and none of them is an input here, so none of them is read.
	for _, name := range []string{"outside-a", "outside-b", "outside-c"} {
		registerCohortSibling(t, f, name, "tree-"+name)
	}
	if got := reads(func() {
		if !c.describeDependencyCohort(context.Background()) {
			t.Fatal("repositories outside the workspace made the cohort undescribable")
		}
	}); got != 0 {
		t.Fatalf("a one-repository workspace read %d roster members' sources with three "+
			"repositories tracked in other workspaces; the per-cycle cost is bounded by "+
			"the daemon rather than by the workspace", got)
	}

	// And a dedicated repository outside the workspace that cannot name its
	// corpus at all — tracked, not yet indexed — is not this checkout's
	// problem. Reading it would refuse a cohort that does not depend on it.
	registerCohortSibling(t, f, "outside-broken", "")
	if !c.describeDependencyCohort(context.Background()) {
		t.Fatalf("a repository outside the workspace with no committed corpus made this "+
			"checkout's cohort undescribable: %q", c.dependencyRevision())
	}

	// The raw half of the same enumeration: a raw repository outside the
	// workspace has no source witness to acquire, so reading it would both cost
	// a bounded wait and refuse the cohort.
	if _, err := f.leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{
		RepoPrefix: "outside-raw", RootIdentity: "root-outside-raw", Incarnation: "inc-outside-raw",
	}, nil); err != nil {
		t.Fatalf("register the out-of-scope raw repository: %v", err)
	}
	if got := reads(func() {
		if !c.describeDependencyCohort(context.Background()) {
			t.Fatalf("a raw repository outside the workspace made this checkout's cohort "+
				"undescribable: %q", c.dependencyRevision())
		}
	}); got != 0 {
		t.Fatalf("a one-repository workspace read %d roster members' sources with a raw "+
			"repository registered in another workspace, want 0", got)
	}

	// And the positive: a repository that IS in scope and is NOT the target is
	// read, once. Without this the zeros above would also pass if the scope
	// filter had swallowed the whole enumeration.
	cfg.WorkspaceMembers = workspaceOf(builderRepoPrefix, "inside-one")
	inside := f.inertCoordinator(t, cfg)
	registerCohortSibling(t, f, "inside-one", "tree-inside-one")
	before := inside.cohortCost.MemberSourceReads.Load()
	if !inside.describeDependencyCohort(context.Background()) {
		t.Fatalf("a workspace with one indexed sibling was refused: %q", inside.dependencyRevision())
	}
	if got := inside.cohortCost.MemberSourceReads.Load() - before; got != 1 {
		t.Fatalf("a two-repository workspace read %d roster members' sources, want exactly 1 "+
			"(the sibling, not the target)", got)
	}
}

// TestARepositorysOwnTreeIsNotAnInputToItsOwnCohort is the fix W5/W4.3-verify
// (major 1) demanded, at the producer.
//
// The cohort named every IN-SCOPE roster member's bytes, and the target is in
// scope by construction, so the target's own committed corpus was an input to
// its own dependency revision. graphBase resolves a dedicated member's corpus
// from its ACTIVE generation, so every published advance moved the target's own
// revision — and a moved revision roots a new chain rather than extending one.
// The measured effect was a full committed-tree index on essentially every
// commit of a running daemon.
//
// A sibling's tree is a different matter: that is a genuine cross-repository
// input and must still move the revision. Both directions are asserted here at
// the producer, so the rule is pinned independently of the publication chain
// test that exercises it end to end.
func TestARepositorysOwnTreeIsNotAnInputToItsOwnCohort(t *testing.T) {
	f := newCoordinatorFixture(t)
	cfg := cohortCoordinatorConfig()
	cfg.WorkspaceMembers = workspaceOf(builderRepoPrefix, "cohort-input")
	c := f.inertCoordinator(t, cfg)

	sibling := registerCohortSibling(t, f, "cohort-input", "tree-input-1")

	lease, err := f.leases.AcquireRepositoryRoster()
	if err != nil {
		t.Fatalf("acquire the roster: %v", err)
	}
	inputs, err := c.cohort.inputs(context.Background(), lease)
	lease.Release()
	if err != nil {
		t.Fatalf("assemble the production cohort: %v", err)
	}

	var target, dependency DependencyRevisionRepository
	for _, repository := range inputs.Repositories {
		switch repository.RepoPrefix {
		case builderRepoPrefix:
			target = repository
		case sibling.prefix:
			dependency = repository
		}
	}
	if target.RepoPrefix == "" {
		t.Fatalf("the cohort does not carry its own target: %+v", inputs.Repositories)
	}
	if got := target.SourceIdentity; got != DependencyRevisionTargetSourceIdentity {
		t.Fatalf("the target's roster entry names its own bytes (%q); every commit in this "+
			"repository re-keys its own cohort and forces a full re-root", got)
	}
	if dependency.RepoPrefix == "" {
		t.Fatalf("the cohort dropped its workspace sibling: %+v", inputs.Repositories)
	}
	if !strings.HasPrefix(dependency.SourceIdentity, "tree:") {
		t.Fatalf("a workspace sibling's roster entry does not name its bytes: %q",
			dependency.SourceIdentity)
	}
	if dependency.SourceIdentity == DependencyRevisionTargetSourceIdentity {
		t.Fatal("a workspace sibling was treated as the target")
	}

	// The target's own committed corpus moving does not move the revision. The
	// baseline is taken after a fresh description so it names the roster the
	// sibling is already in; what moves after it is one input at a time.
	if !c.describeDependencyCohort(context.Background()) {
		t.Fatalf("the production cohort was refused: %q", c.dependencyRevision())
	}
	baseline := c.dependencyRevision()
	if !strings.HasPrefix(baseline, DependencyRevisionEncodingVersion+":") {
		t.Fatalf("the production cohort was refused: %q", baseline)
	}
	moveTargetCorpus(t, f, "tree-target-2")
	if !c.describeDependencyCohort(context.Background()) {
		t.Fatal("the target's own commit made its cohort undescribable")
	}
	if got := c.dependencyRevision(); got != baseline {
		t.Fatalf("the target's own commit moved its own dependency revision:\n before %q\n after  %q",
			baseline, got)
	}

	// The sibling's does.
	moveCohortSiblingTree(t, f, sibling, "tree-input-2")
	if !c.describeDependencyCohort(context.Background()) {
		t.Fatal("a workspace sibling's commit made the cohort undescribable")
	}
	if got := c.dependencyRevision(); got == baseline {
		t.Fatal("a workspace sibling's committed tree moved and the cohort did not; " +
			"excluding the target must not exclude its dependencies")
	}
}

// TestTheDeferralBudgetIsPerTransient pins the reset.
//
// maxCohortBuildDeferrals bounds ONE transient. A budget that is never reset is
// per-coordinator-lifetime instead: after the first transient exhausts it every
// later transient stamps a degraded layer on its FIRST cycle — two forced
// rebuilds per transient (one onto the degraded revision, one back off it)
// where the policy promises zero.
func TestTheDeferralBudgetIsPerTransient(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	cfg := cohortCoordinatorConfig()
	cfg.WorkspaceMembers = workspaceOf(builderRepoPrefix, "transient-a", "transient-b")
	c := f.inertCoordinator(t, cfg)

	if first := coordinatorReconcile(t, c); first.CommitGenerationID == 0 {
		t.Fatalf("the first cycle did not route the layers: %+v", first)
	}
	if !c.dependencyCohortDescribable() {
		t.Fatal("the first cycle did not certify its cohort")
	}

	// Transient one, driven all the way past the budget.
	first := registerCohortSibling(t, f, "transient-a", "")
	worktreeWrite(t, f.worktree, "transient-a.go", "package fixture\n\nfunc TransientA() {}\n")
	for i := 0; i <= maxCohortBuildDeferrals; i++ {
		out := c.reconcile(context.Background())
		if out.Err != nil {
			t.Fatalf("transient one, cycle %d: %v", i, out.Err)
		}
		if i < maxCohortBuildDeferrals && !out.Deferred {
			t.Fatalf("transient one, cycle %d built before the budget ran out: %+v", i, out)
		}
	}

	// It clears, and a describing cycle re-keys the layers onto a certificate.
	bindCohortSiblingCorpus(t, f, first, "tree-transient-a")
	if cleared := c.reconcile(context.Background()); cleared.Err != nil {
		t.Fatalf("the cycle after the transient cleared: %v", cleared.Err)
	}
	if !c.dependencyCohortDescribable() {
		t.Fatal("the cohort stayed undescribable after the first transient cleared")
	}

	// Transient two. It is a NEW transient, so it gets the whole budget again:
	// its first cycle must defer, not stamp.
	registerCohortSibling(t, f, "transient-b", "")
	worktreeWrite(t, f.worktree, "transient-b.go", "package fixture\n\nfunc TransientB() {}\n")
	out := c.reconcile(context.Background())
	if out.Err != nil {
		t.Fatalf("transient two, first cycle: %v", out.Err)
	}
	if !out.Deferred {
		t.Fatalf("a second transient built under a degraded cohort on its FIRST cycle "+
			"instead of deferring; the budget is per-coordinator-lifetime rather than "+
			"per-transient: %+v", out)
	}
	if out.CommitBuilt || out.DirtyBuilt {
		t.Fatalf("the deferred cycle built something: %+v", out)
	}
}

// TestAWorkspaceTopologyChangeUnsettlesAPollWithoutAnEvent is the production
// source for the cohort cache's staleness.
//
// The cache is refreshed by events (InvalidateDependencyCohort) and by every
// build. Neither reaches a checkout that is merely polling while a repository
// is tracked into its workspace — and that repository joining is exactly what
// changes which repositories are inputs at all. The poll therefore re-reads the
// cheap topology token itself: a map walk under the MultiIndexer's own read
// lock, no roster lease and no catalog read, which is what makes it affordable
// where describing the cohort is not.
func TestAWorkspaceTopologyChangeUnsettlesAPollWithoutAnEvent(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	workspace := newMutableWorkspace(builderRepoPrefix)
	cfg := cohortCoordinatorConfig()
	cfg.WorkspaceMembers = workspace.seam()
	c := f.inertCoordinator(t, cfg)

	cycle := coordinatorReconcile(t, c)
	if cycle.CommitGenerationID == 0 || cycle.DirtyGenerationID == 0 {
		t.Fatalf("the cycle did not route both layers: %+v", cycle)
	}
	built := c.dependencyRevision()

	// An unchanged topology still costs nothing: the token is not a
	// description, and re-reading it must not turn the poll back into one.
	roster := c.cohortCost.RosterAcquisitions.Load()
	for i := 0; i < 5; i++ {
		if _, settled := c.settledWithoutBuild(context.Background()); !settled {
			t.Fatalf("poll %d did not settle on the layers the cycle had just routed", i)
		}
	}
	if got := c.cohortCost.RosterAcquisitions.Load() - roster; got != 0 {
		t.Fatalf("five idle polls took %d roster read leases; the topology token is not free", got)
	}

	// A repository is tracked into this workspace and indexed. Nothing tells
	// the coordinator.
	registerCohortSibling(t, f, "joined-repo", "tree-joined-repo")
	workspace.add("joined-repo")

	if _, settled := c.settledWithoutBuild(context.Background()); settled {
		t.Fatal("the poll settled on a layer built under a different input cohort after a " +
			"repository joined the workspace; nothing in production ever re-describes it")
	}
	if got := c.dependencyRevision(); got == built {
		t.Fatalf("the poll did not re-describe the cohort: still %q", got)
	}
	if got := c.cohortCost.RosterAcquisitions.Load() - roster; got != 1 {
		t.Fatalf("the topology change caused %d descriptions, want exactly 1", got)
	}

	// And it settles again afterwards: the token is re-recorded, so the next
	// idle poll is free.
	for i := 0; i < 3; i++ {
		c.settledWithoutBuild(context.Background())
	}
	if got := c.cohortCost.RosterAcquisitions.Load() - roster; got != 1 {
		t.Fatalf("idle polls after the refresh described the cohort again (%d in total)", got)
	}
}
