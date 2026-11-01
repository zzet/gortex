package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func observeDelayedReclaimSync(t *testing.T, db *sql.DB, before func(int)) {
	t.Helper()
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	key, ok := pausableConn(conn)
	require.NoError(t, conn.Close())
	require.True(t, ok)
	reclaimSyncStallState.Store(&reclaimSyncStall{tls: key, beforeSync: before})
	t.Cleanup(func() { reclaimSyncStallState.Store(nil) })
}

type reclaimCopyReturnQueryer struct {
	walCheckpointQueryer
	pause func()
}

func (q reclaimCopyReturnQueryer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	row := q.walCheckpointQueryer.QueryRowContext(ctx, query, args...)
	q.pause()
	return row
}

// Delay the Go handoff after the real checkpoint query. Inside synctest only
// this timer advances fake time; actual SQLite I/O is not charged fake time.
func pauseReclaimCopyReturn(t *testing.T, pause func()) {
	t.Helper()
	previous := walReclaimCreditPassiveQueryerObserver
	walReclaimCreditPassiveQueryerObserver = func(_ context.Context, _ *Store, db *sql.DB) (walCheckpointQueryer, func()) {
		return reclaimCopyReturnQueryer{walCheckpointQueryer: db, pause: pause}, nil
	}
	t.Cleanup(func() { walReclaimCreditPassiveQueryerObserver = previous })
}

func TestReclaimOperationCancellationJoinsSlowCheckpoint(t *testing.T) {
	s, db := finalBackfillFixture(t)
	entered, release := stallReclaimCheckpointSync(t, db)
	operation, cancel := context.WithCancel(t.Context())
	writerCtx, stop := context.WithTimeout(operation, time.Second)
	defer stop()
	done := make(chan error, 1)
	go func() {
		s.writeMu.Lock()
		writer := newWALReclaimWriterCredit(s, time.Now())
		defer writer.release()
		_, err := writer.passive(writerCtx, operation, db, 25*time.Millisecond, false)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("checkpoint never entered sync")
	}
	cancel()
	admission, end := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err := s.writeMu.LockContext(admission)
	end()
	if err == nil {
		s.writeMu.Unlock()
	}
	select {
	case <-done:
		close(release)
		t.Fatal("SQL returned before the VFS sync ended")
	default:
	}
	close(release)
	require.NoError(t, err, "parent cancellation failed to release the foreground gate")
	require.ErrorIs(t, <-done, context.Canceled)
	require.True(t, s.writeMu.TryLock(), "operation retained its writer credit after joining SQL")
	s.writeMu.Unlock()
}

// A completed slow copy gets one final admission opportunity from the unused
// portion of the original writer allowance, not a fresh full hold budget.
func TestReclaimSlowCopyCanResetWithinRemainingWriterCredit(t *testing.T) {
	for _, kind := range []string{"pressure", "short"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			stopStoreBackgroundReaders(s)
			synctest.Test(t, func(t *testing.T) {
				var copies int
				pauseReclaimCopyReturn(t, func() {
					copies++
					time.Sleep(300 * time.Millisecond)
				})
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				res := &walReclaimResult{}
				err := runFinalReclaimStep(ctx, kind, s, db, res)
				require.NoError(t, err)
				require.True(t, res.openGate, "joined durable copy never reached reset admission")
				require.Positive(t, copies, "the real checkpoint return was not delayed")
				require.LessOrEqual(t, res.writerHold, walReclaimPressureHold)
				require.True(t, s.writeMu.TryLock(), "reset retained writer ownership")
				s.writeMu.Unlock()
				var payload string
				require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
				require.Equal(t, "before", payload)
			})
		})
	}
}

func TestReclaimReadmissionSharesAggregateCredit(t *testing.T) {
	s, db := finalBackfillFixture(t)
	stopStoreBackgroundReaders(s)
	synctest.Test(t, func(t *testing.T) {
		pauseReclaimCopyReturn(t, func() { time.Sleep(300 * time.Millisecond) })
		operation, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		s.writeMu.Lock()
		writer := newWALReclaimWriterCredit(s, time.Now())
		defer writer.release()
		original, stop := context.WithTimeout(operation, 50*time.Millisecond)
		defer stop()
		_, err := writer.passive(original, operation, db, 25*time.Millisecond, false)
		require.NoError(t, err)
		require.True(t, writer.readmitted)
		require.Equal(t, 25*time.Millisecond, writer.spent)
		resetCtx := writer.resetContext(original)
		deadline, bounded := resetCtx.Deadline()
		require.True(t, bounded)
		require.Equal(t, 50*time.Millisecond-writer.spent, time.Until(deadline), "readmission renewed a full allowance")
		_, err = s.resetWALForReclaim(resetCtx)
		require.NoError(t, err)
		writer.release()
		require.Equal(t, 25*time.Millisecond, writer.spent, "only the first fake-time slice consumed credit")
	})
}

