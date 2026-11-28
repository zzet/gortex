package indexer

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/search"
)

// These regressions pin the correctness edges of per-family startup
// admission. Each fails on the pre-fix behavior
// and passes with the fix; they exercise the gate and the lifecycle observer
// directly, with channels and barriers rather than elapsed-time sleeps.

// A parked INELIGIBLE interactive waiter (a family-less ref build behind the
// closed latch) must not yield a ready family's background build. Before the
// fix requestYieldLocked fired on ANY interactive waiter and the always-armed
// background cycle (yields=0 on every retry) churned until Open, so a single
// cold ref selection could keep a ready worktree from publishing. The yield
// path now requires an eligible interactive waiter.
func TestViewBuildGateParkedIneligibleInteractiveDoesNotYieldReadyBackground(t *testing.T) {
	ctx := context.Background()
	gate := NewViewBuildGate()
	gate.MarkFamilyReady("fam-a", gate.FamilyReadinessEpoch("fam-a"))

	release, err := gate.AcquireForFamily(ctx, ViewBuildBackground, "fam-a")
	require.NoError(t, err)
	yield, withdraw := gate.NoteYieldable(0)
	defer withdraw()
	require.NotNil(t, yield, "a background holder with yields below the cap arms a yield")

	parkedCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	parkedDone := make(chan error, 1)
	go func() {
		rel, err := gate.Acquire(parkedCtx, ViewBuildInteractive)
		if err == nil {
			rel()
		}
		parkedDone <- err
	}()
	require.Eventually(t, func() bool { return gate.Stats().InteractiveQueued == 1 },
		2*time.Second, time.Millisecond, "the family-less ref build parks in the interactive queue")

	// The parked waiter is ineligible while the latch is closed: it must not
	// have asked the ready background holder to give the lane up.
	select {
	case <-yield:
		t.Fatal("a parked ineligible interactive waiter yielded a ready family's background build")
	default:
	}
	require.Equal(t, uint64(0), gate.Stats().YieldRequests)

	// The ready holder finishes undisturbed; the parked waiter still cannot run.
	release()
	stats := gate.Stats()
	require.False(t, stats.Active, "the lane is idle after the ready holder releases")
	require.Equal(t, 1, stats.InteractiveQueued, "the ineligible waiter keeps its queue place while closed")

	// Opening the latch admits the parked waiter.
	gate.Open()
	require.NoError(t, <-parkedDone)
}

// An eligible arrival takes an idle lane directly even while the latch is
// closed and ineligible waiters fill the queue. Before the fix the immediate
// path required g.open, so a ready family was rejected at the queue limit (or
// at zero capacity) despite an idle lane. Queue limits now apply only to
// callers that must actually wait.
func TestViewBuildGateAdmitsEligibleArrivalOnIdleLaneWhileClosed(t *testing.T) {
	ctx := context.Background()
	t.Run("zero capacity", func(t *testing.T) {
		gate := newViewBuildGateWithLimits(0, 0)
		gate.MarkFamilyReady("fam-a", gate.FamilyReadinessEpoch("fam-a"))
		release, err := gate.AcquireForFamily(ctx, ViewBuildInteractive, "fam-a")
		require.NoError(t, err, "a ready family takes an idle lane even at zero capacity while closed")
		release()
		// A family-less caller while closed is ineligible and still rejected.
		_, err = gate.Acquire(ctx, ViewBuildInteractive)
		require.Error(t, err, "a family-less caller while closed must not take the idle lane")
	})
	t.Run("parked ineligible fill the queue", func(t *testing.T) {
		gate := newViewBuildGateWithLimits(1, 1)
		gate.MarkFamilyReady("fam-a", gate.FamilyReadinessEpoch("fam-a"))
		parkedCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		parkedDone := make(chan error, 1)
		go func() {
			rel, err := gate.Acquire(parkedCtx, ViewBuildInteractive)
			if err == nil {
				rel()
			}
			parkedDone <- err
		}()
		require.Eventually(t, func() bool { return gate.Stats().InteractiveQueued == 1 },
			2*time.Second, time.Millisecond, "a parked ineligible waiter fills the queue")
		// The queue is full of an ineligible waiter, but the lane is idle and
		// this family is ready: admit it rather than reject it.
		release, err := gate.AcquireForFamily(ctx, ViewBuildInteractive, "fam-a")
		require.NoError(t, err, "parked ineligible waiters must not reject a ready family on an idle lane")
		release()
		cancel()
		require.Error(t, <-parkedDone, "the parked waiter is canceled while still ineligible")
	})
}

