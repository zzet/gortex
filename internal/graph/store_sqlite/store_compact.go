package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// The whole-database maintenance lane, and the one-time boot compaction that
// is its heaviest job.
//
// ANALYZE (sqlite_stat1), VACUUM and TRUNCATE checkpoints are structurally
// global: there is one statistics table, one file and one write-ahead log per
// database, and no generation owns any of them. Charging one of them to
// whichever writer happened to trip a boundary is what this lane exists to
// stop — a generation publish paid a database-wide ANALYZE inline, so a
// checkout waiting on the publish paid it too.
//
// All three are wired to live callers, so the lane is not a facility waiting
// for a user:
//
//	ANALYZE      internal/indexer's generation builder publishes through
//	             Store.PublishPayloadGeneration, whose tail schedules the
//	             planner-statistics refresh the publish owes; the direct
//	             publish+route API asks below its own route flip instead.
//	VACUUM       cmd/gortex/daemon_state.go -> maybeCompactStore -> Compact.
//	TRUNCATE     internal/indexer/multi.go's read boundary -> CheckpointWAL,
//	             and Compact's own tail through the ungated inner path.
//
// The lane gives those actions one admission point with three properties:
//
//   - one at a time. maintenanceGate is a context-bounded single token, so a
//     TRUNCATE checkpoint can never interleave with a VACUUM rewriting the
//     same file.
//   - the whole-file jobs are not starved behind the statistics pass. The two
//     occupants are not alike: VACUUM and the TRUNCATE checkpoint carry
//     latency budgets of their own (the checkpoint's is walCheckpointTimeout,
//     10 s, with no retry, and it guards a measured 48x read-performance
//     boundary), while a planner-statistics pass can legitimately hold the
//     token for longer than that. So the pass is PRE-EMPTIBLE and those two
//     are PRIORITY: entering the lane as one of them cancels the pass in
//     flight and keeps the worker from starting another until it is done. A
//     cancelled cooperative refresh is a deferral, not a loss — it keeps its
//     cursor, the worker re-owes the pass, and it resumes as soon as the
//     whole-file job frees the token.
//   - not inside somebody else's window. A job that declares quiesce waits
//     for every publish drain and payload build in flight to finish first, so
//     a whole-file rewrite never STARTS in the middle of one.
//
//     Read that guarantee in one direction only. The lane holds maintenance
//     off a publish; it does not hold a publish off maintenance. Nothing on
//     the publish or build side consults maintenanceGate — a publish only
//     bumps publishDrains — so a publish that opens AFTER a quiescing job
//     observed the store idle runs straight through that job. That is
//     deliberate: writeMu still serialises the SQL and VACUUM is atomic, so
//     what such a publish pays is latency, not correctness, and making the
//     rule two-directional would mean a publish blocking on a lane token,
//     which is the coupling this lane exists to remove.
//   - at most once per publish window. schedulePublishMaintenance coalesces:
//     a burst of publishes arriving while a pass runs owes exactly one more
//     pass, not one per publish.
//
// Lock order: the maintenance gate is taken BEFORE writeMu, never after.
// Nothing that already holds the write gate may enter the lane.
//
// Failure is never fatal. A job that cannot get the lane, or a store that
// never goes quiescent inside the job's budget, returns ErrMaintenanceBusy —
// the verdict that caused it still stands, the counters record the deferral,
// and the next boundary asks again.

// ErrMaintenanceBusy reports that a whole-database maintenance action did not
// run because the store was busy: the lane was occupied, or a publish drain or
// payload build stayed in flight for the action's whole budget. It is a
// deferral, not a fault — the store is untouched and every caller treats it as
// skip-and-continue.
var ErrMaintenanceBusy = errors.New("store_sqlite: whole-database maintenance deferred: store busy")

// errMaintenanceNoCore is the coreless-handle answer. A zero Store has no
// database to maintain, so every lane entry point is inert on one.
var errMaintenanceNoCore = errors.New("store_sqlite: maintenance needs an open store")