// A producer may append a small tail after the completed copy gives up its
// first writer slice. Final admission must use the unused slice to cover that
// tail, rather than discard an otherwise durable completed copy.
func TestReclaimReadmissionCatchesProducerTailWithinRemainingCredit(t *testing.T) {
	for _, kind := range []string{"unblocked", "pinned_reader", "slow_tail"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			stopStoreBackgroundReaders(s)
			synctest.Test(t, func(t *testing.T) {
				copies := 0
				var pin *sql.Tx
				defer func() {
					if pin != nil {
						_ = pin.Rollback()
					}
				}()
				pauseReclaimCopyReturn(t, func() {
					copies++
					if copies != 1 {
						if kind == "slow_tail" {
							time.Sleep(300 * time.Millisecond)
						}
						return
					}
					time.Sleep(300 * time.Millisecond)
					if kind == "pinned_reader" {
						var err error
						pin, err = s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
						require.NoError(t, err)
						var payload string
						require.NoError(t, pin.QueryRow(`SELECT payload FROM wal_churn WHERE id=1`).Scan(&payload))
					}
					s.writeMu.Lock()
					_, err := s.writerDB.Exec(`UPDATE wal_churn SET payload='producer-tail' WHERE id=1`)
					s.writeMu.Unlock()
					require.NoError(t, err)
				})
				operation, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				s.writeMu.Lock()
				writer := newWALReclaimWriterCredit(s, time.Now())
				defer writer.release()
				original, stop := context.WithTimeout(operation, 50*time.Millisecond)
				defer stop()
				_, err := writer.passive(original, operation, db, 25*time.Millisecond, false)
				if kind == "slow_tail" {
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.Equal(t, 2, copies)
					require.True(t, writer.readmitted)
					require.False(t, writer.held)
					require.Equal(t, 50*time.Millisecond, writer.spent, "tail copy renewed the aggregate allowance")
					return
				}
				if pin != nil {
					require.ErrorIs(t, err, errWALReclaimReadersInFlight)
				} else {
					require.NoError(t, err, "the completed copy's new tail never used its remaining writer credit")
				}
				require.True(t, writer.readmitted)
				require.Equal(t, 2, copies, "tail completion must make exactly one additional real PASSIVE")
				resetCtx := writer.resetContext(original)
				deadline, bounded := resetCtx.Deadline()
				require.True(t, bounded)
				require.Equal(t, 50*time.Millisecond-writer.spent, time.Until(deadline))
				if pin == nil {
					_, err = s.resetWALForReclaim(resetCtx)
					require.NoError(t, err)
				}
				writer.release()
				require.Equal(t, 25*time.Millisecond, writer.spent)
				var payload string
				require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id=1`).Scan(&payload))
				require.Equal(t, "producer-tail", payload)
			})
		})
	}
}

func TestReclaimReadmissionDoesNotReviveCancellationOrSpentCredit(t *testing.T) {
	for _, kind := range []string{"spent", "cancel_cause", "writer_demand"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			operation, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			s.writeMu.Lock()
			writer := newWALReclaimWriterCredit(s, time.Now())
			defer writer.release()
			original, stop := context.WithTimeout(operation, 50*time.Millisecond)
			defer stop()
			original, cancelCause := context.WithCancelCause(original)
			defer cancelCause(context.Canceled)
			credit := 25 * time.Millisecond
			if kind == "spent" {
				credit = 50 * time.Millisecond
			}
			writer.yieldToWriters = kind == "writer_demand"
			releaseDemand := func() {}
			defer func() { releaseDemand() }()
			observeDelayedReclaimSync(t, db, func(fileKind int) {
				if fileKind != vfsFileWAL {
					return
				}
				if kind == "cancel_cause" {
					cancelCause(context.DeadlineExceeded)
				}
				time.Sleep(300 * time.Millisecond)
				if kind == "writer_demand" {
					releaseDemand = s.AnnounceWrite()
				}
			})
			_, err := writer.passive(original, operation, db, credit, false)
			want := error(context.DeadlineExceeded)
			switch kind {
			case "cancel_cause":
				want = context.Canceled
			case "writer_demand":
				want = errWALReclaimWriterWaiting
			}
			require.True(t, errors.Is(err, want), "readmission error %v, want %v", err, want)
			require.False(t, writer.held)
			require.False(t, writer.readmitted)
		})
	}
}

func TestReclaimReadmissionPreservesNonurgentWithdrawal(t *testing.T) {
	s, db := finalBackfillFixture(t)
	releaseDemand := func() {}
	defer func() { releaseDemand() }()
	observeDelayedReclaimSync(t, db, func(kind int) {
		if kind == vfsFileWAL {
			time.Sleep(300 * time.Millisecond)
			releaseDemand = s.AnnounceWrite()
		}
	})
	operation, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	res := &walReclaimResult{}
	done, err := s.reclaimWALResetHold(operation, walReclaimConfig{thresholdBytes: 1}, db, res, false, false)
	require.ErrorIs(t, err, errWALReclaimWriterWaiting)
	require.False(t, done)
	require.Equal(t, "writer_waiting", res.reason)
	require.True(t, s.writeMu.TryLock())
	s.writeMu.Unlock()
}
