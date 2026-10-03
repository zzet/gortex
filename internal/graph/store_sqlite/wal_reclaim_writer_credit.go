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
	store   *Store
	held    bool
	since   time.Time
	longest time.Duration
}

func newWALReclaimWriterCredit(s *Store, held time.Time) *walReclaimWriterCredit {
	return &walReclaimWriterCredit{store: s, held: true, since: held}
}

func (w *walReclaimWriterCredit) release() {
	if w.held {
		w.longest = max(w.longest, time.Since(w.since))
		w.store.writeMu.Unlock()
		w.held = false
	}
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
				w.longest = max(w.longest, at.Sub(w.since))
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
	result, err := checkpointWALOnceOn(opCtx, db, "PASSIVE")
	finish()
	if w.held {
		return result, err
	}
	if err != nil && !errors.Is(err, errSQLiteCheckpointIncomplete) {
		return result, err
	}
	// A release admits writers and bulk windows. Reacquire within the SAME
	// original context/deadline, then require CURRENT complete backfill.
	if err := w.store.writeMu.LockContext(ctx); err != nil {
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
