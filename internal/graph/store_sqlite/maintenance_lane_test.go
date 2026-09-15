package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// The whole-database maintenance lane.
//
// ANALYZE, VACUUM and TRUNCATE checkpoints are one-per-file actions that no
// generation owns. These cases pin the properties that follow from that: a
// publish pays none of them inside its own window, the lane runs the one it
// owes afterwards and coalesces a burst of publishes into a single pass, a
// whole-file rewrite waits out the builds and drains in flight rather than
// starting underneath one, all three actions are mutually exclusive because
// each one is admitted by the same token, and no entry to the lane waits for
// that token without a bound.

// settleMaintenanceLane blocks until the lane has no pass running and none
// owed, so a case can observe the effect of a refresh the publish only
// scheduled. Fails the test rather than hanging if the lane never settles.
func settleMaintenanceLane(t *testing.T, store *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.waitMaintenanceIdle(ctx); err != nil {
		t.Fatalf("maintenance lane never settled: %v", err)
	}
}

// waitForCondition polls a predicate until it holds. Used for states another
// goroutine publishes (a publish window opening, a lane pass starting) where
// the only alternative is a sleep long enough to be a flake either way.
func waitForCondition(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// seedStalePlannerStats leaves the store with statistics that describe a
// fraction of it: an anchored fixture, then a generation's worth of counters on
// top, which is the growth verdict a publish must act on.
func seedStalePlannerStats(t *testing.T, store *Store) graph.PlannerStatsFreshness {
	t.Helper()
	seedNamedGoReceiverFixture(store, "a", 100)
	writeIndexStateCounters(t, store, 0, "repo", 200, 100)
	refreshStatsNow(t, store)
	before := mustHealth(t, store)
	if before.Stale {
		t.Fatalf("fixture started stale: %s", before.Reason)
	}
	return before
}

// A publish must not pay for database-wide maintenance itself. The lane is
// occupied for the whole call, so anything the publish tried to run inline
// would either block it or refresh the statistics; neither may happen. The
// refresh is still owed, and lands as soon as the lane frees up.
//
// Occupying the lane rather than timing the call is what makes this a real
// audit: with the refresh back inline (the pre-change shape) the gate is not
// consulted at all, the publish refreshes, and the Refreshes assertion below
// fails.
func TestPublishAndRoute_PaysNoMaintenanceInsideItsWindow(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	before := seedStalePlannerStats(t, store)

	// Hold the lane: no maintenance of any kind can run until this is
	// released, so whatever the publish does is the publish's own doing.
	if err := store.maintenanceGate.LockContext(ctx); err != nil {
		t.Fatalf("occupying the maintenance lane: %v", err)
	}
	laneHeld := true
	releaseLane := func() {
		if laneHeld {
			laneHeld = false
			store.maintenanceGate.Unlock()
		}
	}
	defer releaseLane()

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	if err := handle.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "repo", NodeCount: 400, EdgeCount: 200,
	}); err != nil {
		t.Fatalf("SetRepoIndexState on the generation handle: %v", err)
	}

	if err := store.PublishAndRoute(ctx, generationID, payloadCheckoutID, 0, RouteSlotDirty); err != nil {
		t.Fatalf("PublishAndRoute: %v", err)
	}

	// The publish window is closed: nothing may still be holding it open.
	if reason := store.maintenanceBusyReason(); reason != "" {
		t.Errorf("publish left its maintenance window open: %s", reason)
	}
	during := mustHealth(t, store)
	if during.Refreshes != before.Refreshes {
		t.Fatalf("publish refreshed planner statistics inside its own window (%d -> %d refreshes): maintenance must be scheduled, not charged to a publish",
			before.Refreshes, during.Refreshes)
	}
	if got := store.maintenanceRequests.Load(); got != 1 {
		t.Fatalf("publish scheduled %d maintenance requests, want exactly 1", got)
	}
	if got := store.maintenanceJobs.Load(); got != 0 {
		t.Fatalf("%d maintenance jobs reached their SQL while the lane was held, want 0", got)
	}

	// Freeing the lane is all the owed refresh was waiting for.
	releaseLane()
	settleMaintenanceLane(t, store)
	after := mustHealth(t, store)
	if after.Refreshes <= before.Refreshes {
		t.Fatalf("the lane never ran the refresh the publish owed (%d -> %d refreshes); stale=%v reason=%q",
			before.Refreshes, after.Refreshes, after.Stale, after.Reason)
	}
	if !strings.HasPrefix(after.LastRefreshReason, "growth:") {
		t.Errorf("last refresh reason = %q, want the growth verdict the publish handed to the lane", after.LastRefreshReason)
	}
	if got := store.maintenancePasses.Load(); got != 1 {
		t.Errorf("lane ran %d passes for one publish, want exactly 1", got)
	}
}

// A burst of publishes owes ONE further pass, not one per publish.
//
// The burst is issued against a pass that is demonstrably RUNNING — the lane
// token is held, so the pass it owes is parked inside the lane and cannot have
// read anything yet — because that is the case where a further pass is genuinely
// owed: a request that arrived while a pass was already reading describes
// payload that pass may not have seen. (The cheaper case, a burst arriving
// before its pass has started at all, coalesces into that single pass: the pass
// begins after the last request, so it reads what all of them left behind. The
// lane does that too; it is not asserted here because whether the worker has
// woken by then is a scheduling race.)
func TestMaintenanceLane_CoalescesRequestsIntoOnePass(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)

	if err := store.maintenanceGate.LockContext(ctx); err != nil {
		t.Fatalf("occupying the maintenance lane: %v", err)
	}

	store.schedulePublishMaintenance()
	waitForCondition(t, "the first lane pass to start", func() bool {
		return store.maintenancePasses.Load() >= 1
	})
	if got := store.maintenancePasses.Load(); got != 1 {
		t.Fatalf("%d lane passes started for one request, want 1", got)
	}

	const burst = 12
	for i := 0; i < burst; i++ {
		store.schedulePublishMaintenance()
	}
	if got := store.maintenanceRequests.Load(); got != burst+1 {
		t.Fatalf("recorded %d maintenance requests, want %d", got, burst+1)
	}
	if got := store.maintenancePasses.Load(); got != 1 {
		t.Fatalf("%d lane passes started while the lane was held, want 1", got)
	}

	store.maintenanceGate.Unlock()
	settleMaintenanceLane(t, store)
	if got := store.maintenancePasses.Load(); got != 2 {
		t.Fatalf("%d publishes arriving during one pass produced %d lane passes in total, want 2 (the pass in flight plus the single pass they coalesced into)",
			burst, got)
	}
}

