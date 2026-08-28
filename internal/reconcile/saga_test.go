package reconcile

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// seedForgettableCheckout builds the widest checkout a forget saga has to take
// apart: a route, a dedicated graph of its own with two views rooted in it, a
// tracking intent, and stored path evidence.
func seedForgettableCheckout(f *fixture, checkoutID, incarnation, adminName, graphID string) {
	f.t.Helper()
	f.seedCheckout(checkoutID, incarnation, adminName, store_sqlite.CheckoutModeDedicated)
	f.seedOwnedGraph(graphID, checkoutID)
	f.seedRoute(checkoutID, graphID)
	f.seedRefView(graphID+"-main", graphID)
	f.seedRefView(graphID+"-release", graphID)
	f.seedIntent(checkoutID)
	err := f.catalog.UpsertCheckoutPathEvidence(context.Background(),
		SampledPathEvidence(gitSampleExisting(volumeA)).CatalogRow(checkoutID, 100, 1))
	if err != nil {
		f.t.Fatalf("seed path evidence: %v", err)
	}
}

func TestForgetCheckoutRunsItsPhasesInOrder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	seedForgettableCheckout(f, "co-1", "inc-1", "wt", "graph-1")

	if err := f.rec.ForgetCheckout(ctx, "co-1", "inc-1"); err != nil {
		t.Fatalf("ForgetCheckout: %v", err)
	}

	want := []string{"purge:co-1:inc-1", "release:graph-1"}
	if got := f.hooks.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("hook calls = %v, want %v", got, want)
	}
	f.assertNoCheckoutRows("co-1")
	if f.graphExists("graph-1") {
		t.Error("the dedicated graph row survived")
	}
	if f.refViewExists("graph-1-main") || f.refViewExists("graph-1-release") {
		t.Error("a ref view rooted in the released graph survived")
	}
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("the journal entry outlived the saga: %+v", entries)
	}
	// The family itself is not a forget-checkout's business.
	if !f.familyExists() {
		t.Error("forgetting a checkout deleted its family")
	}
}

// TestForgetCheckoutOutlivesARouteWrittenByTheStoppingBuilder pins the order
// the purge and the route withdrawal run in.
//
// The purge is what stops the coordinator, and a cycle already in flight
// installs its route as that stop waits for it. A withdrawal that ran before
// the stop therefore deletes a row that is written again a moment later, and
// the checkout delete — whose route is the one child that does not cascade —
// is refused by the foreign key.
func TestForgetCheckoutOutlivesARouteWrittenByTheStoppingBuilder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	f.seedPrimaryGraph("graph-primary")
	f.seedCheckout("co-1", "inc-1", "wt", store_sqlite.CheckoutModeAutomatic)
	f.seedRoute("co-1", "graph-primary")
	f.seedIntent("co-1")
	f.hooks.onPurge = func(checkoutID string) { f.seedRoute(checkoutID, "graph-primary") }

	if err := f.rec.ForgetCheckout(ctx, "co-1", "inc-1"); err != nil {
		t.Fatalf("ForgetCheckout: %v", err)
	}
	f.assertNoCheckoutRows("co-1")
}

func TestForgetCheckoutIsANoOpOnReentry(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	seedForgettableCheckout(f, "co-1", "inc-1", "wt", "graph-1")

	if err := f.rec.ForgetCheckout(ctx, "co-1", "inc-1"); err != nil {
		t.Fatalf("first ForgetCheckout: %v", err)
	}
	before := f.hooks.snapshot()
	if err := f.rec.ForgetCheckout(ctx, "co-1", "inc-1"); err != nil {
		t.Fatalf("second ForgetCheckout: %v", err)
	}
	if got := f.hooks.snapshot(); !slices.Equal(got, before) {
		t.Fatalf("re-entering a finished saga called hooks again: %v, want %v", got, before)
	}
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("re-entry left journal entries: %+v", entries)
	}
}