// Revoking a family's readiness re-arms its wake, so a coordinator created
// after a primary switch waits for the NEW primary instead of inheriting the
// old one's readiness. SetPrimary calls this after the catalog switch (the
// dependent coordinators are already dropped), preventing an early
// generation-zero rehome onto an unpublished replacement.
func TestViewBuildGateResetFamilyReadinessRearmsWake(t *testing.T) {
	gate := NewViewBuildGate()
	gate.MarkFamilyReady("fam-a", gate.FamilyReadinessEpoch("fam-a"))
	require.True(t, gate.AdmittedForFamily("fam-a"))

	gate.ResetFamilyReadiness("fam-a")
	require.False(t, gate.AdmittedForFamily("fam-a"), "reset revoked the family's readiness")
	ch := gate.FamilyOpened("fam-a")
	select {
	case <-ch:
		t.Fatal("the re-armed wake fired before the family was ready again")
	default:
	}

	gate.MarkFamilyReady("fam-a", gate.FamilyReadinessEpoch("fam-a"))
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("the re-armed wake did not fire when the family became ready again")
	}

	// Open still admits every family regardless of a prior reset.
	gate.ResetFamilyReadiness("fam-a")
	gate.Open()
	require.True(t, gate.AdmittedForFamily("fam-a"))
}

// Adopting a SECONDARY graph must not mark the family ready while its
// primary base is unpublished; only the primary's own adoption admits the
// family. Before the fix the adoption callback marked the whole family ready on
// any advance, so a worktree could compose over the primary's generation-zero
// fallback and serve another checkout's uncommitted content.
func TestStartupAdmissionSecondaryAdoptionDoesNotAdmitFamily(t *testing.T) {
	f := newStartupAdmissionFixture(t)
	ctx := context.Background()
	gate := NewViewBuildGate()
	mi := NewMultiIndexer(f.store, builderRegistry(), search.NewNull(), nil, zap.NewNop())
	lc, err := NewCheckoutLifecycle(CheckoutLifecycleConfig{MultiIndexer: mi, Graph: f.store, Logger: zap.NewNop()})
	require.NoError(t, err)
	lc.SetBuildGate(gate)
	t.Cleanup(func() { _ = lc.Close(); _ = mi.Close(ctx) })

	// Model a secondary graph publishing before the primary by demoting the
	// family's graph to a non-primary base. An adoption of a non-primary graph
	// must NOT admit the family: its primary base is still unpublished, and a
	// worktree composing over it would serve generation-zero cross-checkout
	// content. (dedicated_graphs enforces unique owner and prefix, so demote the
	// existing row rather than insert a second one.)
	advance := store_sqlite.DedicatedBaseAdoption{GenerationID: 1, PreviousGenerationID: 0, TreeOID: f.a.treeA}
	owner := store_sqlite.DedicatedBaseOwner{CheckoutID: f.a.primaryID}
	graphRow := store_sqlite.DedicatedGraph{
		GraphID: f.a.graphID, OwnerCheckoutID: f.a.primaryID, RepoPrefix: f.a.prefix,
		FamilyID: f.a.familyID, State: reconcile.GraphStateReady,
	}
	graphRow.IsPrimaryBase = false
	require.NoError(t, f.catalog.UpsertDedicatedGraph(ctx, graphRow))
	lc.dedicatedBaseAdopted(store_sqlite.DedicatedBaseAdoptionEvent{
		GraphID: f.a.graphID, FamilyID: f.a.familyID, RepoPrefix: f.a.prefix, Owner: owner, Adoption: advance,
	})
	require.False(t, gate.AdmittedForFamily(f.a.familyID),
		"a non-primary adoption admitted a family whose primary base is unpublished")

	// The primary base's own adoption admits the family (the legitimate wake).
	graphRow.IsPrimaryBase = true
	require.NoError(t, f.catalog.UpsertDedicatedGraph(ctx, graphRow))
	lc.dedicatedBaseAdopted(store_sqlite.DedicatedBaseAdoptionEvent{
		GraphID: f.a.graphID, FamilyID: f.a.familyID, RepoPrefix: f.a.prefix, Owner: owner, Adoption: advance,
	})
	require.True(t, gate.AdmittedForFamily(f.a.familyID),
		"the primary base adoption did not admit the family")
}