// A publish window holds whole-file maintenance off. The window is pinned open
// by taking the write gate the publish's own drain must acquire, so the
// assertion is against a real publish in flight rather than a hand-set flag.
func TestPublishPayloadGeneration_BlocksWholeFileMaintenance(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)

	// Hold the write gate so the publish's drain blocks inside its window. The
	// release is deferred and idempotent: a failed assertion below unwinds
	// through it, so the publish goroutine is never left blocked on a gate no
	// one will hand back — Close takes the same gate and would deadlock.
	store.writeMu.Lock()
	gateHeld := true
	releaseGate := func() {
		if gateHeld {
			gateHeld = false
			store.writeMu.Unlock()
		}
	}
	defer releaseGate()
	published := make(chan error, 1)
	go func() { published <- store.PublishPayloadGeneration(ctx, generationID, 1000) }()

	waitForCondition(t, "the publish window to open", func() bool {
		return strings.Contains(store.maintenanceBusyReason(), "publish drains in flight")
	})

	// A whole-file job must refuse to run while that window stands, and must
	// not reach its own SQL.
	ran := false
	budget, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	err = store.runMaintenance(budget, maintenanceVacuum, true, func(context.Context) error {
		ran = true
		return nil
	})
	cancel()
	if ran {
		t.Fatal("whole-file maintenance ran inside an open publish window")
	}
	if !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("maintenance during a publish window returned %v, want ErrMaintenanceBusy", err)
	}
	if !strings.Contains(err.Error(), "publish drains in flight") {
		t.Errorf("deferral reason = %q, want it to name the publish drain it waited for", err)
	}

	releaseGate()
	if err := <-published; err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}
	if reason := store.maintenanceBusyReason(); reason != "" {
		t.Fatalf("publish left its window open after returning: %s", reason)
	}
	if got := store.maintenanceDeferrals.Load(); got != 1 {
		t.Errorf("recorded %d maintenance deferrals, want 1", got)
	}
}

