package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// installStartupPublisherRuntime mounts the publisher runtime the way
// NewSharedServer does: over the lifecycle's own lease manager, before any
// owner is registered.
func installStartupPublisherRuntime(t *testing.T, f *lifecycleFixture) *DedicatedBaseRuntime {
	t.Helper()
	runtime, err := NewDedicatedBaseRuntime(f.store, f.lc.ViewLeases())
	require.NoError(t, err)
	require.NoError(t, f.lc.SetDedicatedBaseCleanupRuntime(runtime))
	return runtime
}

func startupPublisher(t *testing.T, f *lifecycleFixture) *InitialBasePublisher {
	t.Helper()
	publisher, err := NewInitialBasePublisher(f.lc)
	require.NoError(t, err)
	t.Cleanup(publisher.Close)
	return publisher
}

// warmRestart is a daemon stop and start: the store and the catalog survive,
// nothing in process does. It reproduces what a real warm start does before
// publication — reopen the stack, install the runtime, and bring the
// repository back onto the new MultiIndexer, which is the warmup dispatch's
// ReconcileRepoCtx/TrackRepoCtx step.
func warmRestart(t *testing.T, f *lifecycleFixture, root string) {
	t.Helper()
	f.restart()
	installStartupPublisherRuntime(t, f)
	_, err := f.lc.Register(context.Background(), config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
}

// allocateDependentCheckout writes one DEPENDENT checkout row into a family:
// a non-owner identity in the automatic mode, which is exactly what
// dedicatedBaseConsumers counts and what applyCoordinators would give a
// coordinator to.
//
// It writes the row directly rather than adding a worktree and sweeping,
// because a sweep also STARTS that checkout's coordinator, and a running
// coordinator asks for the committed base on its own schedule
// (CheckoutCoordinator.demandCommittedBase). A test about the CENSUS would
// then be racing a second, asynchronous demand for the same publication. The
// coordinator's own ask has its own test — see
// TestADependentCoordinatorAsksTheDaemonForTheCommittedBase, which drives the
// whole production chain and asserts on its outcome rather than on a count.
func allocateDependentCheckout(t *testing.T, f *lifecycleFixture, familyID, adminName string) string {
	t.Helper()
	owner, err := f.catalog.ListCheckouts(context.Background(), familyID)
	require.NoError(t, err)
	require.NotEmpty(t, owner, "the family holds no checkout to take a head from")
	checkout := store_sqlite.Checkout{
		CheckoutID:     "checkout-" + adminName,
		Incarnation:    "incarnation-" + adminName,
		FamilyID:       familyID,
		RootPath:       filepath.Join(f.dir, adminName),
		GitDir:         filepath.Join(owner[0].RootPath, ".git", "worktrees", adminName),
		AdminName:      adminName,
		State:          store_sqlite.CheckoutStateReady,
		DesiredMode:    store_sqlite.CheckoutModeAutomatic,
		EffectiveMode:  store_sqlite.CheckoutModeAutomatic,
		HeadRef:        "refs/heads/" + adminName,
		HeadCommit:     owner[0].HeadCommit,
		HeadTree:       owner[0].HeadTree,
		LastAccessible: time.Now().Unix(),
		LastSeen:       time.Now().Unix(),
	}
	require.NoError(t, f.catalog.AllocateCheckout(context.Background(), checkout))
	return checkout.CheckoutID
}

// dedicatedGenerations lists every committed generation one graph holds.
func dedicatedGenerations(t *testing.T, f *lifecycleFixture, graphID string) []store_sqlite.ViewGeneration {
	t.Helper()
	rows, err := f.catalog.ListViewGenerations(context.Background(), store_sqlite.ViewGenerationFilter{})
	require.NoError(t, err)
	out := make([]store_sqlite.ViewGeneration, 0, len(rows))
	for _, row := range rows {
		if row.GraphID == graphID && row.GenerationKind == "dedicated" {
			out = append(out, row)
		}
	}
	return out
}

// TestInitialBasePublisherRequiresTheSharedLeaseDomain pins the invariant the
// runtime's own doc comment asserts but nothing enforced before this item: a
// publication driver may not substitute a private graphview.LeaseManager.
//
// A private domain is not a cosmetic difference. The retirement sweep and every
// request reader agree on the lifecycle's manager; a generation advanced under
// another one is invisible to them, so a sweep can collect payload a live
// reader is composing over. Refusing at construction is what makes the runtime's
// `leases` field load-bearing rather than write-only.
func TestInitialBasePublisherRequiresTheSharedLeaseDomain(t *testing.T) {
	t.Run("private manager refused", func(t *testing.T) {
		f := newLifecycleFixture(t)
		defer f.close()
		private, err := NewDedicatedBaseRuntime(f.store, graphview.NewLeaseManager())
		require.NoError(t, err)
		require.NoError(t, f.lc.SetDedicatedBaseCleanupRuntime(private))

		publisher, err := NewInitialBasePublisher(f.lc)
		require.Nil(t, publisher)
		require.ErrorIs(t, err, errInitialBasePublisherInput)
		require.Contains(t, err.Error(), "shared view leases")
	})

	t.Run("shared manager accepted", func(t *testing.T) {
		f := newLifecycleFixture(t)
		defer f.close()
		runtime := installStartupPublisherRuntime(t, f)
		publisher, err := NewInitialBasePublisher(f.lc)
		require.NoError(t, err)
		defer publisher.Close()
		require.Same(t, runtime, publisher.runtime)
		require.Same(t, f.lc.ViewLeases(), publisher.runtime.ViewLeases())
	})

	t.Run("no runtime refused", func(t *testing.T) {
		f := newLifecycleFixture(t)
		defer f.close()
		publisher, err := NewInitialBasePublisher(f.lc)
		require.Nil(t, publisher)
		require.ErrorIs(t, err, errInitialBasePublisherInput)
	})

	t.Run("typed nil runtime refused", func(t *testing.T) {
		f := newLifecycleFixture(t)
		defer f.close()
		// SetDedicatedBaseCleanupRuntime compares an INTERFACE to nil, so a
		// typed-nil handle passes its guard. The publisher must not treat that
		// as an installed runtime.
		require.NoError(t, f.lc.SetDedicatedBaseCleanupRuntime((*DedicatedBaseRuntime)(nil)))
		publisher, err := NewInitialBasePublisher(f.lc)
		require.Nil(t, publisher)
		require.ErrorIs(t, err, errInitialBasePublisherInput)
	})
}

// TestInitialBasePublisherColdStartPublishesOneCommittedBasePerRepository is
// the cold-start acceptance case: one committed base per dedicated repository,
// carrying the complete frozen identity, adopted through the catalog's guarded
// transaction — and generation 0 untouched.
func TestInitialBasePublisherColdStartPublishesOneCommittedBasePerRepository(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	prefixes := make([]string, 0, 2)
	for _, name := range []string{"alpha", "beta"} {
		root := f.gitRepoWithDependent(name)
		registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
		require.NoError(t, err)
		require.NotEmpty(t, registered.Prefix)
		prefixes = append(prefixes, registered.Prefix)
	}

	publisher := startupPublisher(t, f)
	for _, prefix := range prefixes {
		outcome := publisher.PublishRepo(ctx, prefix)
		require.NoError(t, outcome.Err, "publishing %s", prefix)
		require.Empty(t, outcome.Skipped, "publishing %s", prefix)
		require.Positive(t, outcome.GenerationID)
		require.False(t, outcome.AlreadyAdopted, "a cold start adopts for the first time")

		graph := f.familyOf(prefix)
		require.Equal(t, outcome.GenerationID, graph.ActiveGenerationID,
			"the committed base is adopted as the graph's active generation")

		generations := dedicatedGenerations(t, f, graph.GraphID)
		require.Len(t, generations, 1, "exactly one committed base per dedicated repository")
		row := generations[0]
		checkout := f.checkoutOf(prefix)
		require.Equal(t, outcome.GenerationID, row.GenerationID)
		require.Equal(t, store_sqlite.ViewGenerationReady, row.State)
		require.Equal(t, "dedicated_graph", row.OwnerKind)
		require.Equal(t, checkout.CheckoutID, row.CheckoutID)
		require.Equal(t, checkout.HeadTree, row.TreeOID, "the base names the owner's committed tree")
		require.Equal(t, checkout.HeadCommit, row.ProvenanceCommitOID)
		// A dedicated root is self-contained: no lower layer, no fingerprint.
		require.Zero(t, row.BaseGenerationID)
		require.Empty(t, row.LayerID)
		require.Empty(t, row.LowerViewFingerprint)
		// The complete frozen identity.
		require.NotEmpty(t, row.ConfigHash)
		require.NotEmpty(t, row.ExtractorVersions)
		require.NotEmpty(t, row.ResolverVersion)
		require.NotEmpty(t, row.DependencyRevision, "an empty revision reads as 'matches anything'")
		require.False(t, strings.HasPrefix(row.DependencyRevision, DependencyRevisionDegradedPrefix),
			"a tracked, indexed repository describes a real cohort: %s", row.DependencyRevision)
		require.Positive(t, row.CoveredFiles, "the committed base indexed the committed tree")
	}
}

// TestInitialBasePublisherWarmRestartReAdoptsWithoutCatalogWrites is the
// no-op-safety acceptance case. A restart over an unchanged committed tree and
// an unchanged dependency revision must re-adopt the base that is already
// active without allocating, building or writing a single catalog row.
//
// The deterministic authority token is what makes this reachable: any other
// token rotates the authority epoch, which clears the attempt record, and the
// next claim then re-binds a reuse candidate — correct, but a write.
func TestInitialBasePublisherWarmRestartReAdoptsWithoutCatalogWrites(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	root := f.gitRepoWithDependent("warm")
	registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
	prefix := registered.Prefix

	first := startupPublisher(t, f).PublishRepo(ctx, prefix)
	require.NoError(t, first.Err)
	require.Positive(t, first.GenerationID)

	warmRestart(t, f, root)

	audit, err := installDedicatedWriteAudit(ctx, f.dbPath)
	require.NoError(t, err)

	second := startupPublisher(t, f).PublishRepo(ctx, prefix)
	require.NoError(t, second.Err)
	require.Equal(t, first.GenerationID, second.GenerationID, "the warm restart re-adopts the same base")
	require.True(t, second.AlreadyAdopted, "re-adoption, not a new adoption")
	require.True(t, second.Coalesced, "no physical build")
	require.False(t, second.Advanced, "an unchanged tree is not an advancement")
	require.NoError(t, audit(), "a warm restart over an unchanged tree wrote the catalog")

	require.Len(t, dedicatedGenerations(t, f, f.familyOf(prefix).GraphID), 1,
		"the restart allocated no second generation")
}

// TestInitialBasePublisherAdvancesACommittedTreeThatMovedWhileDown covers the
// other half of "cold AND warm startup": the daemon was not running when the
// repository was committed to, so no watcher saw the HEAD change. Startup is
// the only trigger for that window, and it must advance rather than refuse.
func TestInitialBasePublisherAdvancesACommittedTreeThatMovedWhileDown(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	root := f.gitRepoWithDependent("moved")
	registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
	prefix := registered.Prefix
	first := startupPublisher(t, f).PublishRepo(ctx, prefix)
	require.NoError(t, first.Err)
	require.Positive(t, first.GenerationID)

	// Commit while "the daemon is down".
	writeFile(t, filepath.Join(root, "moved_while_down.go"), "package a\n\nfunc WhileDown() {}\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-q", "-m", "committed while the daemon was down")
	// The restart re-reads HEAD into the checkout row; without it the
	// observation would still name the old committed tree.
	warmRestart(t, f, root)

	second := startupPublisher(t, f).PublishRepo(ctx, prefix)
	require.NoError(t, second.Err)
	require.Positive(t, second.GenerationID)
	require.NotEqual(t, first.GenerationID, second.GenerationID, "the moved tree published a new base")
	require.False(t, second.AlreadyAdopted)
	require.True(t, second.Advanced)

	graph := f.familyOf(prefix)
	require.Equal(t, second.GenerationID, graph.ActiveGenerationID)
	row, found, err := f.catalog.GetViewGeneration(ctx, second.GenerationID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, f.checkoutOf(prefix).HeadTree, row.TreeOID)
	// The old route is intact: publication never retires what it replaced.
	old, found, err := f.catalog.GetViewGeneration(ctx, first.GenerationID)
	require.NoError(t, err)
	require.True(t, found)
	require.Contains(t,
		[]store_sqlite.ViewGenerationState{store_sqlite.ViewGenerationReady, store_sqlite.ViewGenerationSuperseded},
		old.State, "the replaced base is retained, not retired")
}

// TestInitialBasePublisherRecoversAnInterruptedPublication drives the failing
// physical leader the acceptance criteria name. A publication whose build
// cannot open its committed tree leaves an attempt behind; the next start must
// recover it and publish, not inherit a permanently wedged claim.
func TestInitialBasePublisherRecoversAnInterruptedPublication(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	root := f.gitRepoWithDependent("interrupted")
	registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
	prefix := registered.Prefix
	checkout := f.checkoutOf(prefix)

	// Move the working copy out from under the builder: the reservation is
	// allocated and the physical build then fails to open the tree. This is
	// the same shape as a process that dies mid-build.
	broken := checkout
	broken.RootPath = filepath.Join(f.dir, "vanished-root")
	require.NoError(t, f.catalog.UpsertCheckout(ctx, broken))

	failed := startupPublisher(t, f).PublishRepo(ctx, prefix)
	require.Error(t, failed.Err, "a build with no tree to read must fail")
	require.Zero(t, f.familyOf(prefix).ActiveGenerationID, "a failed publication adopts nothing")

	// Next start: the root is back.
	require.NoError(t, f.catalog.UpsertCheckout(ctx, checkout))
	warmRestart(t, f, root)

	recovered := startupPublisher(t, f).PublishRepo(ctx, prefix)
	require.NoError(t, recovered.Err, "the next start recovers the interrupted attempt")
	require.Positive(t, recovered.GenerationID)
	require.Equal(t, recovered.GenerationID, f.familyOf(prefix).ActiveGenerationID)
	row, found, err := f.catalog.GetViewGeneration(ctx, recovered.GenerationID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store_sqlite.ViewGenerationReady, row.State)
}

// TestInitialBasePublisherScheduleDoesNotBlockAndShutdownCancelsIt pins the
// two bounding properties: Schedule returns without waiting for the build, and
// closing publisher admission — the FIRST thing CheckoutLifecycle.Close does —
// cancels the publication so the drain it joins LAST cannot be unbounded.
func TestInitialBasePublisherScheduleDoesNotBlockAndShutdownCancelsIt(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	runtime := installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	root := f.gitRepoWithDependent("scheduled")
	registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)

	publisher := startupPublisher(t, f)
	done := make(chan struct{})
	go func() {
		defer close(done)
		publisher.Schedule(registered.Prefix)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Schedule blocked on the publication it queued")
	}
	// Scheduled, and deliberately not yet attempted: nothing drains before
	// BeginDraining, which the daemon calls after the readiness flip.
	require.Equal(t, 1, publisher.Pending())
	require.Empty(t, publisher.Outcomes())
	publisher.BeginDraining()
	require.NoError(t, publisher.Wait(ctx))
	outcomes := publisher.Outcomes()
	require.Len(t, outcomes, 1)
	require.NoError(t, outcomes[0].Err)
	require.Positive(t, outcomes[0].GenerationID)

	// Closing admission is what shutdown does first; it must cancel the
	// publisher's own context so an in-flight build unwinds.
	drained := runtime.CloseDedicatedBaseAdmission()
	select {
	case <-publisher.ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("closing publisher admission did not cancel the publication driver")
	}
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("publisher admission never drained")
	}
	// A schedule after the close is a no-op rather than a queued publication.
	publisher.Schedule("anything-else")
	require.Len(t, publisher.Outcomes(), 1)
	require.Zero(t, publisher.Pending())
}