// maintenanceJob names one whole-database action. The values are the reason
// strings the deferral errors carry; the lane's scheduling asks two questions
// of the kind and nothing else switches on them.
type maintenanceJob string

const (
	maintenancePlannerStats maintenanceJob = "planner_stats"
	maintenanceVacuum       maintenanceJob = "vacuum"
	maintenanceCheckpoint   maintenanceJob = "wal_checkpoint_truncate"
)

// preemptible reports whether a job in flight may be asked to yield the lane.
// Only the statistics pass may: it is resumable by construction (a cancelled
// cooperative refresh keeps its cursor and continues at the next boundary) and
// nothing waits on its result. The two whole-file rewrites are neither — a
// half-done VACUUM or TRUNCATE is not a thing the lane can ask for.
func (j maintenanceJob) preemptible() bool { return j == maintenancePlannerStats }

// priority reports whether a job pre-empts the pre-emptible one. The two
// whole-file jobs do, because each arrives on a latency budget shorter than a
// statistics pass may legitimately take and each has exactly one attempt.
func (j maintenanceJob) priority() bool { return !j.preemptible() }

// maintenanceQuiesceTimeout is the whole budget one lane entry gets: the wait
// for the publishes and builds in flight to finish AND the wait for the lane
// token itself. Generous on purpose — the point of the wait is that
// maintenance yields to real work, and a job that gives up simply runs at the
// next boundary — but finite in every direction, because Compact runs on the
// daemon's warmup path with context.Background() and must never be able to
// stall a boot behind another job.
//
// A var, not a const, only so the in-package cases can shorten the budget
// instead of spending it; production never assigns it.
var maintenanceQuiesceTimeout = 30 * time.Second

const (
	// maintenanceQuiescePoll is the busy-probe interval. The probe is two
	// atomics and a sync.Map range with an early exit, so it costs nothing to
	// ask often.
	maintenanceQuiescePoll = 5 * time.Millisecond
	// maintenanceIdlePoll is the settle-point interval — see
	// waitMaintenanceIdle.
	maintenanceIdlePoll = time.Millisecond
	// maintenanceCloseTimeout bounds Close's join with a pass in flight. Every
	// job is internally bounded well under it; the bound exists so a wedged
	// pass degrades to a noisy shutdown rather than a hung one.
	maintenanceCloseTimeout = 30 * time.Second
)

