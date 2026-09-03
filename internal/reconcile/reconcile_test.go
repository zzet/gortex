package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestConfigDefaultAndValidate(t *testing.T) {
	cfg := Default()
	if cfg.AvailabilityGrace != 30*time.Second || cfg.RemovalGrace != 30*time.Second {
		t.Fatalf("Default() = %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}
	for _, bad := range []Config{
		{AvailabilityGrace: 0, RemovalGrace: time.Second},
		{AvailabilityGrace: -time.Second, RemovalGrace: time.Second},
		{AvailabilityGrace: time.Second, RemovalGrace: 0},
		{AvailabilityGrace: time.Second, RemovalGrace: -time.Second},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%+v.Validate() = %v, want ErrInvalidConfig", bad, err)
		}
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	f := newFixture(t, Default())
	if _, err := New(nil, f.hooks, Default()); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("New(nil catalog) = %v", err)
	}
	if _, err := New(f.catalog, nil, Default()); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("New(nil hooks) = %v", err)
	}
	if _, err := New(f.catalog, f.hooks, Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("New(zero config) = %v", err)
	}
	// A nil option, and a nil override inside one, must leave the defaults in
	// place rather than installing a nil function that panics on first use.
	rec, err := New(f.catalog, f.hooks, Default(), nil, WithClock(nil), WithInventory(nil),
		WithPathSampler(nil), WithHEADSampler(nil))
	if err != nil {
		t.Fatalf("New with nil overrides = %v", err)
	}
	if rec.now == nil || rec.inventory == nil || rec.samplePath == nil || rec.sampleHEAD == nil {
		t.Fatal("a nil override cleared a default dependency")
	}
}

// TestCheckoutActionsAreDistinct guards the report vocabulary. The actions are
// what a caller switches on, so two of them sharing a value would silently
// merge two different outcomes into one.
func TestCheckoutActionsAreDistinct(t *testing.T) {
	actions := []CheckoutAction{
		ActionObserved,
		ActionIdentityAllocated,
		ActionReadyConfirmed,
		ActionAvailabilityRecovered,
		ActionAvailabilityGraceStarted,
		ActionAvailabilityHeld,
		ActionMarkedUnavailable,
		ActionRemovalGraceStarted,
		ActionRemovalHeld,
		ActionRemovalCancelled,
		ActionForgotten,
		ActionPrimaryClosureRetired,
		ActionGuardLost,
	}
	seen := map[CheckoutAction]bool{}
	for _, action := range actions {
		if action == "" {
			t.Error("an action has an empty value")
		}
		if seen[action] {
			t.Errorf("action %q is used twice", action)
		}
		seen[action] = true
	}
}

func TestReconcileFamilyRejectsUnknownFamily(t *testing.T) {
	f := newFixture(t, Default())
	_, err := f.rec.ReconcileFamily(context.Background(), "no-such-family", "/repo")
	if !errors.Is(err, store_sqlite.ErrCatalogNotFound) {
		t.Fatalf("ReconcileFamily on an unknown family = %v", err)
	}
}

// TestReconcileFamilyWithoutPrimaryStaysEphemeral proves the identity gate:
// a perfectly healthy worktree in a family that has nowhere to serve it from
// is reported and forgotten, not written down.
func TestReconcileFamilyWithoutPrimaryStaysEphemeral(t *testing.T) {
	f := newFixture(t, Default())
	f.git.setRecords(mainWorktreeRecord("/repo"), presentRecord("wt", "/repo/wt"))
	f.git.samples["/repo"] = gitSampleExisting(volumeA)
	f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)

	report := f.reconcile()
	if report.PrimaryGraphID != "" || report.Code != graphview.CodeNoPrimary {
		t.Fatalf("report = %+v, want no primary", report)
	}
	if len(report.Checkouts) != 2 {
		t.Fatalf("report covers %d checkouts, want 2", len(report.Checkouts))
	}
	for _, entry := range report.Checkouts {
		if entry.Action != ActionObserved || entry.Durable || entry.CheckoutID != "" {
			t.Errorf("%s = %+v, want an ephemeral observation", entry.AdminName, entry)
		}
		if entry.Classification.Disposition != DispositionPresent {
			t.Errorf("%s classified %q, want present", entry.AdminName, entry.Classification.Disposition)
		}
		if entry.Classification.Code != graphview.CodeNoPrimary {
			t.Errorf("%s code = %q, want no_primary", entry.AdminName, entry.Classification.Code)
		}
	}
	if rows := f.checkouts(); len(rows) != 0 {
		t.Fatalf("ephemeral observations wrote %d checkout rows", len(rows))
	}

	// The same records in a family that does have a primary become durable.
	f.seedPrimaryGraph("graph-primary")
	report = f.reconcile()
	if report.Code != "" || report.PrimaryGraphID != "graph-primary" {
		t.Fatalf("report = %+v, want the primary named", report)
	}
	for _, entry := range report.Checkouts {
		if entry.Action != ActionIdentityAllocated || !entry.Durable || entry.CheckoutID == "" {
			t.Errorf("%s = %+v, want a durable identity", entry.AdminName, entry)
		}
		if entry.Incarnation == "" || entry.Incarnation == entry.CheckoutID {
			t.Errorf("%s incarnation %q must be its own id", entry.AdminName, entry.Incarnation)
		}
	}
	if rows := f.checkouts(); len(rows) != 2 {
		t.Fatalf("allocation wrote %d checkout rows, want 2", len(rows))
	}
	if entry := f.entry(report, gitstate.MainAdminName); !entry.Main {
		t.Error("the main worktree is not reported as main")
	}
}