// TestInitialBasePublisherScheduleNeverBlocksOnQueueDepth pins the enqueue side
// against the repository count.
//
// Schedule is called from the warmup worker pool, upstream of markReady. A
// bounded queue drained by one serial worker — each item a whole-repository
// index — would park that worker once the bound was reached, and the readiness
// flip behind it, for as long as the excess publications take. The pending list
// is therefore unbounded and nothing drains it until BeginDraining, so this
// schedules an order of magnitude more repositories than any plausible bound
// with NO drain running at all: every call must still return.
func TestInitialBasePublisherScheduleNeverBlocksOnQueueDepth(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	publisher := startupPublisher(t, f)

	const repos = 1024
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < repos; i++ {
			publisher.Schedule(fmt.Sprintf("queue-depth-%04d", i))
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("Schedule parked before %d repositories were queued; the readiness path is "+
			"coupled to the publication queue's capacity", repos)
	}
	require.Equal(t, repos, publisher.Pending())
	// Nothing ran: the queue is held until the daemon flips ready.
	require.Empty(t, publisher.Outcomes())
}

// TestALiveAdvanceDoesNotReleaseTheStartupQueue pins the one rule that still
// preserves "ready, then publish" now that a live advance may start the worker.
//
// W4.2's ordering was a property of the worker never running before
// BeginDraining. W4.3 changed that: enqueueAdvance starts the worker itself,
// because a HEAD change on a daemon whose warmup never reached BeginDraining
// would otherwise never be published. What keeps the ordering is popLocked's
// `if !req.live && !p.drainReleased { continue }` — and W5/W4.3-verify (minor 2)
// showed that deleting it left the whole suite green, including the test the
// implementer named as its guard.
//
// Both halves are pinned here: the dequeue rule itself, and the ordering it
// exists for, driven through the real worker.
func TestALiveAdvanceDoesNotReleaseTheStartupQueue(t *testing.T) {
	t.Run("the dequeue refuses a startup request before BeginDraining", func(t *testing.T) {
		f := newLifecycleFixture(t)
		defer f.close()
		installStartupPublisherRuntime(t, f)
		publisher := startupPublisher(t, f)
		// Consume the worker Once so nothing drains under the assertions: this
		// is a statement about popLocked, not about scheduling.
		publisher.worker.Do(func() {})

		publisher.Schedule("startup-repo")
		publisher.mu.Lock()
		_, admitted := publisher.popLocked()
		publisher.mu.Unlock()
		require.False(t, admitted,
			"a startup publication was admitted before BeginDraining released the queue; "+
				"a full committed-tree index per repository can now precede the readiness flip")

		// A live request in the same queue is admitted regardless: the watchers
		// that produce one come up after the flip, so there is no ordering left
		// for the flag to protect.
		publisher.enqueueAdvance(basePublishRequest{prefix: "live-repo"})
		publisher.mu.Lock()
		live, admitted := publisher.popLocked()
		publisher.mu.Unlock()
		require.True(t, admitted, "a live advance was held behind the startup queue")
		require.Equal(t, "live-repo", live.prefix)
		require.True(t, live.live)

		publisher.BeginDraining()
		publisher.mu.Lock()
		startup, admitted := publisher.popLocked()
		publisher.mu.Unlock()
		require.True(t, admitted, "BeginDraining did not release the startup queue")
		require.Equal(t, "startup-repo", startup.prefix)
		require.False(t, startup.live)
	})

	t.Run("a live advance running does not drag the startup queue out with it", func(t *testing.T) {
		f := newLifecycleFixture(t)
		defer f.close()
		installStartupPublisherRuntime(t, f)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		root := f.gitRepoWithDependent("ordering")
		registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
		require.NoError(t, err)
		graphID := GraphIDFor(registered.Prefix)

		publisher := startupPublisher(t, f)
		publisher.Schedule(registered.Prefix)
		require.Equal(t, 1, publisher.Pending())

		// A live advance for a DIFFERENT prefix. It starts the worker, which is
		// the whole hazard: the worker is now running while the daemon has not
		// flipped ready.
		require.True(t, publisher.enqueueAdvance(basePublishRequest{prefix: "no-such-repository"}))
		require.Eventually(t, func() bool {
			for _, outcome := range publisher.Outcomes() {
				if outcome.RepoPrefix == "no-such-repository" {
					return true
				}
			}
			return false
		}, 30*time.Second, 5*time.Millisecond, "the live advance never ran")

		// The startup publication must still be waiting.
		require.Never(t, func() bool { return publisher.Pending() == 0 },
			2*time.Second, 25*time.Millisecond,
			"a scheduled startup publication ran while the daemon had not released the queue; "+
				"a whole-repository committed index is now in front of the readiness flip")
		require.Empty(t, dedicatedGenerations(t, f, graphID),
			"a committed base was published before BeginDraining")

		publisher.BeginDraining()
		require.NoError(t, publisher.Wait(ctx))
		require.Len(t, dedicatedGenerations(t, f, graphID), 1,
			"BeginDraining did not publish the queued startup base")
	})
}