// runMaintenance is the lane's single admission point: it serialises one
// whole-database action against every other one and, when quiesce is set,
// against the publish drains and payload builds it must not be partitioned
// against.
//
// The quiescence wait happens OUTSIDE the gate first, so a job that is only
// waiting for other people's work cannot block a cheap job behind it, and is
// then re-checked once the gate is held — the window between the two is
// exactly what the re-check closes.
//
// All three waits share ONE deadline, maintenanceQuiesceTimeout from entry:
// both quiescence waits and the acquisition of the lane token itself. Bounding
// the token matters as much as bounding the quiescence: Compact enters with
// context.Background() from the daemon's warmup path, so an unbounded
// LockContext there would let one long lane pass stall a boot indefinitely.
// context.WithDeadline keeps whichever bound is earlier, so a caller that
// arrives with a tighter budget (CheckpointWAL's walCheckpointTimeout) keeps
// its own.
//
// Bounding the token is not the same as not waiting behind the wrong thing,
// which is why a priority job announces itself BEFORE either wait: a
// statistics pass holding the token is asked to yield right here, and the
// worker is kept from starting another until this job is done. Without that,
// a checkpoint arriving behind a pass would spend its whole 10 s budget and
// skip the WAL drain — a deferral on the boundary that keeps the indexer's
// global reads off a multi-gigabyte WAL.
//
// The same rule runs the other way for the pass: it registers the handle that
// cancel reaches BEFORE its own first wait, so it is pre-emptible for the
// whole time it can be occupying the lane — queued for the token, parked in
// the post-token quiescence wait, or inside its SQL — and not only for the
// last of those three.
func (s *Store) runMaintenance(ctx context.Context, job maintenanceJob, quiesce bool, fn func(context.Context) error) error {
	if s.coreless() {
		return errMaintenanceNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if job.priority() {
		release := s.beginPriorityMaintenance()
		defer release()
	}
	// The mirror image on the pre-emptible side: the pass registers its
	// cancel BEFORE it waits for anything, and every wait below then runs
	// under the context that cancel ends. Registering later — once the token
	// is held and the second quiescence wait has returned — leaves a pass
	// that is parked in that wait holding the token with NO handle for a
	// priority job to cancel, so a checkpoint arriving there waits out the
	// pass's whole quiescence budget (up to maintenanceQuiesceTimeout, 30 s)
	// inside its own 10 s one, defers, and skips the WAL drain. That is the
	// starvation this lane's priority rule exists to prevent, so the
	// registration has to cover the waits and not just the SQL.
	runCtx, stopPass := ctx, func() {}
	if job.preemptible() {
		runCtx, stopPass = s.beginPreemptiblePass(ctx)
		defer stopPass()
	}
	deadline := time.Now().Add(maintenanceQuiesceTimeout)
	if quiesce {
		if err := s.awaitMaintenanceQuiescence(runCtx, job, deadline); err != nil {
			s.maintenanceDeferrals.Add(1)
			return err
		}
	}
	gateCtx, cancelGate := context.WithDeadline(runCtx, deadline)
	err := s.maintenanceGate.LockContext(gateCtx)
	cancelGate()
	if err != nil {
		s.maintenanceDeferrals.Add(1)
		return fmt.Errorf("%w: %s: lane occupied: %w", ErrMaintenanceBusy, job, err)
	}
	defer s.maintenanceGate.Unlock()
	if quiesce {
		if err := s.awaitMaintenanceQuiescence(runCtx, job, deadline); err != nil {
			s.maintenanceDeferrals.Add(1)
			return err
		}
	}
	// Counted here, immediately before fn, because the counter means "reached
	// its SQL": a pass that yielded at registration never gets this far — its
	// cancelled context resolves it as a deferral on one of the three paths
	// above, and it is counted there. Jobs and deferrals therefore partition
	// the lane entries rather than overlapping on the yield path.
	s.maintenanceJobs.Add(1)
	runErr := fn(runCtx)
	// Unregister while the token is still held. The gate release is deferred
	// and therefore runs after this, so a priority job can never find this
	// pass's handle at a moment when the token it names is already free —
	// which would cost a pre-emption count and re-owe a pass that finished.
	stopPass()
	return runErr
}

// maintenancePassHandle is one pre-emptible pass's cancel, held by identity so
// the pass that unregisters is always the pass that registered.
type maintenancePassHandle struct {
	cancel context.CancelFunc
}

// beginPreemptiblePass registers the job about to enter the lane as the pass a
// priority job may cancel, and hands it the context that cancel ends. stop
// unregisters and releases it.
//
// The priority re-check under the lock is the race the registration would
// otherwise lose. claimMaintenancePass declines to open a pass at all while a
// priority job is waiting or holding the lane, so the ordinary case never gets
// here — but a pass CLAIMED a moment before that job arrived is already on its
// way and finds no mark to read at claim time. Between the claim returning
// true and this lock there is therefore a window in which a priority job can
// take maintenanceSched, find maintenancePass still nil, and have nothing to
// cancel. A pass landing in that window must yield on its own rather than run
// the checkpoint's whole budget out from under it.
func (s *Store) beginPreemptiblePass(ctx context.Context) (context.Context, func()) {
	passCtx, cancel := context.WithCancel(ctx)
	handle := &maintenancePassHandle{cancel: cancel}
	s.maintenanceSched.Lock()
	yield := s.maintenancePriority > 0
	if !yield {
		s.maintenancePass = handle
	}
	s.maintenanceSched.Unlock()
	if yield {
		s.maintenancePreemptions.Add(1)
		cancel()
	}
	return passCtx, func() {
		s.maintenanceSched.Lock()
		if s.maintenancePass == handle {
			s.maintenancePass = nil
		}
		s.maintenanceSched.Unlock()
		cancel()
	}
}

// beginPriorityMaintenance marks a whole-file job as waiting for or holding
// the lane and asks the statistics pass in flight, if any, to yield. The
// returned call retracts the mark; it is idempotent, and the last mark to be
// retracted re-posts the signal for a pass the worker declined to start.
//
// The pass is cleared from the handle as it is cancelled, so a second priority
// job arriving behind the first neither cancels twice nor double-counts.
func (s *Store) beginPriorityMaintenance() func() {
	s.maintenanceSched.Lock()
	s.maintenancePriority++
	handle := s.maintenancePass
	s.maintenancePass = nil
	s.maintenanceSched.Unlock()
	if handle != nil {
		s.maintenancePreemptions.Add(1)
		handle.cancel()
	}
	var once sync.Once
	return func() { once.Do(s.endPriorityMaintenance) }
}

func (s *Store) endPriorityMaintenance() {
	s.maintenanceSched.Lock()
	if s.maintenancePriority > 0 {
		s.maintenancePriority--
	}
	resume := s.maintenancePriority == 0 && s.maintenancePriorityResume && !s.maintenanceClosed
	if resume {
		s.maintenancePriorityResume = false
	}
	signal := s.maintenanceSignal
	s.maintenanceSched.Unlock()
	if !resume || signal == nil {
		return
	}
	select {
	case signal <- struct{}{}:
	default:
	}
}

// claimMaintenancePass opens one worker pass, or declines it because a
// whole-file job is waiting for or holding the lane.
//
// Declining rather than queueing is what keeps the pre-emption from being
// undone one microsecond after it happens: a pass that yielded the token and
// immediately asked for it again would be back in front of the checkpoint that
// cancelled it. The request is not lost — it stays owed, and the last priority
// job to finish re-posts the signal.
//
// How long it stays owed is the priority job's business, and one of the two is
// not short: a checkpoint is bounded by walCheckpointTimeout, but Compact's
// VACUUM body runs unbounded on purpose (the daemon's own comment calls it
// minutes on a multi-GB store), and the decline lasts from the moment that job
// starts WAITING for the lane. So the statistics pass can be owed for the
// whole of a VACUUM. That is latency and never loss — the pass re-owes and
// resumes at its cursor — and it is the trade this lane exists to make: the
// deferrable occupant is the one that defers.
func (s *Store) claimMaintenancePass() bool {
	s.maintenanceSched.Lock()
	defer s.maintenanceSched.Unlock()
	if s.maintenanceClosed {
		return false
	}
	if s.maintenancePriority > 0 {
		s.maintenanceOwed = true
		s.maintenancePriorityResume = true
		return false
	}
	// Owed is cleared as the pass starts, not when it ends: a request
	// arriving from here on describes payload this pass may not have seen, so
	// it must owe a further pass rather than coalesce into this one.
	s.maintenanceOwed = false
	s.maintenanceRunning = true
	return true
}

// releaseMaintenancePass closes one worker pass. A pass that was pre-empted
// re-owes itself: the refresh it was asked to abandon is still owed, and the
// cooperative pass resumes at the cursor it kept.
func (s *Store) releaseMaintenancePass(preempted bool) {
	s.maintenanceSched.Lock()
	s.maintenanceRunning = false
	resume := false
	if preempted && !s.maintenanceClosed {
		s.maintenanceOwed = true
		if s.maintenancePriority > 0 {
			s.maintenancePriorityResume = true
		} else {
			resume = true
		}
	}
	signal := s.maintenanceSignal
	s.maintenanceSched.Unlock()
	if !resume || signal == nil {
		return
	}
	select {
	case signal <- struct{}{}:
	default:
	}
}

// awaitMaintenanceQuiescence blocks until no publish window and no payload
// build is in flight, the deadline passes, or the context ends. The deferral it
// returns names what it waited for, because "maintenance did not run" is only
// actionable with the reason attached.
func (s *Store) awaitMaintenanceQuiescence(ctx context.Context, job maintenanceJob, deadline time.Time) error {
	for {
		reason := s.maintenanceBusyReason()
		if reason == "" {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w: %s: %s", ErrMaintenanceBusy, job, reason)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %s: %s: %w", ErrMaintenanceBusy, job, reason, ctx.Err())
		case <-time.After(maintenanceQuiescePoll):
		}
	}
}