// The catalog seed (familyBasePublished) must ignore a published NON-primary
// graph. This is the path the prepared/restart case uses, so a secondary that
// published while the primary is unpublished must not seed readiness — otherwise
// a worktree composes over the primary's generation-zero fallback. Pins the
// IsPrimaryBase filter in familyBasePublished, not just the adoption observer
// that TestStartupAdmissionSecondaryAdoptionDoesNotAdmitFamily covers.
func TestStartupAdmissionCatalogSeedRequiresPrimaryGraph(t *testing.T) {
	f := newStartupAdmissionFixture(t)
	ctx := context.Background()
	gate := NewViewBuildGate()

	// A's primary publishes a real servable base. (No lifecycle observer is wired
	// here, so readiness is driven explicitly below — this test targets the
	// coordinator's catalog seed, not the observer.)
	f.publishRealBase(t, f.a)

	// Demote that graph to a secondary and revoke any readiness: the family's
	// only published graph is now non-primary, its primary unpublished.
	graphRow := store_sqlite.DedicatedGraph{
		GraphID: f.a.graphID, OwnerCheckoutID: f.a.primaryID, RepoPrefix: f.a.prefix,
		FamilyID: f.a.familyID, State: reconcile.GraphStateReady, IsPrimaryBase: false,
	}
	require.NoError(t, f.catalog.UpsertDedicatedGraph(ctx, graphRow))
	gate.ResetFamilyReadiness(f.a.familyID)
	require.False(t, gate.AdmittedForFamily(f.a.familyID))

	c := f.coordinator(t, f.a, gate)
	require.False(t, c.familyBasePublished(),
		"the catalog seed must ignore a published non-primary graph")
	require.NotNil(t, c.admissionWait(),
		"a family whose only published graph is non-primary must keep waiting")

	// Promote the same published generation back to primary: it now seeds readiness.
	graphRow.IsPrimaryBase = true
	require.NoError(t, f.catalog.UpsertDedicatedGraph(ctx, graphRow))
	require.True(t, c.familyBasePublished(),
		"the primary's published generation must seed readiness")
	require.Nil(t, c.admissionWait(),
		"admissionWait seeds readiness from the primary's published base")
	require.True(t, gate.AdmittedForFamily(f.a.familyID))
}

