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
	store, err := Open(filepath.Join(t.TempDir(), "maintenance_lane_close.sqlite"))
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
	store, err := Open(filepath.Join(t.TempDir(), "maintenance_lane_start.sqlite"))
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