func TestForgetCheckoutRejectsAnEmptyTarget(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	if err := f.rec.ForgetCheckout(ctx, "", "inc-1"); !errors.Is(err, ErrSagaTarget) {
		t.Errorf("ForgetCheckout with no id = %v", err)
	}
	if err := f.rec.ForgetCheckout(ctx, "co-1", ""); !errors.Is(err, ErrSagaTarget) {
		t.Errorf("ForgetCheckout with no incarnation = %v", err)
	}
	if err := f.rec.RetirePrimaryClosure(ctx, "", 0); !errors.Is(err, ErrSagaTarget) {
		t.Errorf("RetirePrimaryClosure with no graph = %v", err)
	}
	if err := f.rec.ForgetFamily(ctx, ""); !errors.Is(err, ErrSagaTarget) {
		t.Errorf("ForgetFamily with no family = %v", err)
	}
}

// TestForgetCheckoutResumesFromEveryPhase simulates a crash at each phase
// boundary: the phases before it really ran, the journal says the next one was
// in flight, and nothing else survived the process. Resume has to converge on
// the same postcondition from every one of those states, and must not repeat a
// side effect the earlier phases already produced.
func TestForgetCheckoutResumesFromEveryPhase(t *testing.T) {
	ctx := context.Background()
	plan := sagaPhases[sagaForgetCheckout]
	for index, phase := range plan {
		t.Run(string(phase), func(t *testing.T) {
			f := newFixture(t, Default())
			seedForgettableCheckout(f, "co-1", "inc-1", "wt", "graph-1")
			target := sagaTarget{
				Kind:        sagaForgetCheckout,
				CheckoutID:  "co-1",
				Incarnation: "inc-1",
				FamilyID:    f.familyID,
				GraphID:     "graph-1",
			}

			// Replay the phases the crashed process completed.
			for _, done := range plan[:index] {
				replay := target
				replay.Phase = done
				if err := f.rec.runPhase(ctx, replay); err != nil {
					t.Fatalf("replaying %s: %v", done, err)
				}
			}
			// The journal says this phase was in flight when the process died.
			target.Phase = phase
			err := f.rec.persistPhase(ctx, target.cleanupID(), target, store_sqlite.CleanupPhaseDeleting)
			if err != nil {
				t.Fatalf("persist crash phase: %v", err)
			}

			if err := f.rec.Resume(ctx); err != nil {
				t.Fatalf("Resume from %s: %v", phase, err)
			}
			f.assertNoCheckoutRows("co-1")
			if f.graphExists("graph-1") || f.refViewExists("graph-1-main") {
				t.Error("resume left graph rows behind")
			}
			if entries := f.journal(); len(entries) != 0 {
				t.Fatalf("resume left journal entries: %+v", entries)
			}
			if n := f.hooks.countPrefix("purge:"); n != 1 {
				t.Errorf("layers purged %d times across the crash, want 1", n)
			}
			if n := f.hooks.countPrefix("release:"); n != 1 {
				t.Errorf("graph released %d times across the crash, want 1", n)
			}
		})
	}
}