// SetPrimary must revoke the family's readiness (call ResetFamilyReadiness)
// when the replacement primary has no published base yet, so a rehome defers
// instead of inheriting the old primary's readiness. Observes the reset between
// the catalog swap and the dependent rehome (which blocks on the closed gate),
// then opens the gate to let the move finish.
func TestSetPrimaryRevokesFamilyReadinessUntilNewBasePublishes(t *testing.T) {
	f := newFamilyFixture(t, "setprimary-readiness")
	defer f.close()
	ctx := context.Background()
	gate := NewViewBuildGate()
	f.lc.SetBuildGate(gate)

	// The incumbent primary is published: the family is ready.
	gate.MarkFamilyReady(f.familyID, gate.FamilyReadinessEpoch(f.familyID))
	require.True(t, gate.AdmittedForFamily(f.familyID))

	// A candidate primary with a committed working tree but NO published dedicated
	// base generation, so after the swap the replacement is unpublished.
	candidate := f.worktreeOf(f.main, "setprimary-readiness-alt")
	runGit(t, candidate, "add", "-A")
	runGit(t, candidate, "commit", "-q", "-m", "alt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: candidate}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	preview, err := f.lc.PreviewSetPrimary(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, preview.Ready, "blockers: %v", preview.Blockers)

	done := make(chan struct{})
	go func() {
		_, _ = f.lc.SetPrimary(ctx, tracked.GraphID)
		close(done)
	}()

	// Wait for the catalog swap; ResetFamilyReadiness runs right after it and
	// before the dependent rehome (which now blocks on the closed, unready gate).
	require.Eventually(t, func() bool {
		g, found, _ := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
		return found && g.IsPrimaryBase
	}, 10*time.Second, 5*time.Millisecond, "the primary swap did not land")
	require.False(t, gate.AdmittedForFamily(f.familyID),
		"SetPrimary must revoke readiness until the replacement primary publishes its base")
	require.Eventually(t, func() bool {
		gate.mu.Lock()
		defer gate.mu.Unlock()
		return gate.familyTurnover[f.familyID] == 0
	}, 10*time.Second, 5*time.Millisecond, "SetPrimary left the family fenced after the swap committed")

	// Let the rehome finish so the move and fixture teardown complete.
	gate.Open()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("SetPrimary did not finish after the gate opened")
	}
}

// When a queued interactive waiter becomes eligible (its family is marked
// ready, or the latch opens), the gate must ask a yieldable background holder to
// give the lane up. Pins requestYieldLocked in MarkFamilyReady and Open — the
// positive complement of the parked-ineligible negative test above.
func TestViewBuildGateReadinessWakeYieldsToEligibleInteractive(t *testing.T) {
	arm := func(t *testing.T, gate *ViewBuildGate, interactiveFamily string) (yield <-chan struct{}, release func(), parkedDone chan error) {
		t.Helper()
		ctx := context.Background()
		gate.MarkFamilyReady("fam-bg", gate.FamilyReadinessEpoch("fam-bg"))
		rel, err := gate.AcquireForFamily(ctx, ViewBuildBackground, "fam-bg")
		require.NoError(t, err)
		y, withdraw := gate.NoteYieldable(0)
		t.Cleanup(withdraw)
		require.NotNil(t, y, "a yieldable background holder arms a yield")
		parkedDone = make(chan error, 1)
		go func() {
			r, err := gate.AcquireForFamily(ctx, ViewBuildInteractive, interactiveFamily)
			if err == nil {
				r()
			}
			parkedDone <- err
		}()
		require.Eventually(t, func() bool { return gate.Stats().InteractiveQueued == 1 },
			2*time.Second, time.Millisecond, "the interactive waiter queues behind the background holder")
		select {
		case <-y:
			t.Fatal("an ineligible interactive waiter must not yield the background holder")
		default:
		}
		return y, rel, parkedDone
	}

	t.Run("MarkFamilyReady", func(t *testing.T) {
		gate := NewViewBuildGate()
		yield, release, parkedDone := arm(t, gate, "fam-a")
		gate.MarkFamilyReady("fam-a", gate.FamilyReadinessEpoch("fam-a"))
		select {
		case <-yield:
		case <-time.After(2 * time.Second):
			t.Fatal("MarkFamilyReady did not yield to the now-eligible interactive waiter")
		}
		release()
		require.NoError(t, <-parkedDone)
	})

	t.Run("Open", func(t *testing.T) {
		gate := NewViewBuildGate()
		yield, release, parkedDone := arm(t, gate, "fam-a")
		gate.Open()
		select {
		case <-yield:
		case <-time.After(2 * time.Second):
			t.Fatal("Open did not yield to the now-eligible interactive waiter")
		}
		release()
		require.NoError(t, <-parkedDone)
	})
}

