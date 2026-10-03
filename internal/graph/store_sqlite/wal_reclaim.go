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
//     cancels the attempt) or a mutation cycle taking the build lane
//     (checkpoint_cycle_yield.go) stops it. This is what lets a store that reopened
//     over a huge recovered log (nBackfill restarts at zero after a crash or a
//     killed shutdown) catch up in one pass instead of never;
//  2. take the write gate (bounded wait) so the application writer is idle,
//     refuse while a bulk connection is pinned, and PASSIVE the small delta;
//  3. close the read-pool gate (sqliteReadGate): new reads wait, reads already
//     in flight are waited for up to the drain deadline (default 250 ms);
//  4. PRAGMA wal_checkpoint(TRUNCATE) on the writer connection (held under
//     the write gate) with a 100 ms busy timeout and its own budget, then
//     reopen the gate. The copies (PASSIVE) run on a dedicated checkpoint
//     connection; the reset does not, because a log reset by another
//     connection empties the writer connection's page cache.
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
	// defaultWALReclaimReaderWait bounds the writer-free rounds (step 2):
	// once the log is fully backfilled, new read transactions take read mark
	// 0 (they read the database file only and never pin the log), so the
	// reclaim only has to outlast the reads admitted before that point —
	// without holding any reader and without the writer. Long enough to
	// outlast the daemon's long analysis reads (tens of seconds).
	defaultWALReclaimReaderWait = 30 * time.Second
	walReclaimReaderWaitMax     = 60 * time.Second
	// walReclaimMaxWriterHold caps how long the closed-gate path (only with
	// the open-gate stages disabled) keeps the application writer. The
	// open-gate stages normally use walReclaimResetHold (50 ms); an urgent
	// attempt with a proven slow-copy tail may take one adaptive completion
	// slice, sharing this two-second cap with all its prior holds. An admitted
	// over-ceiling bulk override uses the existing two-second completion hold
	// with the read gate open, before continuous writes grow the log further. The
	// wait for old readers runs without the writer, which is taken only for
	// the final backfill and the reset. A queued write or an announced
	// mutation (AnnounceWrite) ends a hold at once.
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
	// reclaim threshold. Over it a reclaim runs even while a mutation cycle
	// holds the build lane (checkpoint_cycle_yield.go).
	walReclaimCeilingFactor = 8
	// walReclaimHardCapFactor: over this multiple of the ceiling one reclaim
	// attempt may run while an edit cycle holds the build lane.
	walReclaimHardCapFactor = 4
	// walReclaimLastResortFactor places the last resort, in multiples of the
	// ceiling (4: the hard cap, 8 GiB with the defaults). From there an
	// attempt holds the writer up to walReclaimMaxWriterHold while it waits
	// for the readers admitted before its copy, and logs it: the one step
	// that bounds the log when writes never leave a gap long enough for the
	// short holds. Below it, no attempt holds the writer longer than
	// walReclaimResetHold, except for that proven urgent slow-copy tail or an
	// admitted over-ceiling bulk completion.
	walReclaimLastResortFactor = 4
)

// walReclaimLastResortBytes is the log size of the last resort (0: none).
func walReclaimLastResortBytes(cfg walReclaimConfig) int64 {
	if cfg.ceilingBytes <= 0 {
		return 0
	}
	return walReclaimLastResortFactor * cfg.ceilingBytes
}

// walReclaimHardCapSpacing spaces the attempts the hard cap lets run during
// edit cycles. A var only so the in-package cases can shorten it.
var walReclaimHardCapSpacing = 30 * time.Second

func (s *Store) hardCapDue(now time.Time) bool {
	last := s.walReclaim.cycle.lastHardCap.Load()
	return last == 0 || now.Sub(time.Unix(0, last)) >= walReclaimHardCapSpacing
}

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
	// ceilingBytes is the WAL ceiling: over it an attempt runs despite a
	// mutation cycle holding the build lane (0: never).
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
	ThresholdBytes int64
	Attempts       int64 // attempts that passed the size check and the lease
	Resets         int64 // TRUNCATE checkpoints that reset the log
	OpenGateResets int64 // resets reached without closing the read gate
	// LastResortRuns counts attempts at walReclaimLastResortBytes (each may
	// hold the writer up to walReclaimMaxWriterHold).
	LastResortRuns int64
	WriterHoldMax  time.Duration
	WriterHoldLast time.Duration
	// AdaptiveWriterAttempts counts attempts selecting the longer urgent slice
	// permitted after a completed slow copy acquires a new WAL tail.
	AdaptiveWriterAttempts  int64
	AdaptiveWriterHoldMax   time.Duration
	AdaptiveWriterBudgetMax time.Duration
	Deferrals               int64 // attempts that gave up and backed off
	Skips                   int64 // refused: bulk lease/connection, checkpoint in flight
	Failures                int64 // driver/I/O errors (also backed off)
	FramesReclaimed         int64 // WAL frames discarded by successful resets
	BytesReclaimed          int64 // -wal bytes returned to the filesystem
	PauseCount              int64
	PauseTotal              time.Duration
	PauseMax                time.Duration
	PauseLast               time.Duration
	ReaderWaits             int64
	ReaderWaitTotal         time.Duration
	ReaderWaitMax           time.Duration
	Backoff                 time.Duration // current backoff after the last attempt
	LastOutcome             string
	LastReason              string
	// Build-lane yield (checkpoint_cycle_yield.go): PASSIVE attempts deferred
	// while a mutation cycle held the lane, reclaim attempts refused for the
	// same reason, background attempts of either kind a cycle cut short, and
	// PASSIVE attempts forced past the deferral bound.
	CycleDeferrals int64
	CycleRefusals  int64
	CycleYields    int64
	CycleForced    int64
	// CycleCeilingRuns counts reclaim attempts run despite a held lane
	// because the WAL was over its ceiling.
	CycleCeilingRuns int64
	CeilingBytes     int64
	// LeaseOverrides counts reclaim attempts run inside a generation bulk
	// window because the WAL was over its ceiling.
	LeaseOverrides int64
	// RetirementWaits counts retirement chunks that waited for the reclaim
	// with the WAL over its ceiling; RetirementWaitTimeouts the waits that
	// ran out and proceeded anyway.
	RetirementWaits        int64
	RetirementWaitTimeouts int64
	// RetirementEditYields counts retirement chunks that waited for an
	// edit-path writer (an announced mutation or a writer parked on the
	// gate); RetirementEditYieldTimeouts the waits that ran out.
	RetirementEditYields        int64
	RetirementEditYieldTimeouts int64
	// The incremental shrink (wal_shrink.go): big logs reset in place, the
	// slices that shrank their file afterwards, the bytes those slices
	// returned, and the longest writer hold of one slice.
	ShrinkInPlaceResets int64
	ShrinkSlices        int64
	ShrinkBytes         int64
	ShrinkSliceHoldMax  time.Duration
}