// TestAStartupScheduleDoesNotDowngradeAQueuedLiveAdvance pins the replacement
// rule's direction.
//
// enqueueLocked replaces a queued request in place so a burst of HEAD changes
// coalesces to its newest target. Before this item it replaced in BOTH
// directions, so a Schedule arriving for a prefix whose LIVE advance was already
// queued silently downgraded it: the git-resolved commit was lost (falling back
// to the checkout row's head_tree, which still names the PREVIOUS tree at that
// instant), the live flag was lost (so the advance waited for a drain release it
// should ignore), and the trigger's completion memo was dropped with the
// callback, so the next observation of the same commit paid for a fresh cohort
// description again. Today's daemon orders the two calls so it cannot happen;
// the ordering is an external invariant, not a local one.
func TestAStartupScheduleDoesNotDowngradeAQueuedLiveAdvance(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	publisher := startupPublisher(t, f)
	publisher.worker.Do(func() {}) // nothing drains while the queue is inspected

	const prefix = "downgraded"
	memo := make(chan InitialBasePublication, 1)
	publisher.enqueueAdvance(basePublishRequest{
		prefix: prefix,
		root:   "/observed/root",
		target: dedicatedBaseTarget{CommitOID: "0123456789abcdef0123456789abcdef01234567"},
		done:   func(outcome InitialBasePublication) { memo <- outcome },
	})

	publisher.Schedule(prefix)

	publisher.mu.Lock()
	queued := publisher.queued
	order := append([]string(nil), publisher.pendingOrder...)
	request := publisher.pendingReq[prefix]
	publisher.mu.Unlock()

	require.Equal(t, []string{prefix}, order)
	require.Equal(t, 1, queued, "the startup schedule counted a second queued publication")
	require.True(t, request.live,
		"a startup schedule downgraded a queued live advance; it now waits for a drain release "+
			"and republishes the checkout row's stale head_tree")
	require.Equal(t, "0123456789abcdef0123456789abcdef01234567", request.target.CommitOID,
		"the startup schedule discarded the git-resolved target of the queued advance")
	require.Equal(t, "/observed/root", request.root,
		"the startup schedule discarded the observed working copy of the queued advance")
	require.NotNil(t, request.done,
		"the startup schedule dropped the trigger's completion memo, so the next observation "+
			"of the same commit pays for a fresh cohort description")

	// The other direction is unchanged: a NEWER live target still replaces.
	publisher.enqueueAdvance(basePublishRequest{
		prefix: prefix,
		root:   "/observed/root",
		target: dedicatedBaseTarget{CommitOID: "89abcdef0123456789abcdef0123456789abcdef"},
	})
	publisher.mu.Lock()
	newest := publisher.pendingReq[prefix]
	queued = publisher.queued
	publisher.mu.Unlock()
	require.Equal(t, 1, queued)
	require.Equal(t, "89abcdef0123456789abcdef0123456789abcdef", newest.target.CommitOID,
		"a newer live advance stopped coalescing onto the queued slot")
}