// TestSagaResumesAfterEachHookFailure interrupts the saga at each hook the way
// a real crash would — the hook never completes — and proves the resume
// finishes the job with exactly one successful call at each hook.
func TestSagaResumesAfterEachHookFailure(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		arm        func(h *recordingHooks)
		wantPhase  sagaPhase
		wantCalls  []string
		afterFirst func(f *fixture)
	}{
		{
			name:      "purge fails once",
			arm:       func(h *recordingHooks) { h.failPurge = 1 },
			wantPhase: phasePurgeLayers,
			wantCalls: []string{"purge:co-1:inc-1", "purge:co-1:inc-1", "release:graph-1"},
		},
		{
			name:      "release fails once",
			arm:       func(h *recordingHooks) { h.failRelease = 1 },
			wantPhase: phaseReleaseGraph,
			wantCalls: []string{"purge:co-1:inc-1", "release:graph-1", "release:graph-1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, Default())
			seedForgettableCheckout(f, "co-1", "inc-1", "wt", "graph-1")
			tc.arm(f.hooks)

			err := f.rec.ForgetCheckout(ctx, "co-1", "inc-1")
			if !errors.Is(err, errHookFailed) {
				t.Fatalf("interrupted saga = %v, want the hook failure", err)
			}
			entries := f.journal()
			if len(entries) != 1 || entries[0].Phase != store_sqlite.CleanupPhaseFailed {
				t.Fatalf("journal after the interruption = %+v", entries)
			}
			target, err := decodeSagaTarget(entries[0])
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if target.Phase != tc.wantPhase {
				t.Fatalf("journal stopped at %q, want %q", target.Phase, tc.wantPhase)
			}
			// The checkout must still be there: a failed phase never advances.
			f.checkout("co-1")

			if err := f.rec.Resume(ctx); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			f.assertNoCheckoutRows("co-1")
			if f.graphExists("graph-1") {
				t.Error("resume left the graph row behind")
			}
			if got := f.hooks.snapshot(); !slices.Equal(got, tc.wantCalls) {
				t.Fatalf("hook calls = %v, want %v", got, tc.wantCalls)
			}
			if entries := f.journal(); len(entries) != 0 {
				t.Fatalf("resume left journal entries: %+v", entries)
			}
		})
	}
}

func TestRetirePrimaryClosureSparesIndependentGraphs(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())

	// The primary and its owner.
	seedForgettableCheckout(f, "co-primary", "inc-p", "primary", "graph-primary")
	epoch := f.promoteToPrimary("graph-primary")

	// A dependent automatic checkout: it can only be served off the primary.
	f.seedCheckout("co-auto", "inc-a", "auto", store_sqlite.CheckoutModeAutomatic)
	f.seedRoute("co-auto", "graph-primary")
	f.seedIntent("co-auto")

	// An independent dedicated sibling that does not need the primary.
	seedForgettableCheckout(f, "co-indep", "inc-i", "indep", "graph-indep")

	if err := f.rec.RetirePrimaryClosure(ctx, "graph-primary", epoch); err != nil {
		t.Fatalf("RetirePrimaryClosure: %v", err)
	}

	f.assertNoCheckoutRows("co-primary")
	f.assertNoCheckoutRows("co-auto")
	if f.graphExists("graph-primary") {
		t.Error("the primary graph row survived")
	}
	if f.refViewExists("graph-primary-main") {
		t.Error("a view rooted in the retired primary survived")
	}
	if f.primaryGraphID() != "" {
		t.Errorf("the family still has primary %q", f.primaryGraphID())
	}

	// Everything independent is untouched.
	f.checkout("co-indep")
	if !f.graphExists("graph-indep") {
		t.Error("the independent graph was retired with the primary")
	}
	if !f.refViewExists("graph-indep-main") || !f.refViewExists("graph-indep-release") {
		t.Error("the independent graph's views were deleted")
	}
	if !f.familyExists() {
		t.Error("the family was forgotten while a dedicated graph remained")
	}
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("retirement left journal entries: %+v", entries)
	}
}

func TestRetirePrimaryClosureRefusesAStaleEpoch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	seedForgettableCheckout(f, "co-primary", "inc-p", "primary", "graph-primary")
	epoch := f.promoteToPrimary("graph-primary")

	err := f.rec.RetirePrimaryClosure(ctx, "graph-primary", epoch-1)
	if !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("stale epoch = %v, want a stale guard", err)
	}
	f.checkout("co-primary")
	if !f.graphExists("graph-primary") {
		t.Error("a refused retirement still deleted the graph")
	}
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("a refused retirement wrote journal entries: %+v", entries)
	}

	// A graph that is already gone is a no-op, not a failure.
	if err := f.rec.RetirePrimaryClosure(ctx, "graph-missing", 0); err != nil {
		t.Fatalf("retiring an absent graph = %v, want nil", err)
	}
}

