package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Bounded WAL reclaim.
//
// The periodic checkpoints are PASSIVE: they backfill committed frames into the
// main file but never RESET the log, and SQLite restarts the log from its first
// frame only when a writer begins while no reader is using the WAL. A daemon
// under continuous dirty rebuilds always has some pool reader in flight and a
// writer that appends while the backfill runs, so the reset condition never
// occurs and the -wal file grows without bound — frames are page rewrites, so
// the logical database barely moves while the log reaches tens of gigabytes.
// journal_size_limit is applied only at a reset, so it never fires either.
// Before this job only a restart (last connection close) reclaimed the log.
//
// The reclaim forces the reset condition for a bounded moment:
//
//  1. PASSIVE backfill with nobody blocked, so the later steps copy little.
//     It runs OUTSIDE the lane and with no timeout of its own: SQLite
//     publishes backfill progress (nBackfill) only when a checkpoint pass
//     completes, so a pass interrupted by a deadline copies pages and records
//     nothing. Only shutdown, a generation bulk window opening (the lease
//     cancels the attempt) stops it. This is what lets a store that reopened
//     over a huge recovered log (nBackfill restarts at zero after a crash or a
//     killed shutdown) catch up in one pass instead of never;
//  2. take the write gate (bounded wait) so the application writer is idle,
//     refuse while a bulk connection is pinned, and PASSIVE the small delta;
//  3. close the read-pool gate (sqliteReadGate): new reads wait, reads already
//     in flight are waited for up to the drain deadline (default 250 ms);
//  4. PRAGMA wal_checkpoint(TRUNCATE) on a dedicated checkpoint connection
//     with a 100 ms busy timeout and its own budget, then reopen the gate.
//
// If the readers do not drain inside the deadline the gate reopens, nothing is
// checkpointed, and the next attempt backs off exponentially (5 s .. 5 min).
// Readers are therefore paused at most drain deadline + TRUNCATE budget.
//
// Coordination: the reclaim registers as the background checkpoint attempt,
// so it is refused while a generation bulk window holds the checkpoint lease
// and a bulk window opening mid-reclaim cancels it (the lease acquisition
// waits for it exactly as it waits for the background PASSIVE). It enters the
// maintenance lane as a whole-file job, so it never interleaves with VACUUM or
// the TRUNCATE drain. Lock order is the lane's: lane token, then writeMu, then
// the read gate; every wait is bounded, so no cycle can hold anyone forever.

// maintenanceWALReclaim is the lane job kind of the bounded reclaim. Like the
// TRUNCATE drain it is a whole-file priority job.
const maintenanceWALReclaim maintenanceJob = "wal_reclaim"

const (
	// defaultWALReclaimThresholdBytes is the -wal size above which the reclaim
	// runs. Four times journal_size_limit: well above the size ordinary PASSIVE
	// operation keeps, far below the tens of gigabytes the unbounded case hit.
	defaultWALReclaimThresholdBytes int64 = 256 << 20
	// defaultWALReclaimDrainDeadline bounds the wait for in-flight pool reads
	// once the read gate is closed.
	defaultWALReclaimDrainDeadline = 250 * time.Millisecond
	// walReclaimTruncateBudget bounds the TRUNCATE itself (interrupting it on
	// expiry). With the writer idle and the delta already backfilled it only
	// has to reset and truncate the file.
	walReclaimTruncateBudget = 250 * time.Millisecond
	// walReclaimWriterWait bounds the wait for the application write gate.
	walReclaimWriterWait = time.Second
	// walReclaimLaneBudget bounds the lane part of one attempt (admission,
	// writer wait, delta, drain, TRUNCATE).
	walReclaimLaneBudget = 10 * time.Second
	// defaultWALReclaimReaderWait bounds the open-gate stage: with the writer
	// held and the log fully backfilled, new read transactions take read mark
	// 0 (they read the database file only and never pin the log), so the
	// reclaim only has to outlast the reads admitted before that point —
	// without holding any reader. The writer is held meanwhile, but the stage
	// yields it the moment another writer queues (see yieldToWriters), so the
	// wait costs an edit nothing beyond walReclaimUrgentMinHold. Long enough
	// to outlast the daemon's long analysis reads (tens of seconds).
	defaultWALReclaimReaderWait = 30 * time.Second
	walReclaimReaderWaitMax     = 60 * time.Second
	// walReclaimMaxWriterHold caps how long one attempt keeps the application
	// writer, whatever the reader wait: the wait for old readers runs without
	// the writer, which is taken only for the final backfill, the short wait
	// for readers admitted during it, and the TRUNCATE. A queued write or an
	// announced mutation (AnnounceWrite) ends the hold at once.
	walReclaimMaxWriterHold = 2 * time.Second
	// walReclaimUrgentFactor: from this multiple of the threshold the log is
	// urgent. An urgent attempt does not yield to waiting writes (it still
	// never holds the writer past walReclaimMaxWriterHold), so a store whose
	// writes never pause still gets its WAL reset instead of growing forever.
	walReclaimUrgentFactor = 2
	// walReclaimClosedGateMaxResidue is the most unbackfilled frames the
	// closed-gate fallback accepts: its TRUNCATE has walReclaimTruncateBudget
	// to copy them while every new reader waits, so a larger residue would
	// pause readers only to fail.
	walReclaimClosedGateMaxResidue = 4096
	// walReclaimYieldPoll is how often the stage checks for a queued writer.
	walReclaimYieldPoll = 5 * time.Millisecond
	// walReclaimCeilingFactor: the WAL ceiling is this multiple of the
	// reclaim threshold. Over it a bounded reclaim may run inside a bulk window.
	walReclaimCeilingFactor = 8
)