// TestInitialBasePublisherWaitAccountsForWorkThatWillNeverRun states Wait's
// contract explicitly, because it is the join both cmd wiring tests use.
//
// Wait settles on attempted >= queued. Two ways that count can never be
// reached must report rather than hang: a queue nobody released, and a driver
// the runtime cancelled while items were still pending.
func TestInitialBasePublisherWaitAccountsForWorkThatWillNeverRun(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	publisher := startupPublisher(t, f)

	publisher.Schedule("never-drained")
	require.Equal(t, 1, publisher.Pending())
	// No BeginDraining: Wait must report the caller's own deadline, not claim
	// the queue settled and not hang past it.
	bounded, cancelBounded := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancelBounded()
	require.ErrorIs(t, publisher.Wait(bounded), context.DeadlineExceeded)
	require.Equal(t, 1, publisher.Pending())

	// Cancelling the driver abandons the pending tail; Wait reports the
	// cancellation rather than reporting completion for work nothing ran.
	publisher.Close()
	require.ErrorIs(t, publisher.Wait(context.Background()), context.Canceled)
	// And a schedule after the cancellation adds nothing to account for.
	publisher.Schedule("after-close")
	require.Equal(t, 1, publisher.Pending())

	// PublishRepo is outside the accounting entirely: it hands the outcome
	// back to its caller instead of recording it.
	fresh := startupPublisher(t, f)
	require.Equal(t, "no ready dedicated graph", fresh.PublishRepo(context.Background(), "never-tracked").Skipped)
	require.Zero(t, fresh.Pending())
	require.Empty(t, fresh.Outcomes())
	require.NoError(t, fresh.Wait(context.Background()))
}