// CQ-3: ResetFamilyReadiness must wake a coordinator already parked on the
// family's wake channel (close it before dropping it). Deleting without closing
// strands that waiter: MarkFamilyReady and Open only close channels still in the
// map, so it would recover only on its next poll. A spurious wake is harmless —
// the waiter re-checks eligibility.
func TestViewBuildGateResetWakesPreResetWaiter(t *testing.T) {
	gate := NewViewBuildGate()
	ch := gate.FamilyOpened("fam-a") // unready: a live wake channel
	select {
	case <-ch:
		t.Fatal("the wake channel fired before the family was ready")
	default:
	}

	gate.ResetFamilyReadiness("fam-a")
	select {
	case <-ch:
		// woken to re-check eligibility — the fix
	case <-time.After(2 * time.Second):
		t.Fatal("ResetFamilyReadiness stranded a pre-reset waiter (deleted the channel without closing it)")
	}

	// A channel obtained after the reset still fires when the family re-readies.
	ch2 := gate.FamilyOpened("fam-a")
	gate.MarkFamilyReady("fam-a", gate.FamilyReadinessEpoch("fam-a"))
	select {
	case <-ch2:
	case <-time.After(2 * time.Second):
		t.Fatal("the post-reset wake did not fire when the family became ready again")
	}
}

// A readiness mark whose catalog evidence was read before a primary
// replacement's reset must be refused; one read after it is accepted.
func TestViewBuildGateRefusesMarkFromBeforeReset(t *testing.T) {
	gate := NewViewBuildGate()
	stale := gate.FamilyReadinessEpoch("fam-a")
	gate.ResetFamilyReadiness("fam-a")
	require.False(t, gate.MarkFamilyReady("fam-a", stale),
		"evidence read before the reset re-admitted the family")
	require.False(t, gate.AdmittedForFamily("fam-a"))
	require.True(t, gate.MarkFamilyReady("fam-a", gate.FamilyReadinessEpoch("fam-a")))
	require.True(t, gate.AdmittedForFamily("fam-a"))
}

// A readiness reset wakes a deferred coordinator, but the wake does not make the
// family eligible: the coordinator must re-arm and keep its claim instead of
// entering a cycle that parks on the closed gate.
func TestStartupAdmissionResetWakeKeepsIneligibleCoordinatorDeferred(t *testing.T) {
	f := newStartupAdmissionFixture(t) // B's base never published.

	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		b := f.coordinator(t, f.b, gate)
		b.Signal("checkout registered")
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()

		gate.ResetFamilyReadiness(f.b.familyID)
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()

		if s := gate.Stats(); s.BackgroundQueued+s.InteractiveQueued != 0 || s.AdmittedBackground+s.AdmittedInteractive != 0 {
			t.Fatalf("readiness reset entered a cycle while the family was ineligible: %+v", s)
		}

		// The claim survived: once the family becomes ready the deferred cycle runs.
		require.True(t, gate.MarkFamilyReady(f.b.familyID, gate.FamilyReadinessEpoch(f.b.familyID)))
		synctest.Wait()
		if s := gate.Stats(); s.AdmittedBackground+s.AdmittedInteractive == 0 {
			t.Fatalf("the deferred claim was lost across the reset wake: %+v", s)
		}
	})
}

// A primary move that lands while the catalog seed is mid-read must not let the
// seed re-admit the family from the replaced primary's publication.
func TestStartupAdmissionCatalogSeedRefusedAcrossPrimaryMove(t *testing.T) {
	f := newStartupAdmissionFixture(t)
	gate := NewViewBuildGate()
	f.publishRealBase(t, f.a)
	c := f.coordinator(t, f.a, gate)

	// The seed's evidence (old primary published) is read, then the move resets.
	epoch := gate.FamilyReadinessEpoch(f.a.familyID)
	require.True(t, c.familyBasePublished())
	gate.ResetFamilyReadiness(f.a.familyID)
	require.False(t, gate.MarkFamilyReady(f.a.familyID, epoch),
		"a seed read before the primary move re-admitted the family")
	require.False(t, gate.AdmittedForFamily(f.a.familyID))
}