// TestReconcileFamilySkipsUnusableRecords covers the other two ephemeral
// reasons: a worktree git already calls prunable, and one whose root has never
// been reachable.
func TestReconcileFamilySkipsUnusableRecords(t *testing.T) {
	f := newFixture(t, Default())
	f.seedPrimaryGraph("graph-primary")

	prunable := presentRecord("stale", "/repo/stale")
	prunable.RootAccessible = false
	prunable.Prunable = true
	prunable.PruneReason = "gitdir file points to a non-existent location"

	unreachable := presentRecord("offline", "/repo/offline")
	unreachable.RootAccessible = false

	nameless := presentRecord("", "/repo/nameless")

	f.git.setRecords(prunable, unreachable, nameless)
	report := f.reconcile()

	if len(report.Checkouts) != 3 {
		t.Fatalf("report covers %d records, want 3", len(report.Checkouts))
	}
	for _, entry := range report.Checkouts {
		if entry.Action != ActionObserved || entry.Durable {
			t.Errorf("%q = %+v, want an ephemeral observation", entry.RootPath, entry)
		}
		if entry.Detail == "" {
			t.Errorf("%q gave no reason for staying ephemeral", entry.RootPath)
		}
	}
	if rows := f.checkouts(); len(rows) != 0 {
		t.Fatalf("unusable records wrote %d checkout rows", len(rows))
	}
}

// TestReconcileFamilyInventoryFailureNeverRemoves proves the hard rule: with
// git unusable, a known checkout goes to the availability axis and the removal
// clock is never touched, however gone the path looks.
func TestReconcileFamilyInventoryFailureNeverRemoves(t *testing.T) {
	f := newFixture(t, Default())
	f.seedPrimaryGraph("graph-primary")
	f.git.setRecords(presentRecord("wt", "/repo/wt"))
	f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)
	allocated := f.entry(f.reconcile(), "wt")

	// git stops answering and the directory vanishes at the same time.
	f.git.err = fmt.Errorf("git died: %w", gitstate.ErrInventoryUnavailable)
	f.git.samples["/repo/wt"] = gitSampleAbsent(volumeA)

	report := f.reconcile()
	if report.InventoryUsable {
		t.Fatal("report claims a usable inventory")
	}
	entry := f.entry(report, "wt")
	if entry.Classification.Disposition != DispositionInaccessible {
		t.Fatalf("classified %q, want inaccessible", entry.Classification.Disposition)
	}
	if entry.Action != ActionAvailabilityGraceStarted {
		t.Fatalf("action = %q, want the availability clock started", entry.Action)
	}
	row := f.checkout(allocated.CheckoutID)
	if row.RemovalDetectedAt != 0 || row.RemovalDeadline != 0 || row.RemovalEvidence != "" {
		t.Fatalf("an unusable inventory started a removal clock: %+v", row)
	}

	// A common-dir mismatch is the same class of untrustworthy inventory.
	f.git.err = nil
	f.git.commonDir = "/somewhere/else/.git"
	f.git.setRecords()
	entry = f.entry(f.reconcile(), "wt")
	if entry.Classification.Disposition != DispositionInaccessible {
		t.Fatalf("mismatched common dir classified %q", entry.Classification.Disposition)
	}
	if row := f.checkout(allocated.CheckoutID); row.RemovalDetectedAt != 0 {
		t.Fatalf("a foreign inventory started a removal clock: %+v", row)
	}
}

