package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// The reclaim under a sustained burst of edits.
//
// The paused copy (wal_copy_pause.go) waits while an edit runs, and the
// reset's writer step waits for a gap between edits. A burst that keeps the
// lane busy most of the time leaves the copy too little time and the reset no
// gap: in one measured run the log grew from 0.7 to 6.3 GB over fifteen minutes
// of back-to-back edits and folds, and reset only when they stopped.
//
// Over the pressure mark (walPressureMark: four times the reclaim threshold,
// 1 GiB by default) with the lane busy, an attempt therefore runs anyway:
//
//   - its copy is not paused by edits but still paced by the copy budget
//     (512 MiB per minute while editing), so its cost to an edit is the
//     budget's;
//   - it converges the backfill until the remainder is under
//     walReclaimPressureSmallFrames, so the writer step has almost nothing
//     left to copy;
//   - the writer step then holds the write gate for at most
//     walReclaimPressureHold (not the general 2 s cap): the last backfill and
//     the reset, nothing else. If that does not finish in time it gives up
//     (counted as a pressure give-up) and the next attempt tries again.
//     A proven slow completed copy with a new tail permits one adaptive
//     completion slice for an urgent attempt; its aggregate writer time is
//     still capped by walReclaimMaxWriterHold.
//
// Below the mark nothing changes.

var (
	// walReclaimPressureHold is the ordinary writer step inside a busy lane;
	// proven urgent slow-copy tails may use one bounded adaptive slice.
	walReclaimPressureHold = 50 * time.Millisecond
	// walReclaimPressureWriterWait bounds how long that step queues for the
	// writer.
	walReclaimPressureWriterWait = 3 * time.Second
	// walReclaimResetHold caps every other hold of the writer the reclaim
	// takes (the last copy and the reset); the wait for readers never holds
	// it.
	walReclaimResetHold = walReclaimPressureHold
	// walIdleResetHook, when set by a test, runs once a reset hold has the
	// write gate. nil in production.
	walIdleResetHook func()
	// walIdleResetResultHook, when set by a test, replaces a reset hold's
	// error (an interrupt that arrives after the reset completed). nil in
	// production.
	walIdleResetResultHook func(error) error
	// walReclaimPressureSmallFrames: the remainder the writer step may copy.
	walReclaimPressureSmallFrames uint32 = 256
	// walPressureOff disables the pressure mode, for the mutation checks
	// that prove it carries the bound. Never set in production.
	walPressureOff = false
	// walPressureResetHook, when a test sets it, runs while the pressure
	// reset holds the writer (a slow reset). nil in production.
	walPressureResetHook func(ctx context.Context)
)

// A writer refused on the log's size asks for the reclaim.
//
// The chain fold stops stepping while the log is over its mark, the sweep
// waits over its mark, and the reclaim itself ran through a busy lane only
// over the pressure mark (1 GiB): between those marks nothing could bring the
// log down during a burst, and the fold made no step for minutes. So a writer
// refused on the log's size requests the reclaim (RequestWALReclaim), and for
// walReclaimRequestWindow after a request, a log over the reclaim threshold is
// reclaimed in a busy lane as it is over the pressure mark: the copy paced by
// the budget and the reset under walReclaimPressureHold.

// walReclaimRequestWindow is how long a request keeps the pressure mode open.
var walReclaimRequestWindow = 30 * time.Second

// RequestWALReclaim asks the reclaim to run through a busy lane: the caller
// was refused on the log's size (a fold step, a retirement slice or chunk).
func (s *Store) RequestWALReclaim() {
	if s.coreless() {
		return
	}
	s.walReclaimRequestedAt.Store(time.Now().UnixNano())
	s.walReclaimNudged.Store(true)
	// Wake the loop now rather than at its next poll (5 s).
	if wake := s.walReclaimWake.Load(); wake != nil {
		select {
		case *wake <- struct{}{}:
		default:
		}
	}
}

// walReclaimRequested reports a request inside its window with the log over
// the reclaim threshold.
func (s *Store) walReclaimRequested(now time.Time, size int64, cfg walReclaimConfig) bool {
	return s.walReclaimRequestPending(now) && cfg.thresholdBytes > 0 && size > cfg.thresholdBytes
}

// walReclaimRequestPending reports a request inside its window.
func (s *Store) walReclaimRequestPending(now time.Time) bool {
	if s.coreless() {
		return false
	}
	at := s.walReclaimRequestedAt.Load()
	return at != 0 && now.Sub(time.Unix(0, at)) <= walReclaimRequestWindow
}

// walPressureMark is the log size over which the reclaim runs through a busy
// lane: walPressureMarkFactor x the threshold, capped at the ceiling.
func walPressureMark(cfg walReclaimConfig) int64 {
	if cfg.thresholdBytes > 0 && cfg.ceilingBytes > 0 {
		return min(cfg.ceilingBytes, walPressureMarkFactor*cfg.thresholdBytes)
	}
	return cfg.ceilingBytes
}

// walPressureMarkFactor: the pressure mark is this multiple of the reclaim
// threshold (1 GiB at the default 256 MiB), or the ceiling if that is lower.
const walPressureMarkFactor int64 = 4