// A primary move fences the family: a seed that reads the epoch after the
// move's reset but marks before its commit would otherwise re-admit the family
// from the still-published incumbent, and evidence read during the move must
// stay refused after it.
func TestViewBuildGateFencesMarksDuringPrimaryTurnover(t *testing.T) {
	gate := NewViewBuildGate()
	require.True(t, gate.MarkFamilyReady("fam-a", gate.FamilyReadinessEpoch("fam-a")))

	gate.BeginFamilyTurnover("fam-a")
	require.False(t, gate.AdmittedForFamily("fam-a"))
	during := gate.FamilyReadinessEpoch("fam-a")
	require.False(t, gate.MarkFamilyReady("fam-a", during),
		"a seed during the primary move re-admitted the family from the incumbent")

	gate.EndFamilyTurnover("fam-a")
	require.False(t, gate.MarkFamilyReady("fam-a", during),
		"evidence read during the primary move admitted the family after it")
	require.True(t, gate.MarkFamilyReady("fam-a", gate.FamilyReadinessEpoch("fam-a")))
}

// A coordinator cycle must not park on the closed latch: AcquireAdmittedForFamily
// refuses an ineligible family and drops a queued waiter whose readiness is
// revoked, instead of holding it (and the coordinator's loop) until Open.
func TestViewBuildGateAdmittedAcquireFailsOnRevocation(t *testing.T) {
	ctx := context.Background()
	gate := NewViewBuildGate()
	_, err := gate.AcquireAdmittedForFamily(ctx, ViewBuildBackground, nil, nil, "fam-a")
	require.ErrorIs(t, err, ErrFamilyNotAdmitted)

	require.True(t, gate.MarkFamilyReady("fam-a", gate.FamilyReadinessEpoch("fam-a")))
	hold, err := gate.AcquireForFamily(ctx, ViewBuildInteractive, "fam-a")
	require.NoError(t, err)
	defer hold()
	errs := make(chan error, 1)
	go func() {
		release, err := gate.AcquireAdmittedForFamily(ctx, ViewBuildBackground, nil, nil, "fam-a")
		if release != nil {
			release()
		}
		errs <- err
	}()
	require.Eventually(t, func() bool { return gate.Stats().BackgroundQueued == 1 },
		2*time.Second, time.Millisecond)

	gate.ResetFamilyReadiness("fam-a")
	select {
	case err := <-errs:
		require.ErrorIs(t, err, ErrFamilyNotAdmitted)
	case <-time.After(2 * time.Second):
		t.Fatal("a revoked waiter stayed parked on the closed latch")
	}
	require.Zero(t, gate.Stats().BackgroundQueued)
}

// demoteStartupPrimary makes the family's only published graph a non-primary,
// so the catalog seed cannot re-admit it; restore undoes that.
func demoteStartupPrimary(t testing.TB, f *startupAdmissionFixture, fam startupFamily, primary bool) {
	t.Helper()
	require.NoError(t, f.catalog.UpsertDedicatedGraph(context.Background(), store_sqlite.DedicatedGraph{
		GraphID: fam.graphID, OwnerCheckoutID: fam.primaryID, RepoPrefix: fam.prefix,
		FamilyID: fam.familyID, State: reconcile.GraphStateReady, IsPrimaryBase: primary,
	}))
}