// walReclaimCeilingFloor is the lowest the WAL ceiling may be, so a low
// threshold cannot make the lane override fire on small logs. A var only so
// the in-package cases can lower it; production never assigns it.
var walReclaimCeilingFloor int64 = 1 << 30

var (
	errWALReclaimWriterWaiting = errors.New("store_sqlite: wal reclaim: yielded the writer to a queued write")
	errWALReclaimNothing       = errors.New("store_sqlite: wal reclaim: nothing to reclaim")
)

// Vars, not consts, only so the in-package cases can shorten the cadence;
// production never assigns them.
var (
	walReclaimPollInterval   = 5 * time.Second
	walReclaimBackoffInitial = 5 * time.Second
	walReclaimBackoffMax     = 5 * time.Minute
	// walReclaimPressureBackoffMax caps the wait between attempts while the
	// log is over its threshold (the only time the loop attempts).
	walReclaimPressureBackoffMax = 20 * time.Second
	// walReclaimSkipQuiescence disables step 3 for the mutation check that
	// proves the read gate is load-bearing. Never set in production.
	walReclaimSkipQuiescence = false
	// walReclaimSkipOpenGate disables the open-gate stage (3a) for the
	// mutation check that proves it carries the long-reader case. Never set
	// in production.
	walReclaimSkipOpenGate = false
)

var errWALReclaimReadersInFlight = errors.New("store_sqlite: wal reclaim: pool readers still in flight at the drain deadline")

type walReclaimConfig struct {
	thresholdBytes int64
	drainDeadline  time.Duration
	truncateBudget time.Duration
	// readerWait bounds the open-gate stage (0: skip it and go straight to
	// the closed-gate drain).
	readerWait time.Duration
	// ceilingBytes is the WAL ceiling: over it an attempt may override the
	// generation bulk lease (0: never).
	ceilingBytes int64
}

// walReclaimCeiling is the WAL ceiling for a threshold: the override when one
// is given (GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB), else walReclaimCeilingFactor
// × the threshold, never below walReclaimCeilingFloor. 0 for a disabled
// reclaim.
func walReclaimCeiling(thresholdBytes, overrideBytes int64) int64 {
	if thresholdBytes <= 0 {
		return 0
	}
	ceiling := walReclaimCeilingFactor * thresholdBytes
	if overrideBytes > 0 {
		ceiling = overrideBytes
	}
	return max(ceiling, walReclaimCeilingFloor)
}

// resolveWALReclaimConfig reads the operator overrides once per store:
// GORTEX_SQLITE_WAL_RECLAIM_MB (default 256; 0 disables the reclaim) and
// GORTEX_SQLITE_WAL_RECLAIM_DRAIN_MS (default 250; the longest a new pool
// read may be held while in-flight reads drain). Unparseable or negative
// input fails open to the default.
func resolveWALReclaimConfig() walReclaimConfig {
	cfg := walReclaimConfig{
		thresholdBytes: defaultWALReclaimThresholdBytes,
		drainDeadline:  defaultWALReclaimDrainDeadline,
		truncateBudget: walReclaimTruncateBudget,
		readerWait:     defaultWALReclaimReaderWait,
	}
	// GORTEX_SQLITE_WAL_RECLAIM_READER_WAIT_MS (default 2000, capped at 8000;
	// 0 disables the open-gate stage).
	if raw := strings.TrimSpace(os.Getenv("GORTEX_SQLITE_WAL_RECLAIM_READER_WAIT_MS")); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms >= 0 {
			cfg.readerWait = min(time.Duration(ms)*time.Millisecond, walReclaimReaderWaitMax)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("GORTEX_SQLITE_WAL_RECLAIM_MB")); raw != "" {
		if mb, err := strconv.ParseInt(raw, 10, 64); err == nil && mb >= 0 {
			if mb > 1<<22 {
				mb = 1 << 22
			}
			cfg.thresholdBytes = mb << 20
		}
	}
	if raw := strings.TrimSpace(os.Getenv("GORTEX_SQLITE_WAL_RECLAIM_DRAIN_MS")); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
			if ms > 10_000 {
				ms = 10_000
			}
			cfg.drainDeadline = time.Duration(ms) * time.Millisecond
		}
	}
	// GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB (default 8 × the threshold, never
	// below 1 GiB).
	var ceilingOverride int64
	if raw := strings.TrimSpace(os.Getenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB")); raw != "" {
		if mb, err := strconv.ParseInt(raw, 10, 64); err == nil && mb > 0 {
			ceilingOverride = min(mb, 1<<22) << 20
		}
	}
	cfg.ceilingBytes = walReclaimCeiling(cfg.thresholdBytes, ceilingOverride)
	return cfg
}