// TestAvailabilityGraceExpiresAtTheDeadline walks ready -> grace -> forgotten
// under a fake clock. An inaccessible worktree is removed at the one grace
// deadline; it does not survive as an unavailable shadow graph.
func TestAvailabilityGraceExpiresAtTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := Config{AvailabilityGrace: 30 * time.Second, RemovalGrace: 5 * time.Minute}
		f := newFixture(t, cfg)
		f.seedPrimaryGraph("graph-primary")
		f.git.setRecords(presentRecord("wt", "/repo/wt"))
		f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)

		allocated := f.entry(f.reconcile(), "wt")
		if allocated.State != store_sqlite.CheckoutStateReady {
			t.Fatalf("fresh identity is in state %q", allocated.State)
		}

		unreachable := presentRecord("wt", "/repo/wt")
		unreachable.RootAccessible = false
		unreachable.RootErr = errors.New("device not configured")
		f.git.setRecords(unreachable)
		f.git.samples["/repo/wt"] = gitSampleAbsent(volumeB)

		entry := f.entry(f.reconcile(), "wt")
		if entry.Action != ActionAvailabilityGraceStarted || entry.State != store_sqlite.CheckoutStateAvailabilityGrace {
			t.Fatalf("first unreachable pass = %+v", entry)
		}
		row := f.checkout(allocated.CheckoutID)
		wantDeadline := time.Now().Add(cfg.AvailabilityGrace).Unix()
		if row.UnavailableSince != time.Now().Unix() || row.AvailabilityDeadline != wantDeadline {
			t.Fatalf("availability clock = (%d, %d), want (%d, %d)",
				row.UnavailableSince, row.AvailabilityDeadline, time.Now().Unix(), wantDeadline)
		}
		if entry.RetryAt != wantDeadline {
			t.Fatalf("retry deadline = %d, want %d", entry.RetryAt, wantDeadline)
		}

		synctest.Sleep(cfg.AvailabilityGrace - time.Second)
		entry = f.entry(f.reconcile(), "wt")
		if entry.Action != ActionAvailabilityHeld || entry.State != store_sqlite.CheckoutStateAvailabilityGrace {
			t.Fatalf("one second before the deadline = %+v", entry)
		}
		if entry.RetryAt != wantDeadline {
			t.Fatalf("held retry deadline = %d, want %d", entry.RetryAt, wantDeadline)
		}
		if n := f.hooks.countPrefix("purge:"); n != 0 {
			t.Fatalf("layers purged %d times before the deadline", n)
		}

		synctest.Sleep(time.Second)
		entry = f.entry(f.reconcile(), "wt")
		if entry.Action != ActionForgotten || entry.State != "" {
			t.Fatalf("at the deadline = %+v", entry)
		}
		f.assertNoCheckoutRows(allocated.CheckoutID)
		if n := f.hooks.countPrefix("purge:"); n != 1 {
			t.Fatalf("layers purged %d times at the deadline, want 1", n)
		}

		for range 3 {
			synctest.Sleep(time.Minute)
			f.reconcile()
		}
		f.assertNoCheckoutRows(allocated.CheckoutID)
		if n := f.hooks.countPrefix("purge:"); n != 1 {
			t.Fatalf("layers purged %d times overall, want exactly 1", n)
		}
	})
}

// TestAvailabilityRecoveryKeepsIdentity proves an outage that ends inside the
// grace window costs nothing: same id, same incarnation, clocks cleared, no
// purge.
func TestAvailabilityRecoveryKeepsIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := Config{AvailabilityGrace: 30 * time.Second, RemovalGrace: 30 * time.Second}
		f := newFixture(t, cfg)
		f.seedPrimaryGraph("graph-primary")
		f.git.setRecords(presentRecord("wt", "/repo/wt"))
		f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)
		allocated := f.entry(f.reconcile(), "wt")

		unreachable := presentRecord("wt", "/repo/wt")
		unreachable.RootAccessible = false
		unreachable.RootErr = errors.New("device not configured")
		f.git.setRecords(unreachable)
		f.git.samples["/repo/wt"] = gitSampleAbsent(volumeB)
		f.reconcile()

		synctest.Sleep(cfg.AvailabilityGrace - time.Second)
		f.git.setRecords(presentRecord("wt", "/repo/wt"))
		f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)

		entry := f.entry(f.reconcile(), "wt")
		if entry.Action != ActionAvailabilityRecovered || entry.State != store_sqlite.CheckoutStateReady {
			t.Fatalf("recovery = %+v", entry)
		}
		if entry.CheckoutID != allocated.CheckoutID || entry.Incarnation != allocated.Incarnation {
			t.Fatalf("recovery re-keyed the identity: %+v, want %s/%s",
				entry, allocated.CheckoutID, allocated.Incarnation)
		}
		row := f.checkout(allocated.CheckoutID)
		if row.UnavailableSince != 0 || row.AvailabilityDeadline != 0 {
			t.Fatalf("recovery left the availability clock running: %+v", row)
		}
		if row.LastAccessible != time.Now().Unix() {
			t.Fatalf("last_accessible = %d, want %d", row.LastAccessible, time.Now().Unix())
		}
		if n := f.hooks.countPrefix("purge:"); n != 0 {
			t.Fatalf("a recovery inside the grace window purged %d times", n)
		}
	})
}