var errWALPressureHold = errors.New("store_sqlite: wal reclaim: the reset did not fit its hold inside a busy lane")

// walPressureHold is the writer step of a reset inside a busy lane:
// walReclaimPressureHold unless a test has raised it.
func (s *Store) walPressureHold() time.Duration {
	if s.walPressureHoldOverride > 0 {
		return s.walPressureHoldOverride
	}
	return walReclaimPressureHold
}

// reclaimWALPressureReset is the writer step of a pressure attempt: only when
// the backfill is nearly complete, then a short final copy/reset. A proven
// urgent slow-copy tail gets one longer slice under the aggregate two-second
// allowance, rather than repeatedly letting writes overtake the final sync.
func (s *Store) reclaimWALPressureReset(ctx context.Context, ckptDB *sql.DB, res *walReclaimResult) error {
	return s.reclaimWALPressureResetWithDrain(ctx, ckptDB, res, defaultWALReclaimDrainDeadline)
}

func (s *Store) reclaimWALPressureResetWithDrain(ctx context.Context, ckptDB *sql.DB, res *walReclaimResult, drainAllowance time.Duration) error {
	operationCtx, cancel := context.WithTimeout(ctx, walReclaimLaneBudget)
	defer cancel()
	err := s.reclaimWALPressureResetOnce(operationCtx, ckptDB, res, s.walPressureHold(), false)
	if err == nil || operationCtx.Err() != nil {
		return err
	}
	if !res.pressureResetBusy {
		if budget := res.takeAdaptiveWriterBudget(); budget > 0 {
			res.reason = ""
			err = s.reclaimWALPressureResetOnce(operationCtx, ckptDB, res, budget, false)
		}
	}
	if err != nil && operationCtx.Err() == nil && res.pressureResetBusy && s.readGate != nil && !walReclaimSkipQuiescence && drainAllowance > 0 && res.writerSpent < walReclaimMaxWriterHold {
		return s.reclaimWALPressureResetAfterReaderDrain(operationCtx, ckptDB, res, drainAllowance)
	}
	return err
}

func (s *Store) reclaimWALPressureResetOnce(ctx context.Context, ckptDB *sql.DB, res *walReclaimResult, budget time.Duration, preemptible bool) error {
	res.pressureResetBusy = false
	// What the hold may still copy: half the cap at the rate the convergence
	// measured, and never less than walReclaimPressureSmallFrames.
	allowed := walReclaimPressureSmallFrames
	if c := res.convergence; c != nil && c.rateFramesPerS > 0 {
		allowed = max(allowed, uint32(c.rateFramesPerS*(budget/2).Seconds()))
	}
	// The writer: an edit that holds it keeps it; this queues for it up to
	// walReclaimPressureWriterWait (one edit statement holds the gate for up
	// to about 2.4 s in a burst), then gives up. The hold itself stays
	// walReclaimPressureHold.
	wctx, wcancel := context.WithTimeout(ctx, walReclaimPressureWriterWait)
	err := s.writeMu.LockContext(wctx)
	wcancel()
	if err != nil {
		res.reason = "pressure_writer_busy"
		s.walCopy.pressureGiveUps.Add(1)
		return fmt.Errorf("%w: %w", errWALPressureHold, err)
	}
	held := time.Now()
	writer := newWALReclaimWriterCredit(s, held)
	adaptiveCopy := false
	defer func() {
		writer.release()
		res.recordWriterCredit(writer, adaptiveCopy)
	}()
	if preemptible {
		if s.writeWanted() {
			res.reason = "pressure_foreground_writer"
			return errWALReclaimWriterWaiting
		}
	}
	if s.bulkConn != nil && !res.leaseOverride {
		res.outcome, res.reason = walReclaimSkipped, "bulk_writer"
		return errWALCheckpointDeferredBulk
	}
	if budget > s.walPressureHold() && !res.adaptiveFrontierCurrent(s) {
		return errWALReclaimReadersInFlight
	}
	// With the writer held the remainder is final. Ordinary holds require a
	// fitted small tail; a witnessed adaptive copy uses its bounded time credit.
	snap, ok := readWALIndexSnapshot(s.dbPath)
	if !ok {
		res.reason = "pressure_no_wal_index"
		s.walCopy.pressureGiveUps.Add(1)
		return fmt.Errorf("%w: no wal-index", errWALPressureHold)
	}
	if budget <= s.walPressureHold() && snap.MxFrame > snap.NBackfill && snap.MxFrame-snap.NBackfill > allowed {
		res.reason = fmt.Sprintf("pressure_copy_incomplete remainder_frames=%d allowed=%d", snap.MxFrame-snap.NBackfill, allowed)
		s.walCopy.pressureGiveUps.Add(1)
		return errWALPressureHold
	}
	hctx, hcancel := context.WithDeadline(ctx, held.Add(budget))
	defer hcancel()
	if hook := walPressureResetHook; hook != nil {
		hook(hctx)
	}
	copyCredit := budget / 2
	if budget > s.walPressureHold() {
		copyCredit = budget - s.walPressureHold()
	}
	if budget > s.walPressureHold() {
		if hctx.Err() != nil {
			return hctx.Err()
		}
		if !res.beginAdaptiveWriterCopy(budget) {
			return errWALReclaimReadersInFlight
		}
		adaptiveCopy = true
	}
	if _, err := writer.passive(hctx, ctx, ckptDB, copyCredit, res.leaseOverride); err != nil && !errors.Is(err, errSQLiteCheckpointIncomplete) {
		res.reason = fmt.Sprintf("pressure_backfill error=%v", err)
		s.walCopy.pressureGiveUps.Add(1)
		return fmt.Errorf("%w: %w", errWALPressureHold, err)
	}
	if !writer.held {
		res.reason = "pressure_writer_credit_expired"
		s.walCopy.pressureGiveUps.Add(1)
		return errWALPressureHold
	}
	// Any slow-copy readmission has finished; no own waiter is visible here.
	if preemptible && s.writeWanted() {
		res.reason = "pressure_foreground_writer"
		return errWALReclaimWriterWaiting
	}
	resetCtx := writer.resetContext(hctx)
	if preemptible {
		// Poll only the held reset, after any passive readmission completed.
		// The copy keeps its original timed writer credit; no watcher sees
		// this background operation's own acquisition as a foreground waiter.
		var stopYield func()
		resetCtx, stopYield = s.yieldToWriters(resetCtx)
		defer stopYield()
	}
	result, err := s.resetWALForReclaim(resetCtx)
	if preemptible && err != nil && errors.Is(context.Cause(resetCtx), errWALReclaimWriterWaiting) {
		err = errWALReclaimWriterWaiting
	}
	res.pressureResetBusy = result.Busy > 0 && errors.Is(err, errSQLiteCheckpointIncomplete)
	if err != nil {
		res.reason = fmt.Sprintf("pressure_reset busy=%d wal_frames=%d checkpointed=%d error=%v", result.Busy, result.WALFrames, result.CheckpointedFrames, err)
		s.walCopy.pressureGiveUps.Add(1)
		return fmt.Errorf("%w: %w", errWALPressureHold, err)
	}
	res.openGate = true
	s.walCopy.pressureLaneResets.Add(1)
	log.Printf("store_sqlite: wal reclaim reset inside a busy lane writer_hold=%s", time.Since(held).Round(time.Microsecond))
	return nil
}