// maintenanceBusyReason reports what currently forbids whole-file maintenance,
// or "" when nothing does. Publish drains are checked first because they are
// the shorter window: a job that reports a build is waiting for something that
// takes a while, and that is the more useful reason to surface.
func (s *Store) maintenanceBusyReason() string {
	if drains := s.publishDrains.Load(); drains > 0 {
		return fmt.Sprintf("publish drains in flight: %d", drains)
	}
	if generationID, found := s.anyPayloadBuildFlight(); found {
		return fmt.Sprintf("payload build in flight: generation %d", generationID)
	}
	return ""
}

// anyPayloadBuildFlight reports whether this process owns a physical payload
// build right now, and one generation it is building. The range stops at the
// first entry, so the probe costs one map step rather than a scan.
func (s *Store) anyPayloadBuildFlight() (int64, bool) {
	var generationID int64
	found := false
	s.payloadBuildFlights.Range(func(key, _ any) bool {
		if id, ok := key.(int64); ok {
			generationID = id
		}
		found = true
		return false
	})
	return generationID, found
}

// schedulePublishMaintenance asks the lane for the planner-statistics refresh a
// publish owes, and returns immediately.
//
// The refresh itself must still happen after a publish — a generation adds a
// whole checkout's payload in one step, and issue #651 is what a store planning
// against statistics that describe a fraction of itself does. What must not
// happen is the publisher paying for it: the boundary schedules, the lane runs.
//
// Two production boundaries call it, and each is the tail of its own publish
// window: PublishPayloadGeneration — the physical publish the generation
// builder performs as its last step, which is the one the daemon reaches — and
// PublishAndRoute, whose window spans publish AND route flip and which
// therefore asks below its flip while suppressing the inner publish's request.
// One publish window, one request, either way.
//
// Coalescing is the "at most once per publish window" rule, and the signal slot
// is what enforces it: while a request is owed but has not started, further
// requests add nothing; while a pass runs, the requests arriving behind it owe
// exactly one more pass — and that pass reads the store as it is when it
// starts, which is the state all of those publishes left behind anyway.
//
// Everything a publisher pays for is here: one counter, one mutex, and at most
// one non-blocking send. No goroutine is started on the publish path, and no
// context is created there; see startMaintenanceLane for why that matters.
func (s *Store) schedulePublishMaintenance() {
	if s.coreless() {
		return
	}
	s.maintenanceRequests.Add(1)
	s.maintenanceSched.Lock()
	defer s.maintenanceSched.Unlock()
	if s.maintenanceClosed || s.maintenanceSignal == nil || s.maintenanceOwed {
		return
	}
	s.maintenanceOwed = true
	select {
	case s.maintenanceSignal <- struct{}{}:
	default:
		// The slot is full, so a wakeup the worker has not taken yet is
		// already queued and this request rides on it. Non-blocking on
		// purpose: a publisher must never wait on the lane.
	}
}