// TestRemovalClockIsIndependentOfAvailability is the two-axis rule: a checkout
// inside availability grace still gets a full, fresh removal window once an
// authoritative removal is evidenced.
func TestRemovalClockIsIndependentOfAvailability(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := Config{AvailabilityGrace: 10 * time.Minute, RemovalGrace: 2 * time.Minute}
		f := newFixture(t, cfg)
		f.seedPrimaryGraph("graph-primary")
		f.git.setRecords(presentRecord("wt", "/repo/wt"))
		f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)
		allocated := f.entry(f.reconcile(), "wt")

		// Start the availability clock, but remain inside that grace when Git
		// later provides authoritative removal evidence.
		unreachable := presentRecord("wt", "/repo/wt")
		unreachable.RootAccessible = false
		unreachable.RootErr = errors.New("device not configured")
		f.git.setRecords(unreachable)
		f.git.samples["/repo/wt"] = gitSampleAbsent(volumeB)
		f.reconcile()
		synctest.Sleep(time.Minute)
		unavailableSince := f.checkout(allocated.CheckoutID).UnavailableSince

		// Now git prunes it away: a removal is evidenced for the first time.
		f.git.setRecords()
		removalStarted := time.Now()
		entry := f.entry(f.reconcile(), "wt")
		if entry.Action != ActionRemovalGraceStarted {
			t.Fatalf("first evidenced removal = %+v", entry)
		}
		if entry.Classification.Evidence != EvidenceAuthoritativeOmission {
			t.Fatalf("evidence = %q", entry.Classification.Evidence)
		}
		row := f.checkout(allocated.CheckoutID)
		if entry.State != store_sqlite.CheckoutStateRemovalGrace || row.State != store_sqlite.CheckoutStateRemovalGrace {
			t.Fatalf("removal grace state = report %q catalog %q", entry.State, row.State)
		}
		if row.RemovalDetectedAt != removalStarted.Unix() {
			t.Fatalf("removal_detected_at = %d, want %d", row.RemovalDetectedAt, removalStarted.Unix())
		}
		if want := removalStarted.Add(cfg.RemovalGrace).Unix(); row.RemovalDeadline != want {
			t.Fatalf("removal_deadline = %d, want %d — the removal window must start from zero",
				row.RemovalDeadline, want)
		}
		if row.UnavailableSince != unavailableSince {
			t.Fatalf("the removal disturbed the availability axis: %d, want %d",
				row.UnavailableSince, unavailableSince)
		}

		// Just before the removal deadline, still nothing is deleted.
		synctest.Sleep(cfg.RemovalGrace - time.Second)
		if entry := f.entry(f.reconcile(), "wt"); entry.Action != ActionRemovalHeld {
			t.Fatalf("one second before the removal deadline = %q", entry.Action)
		}
		f.checkout(allocated.CheckoutID)

		// At the deadline the checkout is forgotten outright.
		synctest.Sleep(time.Second)
		entry = f.entry(f.reconcile(), "wt")
		if entry.Action != ActionForgotten {
			t.Fatalf("at the removal deadline = %q, want forgotten", entry.Action)
		}
		f.assertNoCheckoutRows(allocated.CheckoutID)
		if entries := f.journal(); len(entries) != 0 {
			t.Fatalf("a completed forget left %d journal entries: %+v", len(entries), entries)
		}

		// The path coming back is a different checkout, not the old one.
		f.git.setRecords(presentRecord("wt", "/repo/wt"))
		f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)
		reborn := f.entry(f.reconcile(), "wt")
		if reborn.Action != ActionIdentityAllocated {
			t.Fatalf("re-observation = %q, want a fresh allocation", reborn.Action)
		}
		if reborn.CheckoutID == allocated.CheckoutID || reborn.Incarnation == allocated.Incarnation {
			t.Fatalf("a forgotten checkout came back with its old identity: %+v", reborn)
		}
	})
}

// TestRemovalCancelledBySameIncarnation proves a delete-then-recreate inside
// the removal grace costs nothing.
func TestRemovalCancelledBySameIncarnation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := Config{AvailabilityGrace: time.Minute, RemovalGrace: time.Minute}
		f := newFixture(t, cfg)
		f.seedPrimaryGraph("graph-primary")
		f.git.setRecords(presentRecord("wt", "/repo/wt"))
		f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)
		allocated := f.entry(f.reconcile(), "wt")

		f.git.setRecords()
		if entry := f.entry(f.reconcile(), "wt"); entry.Action != ActionRemovalGraceStarted {
			t.Fatalf("removal detection = %q", entry.Action)
		}

		synctest.Sleep(cfg.RemovalGrace - time.Second)
		f.git.setRecords(presentRecord("wt", "/repo/wt"))
		entry := f.entry(f.reconcile(), "wt")
		if entry.Action != ActionRemovalCancelled || entry.State != store_sqlite.CheckoutStateReady {
			t.Fatalf("reappearance = %+v", entry)
		}
		if entry.CheckoutID != allocated.CheckoutID || entry.Incarnation != allocated.Incarnation {
			t.Fatalf("reappearance re-keyed the identity: %+v", entry)
		}
		row := f.checkout(allocated.CheckoutID)
		if row.RemovalDetectedAt != 0 || row.RemovalDeadline != 0 || row.RemovalEvidence != "" {
			t.Fatalf("removal clock survived the reappearance: %+v", row)
		}

		// Well past the original deadline, nothing is deleted.
		synctest.Sleep(2 * cfg.RemovalGrace)
		if entry := f.entry(f.reconcile(), "wt"); entry.Action != ActionReadyConfirmed {
			t.Fatalf("after the cancelled deadline = %q", entry.Action)
		}
		f.checkout(allocated.CheckoutID)
	})
}