func TestRetirePrimaryClosureCascadesIntoFamilyForget(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	seedForgettableCheckout(f, "co-primary", "inc-p", "primary", "graph-primary")
	epoch := f.promoteToPrimary("graph-primary")
	f.seedCheckout("co-main", "inc-m", gitstate.MainAdminName, store_sqlite.CheckoutModeAutomatic)
	f.seedIntent("co-main")

	if err := f.rec.RetirePrimaryClosure(ctx, "graph-primary", epoch); err != nil {
		t.Fatalf("RetirePrimaryClosure: %v", err)
	}
	f.assertNoCheckoutRows("co-primary")
	f.assertNoCheckoutRows("co-main")
	if f.familyExists() {
		t.Error("the family row survived a retirement that left it with nothing")
	}
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("the cascade left journal entries: %+v", entries)
	}
}

func TestForgetFamilyRemovesEverythingItHolds(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	f.seedCheckout("co-main", "inc-m", gitstate.MainAdminName, store_sqlite.CheckoutModeAutomatic)
	f.seedIntent("co-main")
	seedForgettableCheckout(f, "co-wt", "inc-w", "wt", "graph-wt")

	if err := f.rec.ForgetFamily(ctx, f.familyID); err != nil {
		t.Fatalf("ForgetFamily: %v", err)
	}
	f.assertNoCheckoutRows("co-main")
	f.assertNoCheckoutRows("co-wt")
	if f.graphExists("graph-wt") {
		t.Error("a dedicated graph survived the family teardown")
	}
	if f.familyExists() {
		t.Error("the family row survived")
	}
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("ForgetFamily left journal entries: %+v", entries)
	}
	// Re-entering is a no-op rather than an error.
	if err := f.rec.ForgetFamily(ctx, f.familyID); err != nil {
		t.Fatalf("second ForgetFamily = %v, want nil", err)
	}
}

func TestResumeSkipsFinishedAndRejectsUnusableEntries(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())

	// A finished layer purge is evidence, not work: it must stay put and must
	// not call the hook again.
	done := sagaTarget{Kind: sagaPurgeLayers, CheckoutID: "co-9", Incarnation: "inc-9"}
	if err := f.rec.persistPhase(ctx, done.cleanupID(), done, store_sqlite.CleanupPhaseDone); err != nil {
		t.Fatalf("persist a finished purge: %v", err)
	}

	for _, entry := range []store_sqlite.CleanupEntry{
		{CleanupID: "broken-json", OpaqueTargetIDs: "{not json", Reason: "x", Phase: store_sqlite.CleanupPhasePending},
		{CleanupID: "no-kind", OpaqueTargetIDs: `{"phase":"withdraw_route"}`, Reason: "x", Phase: store_sqlite.CleanupPhasePending},
		{CleanupID: "unknown-kind", OpaqueTargetIDs: `{"kind":"invented"}`, Reason: "x", Phase: store_sqlite.CleanupPhasePending},
		{
			CleanupID:       "foreign-phase",
			OpaqueTargetIDs: `{"kind":"forget_family","phase":"withdraw_route","family_id":"fam-1"}`,
			Reason:          "x",
			Phase:           store_sqlite.CleanupPhasePending,
		},
	} {
		if err := f.catalog.UpsertCleanupEntry(ctx, entry); err != nil {
			t.Fatalf("seed %s: %v", entry.CleanupID, err)
		}
	}

	err := f.rec.Resume(ctx)
	if !errors.Is(err, ErrSagaTarget) {
		t.Fatalf("Resume = %v, want ErrSagaTarget", err)
	}
	if n := f.hooks.countPrefix("purge:"); n != 0 {
		t.Errorf("a finished purge entry was re-run %d times", n)
	}
	// Every unusable entry is reported; none of them stops the others being
	// looked at, and none of them is silently dropped.
	if entries := f.journal(); len(entries) != 5 {
		t.Fatalf("journal holds %d entries, want the 5 seeded ones", len(entries))
	}
}