// WALReclaimStats is the bounded reclaim's telemetry. Pause is how long the
// read gate was closed per attempt that closed it; ReaderWait is what readers
// actually spent held at the gate (per held read), measured on their side.
type WALReclaimStats struct {
	ThresholdBytes  int64
	Attempts        int64 // attempts that passed the size check and the lease
	Resets          int64 // TRUNCATE checkpoints that reset the log
	OpenGateResets  int64 // resets reached without closing the read gate
	WriterHoldMax   time.Duration
	WriterHoldLast  time.Duration
	Deferrals       int64 // attempts that gave up and backed off
	Skips           int64 // refused: bulk lease/connection, checkpoint in flight
	Failures        int64 // driver/I/O errors (also backed off)
	FramesReclaimed int64 // WAL frames discarded by successful resets
	BytesReclaimed  int64 // -wal bytes returned to the filesystem
	PauseCount      int64
	PauseTotal      time.Duration
	PauseMax        time.Duration
	PauseLast       time.Duration
	ReaderWaits     int64
	ReaderWaitTotal time.Duration
	ReaderWaitMax   time.Duration
	Backoff         time.Duration // current backoff after the last attempt
	LastOutcome     string
	LastReason      string
	CeilingBytes    int64
	// LeaseOverrides counts reclaim attempts run inside a generation bulk
	// window because the WAL was over its ceiling.
	LeaseOverrides int64
}

type walReclaimState struct {
	mu    sync.Mutex
	stats WALReclaimStats
	// cycle holds only bulk-lease override counters at this stage.
	cycle struct {
		leaseOverrides    atomic.Int64
		lastLeaseOverride atomic.Int64
	}
}

// WALReclaimStats returns a snapshot of the bounded reclaim's counters.
func (s *Store) WALReclaimStats() WALReclaimStats {
	if s.coreless() {
		return WALReclaimStats{}
	}
	s.walReclaim.mu.Lock()
	out := s.walReclaim.stats
	s.walReclaim.mu.Unlock()
	cycle := &s.walReclaim.cycle
	out.LeaseOverrides = cycle.leaseOverrides.Load()
	if g := s.readGate; g != nil {
		out.ReaderWaits = g.waits.Load()
		out.ReaderWaitTotal = time.Duration(g.waitNanos.Load())
		out.ReaderWaitMax = time.Duration(g.maxWaitNano.Load())
	}
	return out
}

func (st *walReclaimState) update(fn func(*WALReclaimStats)) {
	st.mu.Lock()
	fn(&st.stats)
	st.mu.Unlock()
}

func (st *WALReclaimStats) notePause(d time.Duration) {
	st.PauseCount++
	st.PauseTotal += d
	st.PauseLast = d
	if d > st.PauseMax {
		st.PauseMax = d
	}
}

type walReclaimOutcome int

const (
	walReclaimUndecided walReclaimOutcome = iota
	walReclaimSkipped
	walReclaimReset
	walReclaimDeferred
	walReclaimFailed
)

func (o walReclaimOutcome) String() string {
	switch o {
	case walReclaimReset:
		return "reset"
	case walReclaimDeferred:
		return "deferred"
	case walReclaimFailed:
		return "failed"
	case walReclaimSkipped:
		return "skipped"
	default:
		return "undecided"
	}
}

type walReclaimResult struct {
	outcome     walReclaimOutcome
	reason      string
	frames      int
	pause       time.Duration
	pauseClosed bool
	openGate    bool
	writerHold  time.Duration
	// openGateWaited is the writer-free wait for old readers (step 2).
	openGateWaited time.Duration
	// converged: step 2 backfilled the whole log (convergedFrames frames)
	// and every reader admitted before that point has left, so any reader
	// still in flight took read mark 0 unless the log grew since.
	converged       bool
	convergedFrames int
	// urgent: the log is at walReclaimUrgentFactor × the threshold.
	urgent bool
	// resetWriterFree: step 2's TRUNCATE reset the log without the
	// application writer.
	resetWriterFree bool
	// blocker is the oldest reader that outlived the attempt's wait, when
	// one did.
	blocker    activeReader
	hasBlocker bool
	// openGateReport describes the open-gate stage's give-up, "" when it
	// reset or did not run.
	openGateReport string
	bytesBefore    int64
	bytesAfter     int64
	// leaseOverride: the attempt runs inside a bulk window (WAL over the
	// ceiling); its writer step accepts the window's pinned connection.
	leaseOverride bool
	// convergence is what the writer-free passes before the writer step did
	// (nil when they did not run).
	convergence *walReclaimConvergence
	// progressed: the attempt advanced the backfill (nBackfill moved) or the
	// log was reset — a failure that made progress does not back off.
	progressed bool
}