// TestPrunableRemovalUsesVolumeEvidence drives the second removal proof: git
// keeps listing the worktree, but calls it prunable while the filesystem shows
// the root gone from a volume that is still mounted.
func TestPrunableRemovalUsesVolumeEvidence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := Config{AvailabilityGrace: time.Minute, RemovalGrace: time.Minute}
		f := newFixture(t, cfg)
		f.seedPrimaryGraph("graph-primary")
		f.git.setRecords(presentRecord("wt", "/repo/wt"))
		f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)
		allocated := f.entry(f.reconcile(), "wt")

		pruned := presentRecord("wt", "/repo/wt")
		pruned.RootAccessible = false
		pruned.RootErr = fmt.Errorf("lstat /repo/wt: %w", fs.ErrNotExist)
		pruned.Prunable = true
		f.git.setRecords(pruned)

		// A different volume under the missing root is not proof.
		f.git.samples["/repo/wt"] = gitSampleAbsent(volumeB)
		entry := f.entry(f.reconcile(), "wt")
		if entry.Classification.Disposition != DispositionInaccessible {
			t.Fatalf("a moved volume classified %q", entry.Classification.Disposition)
		}

		// The same volume is.
		f.git.samples["/repo/wt"] = gitSampleAbsent(volumeA)
		entry = f.entry(f.reconcile(), "wt")
		if entry.Classification.Evidence != EvidencePrunableConfirmed {
			t.Fatalf("evidence = %q, want prunable confirmed (%s)",
				entry.Classification.Evidence, entry.Classification.Detail)
		}
		if entry.Action != ActionRemovalGraceStarted {
			t.Fatalf("action = %q", entry.Action)
		}
		if row := f.checkout(allocated.CheckoutID); row.RemovalEvidence != string(EvidencePrunableConfirmed) {
			t.Fatalf("stored evidence = %q", row.RemovalEvidence)
		}

		synctest.Sleep(cfg.RemovalGrace)
		if entry := f.entry(f.reconcile(), "wt"); entry.Action != ActionForgotten {
			t.Fatalf("expiry = %q", entry.Action)
		}
		f.assertNoCheckoutRows(allocated.CheckoutID)
	})
}

// TestStaleIncarnationCannotAdvanceOrDelete drives two reconcilers over the
// same store. The one holding the previous incarnation must lose every guarded
// write, and must not be able to delete the identity that replaced it.
func TestStaleIncarnationCannotAdvanceOrDelete(t *testing.T) {
	f := newFixture(t, Default())
	f.seedPrimaryGraph("graph-primary")
	f.git.setRecords(presentRecord("wt", "/repo/wt"))
	f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)
	first := f.entry(f.reconcile(), "wt")

	// A second actor re-keys the row: same admin name, new incarnation. That
	// is what a removed-and-recreated path looks like to the catalog.
	ctx := context.Background()
	row := f.checkout(first.CheckoutID)
	row.Incarnation = "incarnation-2"
	if err := f.catalog.UpsertCheckout(ctx, row); err != nil {
		t.Fatalf("re-key the checkout: %v", err)
	}

	// The first reconciler's own view of the row is now stale. Writing it back
	// must be refused rather than silently reverting the other actor.
	stale := observationFrom(store_sqlite.Checkout{
		CheckoutID: first.CheckoutID, Incarnation: first.Incarnation,
		State: store_sqlite.CheckoutStateUnavailable,
	})
	if err := f.catalog.UpdateCheckoutObservation(ctx, stale); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("stale observation = %v, want a stale guard", err)
	}
	if got := f.checkout(first.CheckoutID); got.State != store_sqlite.CheckoutStateReady {
		t.Fatalf("a stale write advanced the state to %q", got.State)
	}

	// The same guard protects deletion.
	err := f.rec.ForgetCheckout(ctx, first.CheckoutID, first.Incarnation)
	if !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("ForgetCheckout with a stale incarnation = %v, want a stale guard", err)
	}
	f.checkout(first.CheckoutID)
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("a refused forget wrote %d journal entries", len(entries))
	}

	// A pass by the reconciler that lost the race reports the loss instead of
	// overwriting the winner. It sees the row's live incarnation, so to model
	// the loser the row is re-keyed again between its read and its write.
	loser, err := New(f.catalog, f.hooks, Default(),
		WithInventory(f.git.inventory), WithPathSampler(f.git.sample), WithHEADSampler(f.git.head))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)
	racing := &racingSampler{root: "/repo/wt", sample: f.git.sample, race: func() {
		f.rekey(first.CheckoutID, "incarnation-3")
	}}
	WithPathSampler(racing.sampleAndRace)(loser)
	report, err := loser.ReconcileFamily(ctx, f.familyID, "/repo")
	if err != nil {
		t.Fatalf("losing pass: %v", err)
	}
	if entry := f.entry(report, "wt"); entry.Action != ActionGuardLost {
		t.Fatalf("losing pass action = %q, want guard_lost", entry.Action)
	}
	if got := f.checkout(first.CheckoutID); got.Incarnation != "incarnation-3" {
		t.Fatalf("the loser overwrote the winner's incarnation: %q", got.Incarnation)
	}
}

// racingSampler runs another actor's write the first time one root is
// sampled. A pass samples a root after it has read the catalog and before it
// writes anything back, so that moment is exactly the window a competing actor
// has to win — which is the only way to reach the guard-loss branches from
// outside the catalog.
type racingSampler struct {
	root   string
	race   func()
	sample PathSamplerFunc
	done   bool
}

func (s *racingSampler) sampleAndRace(root string) gitstate.PathEvidence {
	if !s.done && root == s.root {
		s.done = true
		s.race()
	}
	return s.sample(root)
}

