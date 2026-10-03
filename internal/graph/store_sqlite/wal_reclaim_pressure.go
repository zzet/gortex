package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
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

// reclaimWALPressureReset is the writer step of a pressure attempt: only when
// the backfill is nearly complete, then a short final copy/reset. A proven
// urgent slow-copy tail gets one longer slice under the aggregate two-second
// allowance, rather than repeatedly letting writes overtake the final sync.
func (s *Store) reclaimWALPressureReset(ctx context.Context, ckptDB *sql.DB, res *walReclaimResult) error {
	operationCtx, cancel := context.WithTimeout(ctx, walReclaimLaneBudget)
	defer cancel()
	err := s.reclaimWALPressureResetOnce(operationCtx, ckptDB, res, walReclaimPressureHold)
	if err == nil || operationCtx.Err() != nil {
		return err
	}
	if budget := res.takeAdaptiveWriterBudget(); budget > 0 {
		res.reason = ""
		return s.reclaimWALPressureResetOnce(operationCtx, ckptDB, res, budget)
	}
	return err
}

func (s *Store) reclaimWALPressureResetOnce(ctx context.Context, ckptDB *sql.DB, res *walReclaimResult, budget time.Duration) error {
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
	acquireStarted := time.Now()
	acquireObserver := walWindowsDiagnosticPhase
	if acquireObserver != nil {
		acquireObserver("pressure_writer_acquire_enter", wctx, acquireStarted, 0, nil)
	}
	err := s.writeMu.LockContext(wctx)
	if acquireObserver != nil {
		acquireObserver("pressure_writer_acquire_return", wctx, acquireStarted, time.Since(acquireStarted), err)
	}
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
	if s.bulkConn != nil && !res.leaseOverride {
		res.outcome, res.reason = walReclaimSkipped, "bulk_writer"
		return errWALCheckpointDeferredBulk
	}
	if budget > walReclaimPressureHold && !res.adaptiveFrontierCurrent(s) {
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
	if budget <= walReclaimPressureHold && snap.MxFrame > snap.NBackfill && snap.MxFrame-snap.NBackfill > allowed {
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
	if budget > walReclaimPressureHold {
		copyCredit = budget - walReclaimPressureHold
	}
	if budget > walReclaimPressureHold {
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
	result, err := s.resetWALForReclaim(writer.resetContext(hctx))
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

// walWindowsDiagnosticPhase is nil outside the isolated diagnostic branch.
// It reports existing call boundaries without changing SQL or admission contexts.
var walWindowsDiagnosticPhase func(string, context.Context, time.Time, time.Duration, error)