// TestInitialBasePublisherSkipsRepositoriesWithNothingToPublish states the
// negative cases as skips rather than failures: an untracked prefix and an
// owner with no committed tree are ordinary startup states, not errors.
func TestInitialBasePublisherSkipsRepositoriesWithNothingToPublish(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	publisher := startupPublisher(t, f)

	unknown := publisher.PublishRepo(ctx, "never-tracked")
	require.NoError(t, unknown.Err)
	require.Equal(t, "no ready dedicated graph", unknown.Skipped)
	require.Zero(t, unknown.GenerationID)

	root := f.gitRepo("headless")
	registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
	checkout := f.checkoutOf(registered.Prefix)
	checkout.HeadTree = ""
	require.NoError(t, f.catalog.UpsertCheckout(ctx, checkout))

	headless := publisher.PublishRepo(ctx, registered.Prefix)
	require.NoError(t, headless.Err)
	require.Equal(t, "owner has no committed tree", headless.Skipped)
	require.Zero(t, f.familyOf(registered.Prefix).ActiveGenerationID)
}

// TestInitialBasePublisherPublishesNothingForAnOwnerOnlyFamily is the
// consumer gate's acceptance case, and the fix for the measured cold-index
// regression.
//
// A committed base is a full index of a committed tree, and NOTHING in an
// owner-only family can read one: the owning repository's own request route
// stays on legacy generation 0, so the base exists solely for a dependent
// checkout or a ref view to key a layer on. Publishing it anyway cost a second
// whole-repository index — +847 MB of logical writes and a store of 61 -> 122
// MB on the 1,500-file cold-index phase — for a reader set of size zero.
//
// The three assertions are the three things a skip has to be: no generation
// allocated, no active pointer moved, and NO CATALOG DML AT ALL. The last one
// is why the gate sits before the authority block rather than inside it —
// AcquireDedicatedBaseAuthority is the first write on this path.
//
// Revert-red: delete the "no dependent checkout" skip from
// InitialBasePublisher.publish and every assertion here fails.
func TestInitialBasePublisherPublishesNothingForAnOwnerOnlyFamily(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// gitRepo, NOT gitRepoWithDependent: one checkout, no worktree, no ref view.
	root := f.gitRepo("owner-only")
	registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
	require.NotEmpty(t, registered.Prefix)
	checkouts, err := f.catalog.ListCheckouts(ctx, registered.FamilyID)
	require.NoError(t, err)
	require.Len(t, checkouts, 1, "the fixture is only owner-only if the family holds one checkout")

	audit, err := installDedicatedWriteAudit(ctx, f.dbPath)
	require.NoError(t, err)

	out := startupPublisher(t, f).PublishRepo(ctx, registered.Prefix)
	require.NoError(t, out.Err)
	require.Equal(t, "no dependent checkout", out.Skipped)
	require.Zero(t, out.GenerationID)
	require.NoError(t, audit(), "a declined publication wrote the catalog")

	graph := f.familyOf(registered.Prefix)
	require.Zero(t, graph.ActiveGenerationID, "nothing was adopted")
	require.Empty(t, dedicatedGenerations(t, f, graph.GraphID),
		"a family with no reader allocated a committed generation")
}