// TestPurgeLayersJournalStopsASecondPurge proves the availability purge is
// guarded by durable evidence rather than by the in-memory pass: a fresh
// reconciler over the same store must not purge the same incarnation again.
func TestPurgeLayersJournalStopsASecondPurge(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	f.seedCheckout("co-1", "inc-1", "wt", store_sqlite.CheckoutModeAutomatic)
	checkout := f.checkout("co-1")

	if err := f.rec.purgeLayersOnce(ctx, checkout); err != nil {
		t.Fatalf("first purge: %v", err)
	}
	entries := f.journal()
	if len(entries) != 1 || entries[0].Phase != store_sqlite.CleanupPhaseDone {
		t.Fatalf("journal after the purge = %+v", entries)
	}

	fresh, err := New(f.catalog, f.hooks, Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := fresh.purgeLayersOnce(ctx, checkout); err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if n := f.hooks.countPrefix("purge:"); n != 1 {
		t.Fatalf("layers purged %d times across two reconcilers, want 1", n)
	}

	// A different incarnation of the same path is a different thing to purge.
	checkout.Incarnation = "inc-2"
	if err := fresh.purgeLayersOnce(ctx, checkout); err != nil {
		t.Fatalf("purge of a new incarnation: %v", err)
	}
	if n := f.hooks.countPrefix("purge:"); n != 2 {
		t.Fatalf("a new incarnation was not purged: %d calls", n)
	}
}

func TestResumeRepairsVerifiableLegacyRetireGraphTarget(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	f.seedCheckout("co-primary", "inc-primary", "main", store_sqlite.CheckoutModeDedicated)
	f.seedOwnedGraph("primary-graph", "co-primary")
	f.promoteToPrimary("primary-graph")
	f.seedCheckout("co-1", "inc-1", "wt", store_sqlite.CheckoutModeDedicated)
	f.seedOwnedGraph("graph-1", "co-1")
	if err := f.catalog.BeginIntentTransition(ctx, store_sqlite.IntentTransition{
		TransitionID:       "demote-1",
		CheckoutID:         "co-1",
		Cause:              explicitUntrackDemotionCause,
		PriorDesiredMode:   store_sqlite.CheckoutModeDedicated,
		PriorEffectiveMode: store_sqlite.CheckoutModeDedicated,
		RequestedMode:      store_sqlite.CheckoutModeAutomatic,
		PriorCheckoutState: store_sqlite.CheckoutStateReady,
		SourceSnapshotHash: "graph-1:primary-graph:1",
		State:              store_sqlite.IntentTransitionRunning,
		CreatedAt:          1,
		LastProgress:       1,
	}); err != nil {
		t.Fatalf("seed durable demotion ownership: %v", err)
	}
	target := sagaTarget{
		Kind: sagaRetireGraph, Phase: phaseReleaseGraph, GraphID: "graph-1",
		CheckoutID: "co-1", FamilyID: f.familyID,
	}
	if err := f.rec.persistPhase(ctx, target.cleanupID(), target, store_sqlite.CleanupPhasePending); err != nil {
		t.Fatalf("persist legacy graph cleanup: %v", err)
	}
	cleanup, found, err := f.catalog.GetCleanupEntry(ctx, target.cleanupID())
	if err != nil || !found {
		t.Fatalf("read legacy graph cleanup = found:%v err:%v", found, err)
	}
	family, found, err := f.catalog.GetRepositoryFamily(ctx, f.familyID)
	if err != nil || !found {
		t.Fatalf("read repository family = found:%v err:%v", found, err)
	}
	if err := f.catalog.CommitAuthorizedDemotion(ctx, store_sqlite.CommitAuthorizedDemotionRequest{
		CheckoutID:           "co-1",
		Incarnation:          "inc-1",
		FamilyID:             f.familyID,
		TransitionID:         "demote-1",
		OwnedGraphID:         "graph-1",
		PrimaryGraphID:       "primary-graph",
		ExpectedPrimaryEpoch: family.PrimaryEpoch,
		RequiredPrimaryState: GraphStateReady,
		State:                store_sqlite.CheckoutStateReady,
		LastSeen:             1,
		Cleanup:              &cleanup,
	}); err != nil {
		t.Fatalf("commit durable demotion ownership: %v", err)
	}
	if err := f.rec.persistPhase(ctx, target.cleanupID(), target, store_sqlite.CleanupPhaseFailed); err != nil {
		t.Fatalf("record interrupted legacy cleanup: %v", err)
	}
	f.hooks.failRelease = 1

	if err := f.rec.Resume(ctx); !errors.Is(err, errHookFailed) {
		t.Fatalf("first Resume = %v, want injected release failure", err)
	}
	repaired, found, err := f.rec.loadSagaTarget(ctx, target.cleanupID())
	if err != nil || !found {
		t.Fatalf("load repaired graph cleanup = found:%v err:%v", found, err)
	}
	if repaired.CheckoutID != "co-1" || repaired.Incarnation != "inc-1" ||
		repaired.FamilyID != f.familyID || repaired.GraphID != "graph-1" ||
		repaired.RepoPrefix != "prefix-graph-1" || repaired.RootPath != "/repo/wt" {
		t.Fatalf("repaired target = %+v", repaired)
	}
	if !f.graphExists("graph-1") {
		t.Fatal("failed release deleted the graph")
	}

	if err := f.rec.Resume(ctx); err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	if f.graphExists("graph-1") {
		t.Fatal("successful repaired cleanup retained the graph")
	}
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("successful repaired cleanup retained journal: %+v", entries)
	}
}