// TestAllocationRaceLeavesOneIdentity drives two allocators at one working
// copy. The pass listed the family before the other actor's row existed, so
// only the guard on the insert can stop it from minting a second identity for
// the same administrative name.
func TestAllocationRaceLeavesOneIdentity(t *testing.T) {
	f := newFixture(t, Default())
	f.seedPrimaryGraph("graph-primary")
	f.git.setRecords(presentRecord("wt", "/repo/wt"))
	f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)

	racing := &racingSampler{root: "/repo/wt", sample: f.git.sample, race: func() {
		f.seedCheckout("co-rival", "inc-rival", "wt", store_sqlite.CheckoutModeAutomatic)
	}}
	WithPathSampler(racing.sampleAndRace)(f.rec)

	entry := f.entry(f.reconcile(), "wt")
	if entry.Action != ActionGuardLost {
		t.Fatalf("losing allocation = %q, want guard_lost", entry.Action)
	}
	if entry.Durable || entry.CheckoutID != "" {
		t.Fatalf("the losing pass claimed an identity: %+v", entry)
	}
	if rows := f.checkouts(); len(rows) != 1 || rows[0].CheckoutID != "co-rival" {
		t.Fatalf("family holds %+v, want only the winner's identity", rows)
	}

	// The next pass matches the record to the winner's row rather than trying
	// to allocate beside it again.
	WithPathSampler(f.git.sample)(f.rec)
	next := f.entry(f.reconcile(), "wt")
	if next.CheckoutID != "co-rival" || next.Action != ActionReadyConfirmed {
		t.Fatalf("second pass = %+v, want the winner confirmed", next)
	}
	if rows := f.checkouts(); len(rows) != 1 {
		t.Fatalf("second pass left %d checkout rows, want 1", len(rows))
	}
}

// TestObservationKeepsTheModeAxis proves a pass reports on a checkout without
// touching how it is served. A promotion commits inside the pass's own read-to-
// write window and under the same incarnation, so nothing about the guard can
// catch it: the pass may not revert it, which it can only manage by leaving the
// mode columns out of its write.
func TestObservationKeepsTheModeAxis(t *testing.T) {
	f := newFixture(t, Default())
	f.seedPrimaryGraph("graph-primary")
	f.seedCheckout("co-1", "inc-1", "wt", store_sqlite.CheckoutModeAutomatic)
	f.git.setRecords(presentRecord("wt", "/repo/wt"))
	f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)

	racing := &racingSampler{root: "/repo/wt", sample: f.git.sample, race: func() {
		err := f.catalog.UpdateCheckoutState(context.Background(), store_sqlite.UpdateCheckoutStateRequest{
			CheckoutID:    "co-1",
			Incarnation:   "inc-1",
			State:         store_sqlite.CheckoutStateReady,
			DesiredMode:   store_sqlite.CheckoutModeDedicated,
			EffectiveMode: store_sqlite.CheckoutModeDedicated,
		})
		if err != nil {
			t.Fatalf("promote mid-pass: %v", err)
		}
	}}
	WithPathSampler(racing.sampleAndRace)(f.rec)

	if entry := f.entry(f.reconcile(), "wt"); entry.Action != ActionReadyConfirmed {
		t.Fatalf("action = %q", entry.Action)
	}
	row := f.checkout("co-1")
	if row.DesiredMode != store_sqlite.CheckoutModeDedicated ||
		row.EffectiveMode != store_sqlite.CheckoutModeDedicated {
		t.Fatalf("the pass reverted a promotion it never saw: %q/%q", row.DesiredMode, row.EffectiveMode)
	}
	if row.State != store_sqlite.CheckoutStateReady || row.LastSeen == 0 {
		t.Fatalf("the observation itself did not land: %+v", row)
	}
}

// TestRemovalGuardLossKeepsThePassGoing covers the forget path's stale guard.
// Another actor re-keys one checkout while the pass is deciding to forget it;
// the pass has to report the loss for that checkout and still reconcile the
// rest of the family.
func TestRemovalGuardLossKeepsThePassGoing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := Config{AvailabilityGrace: time.Minute, RemovalGrace: time.Minute}
		f := newFixture(t, cfg)
		f.seedPrimaryGraph("graph-primary")
		// The admin names order the pass: the racy one is reconciled first, so
		// a pass that aborted on its guard loss would never reach the other.
		f.git.setRecords(presentRecord("wt-a", "/repo/wt-a"), presentRecord("wt-b", "/repo/wt-b"))
		f.git.samples["/repo/wt-a"] = gitSampleExisting(volumeA)
		f.git.samples["/repo/wt-b"] = gitSampleExisting(volumeA)
		first := f.reconcile()
		racy := f.entry(first, "wt-a")
		calm := f.entry(first, "wt-b")

		// git stops listing both: two removal clocks start together.
		f.git.setRecords()
		f.reconcile()
		synctest.Sleep(cfg.RemovalGrace)

		racing := &racingSampler{root: "/repo/wt-a", sample: f.git.sample, race: func() {
			f.rekey(racy.CheckoutID, "incarnation-2")
		}}
		WithPathSampler(racing.sampleAndRace)(f.rec)
		report := f.reconcile()

		if entry := f.entry(report, "wt-a"); entry.Action != ActionGuardLost {
			t.Fatalf("re-keyed checkout = %q, want guard_lost", entry.Action)
		}
		if entry := f.entry(report, "wt-b"); entry.Action != ActionForgotten {
			t.Fatalf("the pass stopped at the guard loss: wt-b = %q", entry.Action)
		}
		if row := f.checkout(racy.CheckoutID); row.Incarnation != "incarnation-2" {
			t.Fatalf("the losing pass wrote over the winner: %+v", row)
		}
		f.assertNoCheckoutRows(calm.CheckoutID)
		if entries := f.journal(); len(entries) != 0 {
			t.Fatalf("the pass left journal entries: %+v", entries)
		}
	})
}