// startMaintenanceLane creates the lane's lifetime and its worker. Open calls
// it once, at the end of a successful open; nothing else does.
//
// The worker is started with the STORE rather than by the first publish that
// needs one, and the reason is not tidiness. A publish can happen anywhere,
// including inside a testing/synctest bubble, and everything created inside a
// bubble belongs to it: cancelling a context minted there from a Close that
// runs outside it is a fatal error, and a bubbled worker parked on a timer
// holds that bubble's fake clock still. Creating both at construction puts the
// create and the cancel on the same side of every such boundary — exactly as
// stopCheckpoint already is — and leaves the publish path with nothing but a
// non-blocking send.
//
// The cost of that choice, stated plainly: one parked goroutine per open store,
// where the lazy shape had none until the first publish. It blocks on a channel
// until a publish asks for something and exits on Close, so a store that
// publishes nothing does no work at all.
//
// The worker holds the BASE handle. A publish is made through whatever handle
// the builder holds, but sqlite_stat1 describes the physical database — every
// generation's rows in one B-tree — so the refresh must not be attributed to,
// or scoped by, the generation that triggered it.
func (s *Store) startMaintenanceLane() {
	if s.coreless() {
		return
	}
	s.maintenanceSched.Lock()
	defer s.maintenanceSched.Unlock()
	if s.maintenanceSignal != nil || s.maintenanceClosed {
		return
	}
	s.maintenanceCtx, s.maintenanceCancel = context.WithCancel(context.Background())
	s.maintenanceSignal = make(chan struct{}, 1)
	s.maintenanceDone = make(chan struct{})
	go s.atBase().runMaintenanceLane(s.maintenanceCtx, s.maintenanceSignal, s.maintenanceDone)
}