type walReclaimState struct {
	mu    sync.Mutex
	stats WALReclaimStats
	// cycle is the build-lane yield shared by the reclaim and the PASSIVE
	// loop (checkpoint_cycle_yield.go).
	cycle checkpointCycleYield
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
	out.CycleDeferrals = cycle.deferrals.Load()
	out.CycleRefusals = cycle.refusals.Load()
	out.CycleYields = cycle.yields.Load()
	out.CycleForced = cycle.forced.Load()
	out.CycleCeilingRuns = cycle.ceiling.Load()
	out.LeaseOverrides = cycle.leaseOverrides.Load()
	out.RetirementWaits = cycle.retirementWaits.Load()
	out.RetirementWaitTimeouts = cycle.retirementTimeouts.Load()
	out.RetirementEditYields = cycle.retirementEditYields.Load()
	out.RetirementEditYieldTimeouts = cycle.retirementEditYieldTimeouts.Load()
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

type walReclaimResetReaders struct {
	epoch uint64
	older int
}

type walReclaimResult struct {
	outcome     walReclaimOutcome
	reason      string
	frames      int
	pause       time.Duration
	pauseClosed bool
	openGate    bool
	// writerHold is the longest single hold of the writer; writerHolds
	// counts the holds (the in-lane stage may take several short ones).
	writerHold     time.Duration
	writerHolds    int
	writerSpent    time.Duration
	adaptiveUsed   bool
	adaptiveBudget time.Duration
	adaptiveHold   time.Duration
	slowTail       *walReclaimSlowTail
	// openGateWaited is the writer-free wait for old readers (step 2).
	openGateWaited time.Duration
	// resetReaders is captured before a positive complete-copy reset. A busy
	// reset (including exhaustion of its own short context) can wait for one
	// member to retire, then ask SQLite again without waiting for harmless
	// database-only readers. Each reset hold clears this transient witness.
	resetReaders *walReclaimResetReaders
	// converged: step 2 backfilled the whole log (convergedFrames frames)
	// and every reader admitted before that point has left, so any reader
	// still in flight took read mark 0 unless the log grew since.
	converged       bool
	convergedFrames int
	// urgent: the log is at walReclaimUrgentFactor × the threshold.
	urgent bool
	// hardCap: the attempt runs despite a busy lane (walReclaimHardCapFactor).
	hardCap bool
	// lastResort: the log is at walReclaimLastResortBytes; this attempt may
	// hold the writer up to walReclaimMaxWriterHold (logged).
	lastResort bool
	// A round promoted after growth must recheck its same-WAL frontier at writer admission.
	lastResortPromotion *walReclaimFrontier
	// bulkCompletion: an admitted over-ceiling bulk lease override may finish
	// a reader-pinned tail inside the same bounded, open-read-gate hold.
	bulkCompletion bool
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
	// The attempt's stamp (reclaimWALOnce): wall start and length, the
	// process CPU consumed meanwhile (all goroutines: an upper bound for the
	// attempt), and whether an edit cycle held the build lane at its start and
	// at its end.
	started                time.Time
	elapsed, processCPU    time.Duration
	laneAtStart, laneAtEnd bool
	// began: the attempt got past the refusals and ran at least its first
	// backfill, so it may have spent real time even when it ends skipped.
	began bool
	// The wal-index before and after the attempt (framesKnown when both
	// reads succeeded). SQLite records nBackfillAttempted before it copies
	// and nBackfill only after a pass completes, so an attempt cut short
	// shows attempted ahead of backfilled: the pages it copied do not count.
	framesBefore, framesAfter walIndexSnapshot
	framesKnown               bool
	// copy is what the attempt's paced passes did; passDiscarded: the last
	// pass was cut short (SQLite recorded an attempt beyond what it
	// backfilled), so its copied pages count for nothing.
	copy          *walCopyAttempt
	passDiscarded bool
	// pressure: the attempt ran through a busy lane over the pressure mark.
	pressure bool
}

// stampSuffix renders the attempt's stamp for the log line.
func (r walReclaimResult) stampSuffix() string {
	if r.started.IsZero() {
		return ""
	}
	s := fmt.Sprintf(" started=%s elapsed=%s process_cpu=%s lane_busy_start=%t lane_busy_end=%t",
		r.started.UTC().Format("15:04:05.000"), r.elapsed.Round(time.Millisecond), r.processCPU.Round(time.Millisecond),
		r.laneAtStart, r.laneAtEnd)
	if r.framesKnown {
		s += fmt.Sprintf(" wal_frames=%d backfilled_before=%d backfilled_after=%d backfill_attempted_after=%d",
			r.framesBefore.MxFrame, r.framesBefore.NBackfill, r.framesAfter.NBackfill, r.framesAfter.NBackfillAttempted)
	}
	if r.passDiscarded {
		s += " pass_discarded=true"
	}
	if r.slowTail != nil {
		s += fmt.Sprintf(" slow_copy_tail=true slow_copy=%s", r.slowTail.copyElapsed)
	}
	if r.adaptiveUsed {
		s += fmt.Sprintf(" adaptive_budget=%s adaptive_hold=%s aggregate_writer_hold=%s", r.adaptiveBudget, r.adaptiveHold, r.writerSpent)
	}
	if r.pressure {
		s += " pressure=true"
	}
	return s + r.copy.suffix()
}

// reclaimWALOnce runs one bounded reclaim attempt and records its counters.
// It never blocks readers longer than cfg.drainDeadline + cfg.truncateBudget
// and never runs while the generation bulk checkpoint lease is held.
func (s *Store) reclaimWALOnce(cfg walReclaimConfig, ckptDB *sql.DB, walPath string) walReclaimResult {
	before, beforeOK := readWALIndexSnapshot(s.dbPath)
	started, cpu0, laneAtStart := time.Now(), storeProcessCPUTime(), s.buildLaneBusy()
	res := s.reclaimWALAttempt(cfg, ckptDB, walPath)
	if res.pressure && res.outcome == walReclaimReset {
		s.walCopy.pressureResets.Add(1)
	}
	res.started, res.laneAtStart = started, laneAtStart
	res.elapsed, res.processCPU, res.laneAtEnd = time.Since(started), storeProcessCPUTime()-cpu0, s.buildLaneBusy()
	if after, ok := readWALIndexSnapshot(s.dbPath); ok && beforeOK {
		res.progressed = after.NBackfill > before.NBackfill || after.MxFrame < before.MxFrame
		res.framesBefore, res.framesAfter, res.framesKnown = before, after, true
		// Attempted beyond backfilled, and beyond where this attempt began:
		// a pass of this attempt stopped before it could record its copy.
		if after.NBackfillAttempted > after.NBackfill && after.NBackfillAttempted > before.NBackfill && after.MxFrame >= before.MxFrame {
			res.passDiscarded = true
			s.walCopy.discarded.Add(1)
		}
	}
	if res.outcome == walReclaimSkipped && res.bytesBefore == 0 {
		// A refusal returns before measuring the log; the skip line reports it.
		res.bytesBefore = walFileSize(walPath)
	}
	if res.outcome == walReclaimReset {
		res.progressed = true
	}
	s.walReclaim.update(func(st *WALReclaimStats) {
		st.ThresholdBytes = cfg.thresholdBytes
		st.LastOutcome = res.outcome.String()
		st.LastReason = res.reason
		if res.adaptiveUsed {
			st.AdaptiveWriterAttempts++
			st.AdaptiveWriterBudgetMax = max(st.AdaptiveWriterBudgetMax, res.adaptiveBudget)
			st.AdaptiveWriterHoldMax = max(st.AdaptiveWriterHoldMax, res.adaptiveHold)
		}
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
	// Ordinary below-pressure attempts wait for an idle lane and yield when
	// an edit starts. A positively above-pressure WAL/request qualifies the
	// bounded pressure class even when it starts in an idle gap; later edits
	// must not withdraw the completion needed to keep the log bounded.
	// Hard-cap qualification still requires a busy lane and its spacing gate;
	// bulk lease overrides retain their distinct yielding policy below hard cap.
	policy := checkpointYieldsToCycle
	laneBusy := s.cycleYieldEnabled() && s.buildLaneBusy()
	hardCap, pressure := false, false
	if cfg.ceilingBytes > 0 {
		size := walFileSize(walPath)
		if laneBusy && size >= walReclaimHardCapFactor*cfg.ceilingBytes && s.hardCapDue(time.Now()) {
			policy, hardCap = checkpointIgnoresCycle, true
			s.walReclaim.cycle.ceiling.Add(1)
			s.walReclaim.cycle.lastHardCap.Store(time.Now().UnixNano())
			log.Printf("store_sqlite: wal reclaim running despite the build lane reason=wal_hard_cap wal_bytes=%d hard_cap=%d", size, walReclaimHardCapFactor*cfg.ceilingBytes)
		} else if mark := walPressureMark(cfg); mark > 0 && !walPressureOff && (size >= mark || s.walReclaimRequested(time.Now(), size, cfg)) {
			// Pressure is a property of this admitted WAL/request, not the
			// lane's instantaneous state. An above-mark pass begun in a gap
			// must retain the same bounded completion policy when an edit starts.
			policy, pressure = checkpointIgnoresCycle, true
		}
	}
	attempt, berr := s.beginBackgroundCheckpointAttempt(policy)
	if errors.Is(berr, errWALCheckpointYieldedToCycle) {
		s.walReclaim.cycle.refusals.Add(1)
		return walReclaimResult{outcome: walReclaimSkipped, reason: "build_lane_busy"}
	}
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
		leasePolicy := checkpointOverridesLease // yields to edit cycles
		if hardCap {
			leasePolicy = checkpointOverridesLeaseAndCycle
		}
		attempt, berr = s.beginBackgroundCheckpointAttempt(leasePolicy)
		if errors.Is(berr, errWALCheckpointYieldedToCycle) {
			s.walReclaim.cycle.refusals.Add(1)
			return walReclaimResult{outcome: walReclaimSkipped, reason: "build_lane_busy"}
		}
		if berr != nil {
			return walReclaimResult{outcome: walReclaimSkipped, reason: "checkpoint_in_flight"}
		}
		leaseOverride = true
		// A bulk lease override retains its existing yielding policy unless
		// the independently qualified hard-cap class also overrides the lane.
		pressure = false
		s.walReclaim.cycle.leaseOverrides.Add(1)
		s.walReclaim.cycle.lastLeaseOverride.Store(time.Now().UnixNano())
		log.Printf("store_sqlite: wal reclaim running inside a bulk window reason=wal_ceiling wal_bytes=%d ceiling=%d", size, cfg.ceilingBytes)
	} else if berr != nil {
		return walReclaimResult{outcome: walReclaimSkipped, reason: "checkpoint_lease"}
	}
	defer s.finishBackgroundCheckpointAttempt(attempt)
	if pressure {
		s.walCopy.pressureRuns.Add(1)
	}
	// Passes of an attempt that yields to edits pause for them; one running
	// despite the lane (the hard cap) copies straight through.
	// Only an attempt that yields to edits pauses for them: one begun with no
	// lane to watch (none installed yet) has nobody to end its pause.
	attempt.copy.pausable.Store(!hardCap && s.cycleYieldEnabled())
	attempt.copy.pressure = pressure
	if cfg.ceilingBytes > 0 {
		attempt.copy.hardCapBytes, attempt.copy.walPath = walReclaimHardCapFactor*cfg.ceilingBytes, walPath
		// A pause ends once the log reaches the pressure mark.
		if mark := walPressureMark(cfg); mark > 0 && !walPressureOff {
			attempt.copy.hardCapBytes = min(attempt.copy.hardCapBytes, mark)
		}
	}
	if !hardCap && !pressure && !s.copyStartAllowed(time.Now()) {
		return walReclaimResult{outcome: walReclaimSkipped, reason: "copy_budget"}
	}

	res := walReclaimResult{bytesBefore: walFileSize(walPath), leaseOverride: leaseOverride, began: true, copy: attempt.copy, pressure: pressure, hardCap: hardCap, bulkCompletion: leaseOverride}
	if mark := walReclaimLastResortBytes(cfg); mark > 0 && res.bytesBefore >= mark {
		res.lastResort = true
	}
	// Nothing in the log and nothing to shrink: never touch the writer.
	if snap, ok := readWALIndexSnapshot(s.dbPath); ok && snap.MxFrame == 0 && res.bytesBefore <= cfg.thresholdBytes {
		return walReclaimResult{outcome: walReclaimSkipped, reason: "nothing_to_reclaim", bytesBefore: res.bytesBefore}
	}
	// Step 1: the unbounded backfill (see the file comment), without the
	// writer. Its error is not fatal — the lane copies whatever remains —
	// unless the attempt was cancelled.
	_, _ = s.pacedPassive(attempt.ctx, ckptDB, attempt)
	// Step 2: without the writer and with the gate OPEN, wait for a log no
	// reader pins: backfill, then wait out every reader admitted before that
	// backfill. A reader admitted after a COMPLETE backfill takes read mark 0
	// and never pins the log; one admitted while it was incomplete (writes
	// landed, or an older reader still needed frames) may, so the rounds
	// repeat — each waits only for readers admitted before its own backfill —
	// until a round finds the backfill complete and the reset goes through.
	// The reset is the only step that takes the writer, for at most
	// walReclaimResetHold. Nothing here holds the writer while it waits,
	// so the rounds run for urgent logs too, and through write bursts: the
	// reset lands in the first writer gap long enough for the readers
	// admitted before it to end (up to twice the longest read in flight).
	res.urgent = cfg.thresholdBytes > 0 && res.bytesBefore >= walReclaimUrgentFactor*cfg.thresholdBytes
	// An attempt that runs through a busy lane (the pressure mark, the hard
	// cap) goes straight to its converged copy and short hold below: it must
	// not spend the reader wait inside an edit's burst.
	if !pressure && !hardCap && !res.lastResort && !res.bulkCompletion && !walReclaimSkipQuiescence && !walReclaimSkipOpenGate && s.readGate != nil && cfg.readerWait > 0 {
		started := time.Now()
		deadline := started.Add(cfg.readerWait)
		// The rounds end when the log reaches the last resort's mark: the
		// attempt goes on to it at once (reclaimWALInLane).
		wctx, stopWatch := s.watchWALMark(attempt.ctx, walPath, walReclaimLastResortBytes(cfg))
		defer stopWatch()
		rounds := 0
		for {
			rounds++
			// A paced pass runs on the attempt's own context: the urgency
			// watch and the round deadline end the rounds between passes,
			// never a pass in the middle (that would discard its copy).
			passCtx := wctx
			if attempt.copy != nil && attempt.copy.pausable.Load() {
				passCtx = attempt.ctx
			}
			pctx, pcancel := context.WithDeadline(passCtx, deadline)
			if passCtx == attempt.ctx {
				pctx, pcancel = context.WithCancel(passCtx)
			}
			result, perr := s.pacedPassive(pctx, ckptDB, attempt)
			pcancel()
			if s.walAttemptHandsOverToRequest(&res) {
				// A request is pending and this attempt may not reset inside
				// the busy lane: hand over to the loop (below).
				res.openGateWaited = time.Since(started)
				break
			}
			if wctx.Err() != nil && attempt.ctx.Err() == nil && !errors.Is(perr, errWALCheckpointYieldedToCycle) {
				res.lastResort = true // the log reached the last resort's mark
			}
			if wctx.Err() != nil || errors.Is(perr, errWALCheckpointYieldedToCycle) {
				// Cancelled (an edit, shutdown, an urgent log) or an edit began:
				// without this the rounds would spin until the deadline, since
				// waiting for older readers returns at once when there are none.
				res.openGateWaited = time.Since(started)
				break
			}
			complete := perr == nil && !result.incomplete()
			if complete {
				res.converged, res.convergedFrames = true, result.WALFrames
			}
			// The reset: one short hold of the writer (reclaimWALResetHold, or
			// the pressure reset inside a busy lane), taken only when what is
			// left to copy fits in it, and handed back at once when a reader
			// still pins the log.
			// Never inside an edit: only an attempt that runs through a busy
			// lane (pressure, hard cap) may hold the writer during one.
			yieldsToLane := !res.pressure && !res.hardCap
			if s.walRemainderFits(&res) && (res.urgent || !s.writeWanted()) && (!yieldsToLane || !s.buildLaneBusy()) {
				outcome, reason := res.outcome, res.reason
				var done bool
				var herr error
				done, herr = s.reclaimWALResetHold(wctx, cfg, ckptDB, &res, false, yieldsToLane)
				if done {
					res.resetWriterFree = true
					break
				}
				res.outcome, res.reason = outcome, reason
				if errors.Is(herr, errWALCheckpointDeferredBulk) {
					break
				}
			} else if observer := walReclaimRoundObserver; observer != nil {
				observer("primary_reset_not_admitted", 0, 0, nil)
			}
			waitStart := time.Now()
			epoch, older, werr := s.waitWALRoundReaders(wctx, &res, deadline)
			res.openGateWaited = time.Since(started)
			if werr != nil {
				res.blocker, res.hasBlocker = s.readGate.oldestOlderThan(epoch, time.Now())
				reason := "older_readers_outlasted_wait"
				switch {
				case attempt.ctx.Err() != nil:
					reason = "cancelled"
				case wctx.Err() != nil:
					reason = "last_resort_mark"
					res.lastResort = true
				}
				res.openGateReport = fmt.Sprintf("reason=%s older_readers=%d waited=%s rounds=%d backfilled=%d/%d",
					reason, older, res.openGateWaited.Round(time.Millisecond), rounds, result.CheckpointedFrames, result.WALFrames)
				break
			}
			if !time.Now().Before(deadline) {
				res.openGateReport = fmt.Sprintf("reason=no_reader_free_gap waited=%s rounds=%d backfilled=%d/%d",
					res.openGateWaited.Round(time.Millisecond), rounds, result.CheckpointedFrames, result.WALFrames)
				break
			}
			if time.Since(waitStart) < time.Millisecond {
				// Nobody to wait for, yet no reset (a write wanted the
				// writer, or writes keep the backfill incomplete): pace the
				// rounds instead of spinning on the backfill.
				select {
				case <-wctx.Done():
				case <-time.After(walReclaimRoundPause):
				}
			}
		}
	}
	if !res.resetWriterFree && s.walAttemptHandsOverToRequest(&res) {
		res.outcome, res.reason = walReclaimSkipped, "reclaim_request"
		res.bytesAfter = walFileSize(walPath)
		return res
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
	// An edit began meanwhile: the copy so far is kept (every pass that
	// completed moved nBackfill); the writer is not taken during an edit.
	if attempt.copy != nil && attempt.copy.pausable.Load() && !attempt.copy.pressure && s.buildLaneBusy() {
		res.outcome, res.reason = walReclaimSkipped, "build_lane_busy"
		res.bytesAfter = walFileSize(walPath)
		return res
	}
	// Before the writer: converge the backfill so the capped writer step
	// only has to copy what fits its slice (wal_reclaim_converge.go).
	if attempt.ctx.Err() == nil {
		smallFrames := convergeSmallFor(attempt)
		if res.bulkCompletion {
			// A near4MiB unmeasured tail can spend the entire existing2s
			// hold on slow storage. Converge it writer-free before completing
			// the admitted over-ceiling bulk override under the same cap.
			smallFrames = min(smallFrames, walReclaimPressureSmallFrames)
		}
		conv := s.convergeBackfillPacedWithSmallRemainder(attempt.ctx, ckptDB, attempt, smallFrames)
		res.convergence = &conv
	}
	if attempt.copy != nil && attempt.copy.pausable.Load() && !attempt.copy.pressure && s.buildLaneBusy() {
		res.outcome, res.reason = walReclaimSkipped, "build_lane_busy"
		res.bytesAfter = walFileSize(walPath)
		return res
	}
	if s.walAttemptHandsOverToRequest(&res) {
		res.outcome, res.reason = walReclaimSkipped, "reclaim_request"
		res.bytesAfter = walFileSize(walPath)
		return res
	}
	if pressure && s.buildLaneBusy() {
		// Inside the busy lane: the reset alone, under its own short cap.
		err := s.reclaimWALPressureReset(attempt.ctx, ckptDB, &res)
		res.bytesAfter = walFileSize(walPath)
		if err == nil {
			res.outcome = walReclaimReset
			return res
		}
		if res.outcome == 0 || res.outcome == walReclaimReset {
			res.outcome = walReclaimDeferred
		}
		if res.reason == "" {
			res.reason = err.Error()
		}
		return res
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
	if cycleCancelled(attempt) {
		res.outcome, res.reason = walReclaimSkipped, "build_lane_busy"
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

// walAttemptHandsOverToRequest reports an attempt that should end so the loop
// can serve a reclaim request: one that may not reset inside a busy lane
// (neither pressure nor hard cap), while an edit holds the lane and a writer
// refused on the log's size has asked for the reclaim. Without it such an
// attempt goes on waiting for a gap between edits (its writer-free rounds
// until the reader wait runs out) while the loop, inside it, cannot run the
// pressure attempt the request asked for.
func (s *Store) walAttemptHandsOverToRequest(res *walReclaimResult) bool {
	return !res.pressure && !res.hardCap && s.buildLaneBusy() && s.walReclaimRequestPending(time.Now())
}

func (s *Store) reclaimWALInLane(ctx context.Context, cfg walReclaimConfig, ckptDB *sql.DB, res *walReclaimResult) error {
	urgent := res.urgent
	// A mutation already waiting for the writer wins outright (unless the log
	// is urgent; the hold stays capped).
	if !urgent && s.writeWanted() {
		res.outcome, res.reason = walReclaimSkipped, "writer_waiting"
		return errWALReclaimWriterWaiting
	}
	if !res.lastResort && !res.bulkCompletion && !walReclaimSkipQuiescence && s.readGate != nil && !walReclaimSkipOpenGate && cfg.readerWait > 0 {
		return s.reclaimWALInLaneOpenGate(ctx, cfg, ckptDB, res)
	}
	if res.bulkCompletion && !res.lastResort {
		defer func() {
			log.Printf("store_sqlite: wal reclaim bulk completion ceiling=%d writer_hold=%s reset=%t reason=%q",
				cfg.ceilingBytes, res.writerHold.Round(time.Millisecond), res.openGate, res.reason)
		}()
	}
	if res.lastResort {
		at := walFileSize(s.dbPath + "-wal")
		defer func() {
			log.Printf("store_sqlite: wal reclaim last resort wal_bytes=%d mark=%d writer_hold=%s reset=%t reason=%q",
				at, walReclaimLastResortBytes(cfg), res.writerHold.Round(time.Millisecond), res.openGate, res.reason)
		}()
		s.walReclaim.update(func(st *WALReclaimStats) { st.LastResortRuns++ })
	}
	// An adaptive slice already used this attempt's completion opportunity.
	// A subsequent last-resort/closed-gate phase must not mint another full
	// two-second allowance; let a new attempt own that separate budget.
	if res.adaptiveUsed {
		res.reason = "adaptive_completion_deferred"
		return errWALReclaimReadersInFlight
	}
	allowance := walReclaimMaxWriterHold
	if res.lastResortPromotion != nil {
		allowance -= res.writerSpent
		if allowance <= 0 {
			res.reason = "last_resort_credit_spent"
			return errWALReclaimReadersInFlight
		}
	}
	// Below: the last resort or over-ceiling bulk completion hold, or the closed-gate path (the
	// open-gate stages disabled).
	// 3. Writer quiescence, capped at walReclaimMaxWriterHold from here on.
	wctx, wcancel := context.WithTimeout(ctx, walReclaimWriterWait)
	err := s.writeMu.LockContext(wctx)
	wcancel()
	if err != nil {
		res.reason = "writer_gate"
		return fmt.Errorf("%w: %w", ErrMaintenanceBusy, err)
	}
	held := time.Now()
	writer := newWALReclaimWriterCredit(s, held)
	writer.yieldToWriters = !res.urgent
	defer func() {
		writer.release()
		res.writerHold = max(res.writerHold, writer.longest)
		res.writerSpent += writer.spent
	}()
	if s.bulkConn != nil && !res.leaseOverride {
		res.outcome, res.reason = walReclaimSkipped, "bulk_writer"
		return errWALCheckpointDeferredBulk
	}
	if expected := res.lastResortPromotion; expected != nil {
		current, ok := readWALReclaimFrontier(s.dbPath)
		if !ok || current.salt != expected.salt || current.mx < expected.mx || current.backfill < expected.backfill || walFileSize(s.dbPath+"-wal") < walReclaimLastResortBytes(cfg) {
			res.reason = "last_resort_frontier_changed"
			return errWALReclaimReadersInFlight
		}
		if !res.pressure && !res.hardCap && s.cycleYieldEnabled() && s.buildLaneBusy() {
			res.outcome, res.reason = walReclaimSkipped, "build_lane_busy"
			return errWALCheckpointYieldedToCycle
		}
	}
	hctx, hcancel := context.WithDeadline(ctx, held.Add(allowance))
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
		result, err := writer.passive(yctx, ctx, ckptDB, allowance, res.leaseOverride)
		yctx = writer.resetContext(yctx)
		if err != nil && errors.Is(err, errSQLiteCheckpointIncomplete) && yctx.Err() == nil {
			// Incomplete is a result, not a failure: a reader still needs
			// frames (result carries the counts).
			return result, nil
		}
		return result, err
	}
	delta, derr := backfill()
	if errors.Is(derr, errWALCheckpointDeferredBulk) {
		res.outcome, res.reason = walReclaimSkipped, "bulk_writer"
		return derr
	}
	if !writer.held {
		return giveUp(fmt.Sprintf("backfill_failed error=%v", derr), derr)
	}
	res.frames = delta.WALFrames
	if derr != nil {
		return giveUp(fmt.Sprintf("backfill_failed error=%v", derr), errWALReclaimReadersInFlight)
	}
	if delta.WALFrames == 0 && res.bytesBefore <= cfg.thresholdBytes {
		res.outcome, res.reason = walReclaimSkipped, "nothing_to_reclaim"
		return errWALReclaimNothing
	}

	// 5. The last resort or admitted over-ceiling bulk completion: with writes
	// stopped, finish converging inside the hold cap: if step
	// 2 converged and nothing was committed since, no reader in flight can pin
	// the log and the reset needs no wait; otherwise backfill, wait out the
	// readers admitted before that backfill (gate open), and repeat until a
	// round starts from a complete backfill.
	quiesce := !walReclaimSkipQuiescence && s.readGate != nil
	openGate := quiesce && !walReclaimSkipOpenGate && cfg.readerWait > 0
	if openGate {
		// Only the last resort or bulk completion reaches here with the open-gate stages on
		// (see reclaimWALInLane): the writer stays held, up to
		// walReclaimMaxWriterHold, while the readers admitted before the
		// backfill end.
		capAt := held.Add(allowance)
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
			_, terr := s.resetWALAfterOlderReaderProgress(tctx, capAt)
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
			if delta, derr = backfill(); derr != nil || !writer.held {
				return giveUp(fmt.Sprintf("backfill_failed error=%v", derr), derr)
			}
		}
		tctx, tcancel := context.WithDeadline(yctx, capAt)
		result, terr := s.resetWALAfterOlderReaderProgress(tctx, capAt)
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
	if room := time.Until(held.Add(allowance)); room < cfg.truncateBudget {
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
	result, err := s.resetWALForReclaim(tctx)
	tcancel()
	reopen()
	res.pause, res.pauseClosed = time.Since(start), quiesce
	if err != nil {
		return giveUp(fmt.Sprintf("truncate busy=%d wal_frames=%d checkpointed=%d error=%v", result.Busy, result.WALFrames, result.CheckpointedFrames, err), err)
	}
	return nil
}

// resetWALAfterOlderReaderProgress is used only by the held, complete-backfill
// path. An epoch counts database-only readers as well as actual WAL pins, so
// ask SQLite again when an older connection retires instead of waiting for the
// whole cohort. Capture the cohort before the first reset: a pin ending during
// that call must not be missed. Every retry keeps the original hold deadline.
func (s *Store) resetWALAfterOlderReaderProgress(ctx context.Context, deadline time.Time) (walCheckpointResult, error) {
	epoch := s.readGate.advance()
	older := s.readGate.olderCount(epoch)
	for {
		result, err := s.resetWALForReclaim(ctx)
		if err == nil || !errors.Is(err, errSQLiteCheckpointIncomplete) || result.Busy == 0 || result.WALFrames <= 0 || result.WALFrames != result.CheckpointedFrames || older == 0 {
			return result, err
		}
		var waitErr error
		older, waitErr = s.readGate.waitOlderDecrease(ctx, epoch, older, deadline)
		if waitErr != nil {
			return result, waitErr
		}
	}
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
	s.walCopy.sawBusy(time.Now())
	if s.writeIntents.Add(1) == 1 {
		s.intentsSince.Store(time.Now().UnixNano())
		s.intentLeakLogged.Store(false)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if s.writeIntents.Add(-1) == 0 {
				s.intentsSince.Store(0)
			}
		})
	}
}

// editIntentStandDownMax bounds how long announced mutations stand background
// work down: a release that never comes must not stop the WAL reclaim for
// good. A var only so the in-package cases can shorten it.
var editIntentStandDownMax = 2 * time.Minute

// editIntentActive reports an announced mutation between its admission and
// its release (after the route flip), unless the announcements have been held
// continuously past editIntentStandDownMax.
func (s *Store) editIntentActive() bool {
	if s.writeIntents.Load() <= 0 {
		return false
	}
	since := s.intentsSince.Load()
	if since == 0 {
		return true
	}
	if age := time.Since(time.Unix(0, since)); age > editIntentStandDownMax {
		if s.intentLeakLogged.CompareAndSwap(false, true) {
			log.Printf("store_sqlite: WARN write announcements held for %s (count=%d); background store work resumes despite them",
				age.Round(time.Second), s.writeIntents.Load())
		}
		return false
	}
	return true
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
	wake := make(chan struct{}, 1)
	s.walReclaimWake.Store(&wake)
	s.walReclaim.cycle.loopThreshold.Store(cfg.thresholdBytes)
	go func() {
		defer s.walReclaim.cycle.loopThreshold.Store(0)
		defer s.walReclaimWake.Store(nil)
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
	var wake chan struct{}
	if w := s.walReclaimWake.Load(); w != nil {
		wake = *w
	}
	for {
		select {
		case <-s.stopCheckpoint:
			return
		case <-ticker.C:
		case <-wake: // a request (RequestWALReclaim): attempt now
		}
		now := time.Now()
		if s.walReclaimNudged.Swap(false) {
			// A residue drain handed its busy TRUNCATE over: try now.
			schedule.nextAt = now
		}
		if s.walShrinkNeeded() {
			// A log reset in place is shrunk in slices before anything
			// else: its file is what the threshold measures.
			if ckptDB == nil {
				if db, err := openWALReclaimCheckpointDB(s.dbPath); err == nil {
					ckptDB = db
				}
			}
			if ckptDB != nil {
				s.shrinkWALUntilStopped(ckptDB)
			}
			now = time.Now()
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
		if res.outcome == walReclaimReset && s.walShrinkNeeded() {
			s.shrinkWALUntilStopped(ckptDB)
		}
	}
}

// shrinkWALUntilStopped runs the incremental shrink on the reclaim loop's
// goroutine, cancelled by shutdown.
func (s *Store) shrinkWALUntilStopped(ckptDB *sql.DB) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-s.stopCheckpoint:
			cancel()
		case <-finished:
		}
	}()
	_, _, _ = s.shrinkWAL(ctx, ckptDB)
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
			stage, res.bytesBefore, res.bytesAfter, res.frames, res.pause, res.writerHold.Round(time.Millisecond), res.convergenceSuffix()+res.stampSuffix())
	case walReclaimDeferred, walReclaimFailed:
		if res.hasBlocker && res.blocker.Age >= walReclaimBlockedReaderWarnAge {
			log.Printf("store_sqlite: WARN wal reclaim blocked by a long reader: age=%s admitted_epoch=%d statement=%q (a read transaction that old pins the WAL snapshot; the log cannot reset until it ends)",
				res.blocker.Age.Round(time.Second), res.blocker.Epoch, res.blocker.Label)
		}
		if res.openGateReport != "" {
			log.Printf("store_sqlite: wal reclaim open_gate gave up %s writer_hold=%s", res.openGateReport, res.writerHold.Round(time.Millisecond))
		}
		log.Printf("store_sqlite: wal reclaim %s reason=%q wal_bytes=%d reader_pause=%s writer_hold=%s progressed=%t next_attempt_in=%s%s",
			res.outcome, res.reason, res.bytesBefore, res.pause, res.writerHold.Round(time.Millisecond), res.progressed, next, res.convergenceSuffix()+res.stampSuffix())
	case walReclaimSkipped:
		if res.began {
			// It ran and was stopped (an edit cycle took the lane, a writer
			// queued, shutdown): its time is real, so it gets its own line.
			log.Printf("store_sqlite: wal reclaim abandoned reason=%q wal_bytes=%d%s", res.reason, res.bytesBefore, res.convergenceSuffix()+res.stampSuffix())
			return
		}
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

// walLogIsReset reports whether the log is at its start: an empty file, or a
// wal-index with no frames. The caller holds the write gate.
func walLogIsReset(dbPath, walPath string) bool {
	if walFileSize(walPath) == 0 {
		return true
	}
	snap, ok := readWALIndexSnapshot(dbPath)
	return ok && snap.MxFrame == 0
}

// The in-lane reset's open-gate stage. The wait for older readers runs
// without the writer, and edits go on meanwhile; the writer is taken only for
// the last backfill and the reset, at most walReclaimResetHold each time (the
// idle and pressure resets' cap), and a hold that finds the log not ready
// (a reader still pins it, or more is left than the hold can copy) hands the
// writer back at once. Rounds repeat until the reset or
// walReclaimLaneReaderWait: copy without the writer, wait out the readers
// admitted before that copy, then one short hold.
var (
	// walReclaimRoundPause paces the writer-free rounds when there is no
	// reader to wait for.
	walReclaimRoundPause     = 20 * time.Millisecond
	walReclaimLaneReaderWait = 5 * time.Second
)

func (s *Store) reclaimWALInLaneOpenGate(ctx context.Context, cfg walReclaimConfig, ckptDB *sql.DB, res *walReclaimResult) error {
	deadline := time.Now().Add(walReclaimLaneReaderWait)
	entry, entryOK := readWALReclaimFrontier(s.dbPath)
	mark := walReclaimLastResortBytes(cfg)
	waitCtx, stopMarkWatch := s.watchWALMark(ctx, s.dbPath+"-wal", mark)
	defer stopMarkWatch()
	for round := 1; ; round++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mark > 0 && walFileSize(s.dbPath+"-wal") >= mark {
			current, ok := readWALReclaimFrontier(s.dbPath)
			if !entryOK || !ok || current.salt != entry.salt || current.mx < entry.mx || current.backfill < entry.backfill {
				res.reason = "last_resort_frontier_changed"
				return errWALReclaimReadersInFlight
			}
			if !res.pressure && !res.hardCap && s.cycleYieldEnabled() && s.buildLaneBusy() {
				res.outcome, res.reason = walReclaimSkipped, "build_lane_busy"
				return errWALCheckpointYieldedToCycle
			}
			res.lastResort, res.lastResortPromotion = true, &current
			if cfg.thresholdBytes > 0 && walFileSize(s.dbPath+"-wal") >= walReclaimUrgentFactor*cfg.thresholdBytes {
				res.urgent = true
			}
			// Keep this operation's deadline and charge every preceding short
			// hold to the existing completion allowance. SQLite still decides reset.
			return s.reclaimWALInLane(ctx, cfg, ckptDB, res)
		}
		done, err := s.reclaimWALResetHold(ctx, cfg, ckptDB, res, round == 1, !res.pressure && !res.hardCap)
		if done || err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			res.reason = fmt.Sprintf("older_readers_in_flight rounds=%d writer_holds=%d", round, res.writerHolds)
			return fmt.Errorf("%w: %s", errWALReclaimReadersInFlight, res.reason)
		}
		// A yielding hold may have declined on a newly busy lane before the
		// watcher polls. Do not start another real checkpoint inside that cycle.
		if err := ctx.Err(); err != nil {
			return err
		}
		if !res.pressure && !res.hardCap && s.cycleYieldEnabled() && s.buildLaneBusy() {
			res.outcome, res.reason = walReclaimSkipped, "build_lane_busy"
			return errWALCheckpointYieldedToCycle
		}
		if res.resetReaders != nil {
			epoch, older, werr := s.waitWALRoundReaders(waitCtx, res, deadline)
			if werr != nil && ctx.Err() == nil && waitCtx.Err() != nil && mark > 0 && walFileSize(s.dbPath+"-wal") >= mark {
				continue // recheck current identity/mark at the round boundary
			}
			if werr != nil {
				res.blocker, res.hasBlocker = s.readGate.oldestOlderThan(epoch, time.Now())
				res.reason = fmt.Sprintf("older_readers_in_flight older_readers=%d rounds=%d writer_holds=%d", older, round, res.writerHolds)
				return fmt.Errorf("%w: %s", errWALReclaimReadersInFlight, res.reason)
			}
			continue
		}
		// Without the writer: copy what is left, then wait out the readers
		// admitted before that copy (a reader admitted after a complete
		// copy takes read mark 0 and does not block the reset).
		result, perr := checkpointWALOnceOn(ctx, ckptDB, "PASSIVE")
		if perr != nil && ctx.Err() != nil {
			res.reason = "cancelled"
			return perr
		}
		epoch := s.readGate.advance()
		observer := walReclaimRoundObserver
		if observer != nil {
			observer("lane_post_copy_full_wait_start", epoch, s.readGate.olderCount(epoch), nil)
		}
		older, werr := s.readGate.waitOlder(waitCtx, epoch, deadline)
		if observer != nil {
			observer("lane_post_copy_full_wait_end", epoch, older, werr)
		}
		if werr != nil && ctx.Err() == nil && waitCtx.Err() != nil && mark > 0 && walFileSize(s.dbPath+"-wal") >= mark {
			continue // a growing WAL now qualifies the existing completion class
		}
		if werr != nil {
			res.blocker, res.hasBlocker = s.readGate.oldestOlderThan(epoch, time.Now())
			res.reason = fmt.Sprintf("older_readers_in_flight older_readers=%d rounds=%d writer_holds=%d backfilled=%d/%d",
				older, round, res.writerHolds, result.CheckpointedFrames, result.WALFrames)
			return fmt.Errorf("%w: %s", errWALReclaimReadersInFlight, res.reason)
		}
	}
}

// walReclaimRoundObserver records reclaim branch and cohort decisions for tests.
// Synchronous and nil in production. Counts identify the captured connection
// cohort, never SQLite WAL lock owners; install/restore only while quiescent.
var walReclaimRoundObserver func(stage string, epoch uint64, older int, err error)

// waitWALRoundReaders keeps the full-drain rule for incomplete copies. A
// positively complete reset that was refused can instead retry when a member
// of its captured cohort retires; newer readers do not cause retries. Capture
// happened before reset SQL, so retirement during SQL cannot be missed.
func (s *Store) waitWALRoundReaders(ctx context.Context, res *walReclaimResult, deadline time.Time) (uint64, int, error) {
	observer := walReclaimRoundObserver
	if cohort := res.resetReaders; cohort != nil {
		res.resetReaders = nil
		if observer != nil {
			observer("partial_wait_start", cohort.epoch, cohort.older, nil)
		}
		older, err := s.readGate.waitOlderDecrease(ctx, cohort.epoch, cohort.older, deadline)
		if observer != nil {
			observer("partial_wait_end", cohort.epoch, older, err)
		}
		return cohort.epoch, older, err
	}
	epoch := s.readGate.advance()
	if observer != nil {
		observer("full_wait_start", epoch, s.readGate.olderCount(epoch), nil)
	}
	older, err := s.readGate.waitOlder(ctx, epoch, deadline)
	if observer != nil {
		observer("full_wait_end", epoch, older, err)
	}
	return epoch, older, err
}

// walRemainderFits reports whether what is left to copy fits in one short
// hold: half the hold at the rate the convergence measured, never less than
// walReclaimPressureSmallFrames.
func (s *Store) walRemainderFits(res *walReclaimResult) bool {
	snap, ok := readWALIndexSnapshot(s.dbPath)
	if !ok {
		return false
	}
	return snap.MxFrame <= snap.NBackfill || snap.MxFrame-snap.NBackfill <= walHoldAllowedFrames(res)
}

func walHoldAllowedFrames(res *walReclaimResult) uint32 {
	return walHoldAllowedFramesFor(res, walReclaimResetHold)
}

func walHoldAllowedFramesFor(res *walReclaimResult, budget time.Duration) uint32 {
	allowed := walReclaimPressureSmallFrames
	if c := res.convergence; c != nil && c.rateFramesPerS > 0 {
		allowed = max(allowed, uint32(c.rateFramesPerS*(budget/2).Seconds()))
	}
	if rate := walHoldCopyRate.Load(); rate > 0 {
		allowed = max(allowed, uint32(min(float64(rate)*(budget/2).Seconds(), 1<<24)))
	}
	return allowed
}

// walHoldCopyRate is the frames per second a hold's copy (unpaced, the
// writer held) has achieved on this host, a moving average; 0 until a hold
// has copied enough to measure. A hold takes the remainder that half of it
// can copy at this rate, so a writer-free copy behind the writes can still
// finish inside one hold.
var walHoldCopyRate atomic.Int64

func noteWALHoldCopy(frames int64, took time.Duration) {
	if frames < 64 || took <= 0 {
		return
	}
	rate := int64(float64(frames) / took.Seconds())
	for {
		prev := walHoldCopyRate.Load()
		next := rate
		if prev > 0 {
			next = (3*prev + rate) / 4
		}
		if walHoldCopyRate.CompareAndSwap(prev, next) {
			return
		}
	}
}

// reclaimWALResetHold is one hold of the writer, at most walReclaimResetHold:
// the backfill of what is left and, when that completes, the reset. done
// reports the reset; (false, nil) hands the writer back for another round.
func (s *Store) reclaimWALResetHold(ctx context.Context, cfg walReclaimConfig, ckptDB *sql.DB, res *walReclaimResult, first, yieldsToLane bool) (done bool, err error) {
	budget := walReclaimResetHold
	if res.adaptiveUsed {
		budget = min(budget, walReclaimMaxWriterHold-res.writerSpent)
		if budget <= 0 {
			return false, errWALReclaimReadersInFlight
		}
	}
	done, err = s.reclaimWALResetHoldOnce(ctx, cfg, ckptDB, res, first, yieldsToLane, budget)
	if done || err != nil || ctx.Err() != nil {
		return done, err
	}
	if budget := res.takeAdaptiveWriterBudget(); budget > 0 {
		return s.reclaimWALResetHoldOnce(ctx, cfg, ckptDB, res, first, yieldsToLane, budget)
	}
	return done, err
}

func (s *Store) reclaimWALResetHoldOnce(ctx context.Context, cfg walReclaimConfig, ckptDB *sql.DB, res *walReclaimResult, first, yieldsToLane bool, budget time.Duration) (done bool, err error) {
	res.resetReaders = nil
	if !res.urgent && s.writeWanted() {
		res.outcome, res.reason = walReclaimSkipped, "writer_waiting"
		return false, errWALReclaimWriterWaiting
	}
	wctx, wcancel := context.WithTimeout(ctx, walReclaimWriterWait)
	err = s.writeMu.LockContext(wctx)
	wcancel()
	if err != nil {
		res.reason = "writer_gate"
		return false, fmt.Errorf("%w: %w", ErrMaintenanceBusy, err)
	}
	held := time.Now()
	writer := newWALReclaimWriterCredit(s, held)
	writer.yieldToWriters = !res.urgent
	adaptiveCopy := false
	defer func() {
		writer.release()
		res.recordWriterCredit(writer, adaptiveCopy)
	}()
	if hook := walIdleResetHook; hook != nil {
		hook()
	}
	if s.bulkConn != nil && !res.leaseOverride {
		res.outcome, res.reason = walReclaimSkipped, "bulk_writer"
		return false, errWALCheckpointDeferredBulk
	}
	if budget > walReclaimResetHold && !res.adaptiveFrontierCurrent(s) {
		return false, errWALReclaimReadersInFlight
	}
	if yieldsToLane && s.buildLaneBusy() {
		return false, nil // an edit began while the gate was taken: hand it back
	}
	hctx, hcancel := context.WithDeadline(ctx, held.Add(budget))
	defer hcancel()
	yctx, stopYield := hctx, func() {}
	if !res.urgent {
		yctx, stopYield = s.yieldToWriters(hctx)
	}
	defer stopYield()
	yielded := func() bool { return errors.Is(context.Cause(yctx), errWALReclaimWriterWaiting) }
	// Ordinary holds copy only a fitted small remainder. A positively witnessed
	// adaptive copy is admitted by its remaining bounded time credit instead.
	snap, ok := readWALIndexSnapshot(s.dbPath)
	if budget <= walReclaimResetHold && ok && snap.MxFrame > snap.NBackfill && snap.MxFrame-snap.NBackfill > walHoldAllowedFramesFor(res, budget) {
		return false, nil // copy more without the writer first
	}
	copyStart := time.Now()
	copyCredit := budget / 2
	if budget > walReclaimResetHold {
		copyCredit = budget - walReclaimResetHold
	}
	if budget > walReclaimResetHold {
		if yctx.Err() != nil {
			return false, yctx.Err()
		}
		if !res.beginAdaptiveWriterCopy(budget) {
			return false, errWALReclaimReadersInFlight
		}
		adaptiveCopy = true
	}
	delta, derr := writer.passive(yctx, ctx, ckptDB, copyCredit, res.leaseOverride)
	yctx = writer.resetContext(yctx)
	if ok && (derr == nil || errors.Is(derr, errSQLiteCheckpointIncomplete)) {
		noteWALHoldCopy(int64(delta.CheckpointedFrames)-int64(snap.NBackfill), time.Since(copyStart))
	}
	if errors.Is(derr, errWALCheckpointDeferredBulk) {
		res.outcome, res.reason = walReclaimSkipped, "bulk_writer"
		return false, derr
	}
	if derr != nil && !errors.Is(derr, errSQLiteCheckpointIncomplete) {
		if yielded() || errors.Is(derr, errWALReclaimWriterWaiting) {
			res.outcome, res.reason = walReclaimSkipped, "writer_waiting"
			return false, errWALReclaimWriterWaiting
		}
		return false, nil
	}
	if !writer.held {
		return false, derr
	}
	if first {
		res.frames = delta.WALFrames
		if delta.WALFrames == 0 && res.bytesBefore <= cfg.thresholdBytes {
			res.outcome, res.reason = walReclaimSkipped, "nothing_to_reclaim"
			return false, errWALReclaimNothing
		}
	}
	if delta.incomplete() {
		return false, nil
	}
	var cohort *walReclaimResetReaders
	if delta.WALFrames > 0 && s.readGate != nil {
		epoch := s.readGate.advance()
		if older := s.readGate.olderCount(epoch); older > 0 {
			cohort = &walReclaimResetReaders{epoch: epoch, older: older}
		}
	}
	if observer := walReclaimRoundObserver; observer != nil {
		epoch, older := uint64(0), 0
		if cohort != nil {
			epoch, older = cohort.epoch, cohort.older
		}
		observer("short_reset_before", epoch, older, nil)
	}
	_, terr := s.resetWALForReclaim(writer.resetContext(yctx))
	if observer := walReclaimRoundObserver; observer != nil {
		observer("short_reset_after", 0, 0, terr)
	}
	if hook := walIdleResetResultHook; hook != nil {
		terr = hook(terr)
	}
	if terr != nil && walLogIsReset(s.dbPath, s.dbPath+"-wal") {
		// The reset completed and the interrupt (a writer queued, or the
		// hold ran out) arrived after it: the log is reset, and counted so.
		// Checked before the gate is released, so no write can refill it.
		terr = nil
	}
	if terr == nil {
		res.openGate = true
		return true, nil
	}
	if yielded() {
		res.outcome, res.reason = walReclaimSkipped, "writer_waiting"
		return false, errWALReclaimWriterWaiting
	}
	if errors.Is(terr, errSQLiteCheckpointIncomplete) || errors.Is(terr, context.DeadlineExceeded) {
		res.resetReaders = cohort
		if observer := walReclaimRoundObserver; observer != nil {
			epoch, older := uint64(0), 0
			if cohort != nil {
				epoch, older = cohort.epoch, cohort.older
			}
			stage := "reset_refusal_cohort_retained"
			if cohort == nil {
				stage = "reset_refusal_no_cohort"
			}
			observer(stage, epoch, older, terr)
		}
	}
	return false, nil
}

// watchWALMark returns a context cancelled once the -wal file reaches
// markBytes (polled every 50 ms), so the writer-free rounds hand over to the
// last resort (walReclaimLastResortBytes) as soon as the log reaches it.
func (s *Store) watchWALMark(parent context.Context, walPath string, markBytes int64) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	if markBytes <= 0 {
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
				if walFileSize(walPath) >= markBytes {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}