// reclaimWALOnce runs one bounded reclaim attempt and records its counters.
// It never blocks readers longer than cfg.drainDeadline + cfg.truncateBudget
// and never runs while the generation bulk checkpoint lease is held.
func (s *Store) reclaimWALOnce(cfg walReclaimConfig, ckptDB *sql.DB, walPath string) walReclaimResult {
	before, beforeOK := readWALIndexSnapshot(s.dbPath)
	res := s.reclaimWALAttempt(cfg, ckptDB, walPath)
	if after, ok := readWALIndexSnapshot(s.dbPath); ok && beforeOK {
		res.progressed = after.NBackfill > before.NBackfill || after.MxFrame < before.MxFrame
	}
	if res.outcome == walReclaimReset {
		res.progressed = true
	}
	s.walReclaim.update(func(st *WALReclaimStats) {
		st.ThresholdBytes = cfg.thresholdBytes
		st.LastOutcome = res.outcome.String()
		st.LastReason = res.reason
		if res.writerHold > 0 {
			st.WriterHoldLast = res.writerHold
			st.WriterHoldMax = max(st.WriterHoldMax, res.writerHold)
		}
		if res.pauseClosed {
			st.notePause(res.pause)
		}
		switch res.outcome {
		case walReclaimSkipped:
			st.Skips++
			return
		case walReclaimReset:
			st.Resets++
			if res.openGate {
				st.OpenGateResets++
			}
			st.FramesReclaimed += int64(res.frames)
			if res.bytesBefore > res.bytesAfter {
				st.BytesReclaimed += res.bytesBefore - res.bytesAfter
			}
		case walReclaimDeferred:
			st.Deferrals++
		case walReclaimFailed:
			st.Failures++
		}
		st.Attempts++
	})
	return res
}