func TestResumeKeepsUnverifiableLegacyRetireGraphTargetDurable(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*fixture) sagaTarget
	}{
		{
			name: "graph has no owner",
			seed: func(f *fixture) sagaTarget {
				f.seedPrimaryGraph("graph-1")
				return sagaTarget{Kind: sagaRetireGraph, Phase: phaseReleaseGraph, GraphID: "graph-1"}
			},
		},
		{
			name: "target names another checkout",
			seed: func(f *fixture) sagaTarget {
				f.seedCheckout("co-1", "inc-1", "one", store_sqlite.CheckoutModeDedicated)
				f.seedCheckout("co-2", "inc-2", "two", store_sqlite.CheckoutModeDedicated)
				f.seedOwnedGraph("graph-1", "co-1")
				return sagaTarget{
					Kind: sagaRetireGraph, Phase: phaseReleaseGraph,
					GraphID: "graph-1", CheckoutID: "co-2",
				}
			},
		},
		{
			name: "target names another family",
			seed: func(f *fixture) sagaTarget {
				f.seedCheckout("co-1", "inc-1", "one", store_sqlite.CheckoutModeDedicated)
				f.seedOwnedGraph("graph-1", "co-1")
				return sagaTarget{
					Kind: sagaRetireGraph, Phase: phaseReleaseGraph,
					GraphID: "graph-1", CheckoutID: "co-1", FamilyID: "another-family",
				}
			},
		},
		{
			name: "matching rows have no durable demotion ownership",
			seed: func(f *fixture) sagaTarget {
				f.seedCheckout("co-1", "inc-1", "one", store_sqlite.CheckoutModeDedicated)
				f.seedOwnedGraph("graph-1", "co-1")
				return sagaTarget{
					Kind: sagaRetireGraph, Phase: phaseReleaseGraph,
					GraphID: "graph-1", FamilyID: f.familyID,
				}
			},
		},
		{
			name: "demotion transition has not committed",
			seed: func(f *fixture) sagaTarget {
				f.seedCheckout("co-1", "inc-1", "one", store_sqlite.CheckoutModeDedicated)
				f.seedOwnedGraph("graph-1", "co-1")
				if err := f.catalog.BeginIntentTransition(context.Background(), store_sqlite.IntentTransition{
					TransitionID:       "demote-1",
					CheckoutID:         "co-1",
					Cause:              explicitUntrackDemotionCause,
					PriorDesiredMode:   store_sqlite.CheckoutModeDedicated,
					PriorEffectiveMode: store_sqlite.CheckoutModeDedicated,
					RequestedMode:      store_sqlite.CheckoutModeAutomatic,
					PriorCheckoutState: store_sqlite.CheckoutStateReady,
					SourceSnapshotHash: "graph-1:primary-graph:1",
					State:              store_sqlite.IntentTransitionRunning,
					CreatedAt:          1,
					LastProgress:       1,
				}); err != nil {
					f.t.Fatalf("begin uncommitted demotion: %v", err)
				}
				return sagaTarget{
					Kind: sagaRetireGraph, Phase: phaseReleaseGraph,
					GraphID: "graph-1", CheckoutID: "co-1", FamilyID: f.familyID,
				}
			},
		},
		{
			name: "reused checkout and graph IDs have no old transition",
			seed: func(f *fixture) sagaTarget {
				f.seedCheckout("co-1", "inc-new", "one", store_sqlite.CheckoutModeDedicated)
				f.seedOwnedGraph("graph-1", "co-1")
				return sagaTarget{
					Kind: sagaRetireGraph, Phase: phaseReleaseGraph,
					GraphID: "graph-1", CheckoutID: "co-1", FamilyID: f.familyID,
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, Default())
			target := tc.seed(f)
			if err := f.rec.persistPhase(ctx, target.cleanupID(), target, store_sqlite.CleanupPhaseFailed); err != nil {
				t.Fatalf("persist ambiguous legacy cleanup: %v", err)
			}
			before, found, err := f.catalog.GetCleanupEntry(ctx, target.cleanupID())
			if err != nil || !found {
				t.Fatalf("read seeded cleanup = found:%v err:%v", found, err)
			}

			if err := f.rec.Resume(ctx); !errors.Is(err, ErrSagaTarget) {
				t.Fatalf("Resume = %v, want ErrSagaTarget", err)
			}
			after, found, err := f.catalog.GetCleanupEntry(ctx, target.cleanupID())
			if err != nil || !found {
				t.Fatalf("read retained cleanup = found:%v err:%v", found, err)
			}
			if after.Phase != before.Phase || after.OpaqueTargetIDs != before.OpaqueTargetIDs {
				t.Fatalf("ambiguous cleanup changed from %+v to %+v", before, after)
			}
			if f.hooks.countPrefix("release:") != 0 {
				t.Fatal("ambiguous legacy cleanup invoked graph release")
			}
			if !f.graphExists("graph-1") {
				t.Fatal("ambiguous legacy cleanup deleted the graph")
			}
		})
	}
}

