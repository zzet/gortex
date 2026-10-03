package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// Only the caller changes this state. The timer releases writeMu through its
// own once/channel handoff; it never reads the writer state or result counters.
type walReclaimWriterCredit struct {
	store          *Store
	held           bool
	since          time.Time
	longest        time.Duration
	spent          time.Duration
	resetCtx       context.Context
	cancelReset    context.CancelFunc
	readmitted     bool
	yieldToWriters bool
	slowTail       *walReclaimSlowTail
}

func newWALReclaimWriterCredit(s *Store, held time.Time) *walReclaimWriterCredit {
	return &walReclaimWriterCredit{store: s, held: true, since: held}
}

func (w *walReclaimWriterCredit) release() {
	if w.held {
		w.store.writeMu.Unlock()
		took := time.Since(w.since)
		w.longest = max(w.longest, took)
		w.spent += took
		w.held = false
	}
	if w.cancelReset != nil {
		w.cancelReset()
		w.cancelReset = nil
	}
}

// resetContext names the admission scope that also governs the reset. A slow
// copy may use a single fresh scope, without increasing total writer credit.
func (w *walReclaimWriterCredit) resetContext(original context.Context) context.Context {
	if w.resetCtx != nil {
		return w.resetCtx
	}
	return original
}

// PASSIVE uses the separate checkpoint connection and never takes SQLite's
// writer lock. Its WAL-copy/database-sync ordering is unchanged. Fast passes
// retain a short writer gap so queued writes cannot starve reset admission;
// an uninterruptible call outliving its credit releases only our Go gate.
// The checkpoint reservation and connection remain owned until the call ends.
func (w *walReclaimWriterCredit) passive(ctx, operationCtx context.Context, db *sql.DB, credit time.Duration, leaseOverride bool) (walCheckpointResult, error) {
	if !w.held {
		return walCheckpointResult{}, ErrMaintenanceBusy
	}
	// Capture the original aggregate hold allowance before a slow SQL call
	// spends wall time without the writer. The initial and final slices share it.
	var allowance time.Duration
	if limit, ok := ctx.Deadline(); ok {
		allowance = limit.Sub(w.since) + w.spent
	}
	deadline := time.Now().Add(credit)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	stop, joined := make(chan struct{}), make(chan struct{})
	released := make(chan time.Time, 1)
	var once sync.Once
	release := func() {
		once.Do(func() {
			w.store.writeMu.Unlock()
			released <- time.Now()
		})
	}
	go func() {
		defer close(joined)
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-timer.C:
			release()
		case <-ctx.Done():
			release()
		case <-stop:
		}
	}()
	var finishOnce sync.Once
	finish := func() {
		finishOnce.Do(func() {
			close(stop)
			<-joined
			// A completed call must not win against already-expired credit.
			if !time.Now().Before(deadline) || ctx.Err() != nil {
				release()
			}
			select {
			case at := <-released:
				took := at.Sub(w.since)
				w.longest = max(w.longest, took)
				w.spent += took
				w.held = false
			default:
			}
		})
	}
	defer finish() // includes panic/Goexit cleanup of the timer handoff
	// The writer credit may end during an uninterruptible sync. Interrupting
	// the SQL at that point discards its copied frontier and connection; the
	// next short hold would repeat the same work. Keep SQL under its parent
	// operation cancellation and the existing finite lane budget instead.
	// Neither reset admission nor the writer hold inherits this longer scope.
	opCtx, cancelOperation := context.WithTimeout(operationCtx, walReclaimLaneBudget)
	defer cancelOperation()
	before, beforeOK := readWALReclaimFrontier(w.store.dbPath)
	beforeOK = beforeOK && time.Now().Before(deadline) && ctx.Err() == nil
	copyStarted := time.Now()
	result, err := checkpointWALOnceOn(opCtx, db, "PASSIVE")
	copyElapsed := time.Since(copyStarted)
	finish()
	if w.held {
		return result, err
	}
	if err != nil && !errors.Is(err, errSQLiteCheckpointIncomplete) {
		return result, err
	}
	// Observe a completed frontier before queuing for the remaining short
	// credit: a real foreground SQL writer may consume that entire queue
	// window. This is only a budget hint; the adaptive entry checks it again.
	if ctx.Err() == nil || (errors.Is(ctx.Err(), context.DeadlineExceeded) && errors.Is(context.Cause(ctx), context.DeadlineExceeded)) {
		if after, afterOK := readWALReclaimFrontier(w.store.dbPath); beforeOK && afterOK && err == nil && copyElapsed > allowance && result.WALFrames > 0 && result.CheckpointedFrames == result.WALFrames && uint32(result.CheckpointedFrames) >= before.mx && after.backfill >= before.mx && after.mx > after.backfill && before.salt == after.salt {
			w.slowTail = &walReclaimSlowTail{salt: after.salt, copyElapsed: copyElapsed, covered: before.mx}
		}
	}
	// A completed copy may outlive the original hold deadline without
	// consuming its remaining writer credit. Give reset admission one fresh
	// slice of only that unused credit, under the same finite operation budget.
	// Parent cancellation and an interactive yield never earn readmission.
	admissionCtx := ctx
	if ctx.Err() != nil {
		unused := allowance - w.spent
		if w.readmitted || unused <= 0 || operationCtx.Err() != nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) || !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			return result, ctx.Err()
		}
		if w.yieldToWriters && w.store.writeWanted() {
			return result, errWALReclaimWriterWaiting
		}
		limit := time.Now().Add(unused)
		if operationLimit, ok := opCtx.Deadline(); ok && operationLimit.Before(limit) {
			limit = operationLimit
		}
		var cancel context.CancelFunc
		admissionCtx, cancel = context.WithDeadline(operationCtx, limit)
		stopYield := func() {}
		if w.yieldToWriters {
			admissionCtx, stopYield = w.store.yieldToWriters(admissionCtx)
		}
		w.readmitted = true
		w.resetCtx = admissionCtx
		w.cancelReset = func() { stopYield(); cancel() }
	}
	if err := w.store.writeMu.LockContext(admissionCtx); err != nil {
		return result, err
	}
	w.since, w.held = time.Now(), true
	if w.store.bulkConn != nil && !leaseOverride {
		return result, errWALCheckpointDeferredBulk
	}
	snap, ok := readWALIndexSnapshot(w.store.dbPath)
	if !ok || snap.MxFrame != snap.NBackfill {
		return result, errWALReclaimReadersInFlight
	}
	return walCheckpointResult{WALFrames: int(snap.MxFrame), CheckpointedFrames: int(snap.NBackfill)}, nil
}