// TestPrimaryClosureGuardLossIsReported covers the other saga entry point a
// removal can reach. The family's primary epoch moves after the pass read it,
// which is the catalog telling the pass that the primary it was about to
// retire is not the one that is current.
func TestPrimaryClosureGuardLossIsReported(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := Config{AvailabilityGrace: time.Minute, RemovalGrace: time.Minute}
		f := newFixture(t, cfg)
		seedForgettableCheckout(f, "co-primary", "inc-p", "wt", "graph-primary")
		f.promoteToPrimary("graph-primary")

		// git never lists it, so the removal is evidenced on the first pass.
		f.git.samples["/repo/wt"] = gitSampleAbsent(volumeA)
		if entry := f.entry(f.reconcile(), "wt"); entry.Action != ActionRemovalGraceStarted {
			t.Fatalf("removal detection = %q", entry.Action)
		}
		synctest.Sleep(cfg.RemovalGrace)

		racing := &racingSampler{root: "/repo/wt", sample: f.git.sample, race: func() {
			// Re-installing the same primary is the cheapest way to move the
			// epoch the pass is holding without changing anything else.
			f.promoteToPrimary("graph-primary")
		}}
		WithPathSampler(racing.sampleAndRace)(f.rec)

		if entry := f.entry(f.reconcile(), "wt"); entry.Action != ActionGuardLost {
			t.Fatalf("stale primary epoch = %q, want guard_lost", entry.Action)
		}
		f.checkout("co-primary")
		if !f.graphExists("graph-primary") {
			t.Error("a refused retirement still released the primary graph")
		}
		if !f.familyExists() {
			t.Error("a refused retirement still forgot the family")
		}
		if entries := f.journal(); len(entries) != 0 {
			t.Fatalf("a refused retirement wrote journal entries: %+v", entries)
		}

		// The next pass, holding the current epoch, retires it for real.
		WithPathSampler(f.git.sample)(f.rec)
		if entry := f.entry(f.reconcile(), "wt"); entry.Action != ActionPrimaryClosureRetired {
			t.Fatalf("retry after the guard loss = %q", entry.Action)
		}
		f.assertNoCheckoutRows("co-primary")
	})
}

// TestReconcileRecordsHeadAndPathEvidence proves the pass writes back what it
// observed: HEAD from the sampler, and a fresh filesystem sample whose
// generation advances every accessible reconciliation.
func TestReconcileRecordsHeadAndPathEvidence(t *testing.T) {
	f := newFixture(t, Default())
	f.seedPrimaryGraph("graph-primary")
	f.git.setRecords(presentRecord("wt", "/repo/wt"))
	f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)
	f.git.heads["/repo/wt"] = gitstate.HEADState{
		Ref:       "refs/heads/wt",
		CommitOID: "b1",
		TreeOID:   "t1",
	}

	allocated := f.entry(f.reconcile(), "wt")
	row := f.checkout(allocated.CheckoutID)
	if row.HeadRef != "refs/heads/wt" || row.HeadCommit != "b1" || row.HeadTree != "t1" {
		t.Fatalf("head columns = %q/%q/%q", row.HeadRef, row.HeadCommit, row.HeadTree)
	}
	// The pass joins the admin directory with the host separator, so the
	// slash-spelled fixture constant is compared through the same join.
	if row.GitDir != filepath.Join(testCommonDir, "worktrees", "wt") {
		t.Fatalf("git_dir = %q", row.GitDir)
	}

	ctx := context.Background()
	evidence, ok, err := f.catalog.GetCheckoutPathEvidence(ctx, allocated.CheckoutID)
	if err != nil || !ok {
		t.Fatalf("GetCheckoutPathEvidence = %v %v", ok, err)
	}
	if evidence.RootVolumeToken != volumeA || evidence.SampleGeneration != 1 {
		t.Fatalf("first sample = %+v", evidence)
	}

	f.reconcile()
	evidence, _, err = f.catalog.GetCheckoutPathEvidence(ctx, allocated.CheckoutID)
	if err != nil {
		t.Fatalf("GetCheckoutPathEvidence: %v", err)
	}
	if evidence.SampleGeneration != 2 {
		t.Fatalf("sample generation = %d, want 2", evidence.SampleGeneration)
	}

	// A HEAD the sampler cannot read falls back to what the inventory said,
	// and never invents a tree.
	delete(f.git.heads, "/repo/wt")
	f.reconcile()
	row = f.checkout(allocated.CheckoutID)
	if row.HeadCommit != strings.Repeat("a", 40) || row.HeadTree != "" {
		t.Fatalf("head fallback = %q/%q", row.HeadCommit, row.HeadTree)
	}

	// The main worktree reads the shared directory directly.
	f.git.setRecords(mainWorktreeRecord("/repo"))
	f.git.samples["/repo"] = gitSampleExisting(volumeA)
	main := f.entry(f.reconcile(), gitstate.MainAdminName)
	if got := f.checkout(main.CheckoutID); got.GitDir != testCommonDir {
		t.Fatalf("main git_dir = %q, want %q", got.GitDir, testCommonDir)
	}
}