// TestARefViewIsAConsumerOfTheCommittedBase states the other half of the
// census. A named view resolves its lower snapshot through the same graphBase
// a dependent checkout does (RefViewManager.base), so a graph with a ref view
// has a reader even when the family holds nothing but its owner.
//
// Revert-red: drop the ListRefViews arm from dedicatedBaseConsumers and the
// publication is declined.
func TestARefViewIsAConsumerOfTheCommittedBase(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	root := f.gitRepo("ref-view-only")
	registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
	publisher := startupPublisher(t, f)
	require.Equal(t, "no dependent checkout", publisher.PublishRepo(ctx, registered.Prefix).Skipped,
		"the premise: without the view this family publishes nothing")

	graphID := GraphIDFor(registered.Prefix)
	_, err = f.catalog.GetOrCreateRefView(ctx, store_sqlite.RefView{
		RefViewID:         "ref-view-consumer",
		GraphID:           graphID,
		SelectorKind:      "git_ref",
		SelectorValue:     "refs/heads/main",
		EnrichmentProfile: "default",
		State:             store_sqlite.RefViewPending,
		ExactView:         true,
	})
	require.NoError(t, err)

	out := publisher.PublishRepo(ctx, registered.Prefix)
	require.NoError(t, out.Err)
	require.Empty(t, out.Skipped, "a ref view is a reader of the committed base")
	require.Positive(t, out.GenerationID)
	require.Equal(t, out.GenerationID, f.familyOf(registered.Prefix).ActiveGenerationID)
}