// runMaintenanceLane is the lane's worker: it parks until a publish boundary
// asks for the planner-statistics refresh it owes, runs one pass through the
// lane, and parks again. Requests that arrive while a pass runs are served by
// the single pass that follows it.
//
// The pass enters as maintenancePlannerStats, which is what makes it
// pre-emptible, and it quiesces: a refresh must not start inside a publish
// window or underneath a payload build in flight, which is the partitioning
// this whole lane exists to remove.
func (s *Store) runMaintenanceLane(ctx context.Context, signal <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-signal:
		}
		if ctx.Err() != nil {
			return
		}
		if !s.claimMaintenancePass() {
			// Closed, or a whole-file job owns the lane. In the second case
			// the pass stays owed and that job re-posts the signal; parking
			// again is what keeps this worker from spinning on the token.
			continue
		}

		s.maintenancePasses.Add(1)
		// Sampled around the pass rather than plumbed out of it: the pass's
		// own context is cancelled by whoever pre-empts it, and this worker
		// only needs to know that it happened, so that the refresh the pass
		// abandoned is owed again.
		preemptions := s.maintenancePreemptions.Load()
		// The error is deliberately dropped, for the same reason every other
		// planner-statistics call site drops it: a store that could not
		// refresh its statistics plans through the cost model it already had,
		// and a deferral is already counted.
		_ = s.runMaintenance(ctx, maintenancePlannerStats, true, func(ctx context.Context) error {
			_, err := s.EnsurePlannerStatsFresh(ctx)
			return err
		})

		s.releaseMaintenancePass(s.maintenancePreemptions.Load() != preemptions)
	}
}

// waitMaintenanceIdle blocks until no pass is running and none is owed. It is
// the lane's settle point: a caller that needs to observe the effect of a
// refresh a publish only scheduled waits on it first.
func (s *Store) waitMaintenanceIdle(ctx context.Context) error {
	if s.coreless() {
		return nil
	}
	for {
		s.maintenanceSched.Lock()
		idle := !s.maintenanceRunning && !s.maintenanceOwed
		s.maintenanceSched.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(maintenanceIdlePoll):
		}
	}
}

// stopMaintenanceLane closes the lane for good: no further pass can be
// scheduled, the pass in flight is cancelled, and Close joins the worker before
// the pools it writes through are torn down. Idempotent, and inert on a store
// that never started one.
func (s *Store) stopMaintenanceLane() {
	if s.coreless() {
		return
	}
	s.maintenanceSched.Lock()
	if s.maintenanceClosed {
		s.maintenanceSched.Unlock()
		return
	}
	s.maintenanceClosed = true
	s.maintenanceOwed = false
	cancel := s.maintenanceCancel
	done := s.maintenanceDone
	s.maintenanceSched.Unlock()
	if cancel != nil {
		cancel()
	}
	if done == nil {
		return
	}
	// Bounded so a wedged pass makes shutdown noisy rather than hung. Every
	// job the lane runs is internally bounded far below this.
	select {
	case <-done:
	case <-time.After(maintenanceCloseTimeout):
	}
}