func (s *Store) reclaimWALAttempt(cfg walReclaimConfig, ckptDB *sql.DB, walPath string) walReclaimResult {
	if s.coreless() || ckptDB == nil {
		return walReclaimResult{outcome: walReclaimSkipped, reason: "no_store"}
	}
	// Same refusal the background PASSIVE honours: a generation bulk window
	// holding the lease, or another background checkpoint in flight.
	attempt, berr := s.beginReclaimCheckpointAttempt(false)
	if errors.Is(berr, errWALCheckpointInFlight) {
		return walReclaimResult{outcome: walReclaimSkipped, reason: "checkpoint_in_flight"}
	}
	leaseOverride := false
	if errors.Is(berr, errWALCheckpointDeferredBulk) {
		// A generation bulk window holds the checkpoint lease. Bulk windows
		// can last many minutes while their writes grow the log, so over the
		// ceiling one attempt runs anyway — bounded by the writer-hold cap,
		// its writer step taking the write gate between the window's own
		// transactions — at most once per walReclaimLeaseOverrideSpacing.
		size := walFileSize(walPath)
		if cfg.ceilingBytes <= 0 || size < cfg.ceilingBytes || !s.leaseOverrideDue(time.Now()) {
			return walReclaimResult{outcome: walReclaimSkipped, reason: "checkpoint_lease"}
		}
		attempt, berr = s.beginReclaimCheckpointAttempt(true)
		if berr != nil {
			return walReclaimResult{outcome: walReclaimSkipped, reason: "checkpoint_in_flight"}
		}
		leaseOverride = true
		s.walReclaim.cycle.leaseOverrides.Add(1)
		s.walReclaim.cycle.lastLeaseOverride.Store(time.Now().UnixNano())
		log.Printf("store_sqlite: wal reclaim running inside a bulk window reason=wal_ceiling wal_bytes=%d ceiling=%d", size, cfg.ceilingBytes)
	} else if berr != nil {
		return walReclaimResult{outcome: walReclaimSkipped, reason: "checkpoint_lease"}
	}
	defer s.finishBackgroundCheckpointAttempt(attempt)

	res := walReclaimResult{bytesBefore: walFileSize(walPath), leaseOverride: leaseOverride}
	// Nothing in the log and nothing to shrink: never touch the writer.
	if snap, ok := readWALIndexSnapshot(s.dbPath); ok && snap.MxFrame == 0 && res.bytesBefore <= cfg.thresholdBytes {
		return walReclaimResult{outcome: walReclaimSkipped, reason: "nothing_to_reclaim", bytesBefore: res.bytesBefore}
	}
	// Step 1: the unbounded backfill (see the file comment), without the
	// writer. Its error is not fatal — the lane copies whatever remains —
	// unless the attempt was cancelled.
	_, _ = checkpointWALOnceOn(attempt.ctx, ckptDB, "PASSIVE")
	// Step 2: without the writer and with the gate OPEN, converge on a log
	// no reader pins: backfill, then wait out every reader admitted before
	// that backfill. A reader admitted after a COMPLETE backfill takes read
	// mark 0 and never pins the log; one admitted while it was incomplete
	// (an older reader still needed frames) may, so the loop repeats — each
	// round waits only for readers younger than the last — until a round
	// starts from a complete backfill. This costs no reader and no write
	// anything; the writer is taken only afterwards, for at most
	// walReclaimMaxWriterHold.
	// An urgent log (see walReclaimUrgentFactor) skips straight to the capped
	// writer hold: its writes are not pausing, so the writer-free rounds
	// cannot converge.
	res.urgent = cfg.thresholdBytes > 0 && res.bytesBefore >= walReclaimUrgentFactor*cfg.thresholdBytes
	if !res.urgent && !walReclaimSkipQuiescence && !walReclaimSkipOpenGate && s.readGate != nil && cfg.readerWait > 0 {
		started := time.Now()
		deadline := started.Add(cfg.readerWait)
		// The rounds end early when the log turns urgent meanwhile: the
		// capped writer hold takes over.
		wctx, stopWatch := s.watchWALUrgency(attempt.ctx, walPath, walReclaimUrgentFactor*cfg.thresholdBytes)
		defer stopWatch()
		rounds := 0
		prevFrames := -1
		for {
			rounds++
			pctx, pcancel := context.WithDeadline(wctx, deadline)
			result, perr := checkpointWALOnceOn(pctx, ckptDB, "PASSIVE")
			pcancel()
			complete := perr == nil && !result.incomplete()
			if complete && !s.writeWanted() {
				// Let SQLite decide first: readers that began after a
				// complete backfill hold slot 0 and do not block a reset. The
				// TRUNCATE takes SQLite's write lock only for the reset
				// itself (the log is already copied); a busy one returns
				// within the checkpoint connection's busy timeout. Not while
				// a mutation is waiting for the writer.
				tctx, tcancel := context.WithTimeout(wctx, cfg.truncateBudget)
				_, terr := checkpointWALOnceOn(tctx, ckptDB, "TRUNCATE")
				tcancel()
				if terr == nil {
					res.resetWriterFree = true
					break
				}
			}
			epoch := s.readGate.advance()
			older, werr := s.readGate.waitOlder(wctx, epoch, deadline)
			res.openGateWaited = time.Since(started)
			if werr != nil {
				res.blocker, res.hasBlocker = s.readGate.oldestOlderThan(epoch, time.Now())
				reason := "older_readers_outlasted_wait"
				switch {
				case attempt.ctx.Err() != nil:
					reason = "cancelled"
				case wctx.Err() != nil:
					reason = "log_turned_urgent"
				}
				res.openGateReport = fmt.Sprintf("reason=%s older_readers=%d waited=%s rounds=%d backfilled=%d/%d",
					reason, older, res.openGateWaited.Round(time.Millisecond), rounds, result.CheckpointedFrames, result.WALFrames)
				break
			}
			if complete {
				res.converged, res.convergedFrames = true, result.WALFrames
				break
			}
			if !time.Now().Before(deadline) {
				break
			}
			if prevFrames >= 0 && result.WALFrames > prevFrames {
				// Writes keep landing: without the writer the log cannot be
				// fully backfilled. The capped writer hold finishes the job.
				res.openGateReport = fmt.Sprintf("reason=writes_active waited=%s rounds=%d backfilled=%d/%d",
					res.openGateWaited.Round(time.Millisecond), rounds, result.CheckpointedFrames, result.WALFrames)
				break
			}
			prevFrames = result.WALFrames
		}
	}
	if res.resetWriterFree {
		res.openGate = true
		res.outcome = walReclaimReset
		res.bytesAfter = walFileSize(walPath)
		return res
	}
	if cfg.thresholdBytes > 0 && walFileSize(walPath) >= walReclaimUrgentFactor*cfg.thresholdBytes {
		res.urgent = true
	}
	// Before the writer: converge the backfill so the capped writer step
	// only has to copy what fits its slice (wal_reclaim_converge.go).
	if attempt.ctx.Err() == nil {
		conv := s.convergeBackfill(attempt.ctx, ckptDB)
		res.convergence = &conv
	}
	ctx, cancel := context.WithTimeout(attempt.ctx, walReclaimLaneBudget)
	defer cancel()
	var err error
	if attempt.ctx.Err() != nil {
		err = attempt.ctx.Err()
	} else {
		err = s.runMaintenance(ctx, maintenanceWALReclaim, false, func(ctx context.Context) error {
			return s.reclaimWALInLane(ctx, cfg, ckptDB, &res)
		})
	}
	res.bytesAfter = walFileSize(walPath)
	if err == nil {
		res.outcome = walReclaimReset
		return res
	}
	if errors.Is(context.Cause(attempt.ctx), errWALCheckpointDeferredBulk) {
		res.outcome, res.reason = walReclaimSkipped, "bulk_lease"
		return res
	}
	if attempt.ctx.Err() != nil {
		res.outcome, res.reason = walReclaimSkipped, "shutdown"
		return res
	}
	if res.reason == "" {
		res.reason = err.Error()
	}
	switch {
	case res.outcome == walReclaimSkipped:
		// set by the lane body (bulk connection pinned)
	case errors.Is(err, ErrMaintenanceBusy), errors.Is(err, errWALReclaimReadersInFlight),
		errors.Is(err, context.DeadlineExceeded), isSQLiteBusyErr(err):
		res.outcome = walReclaimDeferred
	default:
		res.outcome = walReclaimFailed
	}
	return res
}