// TestTheFirstDependentGetsTheCommittedBaseThePublisherDeferred is the
// deferral's other end: the gate is defer, not drop.
//
// The sequence is the production one. A daemon start declines an owner-only
// family; a worktree appears; the family census now holds a dependent, and the
// consumer's own request (RequestBase — what
// CheckoutCoordinator.demandCommittedBase calls) publishes the base the start
// did not. The demand carries no target of its own, so what it publishes is
// the tree the owner is at NOW, not the one the declined attempt saw.
//
// Revert-red: remove RequestBase's bypass of the once-per-daemon `scheduled`
// memo (make it call Schedule) and the second publication never happens,
// because the startup Schedule already filed this prefix.
func TestTheFirstDependentGetsTheCommittedBaseThePublisherDeferred(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	root := f.gitRepo("deferred")
	registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)

	publisher := startupPublisher(t, f)
	publisher.Schedule(registered.Prefix)
	publisher.BeginDraining()
	require.NoError(t, publisher.Wait(ctx))
	outcomes := publisher.Outcomes()
	require.Len(t, outcomes, 1)
	require.Equal(t, "no dependent checkout", outcomes[0].Skipped)

	// The owner moves on while no base exists, so the tree the declined
	// attempt would have published is not the tree the demand must publish.
	writeFile(t, filepath.Join(root, "after.go"), "package a\n\nfunc After() {}\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-q", "-m", "after the declined publication")

	allocateDependentCheckout(t, f, registered.FamilyID, "deferred-dependent")

	require.True(t, publisher.RequestBase(registered.Prefix),
		"the publisher refused a consumer's demand")
	require.NoError(t, publisher.Wait(ctx))
	outcomes = publisher.Outcomes()
	require.Len(t, outcomes, 2, "the demand was swallowed by the startup schedule memo")
	demanded := outcomes[1]
	require.NoError(t, demanded.Err)
	require.Empty(t, demanded.Skipped)
	require.True(t, demanded.Demanded, "the outcome does not say a consumer asked for it")
	require.Positive(t, demanded.GenerationID)

	checkout := f.checkoutOf(registered.Prefix)
	require.Equal(t, checkout.HeadTree, demanded.TreeOID,
		"the demand published a stale tree instead of the owner's current one")
	require.Equal(t, demanded.GenerationID, f.familyOf(registered.Prefix).ActiveGenerationID)
}

// TestRequestBaseCoalescesOntoAQueuedRequest pins the throttle the coordinator
// side relies on: a consumer polling every fifteen seconds must cost one queue
// lookup, not one publication per poll.
func TestRequestBaseCoalescesOntoAQueuedRequest(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	root := f.gitRepoWithDependent("coalesced")
	registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)

	publisher := startupPublisher(t, f)
	// Nothing is draining yet, so every request stays queued and the
	// coalescing is observable as a queue depth rather than as an outcome.
	for i := 0; i < 5; i++ {
		require.True(t, publisher.RequestBase(registered.Prefix))
	}
	require.Equal(t, 1, publisher.Pending(), "five demands queued more than one publication")

	publisher.BeginDraining()
	require.NoError(t, publisher.Wait(ctx))
	outcomes := publisher.Outcomes()
	require.Len(t, outcomes, 1)
	require.True(t, outcomes[0].Demanded)
	require.Empty(t, outcomes[0].Skipped)
	require.Positive(t, outcomes[0].GenerationID)
	require.Len(t, dedicatedGenerations(t, f, GraphIDFor(registered.Prefix)), 1)
}

// TestInitialDedicatedBaseAuthorityTokenIsStableAndOwnerScoped pins the token
// derivation the zero-write warm restart depends on.
func TestInitialDedicatedBaseAuthorityTokenIsStableAndOwnerScoped(t *testing.T) {
	owner := store_sqlite.DedicatedBaseOwner{CheckoutID: "checkout-a", Incarnation: "incarnation-1"}
	token := initialDedicatedBaseAuthorityToken("graph-a", owner)
	require.NotEmpty(t, token)
	require.Equal(t, token, initialDedicatedBaseAuthorityToken("graph-a", owner),
		"a restart must claim the same token or the authority epoch rotates")

	retracked := owner
	retracked.Incarnation = "incarnation-2"
	require.NotEqual(t, token, initialDedicatedBaseAuthorityToken("graph-a", retracked),
		"a retracked checkout is a different owner and must rotate")
	require.NotEqual(t, token, initialDedicatedBaseAuthorityToken("graph-b", owner))
}