// One-time boot compaction support.
//
// The graph store only ever grows on disk: deleted rows (a purged repo, the
// duplicate-collapse migration, resolver cleanups) return their pages to
// SQLite's freelist, where future writes reuse them — but nothing short of
// VACUUM returns them to the filesystem. A long-lived store that shed a large
// fraction of its rows can therefore pin gigabytes of dead file forever (a
// live store sat at 64% freelist — 4.4 GB reclaimable in a 6.8 GB file).
// These methods give the daemon the numbers to decide whether that one-time
// rewrite is worth it, and the lever to run it. The policy (thresholds, disk
// headroom, kill-switch) deliberately lives with the caller: the store cannot
// know whether minutes of exclusive I/O are acceptable right now.

// Path returns the on-disk database file path. Empty when the store was not
// opened from a file — callers using it to reason about the underlying
// filesystem (disk-headroom checks) must treat "" as "unknown, don't".
func (s *Store) Path() string {
	return s.dbPath
}

// CompactStats reports how much of the database file is reclaimable dead
// space: freeBytes is the freelist (freelist_count × page_size), totalBytes
// the whole main file (page_count × page_size). Zeros on any pragma error —
// a read failing here is the same teardown race panicOnFatal swallows, and
// "nothing reclaimable" is the answer that makes every caller do nothing.
// The -wal file is excluded on purpose: the checkpoint loop already bounds
// it, and VACUUM only rewrites the main file.
func (s *Store) CompactStats() (freeBytes, totalBytes int64) {
	var pageSize, pageCount, freePages int64
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, 0
	}
	if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, 0
	}
	if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&freePages); err != nil {
		return 0, 0
	}
	return freePages * pageSize, pageCount * pageSize
}

// Compact rewrites the database file (VACUUM), returning freelist pages to
// the filesystem, then drains the write-ahead log with a TRUNCATE checkpoint
// so the rewrite's WAL traffic doesn't linger as a second oversized file.
//
// Cost model callers must respect: VACUUM copies the live content into a
// temporary database (up to a full extra copy on the same filesystem) and
// needs exclusive access — it blocks Go-side writers via writeMu here, and a
// concurrent reader on another pooled connection makes SQLite wait out
// busy_timeout and then fail. That failure is clean (the store is untouched,
// freelist pages remain reusable), which is why the daemon treats a Compact
// error as skip-and-continue rather than fatal.
//
// It runs through the maintenance lane and waits for the store to go quiescent
// first: rewriting the file underneath a publish window or a payload build is
// exactly the partitioning the lane exists to prevent. A store that stays busy
// for the whole budget yields ErrMaintenanceBusy — a deferral the caller
// retries at the next boot, not a failure. The tail checkpoint runs under the
// same lane hold, through the ungated inner path, because re-entering the lane
// from inside it would deadlock on its own token.
func (s *Store) Compact() error {
	return s.runMaintenance(context.Background(), maintenanceVacuum, true, func(ctx context.Context) error {
		if err := s.vacuum(ctx); err != nil {
			return err
		}
		checkpointCtx, cancel := context.WithTimeout(ctx, walCheckpointTimeout)
		defer cancel()
		return s.checkpointWALWithContext(checkpointCtx)
	})
}

// vacuum runs the rewrite itself under the write gate. The bulk-connection
// refusal mirrors the WAL checkpoint's: a coordinated cold load pins one
// connection carrying its own PRAGMAs, and a VACUUM against it would either
// fail on the pinned connection's state or discard the settings FlushBulk has
// to restore. Deferring is the honest answer — the freelist stays reusable.
func (s *Store) vacuum(ctx context.Context) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return fmt.Errorf("%w: %s: write gate: %w", ErrMaintenanceBusy, maintenanceVacuum, err)
	}
	defer s.writeMu.Unlock()
	if s.bulkConn != nil {
		return fmt.Errorf("%w: %s: bulk writer pinned", ErrMaintenanceBusy, maintenanceVacuum)
	}
	_, err := s.writerDB.ExecContext(ctx, `VACUUM`)
	return err
}