// VACUUM waits out a payload build rather than starting the rewrite underneath
// one already in flight. Compact is the daemon's own entry point, so this is
// the boot path with a build in flight.
//
// The direction matters and is asserted in one direction only: the lane holds
// the rewrite off a build it can see, not a build off a rewrite already
// running. See the lane's own note in store_compact.go for why the reverse is
// deliberately absent.
func TestCompact_WaitsForAPayloadBuildInFlight(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)

	flight, leader, ready, err := store.JoinPayloadBuildFlight(ctx, generationID, false)
	if err != nil {
		t.Fatalf("JoinPayloadBuildFlight: %v", err)
	}
	if !leader || ready {
		t.Fatalf("want the physical build leader, got leader=%v ready=%v", leader, ready)
	}

	compacted := make(chan error, 1)
	go func() { compacted <- store.Compact() }()

	// Precondition: the build is what the lane can see, and it is what must
	// hold the rewrite off.
	waitForCondition(t, "the payload build to become visible to the lane", func() bool {
		return strings.Contains(store.maintenanceBusyReason(), "payload build in flight")
	})
	select {
	case err := <-compacted:
		t.Fatalf("Compact rewrote the file while a payload build was in flight (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}

	flight.Complete(nil)
	select {
	case err := <-compacted:
		if err != nil {
			t.Fatalf("Compact after the build finished: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Compact never ran after the payload build finished")
	}
	if got := store.maintenanceJobs.Load(); got == 0 {
		t.Error("the lane recorded no job for the VACUUM it ran")
	}

	// The store is still the store: the build's generation survived the
	// rewrite and remains writable.
	if node := store.AtGeneration(generationID).GetNode(payloadAdded); node == nil {
		t.Error("the building generation's payload did not survive the VACUUM")
	}
}

// Close must not leave a scheduled pass running against pools it is about to
// tear down, and must stay closed: a request arriving after it is dropped
// rather than starting a goroutine on a dead store.
func TestMaintenanceLane_ClosedStoreSchedulesNothing(t *testing.T) {
	store, err := openPristine(t, filepath.Join(t.TempDir(), "maintenance_lane_close.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.schedulePublishMaintenance()
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	passesAtClose := store.maintenancePasses.Load()
	store.schedulePublishMaintenance()
	settleMaintenanceLane(t, store)
	if got := store.maintenancePasses.Load(); got != passesAtClose {
		t.Fatalf("a closed store started %d more lane passes, want none", got-passesAtClose)
	}
}

// shortenMaintenanceBudget replaces the lane's one shared budget for the
// duration of a case, so a deferral that is real but 30 s away can be observed
// in a fraction of a second. Restored on cleanup; the three parallel cases in
// this package resume only after the sequential ones finish, and none of them
// enters the lane.
func shortenMaintenanceBudget(t *testing.T, d time.Duration) {
	t.Helper()
	previous := maintenanceQuiesceTimeout
	maintenanceQuiesceTimeout = d
	t.Cleanup(func() { maintenanceQuiesceTimeout = previous })
}

// An explicit TRUNCATE checkpoint is a whole-file action — it rewrites the
// main database from the WAL — so it is admitted by the lane and can never
// interleave with a VACUUM rewriting the same file.
//
// Both halves are asserted because they fail differently: the job counter
// catches a checkpoint that quietly stopped going through the lane (an ungated
// call still succeeds, so nothing else would notice), and the held-lane arm
// catches the mutual exclusion itself plus the deferral class the routing
// newly makes visible to callers.
func TestCheckpointWAL_RunsThroughTheMaintenanceLane(t *testing.T) {
	store := openPayloadStore(t)
	seedPayloadBase(t, store)

	// With the lane free, the checkpoint runs — and it is the LANE that ran
	// it. A checkpoint that bypasses the lane leaves this counter untouched.
	jobsBefore := store.maintenanceJobs.Load()
	if err := store.CheckpointWAL(); err != nil {
		t.Fatalf("CheckpointWAL with a free lane: %v", err)
	}
	jobsAfter := store.maintenanceJobs.Load()
	if jobsAfter != jobsBefore+1 {
		t.Fatalf("CheckpointWAL recorded %d lane jobs, want exactly one more than %d: a TRUNCATE checkpoint must be admitted by the maintenance lane, not run beside it",
			jobsAfter, jobsBefore)
	}

	// While another whole-file job holds the lane — a VACUUM is the one that
	// matters — the checkpoint defers instead of rewriting the same file
	// underneath it.
	shortenMaintenanceBudget(t, 300*time.Millisecond)
	if err := store.maintenanceGate.LockContext(context.Background()); err != nil {
		t.Fatalf("occupying the maintenance lane: %v", err)
	}
	laneHeld := true
	releaseLane := func() {
		if laneHeld {
			laneHeld = false
			store.maintenanceGate.Unlock()
		}
	}
	defer releaseLane()

	deferralsBefore := store.maintenanceDeferrals.Load()
	deferred := make(chan error, 1)
	go func() { deferred <- store.CheckpointWAL() }()
	select {
	case err := <-deferred:
		if !errors.Is(err, ErrMaintenanceBusy) {
			t.Fatalf("CheckpointWAL against a held lane returned %v, want ErrMaintenanceBusy", err)
		}
		if !strings.Contains(err.Error(), string(maintenanceCheckpoint)) {
			t.Errorf("deferral = %q, want it to name the %s job it was refused for", err, maintenanceCheckpoint)
		}
		if !strings.Contains(err.Error(), "lane occupied") {
			t.Errorf("deferral = %q, want it to name the lane as the thing it could not get", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("CheckpointWAL never returned while the lane was held: the explicit checkpoint must defer, not queue behind a file rewrite")
	}
	if got := store.maintenanceJobs.Load(); got != jobsAfter {
		t.Errorf("%d checkpoint jobs reached their SQL while the lane was held, want 0", got-jobsAfter)
	}
	if got := store.maintenanceDeferrals.Load(); got != deferralsBefore+1 {
		t.Errorf("recorded %d deferrals for the refused checkpoint, want 1", got-deferralsBefore)
	}

	// The converse: the refusal is about the lane and nothing else. Handing
	// the token back is all the checkpoint was waiting for.
	releaseLane()
	if err := store.CheckpointWAL(); err != nil {
		t.Fatalf("CheckpointWAL after the lane was freed: %v", err)
	}
	if got := store.maintenanceJobs.Load(); got != jobsAfter+1 {
		t.Fatalf("the freed lane ran %d further checkpoint jobs, want exactly 1", got-jobsAfter)
	}
}

// VACUUM refuses while a bulk writer is pinned, for the reason the WAL
// checkpoint already refuses: a coordinated cold load holds one connection
// carrying its own PRAGMAs, and rewriting the file against it would either
// fail on that connection's state or discard the settings FlushBulk has to
// restore. Deferring is the honest answer — the freelist stays reusable and
// the next boot asks again.
func TestCompact_DefersWhileABulkWriterIsPinned(t *testing.T) {
	store := openPayloadStore(t)
	if !store.BeginCoordinatedBulkLoad() {
		t.Fatal("BeginCoordinatedBulkLoad did not engage on a fresh on-disk store: the fixture cannot pin a bulk writer")
	}
	if store.bulkConn == nil {
		t.Fatal("the coordinated bulk window left no pinned connection")
	}

	err := store.Compact()
	if !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("Compact with a bulk writer pinned returned %v, want ErrMaintenanceBusy: VACUUM must not rewrite the file against a pinned bulk connection", err)
	}
	if !strings.Contains(err.Error(), "bulk writer pinned") {
		t.Errorf("deferral = %q, want it to name the pinned bulk writer", err)
	}

	// The converse: closing the bulk window is all the rewrite was waiting
	// for, so the refusal cannot be a permanently stuck Compact.
	if err := store.EndCoordinatedBulkLoad(); err != nil {
		t.Fatalf("EndCoordinatedBulkLoad: %v", err)
	}
	if err := store.Compact(); err != nil {
		t.Fatalf("Compact after the bulk window closed: %v", err)
	}
}

// Compact enters the lane from the daemon's warmup path with
// context.Background() (cmd/gortex/daemon_state.go -> maybeCompactStore), so
// the wait for the lane token has to be bounded by the job's own budget. An
// unbounded acquisition there turns one long lane pass into a stalled boot.
func TestCompact_DoesNotWaitForeverOnALaneItCannotGet(t *testing.T) {
	shortenMaintenanceBudget(t, 300*time.Millisecond)
	store := openPayloadStore(t)

	if err := store.maintenanceGate.LockContext(context.Background()); err != nil {
		t.Fatalf("occupying the maintenance lane: %v", err)
	}
	defer store.maintenanceGate.Unlock()

	compacted := make(chan error, 1)
	go func() { compacted <- store.Compact() }()
	select {
	case err := <-compacted:
		if !errors.Is(err, ErrMaintenanceBusy) {
			t.Fatalf("Compact against a held lane returned %v, want ErrMaintenanceBusy", err)
		}
		if !strings.Contains(err.Error(), "lane occupied") {
			t.Errorf("deferral = %q, want it to name the lane it could not get", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Compact never gave up on a lane it could not get: it enters with context.Background(), so an unbounded acquisition stalls the daemon's warmup behind another job")
	}
}

// The DAEMON's publish — the physical one the generation builder performs as
// its last step (internal/indexer/builder_generation.go calls exactly this
// method) — is the boundary that owes the issue-#651 planner-statistics
// refresh, and it must schedule it rather than pay for it.
//
// This is the wiring case, not a repeat of the PublishAndRoute one: that API
// has no non-test caller, so a lane fed only from it would be a facility no
// daemon ever reaches. Holding the lane for the whole publish is what makes the
// audit real — with the refresh run inline the gate is not consulted at all and
// the Refreshes assertion below fails; with no ask at all the refresh never
// lands, which is the #651 regression.
func TestPublishPayloadGeneration_SchedulesTheRefreshItOwes(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	before := seedStalePlannerStats(t, store)

	if err := store.maintenanceGate.LockContext(ctx); err != nil {
		t.Fatalf("occupying the maintenance lane: %v", err)
	}
	laneHeld := true
	releaseLane := func() {
		if laneHeld {
			laneHeld = false
			store.maintenanceGate.Unlock()
		}
	}
	defer releaseLane()

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	// The growth the publish hands to the lane: this generation's counters on
	// top of the anchored fixture are what makes the verdict stale.
	if err := handle.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "repo", NodeCount: 400, EdgeCount: 200,
	}); err != nil {
		t.Fatalf("SetRepoIndexState on the generation handle: %v", err)
	}

	if err := store.PublishPayloadGeneration(ctx, generationID, 1000); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}

	if reason := store.maintenanceBusyReason(); reason != "" {
		t.Errorf("the publish left its maintenance window open: %s", reason)
	}
	during := mustHealth(t, store)
	if during.Refreshes != before.Refreshes {
		t.Fatalf("the daemon's physical publish refreshed planner statistics inside its own window (%d -> %d refreshes): the ANALYZE belongs to the lane, not to a generation's publish",
			before.Refreshes, during.Refreshes)
	}
	if got := store.maintenanceRequests.Load(); got != 1 {
		t.Fatalf("PublishPayloadGeneration scheduled %d maintenance requests, want exactly 1: the physical publish is the boundary that owes the #651 refresh",
			got)
	}
	if got := store.maintenanceJobs.Load(); got != 0 {
		t.Fatalf("%d maintenance jobs reached their SQL while the lane was held, want 0", got)
	}

	// A publish that did not happen owes nothing: the generation is ready now,
	// so this second attempt is refused and must schedule no further pass.
	if err := store.PublishPayloadGeneration(ctx, generationID, 1000); err == nil {
		t.Fatal("publishing an already-published generation succeeded; the fixture cannot exercise the failed-publish arm")
	}
	if got := store.maintenanceRequests.Load(); got != 1 {
		t.Errorf("a refused publish scheduled maintenance (%d requests, want 1)", got)
	}

	releaseLane()
	settleMaintenanceLane(t, store)
	after := mustHealth(t, store)
	if after.Refreshes <= before.Refreshes {
		t.Fatalf("the lane never ran the refresh the daemon's publish owed (%d -> %d refreshes); stale=%v reason=%q",
			before.Refreshes, after.Refreshes, after.Stale, after.Reason)
	}
	if !strings.HasPrefix(after.LastRefreshReason, "growth:") {
		t.Errorf("last refresh reason = %q, want the growth verdict the publish handed to the lane", after.LastRefreshReason)
	}
	if got := store.maintenancePasses.Load(); got != 1 {
		t.Errorf("lane ran %d passes for one publish, want exactly 1", got)
	}
}

// The two whole-file rewrites exclude each other, in both directions, through
// their REAL entry points: Compact (the daemon's boot compaction) and
// CheckpointWAL (the indexer's read boundary). One file cannot be rewritten by
// a VACUUM and a TRUNCATE checkpoint at the same time.
//
// The lane token is held by a genuine job in each arm rather than by the test:
// both jobs take the write gate as their first act inside the lane, so holding
// that gate parks a real VACUUM (or a real checkpoint) inside the lane for as
// long as the case needs, and the lane-job counter is the deterministic signal
// that it is in there. What the other entry point then does — defer, naming the
// job and the lane, without reaching any SQL — is the mutual exclusion itself.
func TestCompactAndCheckpointWALExcludeEachOther(t *testing.T) {
	shortenMaintenanceBudget(t, 300*time.Millisecond)
	store := openPayloadStore(t)
	seedPayloadBase(t, store)

	store.writeMu.Lock()
	gateHeld := true
	releaseGate := func() {
		if gateHeld {
			gateHeld = false
			store.writeMu.Unlock()
		}
	}
	defer releaseGate()

	// Arm 1: a VACUUM is inside the lane; the explicit checkpoint must not be.
	jobsBefore := store.maintenanceJobs.Load()
	compacted := make(chan error, 1)
	go func() { compacted <- store.Compact() }()
	waitForCondition(t, "the VACUUM to take the lane", func() bool {
		return store.maintenanceJobs.Load() > jobsBefore
	})

	err := store.CheckpointWAL()
	if !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("CheckpointWAL while a VACUUM held the lane returned %v, want ErrMaintenanceBusy: a TRUNCATE checkpoint must never rewrite the file underneath a VACUUM", err)
	}
	if !strings.Contains(err.Error(), string(maintenanceCheckpoint)) {
		t.Errorf("deferral = %q, want it to name the %s job", err, maintenanceCheckpoint)
	}
	if !strings.Contains(err.Error(), "lane occupied") {
		t.Errorf("deferral = %q, want it to name the lane as the thing it could not get", err)
	}
	if got := store.maintenanceJobs.Load(); got != jobsBefore+1 {
		t.Fatalf("%d jobs reached their SQL while the VACUUM held the lane, want only the VACUUM's own", got-jobsBefore)
	}

	releaseGate()
	select {
	case err := <-compacted:
		if err != nil {
			t.Fatalf("Compact after the write gate was handed back: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Compact never finished after the write gate was handed back")
	}

	// Arm 2: the converse. A checkpoint inside the lane holds the VACUUM off,
	// which is the direction that matters for the daemon's boot compaction.
	store.writeMu.Lock()
	gateHeld = true
	jobsBefore = store.maintenanceJobs.Load()
	checkpointed := make(chan error, 1)
	go func() { checkpointed <- store.CheckpointWAL() }()
	waitForCondition(t, "the checkpoint to take the lane", func() bool {
		return store.maintenanceJobs.Load() > jobsBefore
	})

	err = store.Compact()
	if !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("Compact while a TRUNCATE checkpoint held the lane returned %v, want ErrMaintenanceBusy", err)
	}
	if !strings.Contains(err.Error(), string(maintenanceVacuum)) {
		t.Errorf("deferral = %q, want it to name the %s job", err, maintenanceVacuum)
	}
	if !strings.Contains(err.Error(), "lane occupied") {
		t.Errorf("deferral = %q, want it to name the lane as the thing it could not get", err)
	}
	if got := store.maintenanceJobs.Load(); got != jobsBefore+1 {
		t.Fatalf("%d jobs reached their SQL while the checkpoint held the lane, want only the checkpoint's own", got-jobsBefore)
	}

	releaseGate()
	select {
	case err := <-checkpointed:
		if err != nil {
			t.Fatalf("CheckpointWAL after the write gate was handed back: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("CheckpointWAL never finished after the write gate was handed back")
	}
}

// The lane's worker and its lifetime belong to the STORE, created by Open —
// not to the first publish that needs them.
//
// This is not tidiness. A publish can happen anywhere, including inside a
// testing/synctest bubble (internal/indexer's coordinator cases publish inside
// one), and everything created inside a bubble belongs to it: a context minted
// there cannot be cancelled by a Close that runs outside it — Go makes that a
// fatal error, not a recoverable one — and a bubbled worker parked on a timer
// holds that bubble's fake clock still. Creating both with the store puts the
// create and the cancel on the same side of every such boundary.
func TestMaintenanceLane_StartsWithTheStoreNotWithAPublish(t *testing.T) {
	store, err := openPristine(t, filepath.Join(t.TempDir(), "maintenance_lane_start.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	store.maintenanceSched.Lock()
	signal, laneCtx, done := store.maintenanceSignal, store.maintenanceCtx, store.maintenanceDone
	store.maintenanceSched.Unlock()
	if signal == nil || laneCtx == nil || done == nil {
		t.Fatal("Open left the maintenance lane unstarted: the first publish would have to mint its context and its worker, and a publish inside a synctest bubble makes Close's cancel a fatal error")
	}

	// A publish posts to the slot the store already holds; it creates nothing.
	store.schedulePublishMaintenance()
	settleMaintenanceLane(t, store)
	store.maintenanceSched.Lock()
	sameSignal, sameCtx := store.maintenanceSignal == signal, store.maintenanceCtx == laneCtx
	store.maintenanceSched.Unlock()
	if !sameSignal || !sameCtx {
		t.Error("a publish replaced the lane's signal slot or lifetime: both must be the store's own")
	}
	if got := store.maintenancePasses.Load(); got != 1 {
		t.Errorf("the store's own worker ran %d passes for one request, want 1", got)
	}

	// And Close joins that worker rather than leaving it running against pools
	// it is about to tear down.
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Close returned without joining the lane worker")
	}
}

// The pass the lane schedules for a publish QUIESCES: it waits out the payload
// builds and publish drains in flight instead of running the database-wide
// ANALYZE underneath one.
//
// That is the property the whole item rests on — a refresh charged to a
// publish is the partitioning the lane exists to remove, and a refresh that
// merely moved off the publisher's stack onto a worker that starts underneath
// the next build has moved the cost, not removed it. The lane's other job kind
// (VACUUM through Compact) has its own case for the same rule; this one is for
// the kind the daemon's publish actually schedules, and it is the one the
// quiesce flag at the worker's own call site decides.
//
// Both directions are asserted through real state: a genuine payload build
// flight is what the pass must wait for, and completing that flight is all the
// deferred refresh is waiting for. The budget is shortened so the deferral —
// which is real but 30 s away — is observable as a counter rather than as a
// wall-clock window that has to be guessed.
func TestMaintenanceLane_ScheduledPassWaitsOutAPayloadBuild(t *testing.T) {
	shortenMaintenanceBudget(t, 300*time.Millisecond)
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	before := seedStalePlannerStats(t, store)
	// Grow the store past the statistics that describe it, so the pass the
	// lane runs has real work: without a stale verdict a pass that ignored
	// quiescence would refresh nothing and the Refreshes assertions below
	// would pass for the wrong reason.
	writeIndexStateCounters(t, store, 0, "repo", 800, 400)
	if stale := mustHealth(t, store); !stale.Stale {
		t.Fatalf("fixture is not stale after growth (reason=%q): the lane pass would have nothing to do", stale.Reason)
	}

	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)

	flight, leader, ready, err := store.JoinPayloadBuildFlight(ctx, generationID, false)
	if err != nil {
		t.Fatalf("JoinPayloadBuildFlight: %v", err)
	}
	if !leader || ready {
		t.Fatalf("want the physical build leader, got leader=%v ready=%v", leader, ready)
	}
	completed := false
	complete := func() {
		if !completed {
			completed = true
			flight.Complete(nil)
		}
	}
	defer complete()
	waitForCondition(t, "the payload build to become visible to the lane", func() bool {
		return strings.Contains(store.maintenanceBusyReason(), "payload build in flight")
	})

	deferralsBefore := store.maintenanceDeferrals.Load()
	store.schedulePublishMaintenance()
	waitForCondition(t, "the lane pass to start", func() bool {
		return store.maintenancePasses.Load() >= 1
	})
	// The pass has started. It must now resolve as a DEFERRAL, not as a job:
	// a pass that ignored quiescence would be in its ANALYZE by now.
	waitForCondition(t, "the scheduled pass to defer or reach its SQL", func() bool {
		return store.maintenanceJobs.Load() > 0 || store.maintenanceDeferrals.Load() > deferralsBefore
	})
	if got := store.maintenanceJobs.Load(); got != 0 {
		t.Fatalf("%d lane jobs reached their SQL while a payload build was in flight, want 0: the pass a publish schedules must wait the build out, not run the database-wide ANALYZE underneath it", got)
	}
	if got := store.maintenanceDeferrals.Load(); got != deferralsBefore+1 {
		t.Errorf("recorded %d deferrals for the pass that met the build, want 1", got-deferralsBefore)
	}
	during := mustHealth(t, store)
	if during.Refreshes != before.Refreshes {
		t.Fatalf("the lane refreshed planner statistics underneath a payload build (%d -> %d refreshes)", before.Refreshes, during.Refreshes)
	}

	// And the deferral is not the end of it: the build finishing is all the
	// refresh was waiting for.
	complete()
	if reason := store.maintenanceBusyReason(); reason != "" {
		t.Fatalf("the completed build left the lane busy: %s", reason)
	}
	store.schedulePublishMaintenance()
	settleMaintenanceLane(t, store)
	if got := store.maintenanceJobs.Load(); got != 1 {
		t.Fatalf("the lane ran %d jobs once the build was done, want exactly 1", got)
	}
	after := mustHealth(t, store)
	if after.Refreshes <= before.Refreshes {
		t.Fatalf("the lane never ran the refresh it deferred (%d -> %d refreshes); stale=%v reason=%q",
			before.Refreshes, after.Refreshes, after.Stale, after.Reason)
	}
}

// holdTheOnlyReadConnection parks the store's read pool at one connection and
// takes it, so the next read to reach the pool blocks until the returned
// release is called. It is how a case holds a REAL planner-statistics pass
// inside the lane: the refresh's first act is a health probe on the read pool,
// so a pass that cannot get a connection sits there holding the lane token and
// nothing else — no write gate, no writer connection — which is exactly the
// shape a checkpoint has to get past.
func holdTheOnlyReadConnection(t *testing.T, store *Store) (release func()) {
	t.Helper()
	store.db.SetMaxOpenConns(1)
	conn, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("taking the read pool's only connection: %v", err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = conn.Close()
		}
	}
	t.Cleanup(release)
	return release
}

// A TRUNCATE checkpoint must not be starved behind a planner-statistics pass.
//
// The two lane occupants are not alike. The checkpoint carries the indexer's
// read boundary (internal/indexer/multi.go): one attempt, a 10 s budget, no
// retry, and skipping it is what leaves a global read pass running against an
// undrained multi-gigabyte WAL — a measured 48x. A statistics pass can hold the
// lane token for its pass budget plus one index's ANALYZE plus a reload, which
// is longer than that budget. Admitting the checkpoint behind such a pass would
// therefore turn a statistics refresh into a skipped WAL drain.
//
// So the pass yields. This case pins it end to end through the real entry
// points: the pass is the one the store's own lane worker runs (scheduled the
// way a publish schedules it, entering the lane the way the worker enters it),
// and the checkpoint is CheckpointWAL itself — the method internal/indexer
// calls. The pass is held inside the lane by starving the read pool, so it
// holds the token and nothing else; the checkpoint's own resources (the writer
// connection, the write gate) stay free, which is what makes "it waited for the
// lane" the only possible reading of a failure here.
func TestCheckpointWAL_PreemptsALanePassRatherThanWaitingBehindIt(t *testing.T) {
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	release := holdTheOnlyReadConnection(t, store)

	jobsBefore := store.maintenanceJobs.Load()
	store.schedulePublishMaintenance()
	waitForCondition(t, "the lane pass to take the lane token", func() bool {
		return store.maintenanceJobs.Load() > jobsBefore
	})
	if got := store.maintenancePreemptions.Load(); got != 0 {
		t.Fatalf("%d pre-emptions before any priority job asked for the lane, want 0", got)
	}

	started := time.Now()
	err := store.CheckpointWAL()
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("CheckpointWAL behind a planner-statistics pass returned %v after %s, want the checkpoint to run: the pass must yield the lane rather than make the read boundary skip its WAL drain", err, elapsed)
	}
	if elapsed >= walCheckpointTimeout {
		t.Errorf("CheckpointWAL waited %s behind a planner-statistics pass, want well under its own %s budget", elapsed, walCheckpointTimeout)
	}
	if got := store.maintenancePreemptions.Load(); got != 1 {
		t.Errorf("the lane recorded %d pre-emptions for one checkpoint, want exactly 1", got)
	}
	if got := store.maintenanceJobs.Load(); got < jobsBefore+2 {
		t.Errorf("the lane ran %d jobs, want at least the pass plus the checkpoint (the re-owed pass may already have added a third)", got-jobsBefore)
	}

	// The pass is owed again rather than lost: a cancelled cooperative refresh
	// keeps its cursor, and the worker asks for the pass it abandoned. It is
	// waiting on the same starved read pool, so handing that back is all it
	// needs.
	waitForCondition(t, "the pre-empted pass to be owed again", func() bool {
		return store.maintenancePasses.Load() >= 2
	})
	release()
	settleMaintenanceLane(t, store)
}

// The converse of the pre-emption, and the half that keeps it from being undone
// one microsecond later: while a whole-file job holds the lane, the worker does
// not start a pass at all.
//
// Without it a pre-empted pass would re-own the token the instant it yielded —
// ahead of the checkpoint that asked for it — and the checkpoint would be back
// where it started. The request is not dropped: it stays owed, and the job that
// held the lane re-posts it on the way out.
func TestMaintenanceLane_StartsNoPassWhileAWholeFileJobHoldsTheLane(t *testing.T) {
	store := openPayloadStore(t)
	seedPayloadBase(t, store)

	inside := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- store.runMaintenance(context.Background(), maintenanceCheckpoint, false, func(context.Context) error {
			close(inside)
			<-release
			return nil
		})
	}()
	select {
	case <-inside:
	case <-time.After(30 * time.Second):
		t.Fatal("the whole-file job never reached the inside of the lane")
	}

	store.schedulePublishMaintenance()
	// Deliberately a window rather than a state: the assertion is that
	// something does NOT happen, and the worker has had its wakeup by now.
	time.Sleep(100 * time.Millisecond)
	if got := store.maintenancePasses.Load(); got != 0 {
		t.Fatalf("the worker started %d passes while a whole-file job held the lane, want 0: a pass that queues there takes the token back ahead of the job that pre-empted it", got)
	}

	close(release)
	if err := <-finished; err != nil {
		t.Fatalf("the whole-file job: %v", err)
	}
	// The pass was owed all along, and the job that held the lane is what
	// wakes it.
	waitForCondition(t, "the owed pass to run once the lane is free", func() bool {
		return store.maintenancePasses.Load() >= 1
	})
	settleMaintenanceLane(t, store)
	if got := store.maintenancePasses.Load(); got != 1 {
		t.Errorf("the lane ran %d passes for one request, want exactly 1", got)
	}
}

// maintenancePassRegistered reports whether a pre-emptible pass currently has
// a handle a priority job could cancel. It reads the same field under the same
// mutex beginPriorityMaintenance does, so "registered" here means exactly
// "cancellable by the next whole-file job to arrive".
func maintenancePassRegistered(store *Store) bool {
	store.maintenanceSched.Lock()
	defer store.maintenanceSched.Unlock()
	return store.maintenancePass != nil
}

// requirePreemptibleRegistration blocks until the lane's pass is cancellable,
// and says what it means when it is not. Separate from waitForCondition so the
// failure names the defect rather than the poll, and NOT fatal: a case that
// finds no handle here has more to prove — what the missing handle costs the
// checkpoint that arrives behind that pass — so it records the defect and
// carries on into the symptom.
func requirePreemptibleRegistration(t *testing.T, store *Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if maintenancePassRegistered(store) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("the lane pass queued for the lane token registered no pre-emption handle: a priority job arriving now finds nothing to cancel, so a checkpoint would wait out the pass's whole quiescence budget (%s) inside its own %s one and skip the WAL drain", maintenanceQuiesceTimeout, walCheckpointTimeout)
}

// A pass is pre-emptible for the whole time it can be occupying the lane, not
// only once it reaches its SQL.
//
// The pass's own path through runMaintenance has three stages that can hold or
// be about to hold the token: the wait for the token, the quiescence wait it
// performs WITH the token already held, and the SQL itself. A pass parked in
// the middle one is the dangerous shape — it owns the token and is doing
// nothing with it, waiting out publishes and builds that can last far longer
// than a checkpoint's 10 s budget. If its cancel handle is only registered
// after that wait returns, a checkpoint arriving there finds no pass to
// pre-empt, waits out the pass's budget instead of its own, defers with no
// retry, and the indexer's global read pass runs against an undrained WAL (the
// ~11 s vs ~533 s census at internal/indexer/multi.go).
//
// This case builds that exact state without a sleep anywhere: the token is
// held by the case itself, so the pass the store's own worker starts is pinned
// between its first quiescence wait (which it clears — the store is idle) and
// the token. The registration assertion then fires while the pass is in that
// parked state. A real payload build flight is opened before the token is
// handed over, so the pass takes the token straight into the post-token
// quiescence wait, and CheckpointWAL — the method internal/indexer calls — has
// to get past a pass sitting in precisely the middle stage.
func TestCheckpointWAL_PreemptsAPassParkedInItsQuiescenceWait(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)

	// Hold the lane token, so the pass scheduled below cannot get past it.
	if err := store.maintenanceGate.LockContext(ctx); err != nil {
		t.Fatalf("taking the lane token: %v", err)
	}
	tokenHeld := true
	releaseToken := func() {
		if tokenHeld {
			tokenHeld = false
			store.maintenanceGate.Unlock()
		}
	}
	defer releaseToken()

	store.schedulePublishMaintenance()
	waitForCondition(t, "the lane worker to start its pass", func() bool {
		return store.maintenancePasses.Load() >= 1
	})
	// The pass is queued for the token the case holds. It must ALREADY be
	// cancellable: everything it does from here on happens with the token in
	// hand or on the way to it. Recorded rather than fatal — the rest of the
	// case then shows what an unregistered pass costs the checkpoint.
	requirePreemptibleRegistration(t, store)

	// Open a real payload build flight, so the quiescence wait the pass
	// performs after it takes the token blocks. This is the state a publish
	// window leaves the store in, and the one a pass must not sit on the token
	// through.
	generationID, handle, err := store.BeginPayloadGeneration(ctx, payloadRequest())
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	writePayloadOverlay(t, handle)
	flight, leader, ready, err := store.JoinPayloadBuildFlight(ctx, generationID, false)
	if err != nil {
		t.Fatalf("JoinPayloadBuildFlight: %v", err)
	}
	if !leader || ready {
		t.Fatalf("want the physical build leader, got leader=%v ready=%v", leader, ready)
	}
	completed := false
	complete := func() {
		if !completed {
			completed = true
			flight.Complete(nil)
		}
	}
	defer complete()
	waitForCondition(t, "the payload build to become visible to the lane", func() bool {
		return strings.Contains(store.maintenanceBusyReason(), "payload build in flight")
	})

	if got := store.maintenancePreemptions.Load(); got != 0 {
		t.Fatalf("%d pre-emptions before any priority job asked for the lane, want 0", got)
	}
	// Hand the token over. The pass takes it (it queued first) and parks in
	// its post-token quiescence wait, holding the lane and doing nothing.
	releaseToken()

	started := time.Now()
	cpErr := store.CheckpointWAL()
	elapsed := time.Since(started)
	if cpErr != nil {
		t.Fatalf("CheckpointWAL met a pass parked in its quiescence wait and returned %v after %s: the pass must yield the lane rather than make the indexer's read boundary skip its WAL drain", cpErr, elapsed)
	}
	if elapsed >= walCheckpointTimeout {
		t.Errorf("CheckpointWAL waited %s behind a parked pass, want well under its own %s budget", elapsed, walCheckpointTimeout)
	}
	if got := store.maintenancePreemptions.Load(); got != 1 {
		t.Errorf("the lane recorded %d pre-emptions for one checkpoint, want exactly 1", got)
	}
	// The yielded pass is a deferral, not a job: it never reached its SQL.
	if got := store.maintenanceJobs.Load(); got != 1 {
		t.Errorf("the lane counted %d jobs, want exactly 1 (the checkpoint): a pass that yielded reached no SQL and is counted as a deferral", got)
	}

	// And the refresh is owed again rather than lost. Completing the build is
	// all the re-owed pass is waiting for.
	waitForCondition(t, "the pre-empted pass to be owed again", func() bool {
		return store.maintenancePasses.Load() >= 2
	})
	complete()
	settleMaintenanceLane(t, store)
}

// The lock-held priority re-check inside beginPreemptiblePass, on its own.
//
// claimMaintenancePass refuses to open a pass while a whole-file job is
// waiting for or holding the lane, which covers every pass that has not
// started yet. What it cannot cover is the pass claimed one instant BEFORE
// such a job arrived: that pass read a zero priority mark at claim time and is
// already on its way to the lane. Between the claim and the registration the
// priority job can take maintenanceSched, find no pass registered, and have
// nothing to cancel — so the pass has to re-read the mark under that same lock
// and yield on its own.
//
// The window is microseconds wide through the worker, so the case reproduces
// the interleaving directly: a real priority job is inside the lane (the shape
// CheckpointWAL and Compact both enter as), and the pass then enters
// runMaintenance the way the worker enters it. Without the re-check the pass
// registers, queues for a token a whole-file job is holding, and spends its
// entire budget there.
func TestMaintenanceLane_PassClaimedBeforeAPriorityJobYieldsAtRegistration(t *testing.T) {
	budget := 2 * time.Second
	shortenMaintenanceBudget(t, budget)
	store := openPayloadStore(t)
	seedPayloadBase(t, store)

	inside := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- store.runMaintenance(context.Background(), maintenanceCheckpoint, false, func(context.Context) error {
			close(inside)
			<-release
			return nil
		})
	}()
	select {
	case <-inside:
	case <-time.After(30 * time.Second):
		t.Fatal("the whole-file job never reached the inside of the lane")
	}

	preemptionsBefore := store.maintenancePreemptions.Load()
	jobsBefore := store.maintenanceJobs.Load()
	ran := make(chan error, 1)
	started := time.Now()
	err := store.runMaintenance(context.Background(), maintenancePlannerStats, true, func(passCtx context.Context) error {
		ran <- passCtx.Err()
		return nil
	})
	elapsed := time.Since(started)

	if got := store.maintenancePreemptions.Load(); got != preemptionsBefore+1 {
		t.Fatalf("the pass recorded %d pre-emptions, want 1: a pass that reaches registration with a priority job already marked must yield on its own, not queue for the token that job is holding", got-preemptionsBefore)
	}
	if elapsed >= budget/2 {
		t.Errorf("the pass spent %s before giving up, want it to yield immediately (budget %s): it queued for the lane token instead of yielding", elapsed, budget)
	}
	if !errors.Is(err, ErrMaintenanceBusy) {
		t.Errorf("the yielded pass returned %v, want a %v deferral", err, ErrMaintenanceBusy)
	}
	select {
	case passErr := <-ran:
		t.Errorf("the yielded pass ran its SQL (ctx err %v) while a whole-file job held the lane", passErr)
	default:
	}
	if got := store.maintenanceJobs.Load(); got != jobsBefore {
		t.Errorf("the lane counted %d further jobs for a pass that yielded, want 0", got-jobsBefore)
	}

	close(release)
	if err := <-finished; err != nil {
		t.Fatalf("the whole-file job: %v", err)
	}
}