func TestResumeSkipsStalePositiveRetireGraphIdentity(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	f.seedCheckout("co-1", "inc-old", "wt", store_sqlite.CheckoutModeDedicated)
	f.seedOwnedGraph("graph-1", "co-1")
	target := sagaTarget{
		Kind: sagaRetireGraph, Phase: phaseReleaseGraph,
		GraphID: "graph-1", FamilyID: f.familyID,
		CheckoutID: "co-1", Incarnation: "inc-old",
	}
	if err := f.rec.persistPhase(ctx, target.cleanupID(), target, store_sqlite.CleanupPhaseFailed); err != nil {
		t.Fatalf("persist stale cleanup: %v", err)
	}
	f.rekey("co-1", "inc-new")

	if err := f.rec.Resume(ctx); err != nil {
		t.Fatalf("Resume stale cleanup: %v", err)
	}
	if !f.graphExists("graph-1") {
		t.Fatal("stale cleanup deleted the replacement graph")
	}
	if f.hooks.countPrefix("release:") != 0 {
		t.Fatal("stale cleanup invoked graph release")
	}
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("stale cleanup retained obsolete journal: %+v", entries)
	}
}

func TestResumeFinishesGraphReleaseAfterCatalogRowAlreadyGone(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	target := sagaTarget{
		Kind: sagaRetireGraph, Phase: phaseReleaseGraph,
		GraphID: "graph-gone", FamilyID: f.familyID,
		CheckoutID: "co-gone", Incarnation: "inc-gone",
		RepoPrefix: "durable-prefix", RootPath: "/repo/durable-root",
	}
	if err := f.rec.persistPhase(ctx, target.cleanupID(), target, store_sqlite.CleanupPhaseFailed); err != nil {
		t.Fatalf("persist partially committed graph release: %v", err)
	}

	if err := f.rec.Resume(ctx); err != nil {
		t.Fatalf("Resume row-absent graph release: %v", err)
	}
	released := f.hooks.releasedTargets()
	if len(released) != 1 {
		t.Fatalf("release targets = %+v, want one", released)
	}
	if released[0].GraphID != target.GraphID || released[0].CheckoutID != target.CheckoutID ||
		released[0].Incarnation != target.Incarnation || released[0].RepoPrefix != target.RepoPrefix ||
		released[0].RootPath != target.RootPath {
		t.Fatalf("row-absent retry lost durable address: %+v", released[0])
	}
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("row-absent retry retained journal: %+v", entries)
	}
}

