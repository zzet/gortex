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
		root := f.gitRepo(name)
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

	root := f.gitRepo("warm")
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

	root := f.gitRepo("moved")
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

	root := f.gitRepo("interrupted")
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

	root := f.gitRepo("scheduled")
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

	root := f.gitRepo("frozen")
	registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
	publisher := startupPublisher(t, f)

	observation, err := publisher.observe(ctx, GraphIDFor(registered.Prefix), registered.Prefix)
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