func (s *Store) reclaimWALInLane(ctx context.Context, cfg walReclaimConfig, ckptDB *sql.DB, res *walReclaimResult) error {
	urgent := res.urgent
	// A mutation already waiting for the writer wins outright (unless the log
	// is urgent; the hold stays capped).
	if !urgent && s.writeWanted() {
		res.outcome, res.reason = walReclaimSkipped, "writer_waiting"
		return errWALReclaimWriterWaiting
	}
	// 3. Writer quiescence, capped at walReclaimMaxWriterHold from here on.
	wctx, wcancel := context.WithTimeout(ctx, walReclaimWriterWait)
	err := s.writeMu.LockContext(wctx)
	wcancel()
	if err != nil {
		res.reason = "writer_gate"
		return fmt.Errorf("%w: %w", ErrMaintenanceBusy, err)
	}
	held := time.Now()
	defer func() {
		res.writerHold = time.Since(held)
		s.writeMu.Unlock()
	}()
	if s.bulkConn != nil && !res.leaseOverride {
		res.outcome, res.reason = walReclaimSkipped, "bulk_writer"
		return errWALCheckpointDeferredBulk
	}
	hctx, hcancel := context.WithDeadline(ctx, held.Add(walReclaimMaxWriterHold))
	defer hcancel()
	yctx, stopYield := hctx, func() {}
	if !urgent {
		yctx, stopYield = s.yieldToWriters(hctx)
	}
	defer stopYield()
	yielded := func() bool { return errors.Is(context.Cause(yctx), errWALReclaimWriterWaiting) }
	giveUp := func(reason string, cause error) error {
		if yielded() {
			res.outcome, res.reason = walReclaimSkipped, "writer_waiting"
			return errWALReclaimWriterWaiting
		}
		res.reason = reason
		if cause == nil {
			cause = errWALReclaimReadersInFlight
		}
		return fmt.Errorf("%w: %s", cause, reason)
	}

	// 4. Final backfill of what the writer-free steps left. One PASSIVE, no
	// retry: an interrupted or refused PASSIVE reports 0/0 and must not be
	// spun on while the writer is held.
	backfill := func() (walCheckpointResult, error) {
		result, err := checkpointWALOnceOn(yctx, ckptDB, "PASSIVE")
		if err != nil && errors.Is(err, errSQLiteCheckpointIncomplete) && yctx.Err() == nil {
			// Incomplete is a result, not a failure: a reader still needs
			// frames (result carries the counts).
			return result, nil
		}
		return result, err
	}
	delta, derr := backfill()
	res.frames = delta.WALFrames
	if derr != nil {
		return giveUp(fmt.Sprintf("backfill_failed error=%v", derr), errWALReclaimReadersInFlight)
	}
	if delta.WALFrames == 0 && res.bytesBefore <= cfg.thresholdBytes {
		res.outcome, res.reason = walReclaimSkipped, "nothing_to_reclaim"
		return errWALReclaimNothing
	}

	// 5. With writes stopped, finish converging inside the hold cap: if step
	// 2 converged and nothing was committed since, no reader in flight can pin
	// the log and the reset needs no wait; otherwise backfill, wait out the
	// readers admitted before that backfill (gate open), and repeat until a
	// round starts from a complete backfill.
	quiesce := !walReclaimSkipQuiescence && s.readGate != nil
	openGate := quiesce && !walReclaimSkipOpenGate && cfg.readerWait > 0
	if openGate {
		capAt := held.Add(walReclaimMaxWriterHold)
		// A complete backfill may already be resettable: SQLite blocks a
		// TRUNCATE only on readers holding a WAL read mark (slots 1..n); a
		// reader that began after the backfill completed took slot 0 and does
		// not block it. The gate's admission epochs cannot tell the two apart
		// (they count every active connection), so ask SQLite first, for one
		// busy-timeout, before waiting on anybody.
		if !delta.incomplete() {
			// With the writer held and the log backfilled the reset only has
			// to truncate the file (and sync what the last copy wrote), so it
			// gets the rest of the hold cap rather than a fixed slice: a big
			// -wal can take longer than walReclaimTruncateBudget to free.
			tctx, tcancel := context.WithDeadline(yctx, capAt)
			_, terr := checkpointWALOnceOn(tctx, ckptDB, "TRUNCATE")
			tcancel()
			if terr == nil {
				res.openGate = true
				return nil
			}
			if yielded() {
				return giveUp("", nil)
			}
		}
		ready := res.converged && !delta.incomplete() && delta.WALFrames == res.convergedFrames
		for !ready {
			complete := !delta.incomplete()
			epoch := s.readGate.advance()
			if _, err := s.readGate.waitOlder(yctx, epoch, capAt); err != nil {
				res.blocker, res.hasBlocker = s.readGate.oldestOlderThan(epoch, time.Now())
				return giveUp(fmt.Sprintf("writer_hold_cap older_readers_in_flight backfilled=%d/%d", delta.CheckpointedFrames, delta.WALFrames), nil)
			}
			if complete {
				break
			}
			if delta, derr = backfill(); derr != nil {
				return giveUp(fmt.Sprintf("backfill_failed error=%v", derr), nil)
			}
		}
		tctx, tcancel := context.WithDeadline(yctx, capAt)
		result, terr := checkpointWALOnceOn(tctx, ckptDB, "TRUNCATE")
		tcancel()
		if terr == nil {
			res.openGate = true
			return nil
		}
		return giveUp(fmt.Sprintf("truncate busy=%d wal_frames=%d checkpointed=%d error=%v", result.Busy, result.WALFrames, result.CheckpointedFrames, terr), terr)
	}
	// 6. Closed-gate path (the open-gate stages disabled): only for a residue
	// its TRUNCATE can copy inside its budget, and only when the hold cap
	// still has room for it. With the open-gate stages on, no reader is ever
	// paused.
	if residue := delta.WALFrames - delta.CheckpointedFrames; residue > walReclaimClosedGateMaxResidue {
		return giveUp(fmt.Sprintf("backfill_incomplete residue_frames=%d", residue), nil)
	}
	if room := time.Until(held.Add(walReclaimMaxWriterHold)); room < cfg.truncateBudget {
		return giveUp(fmt.Sprintf("writer_hold_cap room=%s", room.Round(time.Millisecond)), nil)
	}
	start := time.Now()
	reopen := func() {}
	if quiesce {
		var inFlight int
		reopen, inFlight, err = s.readGate.quiesce(yctx, start.Add(cfg.drainDeadline))
		if err != nil {
			res.pause, res.pauseClosed = time.Since(start), true
			return giveUp(fmt.Sprintf("readers_in_flight=%d", inFlight), nil)
		}
	}
	tctx, tcancel := context.WithTimeout(yctx, cfg.truncateBudget)
	result, err := checkpointWALOnceOn(tctx, ckptDB, "TRUNCATE")
	tcancel()
	reopen()
	res.pause, res.pauseClosed = time.Since(start), quiesce
	if err != nil {
		return giveUp(fmt.Sprintf("truncate busy=%d wal_frames=%d checkpointed=%d error=%v", result.Busy, result.WALFrames, result.CheckpointedFrames, err), err)
	}
	return nil
}