func TestResumeSkipsRowAbsentGraphCleanupForReplacementCheckout(t *testing.T) {
	for _, kind := range []sagaKind{sagaRetireGraph, sagaForgetCheckout} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, Default())
			f.seedCheckout("co-1", "inc-new", "replacement", store_sqlite.CheckoutModeAutomatic)
			target := sagaTarget{
				Kind: kind, Phase: phaseReleaseGraph,
				GraphID: "graph-gone", FamilyID: f.familyID,
				CheckoutID: "co-1", Incarnation: "inc-old",
				RepoPrefix: "durable-prefix", RootPath: "/repo/old",
			}
			if err := f.rec.persistPhase(ctx, target.cleanupID(), target, store_sqlite.CleanupPhaseFailed); err != nil {
				t.Fatalf("persist stale row-absent graph release: %v", err)
			}

			if err := f.rec.Resume(ctx); err != nil {
				t.Fatalf("Resume stale row-absent cleanup: %v", err)
			}
			if f.hooks.countPrefix("release:") != 0 {
				t.Fatal("stale row-absent cleanup invoked graph release")
			}
			checkout, present, err := f.catalog.GetCheckout(ctx, "co-1")
			if err != nil || !present || checkout.Incarnation != "inc-new" {
				t.Fatalf("replacement checkout changed: %+v, present:%v err:%v", checkout, present, err)
			}
			if entries := f.journal(); len(entries) != 0 {
				t.Fatalf("stale row-absent cleanup retained obsolete journal: %+v", entries)
			}
		})
	}
}

// TestPostconditionsRefuseALeftoverRow drives the two verification phases
// against state they must reject, which is the only way they fire: a finished
// saga leaves nothing for them to find.
func TestPostconditionsRefuseALeftoverRow(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	seedForgettableCheckout(f, "co-1", "inc-1", "wt", "graph-1")
	target := sagaTarget{
		Kind:        sagaForgetCheckout,
		Phase:       phaseVerifyCheckoutGone,
		CheckoutID:  "co-1",
		Incarnation: "inc-1",
		FamilyID:    f.familyID,
		GraphID:     "graph-1",
	}
	if err := f.rec.runPhase(ctx, target); !errors.Is(err, ErrPostcondition) {
		t.Fatalf("verify over a live checkout = %v, want ErrPostcondition", err)
	}

	f.promoteToPrimary("graph-1")
	closure := sagaTarget{
		Kind:     sagaRetirePrimaryClosure,
		Phase:    phaseVerifyClosureGone,
		FamilyID: f.familyID,
		GraphID:  "graph-1",
	}
	if err := f.rec.runPhase(ctx, closure); !errors.Is(err, ErrPostcondition) {
		t.Fatalf("closure verify with a live primary = %v, want ErrPostcondition", err)
	}
}