// An idle coordinator admitted at start holds no wait channel. A revocation
// must still be seen on its next wake: the claim defers instead of entering a
// cycle for an ineligible family.
func TestStartupAdmissionAdmittedIdleCoordinatorSeesRevocation(t *testing.T) {
	f := newStartupAdmissionFixture(t)
	f.publishRealBase(t, f.a)

	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		a := f.coordinator(t, f.a, gate)
		synctest.Wait()
		require.True(t, gate.AdmittedForFamily(f.a.familyID), "the published base seeds readiness")

		preflights := 0
		a.cyclePreflight = func(context.Context) (CheckoutCycle, bool) {
			preflights++
			return CheckoutCycle{}, false
		}
		demoteStartupPrimary(t, f, f.a, false)
		gate.ResetFamilyReadiness(f.a.familyID)
		a.Signal("edit")
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		require.Zero(t, preflights, "a claim started preflight while its family was ineligible")
		if s := gate.Stats(); s.BackgroundQueued+s.InteractiveQueued != 0 || s.AdmittedBackground+s.AdmittedInteractive != 0 {
			t.Fatalf("an admitted coordinator cycled after its family's readiness was revoked: %+v", s)
		}

		demoteStartupPrimary(t, f, f.a, true)
		require.True(t, gate.MarkFamilyReady(f.a.familyID, gate.FamilyReadinessEpoch(f.a.familyID)))
		synctest.Wait()
		if s := gate.Stats(); s.AdmittedBackground+s.AdmittedInteractive == 0 {
			t.Fatalf("the deferred claim was lost across the revocation: %+v", s)
		}
	})
}

// A revocation between the cycle's preflight and its lane acquisition must
// defer the cycle, not park it on the closed latch holding the cycle lock.
func TestStartupAdmissionRevocationBeforeLaneDefersCycle(t *testing.T) {
	f := newStartupAdmissionFixture(t)
	f.publishRealBase(t, f.a)

	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		a := f.coordinator(t, f.a, gate)
		synctest.Wait()
		revoked := false
		a.cyclePreflight = func(context.Context) (CheckoutCycle, bool) {
			if !revoked {
				revoked = true
				demoteStartupPrimary(t, f, f.a, false)
				gate.ResetFamilyReadiness(f.a.familyID)
			}
			return CheckoutCycle{}, false
		}
		a.Signal("edit")
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		require.True(t, revoked, "the cycle never reached preflight")
		if s := gate.Stats(); s.BackgroundQueued+s.InteractiveQueued != 0 || s.AdmittedBackground+s.AdmittedInteractive != 0 {
			t.Fatalf("a cycle revoked before the lane parked on or took it: %+v", s)
		}

		demoteStartupPrimary(t, f, f.a, true)
		require.True(t, gate.MarkFamilyReady(f.a.familyID, gate.FamilyReadinessEpoch(f.a.familyID)))
		synctest.Wait()
		if s := gate.Stats(); s.AdmittedBackground+s.AdmittedInteractive == 0 {
			t.Fatalf("the deferred claim was lost across the revocation: %+v", s)
		}
	})
}

// A lane granted before the warmup latch opens must not build over a primary
// that has published no base (a grant issued before a primary move, reaching
// base selection after it): the cycle defers and routes nothing.
func TestStartupAdmissionEarlyGrantRefusesUnpublishedBase(t *testing.T) {
	f := newStartupAdmissionFixture(t) // B's base never published.

	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		require.True(t, gate.MarkFamilyReady(f.b.familyID, gate.FamilyReadinessEpoch(f.b.familyID)))
		b := f.coordinator(t, f.b, gate)
		b.Signal("checkout registered")
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()

		if s := gate.Stats(); s.AdmittedBackground+s.AdmittedInteractive == 0 {
			t.Fatalf("the stale grant never reached base selection: %+v", s)
		}
		route, found, err := f.catalog.GetCheckoutRoute(context.Background(), f.b.checkoutID)
		require.NoError(t, err)
		require.False(t, found && route.CommitGenerationID > 0,
			"an early grant routed a layer over an unpublished primary: %+v", route)
	})
}