// yieldToWriters returns a context that is cancelled (cause
// errWALReclaimWriterWaiting) as soon as a write wants the writer — a caller
// parked on the write gate, or a mutation announced through AnnounceWrite
// before it waits on anything else — so every checkpoint running under it is
// interrupted and the writer handed back within walReclaimYieldPoll.
func (s *Store) yieldToWriters(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(walReclaimYieldPoll)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if s.writeWanted() {
					cancel(errWALReclaimWriterWaiting)
					return
				}
			}
		}
	}()
	return ctx, func() {
		close(done)
		cancel(context.Canceled)
	}
}

// writeWanted reports a write waiting for the writer: a caller parked on the
// write gate, or an announced mutation.
func (s *Store) writeWanted() bool {
	return s.writeMu.waiting() > 0 || s.writeIntents.Load() > 0
}

// AnnounceWrite marks a mutation that is about to need the store's writer.
// A mutating request (an edit's admission, a build about to publish) calls it
// before it waits on ANY lock of its own — the coordinator's cycle, the build
// lane, a bulk window — and releases it when it has the writer or gives up.
// Background holders of the writer (the WAL reclaim) yield to it at once, so a
// mutation queued behind them upstream of the write gate is not held by them
// either. The release is idempotent.
func (s *Store) AnnounceWrite() (release func()) {
	if s.coreless() {
		return func() {}
	}
	s.writeIntents.Add(1)
	var once sync.Once
	return func() { once.Do(func() { s.writeIntents.Add(-1) }) }
}

func walFileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func openWALReclaimCheckpointDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteCheckpointDSN(dbPath))
	if err != nil {
		return nil, err
	}
	configureWriterPool(db)
	return db, nil
}

// walReclaimSchedule is the loop's backoff: a deferral or failure pushes the
// next attempt out by the current backoff and doubles it (capped); a reset
// restores the initial backoff; a skip (bulk window, checkpoint in flight) is
// retried at the next poll without backing off, because it is someone else's
// bounded window rather than contention the reclaim caused.
type walReclaimSchedule struct {
	initial, max time.Duration
	backoff      time.Duration
	nextAt       time.Time
}

func newWALReclaimSchedule(initial, maximum time.Duration) walReclaimSchedule {
	if initial <= 0 {
		initial = walReclaimBackoffInitial
	}
	if maximum < initial {
		maximum = initial
	}
	return walReclaimSchedule{initial: initial, max: maximum, backoff: initial}
}

func (s *walReclaimSchedule) ready(now time.Time) bool { return !now.Before(s.nextAt) }

func (s *walReclaimSchedule) observe(now time.Time, outcome walReclaimOutcome) {
	s.observeProgress(now, outcome, false)
}

// observeProgress is observe with the attempt's progress: a deferral or
// failure that advanced the backfill is not contention the reclaim can wait
// out — the log is being worked down — so it retries after the initial
// backoff and restores it, instead of doubling. Only a failure that moved
// nothing doubles.
func (s *walReclaimSchedule) observeProgress(now time.Time, outcome walReclaimOutcome, progressed bool) {
	switch outcome {
	case walReclaimReset:
		s.backoff = s.initial
		s.nextAt = time.Time{}
	case walReclaimDeferred, walReclaimFailed:
		if progressed {
			s.backoff = s.initial
			s.nextAt = now.Add(s.initial)
			return
		}
		s.nextAt = now.Add(s.backoff)
		s.backoff = growWALCheckpointRetry(s.backoff, s.max)
	}
}

// startWALReclaimLoop starts the reclaim's poller for an on-disk store and
// returns the join the checkpoint loop's cleanup calls. The poller exits when
// stopCheckpoint closes; an attempt in flight is cancelled by the same signal
// through its background-attempt context.
func (s *Store) startWALReclaimLoop(walPath string) (join func()) {
	cfg := resolveWALReclaimConfig()
	if s.coreless() || cfg.thresholdBytes <= 0 || s.stopCheckpoint == nil || s.db == s.writerDB {
		return func() {}
	}
	s.walReclaim.update(func(st *WALReclaimStats) {
		st.ThresholdBytes = cfg.thresholdBytes
		st.CeilingBytes = cfg.ceilingBytes
	})
	done := make(chan struct{})
	go func() {
		s.runWALReclaimLoop(cfg, walPath, walReclaimPollInterval, done)
	}()
	return func() { <-done }
}