// steppedClock is a clock a test moves by hand. It exists so the deadline
// arithmetic can be checked against an instant chosen by the test rather than
// against whatever the process clock happened to read.
type steppedClock struct{ now time.Time }

func (c *steppedClock) Now() time.Time          { return c.now }
func (c *steppedClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func TestWithClockDrivesEveryTimestamp(t *testing.T) {
	clock := &steppedClock{now: time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)}
	cfg := Config{AvailabilityGrace: 90 * time.Second, RemovalGrace: 10 * time.Minute}
	f := newFixture(t, cfg, WithClock(clock.Now))
	f.seedPrimaryGraph("graph-primary")
	f.git.setRecords(presentRecord("wt", "/repo/wt"))
	f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)

	allocation := clock.now
	allocated := f.entry(f.reconcile(), "wt")
	row := f.checkout(allocated.CheckoutID)
	if row.LastSeen != allocation.Unix() || row.LastAccessible != allocation.Unix() {
		t.Fatalf("allocation timestamps = (%d, %d), want %d",
			row.LastSeen, row.LastAccessible, allocation.Unix())
	}

	clock.advance(time.Hour)
	unreachable := presentRecord("wt", "/repo/wt")
	unreachable.RootAccessible = false
	unreachable.RootErr = errors.New("device not configured")
	f.git.setRecords(unreachable)
	f.git.samples["/repo/wt"] = gitSampleAbsent(volumeB)

	outage := clock.now
	f.reconcile()
	row = f.checkout(allocated.CheckoutID)
	if row.UnavailableSince != outage.Unix() {
		t.Fatalf("unavailable_since = %d, want %d", row.UnavailableSince, outage.Unix())
	}
	if want := outage.Add(cfg.AvailabilityGrace).Unix(); row.AvailabilityDeadline != want {
		t.Fatalf("availability_deadline = %d, want %d", row.AvailabilityDeadline, want)
	}

	clock.advance(24 * time.Hour)
	f.git.setRecords()
	removal := clock.now
	f.reconcile()
	row = f.checkout(allocated.CheckoutID)
	if row.RemovalDetectedAt != removal.Unix() {
		t.Fatalf("removal_detected_at = %d, want %d", row.RemovalDetectedAt, removal.Unix())
	}
	if want := removal.Add(cfg.RemovalGrace).Unix(); row.RemovalDeadline != want {
		t.Fatalf("removal_deadline = %d, want %d", row.RemovalDeadline, want)
	}
}

// TestRemovalOfThePrimaryOwnerRetiresTheClosure proves the expiry branch picks
// the closure teardown, not the plain forget, when the checkout that went away
// is the one its family's primary graph hangs off.
func TestRemovalOfThePrimaryOwnerRetiresTheClosure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		cfg := Config{AvailabilityGrace: time.Minute, RemovalGrace: time.Minute}
		f := newFixture(t, cfg)
		f.seedPrimaryGraph("graph-primary")
		f.git.setRecords(presentRecord("wt", "/repo/wt"))
		f.git.samples["/repo/wt"] = gitSampleExisting(volumeA)
		allocated := f.entry(f.reconcile(), "wt")

		// The allocated checkout takes ownership of the family's primary.
		err := f.catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
			GraphID:         "graph-primary",
			OwnerCheckoutID: allocated.CheckoutID,
			RepoPrefix:      "prefix-graph-primary",
			FamilyID:        f.familyID,
			IsPrimaryBase:   true,
			State:           "graph_ready",
		})
		if err != nil {
			t.Fatalf("attach the primary to its owner: %v", err)
		}
		f.seedRefView("graph-primary-main", "graph-primary")

		f.git.setRecords()
		if entry := f.entry(f.reconcile(), "wt"); entry.Action != ActionRemovalGraceStarted {
			t.Fatalf("removal detection = %q", entry.Action)
		}
		synctest.Sleep(cfg.RemovalGrace)
		entry := f.entry(f.reconcile(), "wt")
		if entry.Action != ActionPrimaryClosureRetired {
			t.Fatalf("expiry of the primary's owner = %q, want the closure retired", entry.Action)
		}

		f.assertNoCheckoutRows(allocated.CheckoutID)
		if f.graphExists("graph-primary") || f.refViewExists("graph-primary-main") {
			t.Error("the primary graph or its views survived the closure")
		}
		if f.primaryGraphID() != "" {
			t.Errorf("the family still has primary %q", f.primaryGraphID())
		}
		// Nothing is left to serve the family from, so it goes too.
		if f.familyExists() {
			t.Error("the family survived a closure that emptied it")
		}
		if entries := f.journal(); len(entries) != 0 {
			t.Fatalf("the closure left journal entries: %+v", entries)
		}
		want := []string{"purge:" + allocated.CheckoutID + ":" + allocated.Incarnation, "release:graph-primary"}
		if got := f.hooks.snapshot(); !slices.Equal(got, want) {
			t.Fatalf("hook calls = %v, want %v", got, want)
		}
	})
}