// TestDedicatedBaseRuntimeTypedNilHandleDegradesInsteadOfPanicking closes the
// typed-nil installation gap from the shutdown side. A caller that installs
// (*DedicatedBaseRuntime)(nil) passes SetDedicatedBaseCleanupRuntime's
// interface-nil guard; promoting a method through the nil embedded pointer
// would then panic inside CheckoutLifecycle.Close.
func TestDedicatedBaseRuntimeTypedNilHandleDegradesInsteadOfPanicking(t *testing.T) {
	var handle *DedicatedBaseRuntime
	require.Nil(t, handle.ViewLeases())
	owner := store_sqlite.DedicatedBaseOwner{CheckoutID: "c", Incarnation: "i"}
	require.ErrorIs(t, handle.RegisterDedicatedBaseOwner("g", owner), errDedicatedBaseRuntimeInput)
	_, err := handle.CloseDedicatedBaseOwner("g", owner)
	require.ErrorIs(t, err, errDedicatedBaseRuntimeInput)
	require.ErrorIs(t, handle.FinalizeDedicatedBaseOwner("g", owner, make(chan struct{})), errDedicatedBaseRuntimeInput)
	select {
	case <-handle.CloseDedicatedBaseAdmission():
	default:
		t.Fatal("a typed-nil handle must report an already-drained admission")
	}

	empty := &DedicatedBaseRuntime{}
	require.Nil(t, empty.ViewLeases())
	require.ErrorIs(t, empty.RegisterDedicatedBaseOwner("g", owner), errDedicatedBaseRuntimeInput)
	select {
	case <-empty.CloseDedicatedBaseAdmission():
	default:
		t.Fatal("a handle with no runtime must report an already-drained admission")
	}

	// And the lifecycle's shutdown path survives one being installed.
	f := newLifecycleFixture(t)
	require.NoError(t, f.lc.SetDedicatedBaseCleanupRuntime((*DedicatedBaseRuntime)(nil)))
	require.NoError(t, f.lc.Close())
	require.NoError(t, f.mi.Close(context.Background()))
	require.NoError(t, f.store.Close())
}

// TestInitialBasePublisherPublicationContextIsCancelledByAdmissionClose is the
// focused unit behind the shutdown bound above.
func TestInitialBasePublisherPublicationContextIsCancelledByAdmissionClose(t *testing.T) {
	dir := t.TempDir()
	store, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	runtime, err := NewDedicatedBaseRuntime(store, graphview.NewLeaseManager())
	require.NoError(t, err)

	ctx, cancel, release := runtime.publicationContext(context.Background())
	defer cancel()
	require.NoError(t, ctx.Err())
	runtime.CloseDedicatedBaseAdmission()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	release()

	// A context requested after the close starts cancelled: nothing may be
	// admitted once shutdown has begun.
	after, cancelAfter, releaseAfter := runtime.publicationContext(context.Background())
	defer cancelAfter()
	defer releaseAfter()
	require.ErrorIs(t, after.Err(), context.Canceled)
}

// TestInitialBasePublisherObservationCarriesTheFrozenIdentity reads the
// observation the publisher would publish under, without publishing, so a
// drift in any one identity input is attributable.
func TestInitialBasePublisherObservationCarriesTheFrozenIdentity(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	root := f.gitRepoWithDependent("frozen")
	registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
	publisher := startupPublisher(t, f)

	observation, err := publisher.observe(ctx, GraphIDFor(registered.Prefix), registered.Prefix, dedicatedBaseTarget{})
	require.NoError(t, err)
	checkout := f.checkoutOf(registered.Prefix)
	require.Equal(t, checkout.HeadTree, observation.Identity.TreeOID)
	require.Equal(t, checkout.HeadCommit, observation.ProvenanceCommitOID)
	require.Equal(t, checkout.RootPath, observation.RootPath)
	require.Equal(t, extractorVersionsFingerprint(), observation.Identity.ExtractorVersions)
	require.Equal(t, resolverVersionFingerprint(), observation.Identity.ResolverVersion)
	require.NotEmpty(t, observation.Identity.ConfigHash)
	require.NotEmpty(t, observation.Identity.DependencyRevision)
	require.Zero(t, observation.ExpectedActiveGenerationID, "nothing is published yet")
	require.Same(t, f.store, observation.Builder.Store)
	require.NotNil(t, observation.Builder.Registry)
	require.NotNil(t, observation.Builder.Logger)

	// The configuration digest is the widened one — the same value a
	// coordinator derives — not the bare index digest.
	repoCfg := f.cm.GetRepoConfig(registered.Prefix)
	idx := f.mi.GetIndexer(registered.Prefix)
	require.NotNil(t, idx)
	_, fingerprint, err := snapshotDedicatedBaseConfig(repoCfg.Index, registered.Prefix, idx.WorkspaceID(), idx.ProjectID())
	require.NoError(t, err)
	require.Equal(t, checkoutConfigHash(fingerprint, dedicatedBaseConfigSections(repoCfg)), observation.Identity.ConfigHash)
	require.NotEqual(t, fingerprint, observation.Identity.ConfigHash)
}

func TestInitialBasePublisherRefusesAMissingLifecycle(t *testing.T) {
	publisher, err := NewInitialBasePublisher(nil)
	require.Nil(t, publisher)
	require.ErrorIs(t, err, errInitialBasePublisherInput)
	var nilPublisher *InitialBasePublisher
	nilPublisher.Schedule("x")
	nilPublisher.Close()
	require.Nil(t, nilPublisher.Outcomes())
	require.NoError(t, nilPublisher.Wait(context.Background()))
	require.Equal(t, "no publisher", nilPublisher.PublishRepo(context.Background(), "x").Skipped)
}