// A witnessed busy reset may close admission briefly, without holding the
// writer while existing readers drain. Drain, writer admission and reset all
// share the configured drain allowance; the writer still gets at most its
// existing short credit and remaining aggregate allowance.
func (s *Store) reclaimWALPressureResetAfterReaderDrain(ctx context.Context, ckptDB *sql.DB, res *walReclaimResult, allowance time.Duration) error {
	if s.writeWanted() {
		res.reason = "pressure_foreground_writer"
		return errWALReclaimWriterWaiting
	}
	started := time.Now()
	pauseCtx, cancel := context.WithDeadline(ctx, started.Add(allowance))
	defer cancel()
	yieldCtx, stopYield := s.yieldToWriters(pauseCtx)
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(stopYield) }
	defer stop()
	reopen, inFlight, err := s.readGate.quiesce(yieldCtx, started.Add(allowance))
	if err != nil {
		if !errors.Is(err, errReadGateBusy) {
			res.pause += time.Since(started)
			res.pauseClosed = true
		}
		if errors.Is(context.Cause(yieldCtx), errWALReclaimWriterWaiting) {
			err = errWALReclaimWriterWaiting
		}
		res.reason = fmt.Sprintf("pressure_reader_drain readers=%d error=%v", inFlight, err)
		return err
	}
	// A slow uninterruptible checkpoint must not extend closed admission.
	var resumeOnce sync.Once
	released := make(chan time.Time, 1)
	resume := func() { resumeOnce.Do(func() { reopen(); released <- time.Now() }) }
	timerJoined := make(chan struct{})
	stopReopen := context.AfterFunc(pauseCtx, func() { resume(); close(timerJoined) })
	defer func() {
		stopped := stopReopen()
		resume()
		if !stopped {
			<-timerJoined
		}
		res.pause += (<-released).Sub(started)
		res.pauseClosed = true
	}()
	if err := yieldCtx.Err(); err != nil {
		if errors.Is(context.Cause(yieldCtx), errWALReclaimWriterWaiting) {
			err = errWALReclaimWriterWaiting
		}
		res.reason = fmt.Sprintf("pressure_reader_drain error=%v", err)
		return err
	}
	if s.writeWanted() {
		res.reason = "pressure_foreground_writer"
		return errWALReclaimWriterWaiting
	}
	credit := min(s.walPressureHold(), walReclaimMaxWriterHold-res.writerSpent, time.Until(started.Add(allowance)))
	if credit <= 0 {
		res.reason = "pressure_reader_pause_credit_exhausted"
		return context.DeadlineExceeded
	}
	// Never count our own pending writer admission as a foreground waiter.
	stop()
	res.reason = ""
	return s.reclaimWALPressureResetOnce(pauseCtx, ckptDB, res, credit, true)
}