func (s *Store) runWALReclaimLoop(cfg walReclaimConfig, walPath string, poll time.Duration, done chan<- struct{}) {
	defer close(done)
	var ckptDB *sql.DB
	defer func() {
		if ckptDB != nil {
			_ = ckptDB.Close()
		}
	}()
	// The loop attempts only while the log is over its threshold, so the
	// pressure cap bounds every wait between attempts there.
	schedule := newWALReclaimSchedule(walReclaimBackoffInitial, min(walReclaimBackoffMax, walReclaimPressureBackoffMax))
	var skipLog walReclaimSkipLog
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCheckpoint:
			return
		case <-ticker.C:
		}
		now := time.Now()
		if s.walReclaimNudged.Swap(false) {
			// A residue drain handed its busy TRUNCATE over: try now.
			schedule.nextAt = now
		}
		if !schedule.ready(now) || walFileSize(walPath) <= cfg.thresholdBytes {
			continue
		}
		if ckptDB == nil {
			db, err := openWALReclaimCheckpointDB(s.dbPath)
			if err != nil {
				log.Printf("store_sqlite: wal reclaim deferred reason=open error=%q", err)
				schedule.observe(now, walReclaimFailed)
				continue
			}
			ckptDB = db
		}
		res := s.reclaimWALOnce(cfg, ckptDB, walPath)
		schedule.observeProgress(now, res.outcome, res.progressed)
		s.walReclaim.update(func(st *WALReclaimStats) { st.Backoff = schedule.backoff })
		logWALReclaimOutcome(res, schedule.nextAt.Sub(now), &skipLog, now)
	}
}

// walReclaimSkipLog rate-limits the skip line: a skip (someone else's bulk
// window or checkpoint, a queued writer) retries at the next poll, so one line
// a minute with the count says the loop is alive without flooding the log.
type walReclaimSkipLog struct {
	count  int
	reason string
	last   time.Time
}

const walReclaimSkipLogInterval = time.Minute

// walReclaimBlockedReaderWarnAge is the reader age from which an attempt that
// gave up on it logs the reader's identity as a warning. A var only so the
// in-package cases can lower it.
var walReclaimBlockedReaderWarnAge = time.Minute

// logWALReclaimOutcome writes one line per reset (with the stage that reset),
// one per give-up (with the open-gate stage's own reason when it ran), and a
// rate-limited summary of skips.
func logWALReclaimOutcome(res walReclaimResult, next time.Duration, skips *walReclaimSkipLog, now time.Time) {
	stage := "closed_gate"
	if res.openGate {
		stage = "open_gate"
	}
	switch res.outcome {
	case walReclaimReset:
		log.Printf("store_sqlite: wal reclaimed stage=%s bytes_before=%d bytes_after=%d frames=%d reader_pause=%s writer_hold=%s%s",
			stage, res.bytesBefore, res.bytesAfter, res.frames, res.pause, res.writerHold.Round(time.Millisecond), res.convergenceSuffix())
	case walReclaimDeferred, walReclaimFailed:
		if res.hasBlocker && res.blocker.Age >= walReclaimBlockedReaderWarnAge {
			log.Printf("store_sqlite: WARN wal reclaim blocked by a long reader: age=%s admitted_epoch=%d statement=%q (a read transaction that old pins the WAL snapshot; the log cannot reset until it ends)",
				res.blocker.Age.Round(time.Second), res.blocker.Epoch, res.blocker.Label)
		}
		if res.openGateReport != "" {
			log.Printf("store_sqlite: wal reclaim open_gate gave up %s writer_hold=%s", res.openGateReport, res.writerHold.Round(time.Millisecond))
		}
		log.Printf("store_sqlite: wal reclaim %s reason=%q wal_bytes=%d reader_pause=%s writer_hold=%s progressed=%t next_attempt_in=%s%s",
			res.outcome, res.reason, res.bytesBefore, res.pause, res.writerHold.Round(time.Millisecond), res.progressed, next, res.convergenceSuffix())
	case walReclaimSkipped:
		skips.count++
		skips.reason = res.reason
		if res.openGateReport != "" {
			skips.reason += " (open_gate " + res.openGateReport + ")"
		}
		if now.Sub(skips.last) >= walReclaimSkipLogInterval {
			log.Printf("store_sqlite: wal reclaim skipped n=%d last_reason=%q wal_bytes=%d", skips.count, skips.reason, res.bytesBefore)
			skips.count, skips.last = 0, now
		}
	}
}

// watchWALUrgency returns a context cancelled once the -wal file reaches
// urgentBytes (polled every 50 ms), so the writer-free rounds hand over to the
// capped writer hold as soon as the log's growth makes them pointless.
func (s *Store) watchWALUrgency(parent context.Context, walPath string, urgentBytes int64) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	if urgentBytes <= 0 {
		return ctx, cancel
	}
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if walFileSize(walPath) >= urgentBytes {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}
