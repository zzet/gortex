package store_sqlite

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSlowConvergenceProofRefusesInvalidCopies(t *testing.T) {
	for _, kind := range []string{"cancelled", "new_WAL", "incomplete", "no_actual_copy"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			lane := &fakeBuildLane{}
			lane.install(s)
			lane.held.Store(true)
			t.Cleanup(func() { lane.held.Store(false) })
			// Every case must establish real copied/backfilled state before
			// testing invalid proof. This is a finite fixture setup guard,
			// not a five-second production SLA for writer-free SQL or sync.
			// Cancellation below remains tied to a returned full-copy tuple.
			const copySetupBudget = 30 * time.Second
			ctx, cancel := context.WithTimeout(t.Context(), copySetupBudget)
			defer cancel()
			if kind == "incomplete" {
				reader, err := s.db.BeginTx(ctx, nil)
				require.NoError(t, err)
				t.Cleanup(func() { _ = reader.Rollback() })
				var payload string
				require.NoError(t, reader.QueryRowContext(ctx, `SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			}
			if kind == "no_actual_copy" {
				_, err := checkpointWALOnceOn(ctx, db, "PASSIVE")
				require.NoError(t, err)
			} else {
				s.writeMu.Lock()
				_, err := s.writerDB.ExecContext(ctx, `UPDATE wal_churn SET payload = 'needs-copy' WHERE id % 8 = 1`)
				s.writeMu.Unlock()
				require.NoError(t, err)
			}
			beforeFrontier, frontierKnown := readWALReclaimFrontier(s.dbPath)
			require.True(t, frontierKnown)
			var observed, controlledCancel atomic.Bool
			var first walCheckpointResult
			previousObserver := walCheckpointResultObserver
			walCheckpointResultObserver = func(mode string, _ time.Time, _ time.Duration, result walCheckpointResult, scanErr error) {
				if mode != "PASSIVE" || !observed.CompareAndSwap(false, true) {
					return
				}
				first = result
				switch kind {
				case "cancelled":
					if scanErr == nil && result.Busy == 0 && result.WALFrames > 0 && result.CheckpointedFrames == result.WALFrames {
						controlledCancel.Store(true)
						cancel()
					}
				case "new_WAL":
					_, err := checkpointWALOnceOn(t.Context(), s.writerDB, "TRUNCATE")
					require.NoError(t, err)
				}
				if kind == "cancelled" || kind == "new_WAL" {
					s.writeMu.Lock()
					_, err := s.writerDB.ExecContext(t.Context(), `UPDATE wal_churn SET payload = 'new-tail' WHERE id % 8 = 1`)
					s.writeMu.Unlock()
					require.NoError(t, err)
					if kind == "new_WAL" {
						after, known := readWALReclaimFrontier(s.dbPath)
						require.True(t, known)
						require.NotEqual(t, beforeFrontier.salt, after.salt, "real TRUNCATE did not replace the WAL identity")
					}
				}
			}
			t.Cleanup(func() { walCheckpointResultObserver = previousObserver })
			observeAdaptiveMultiFrameSync(t, db, func(file int) {
				if file == vfsFileWAL {
					time.Sleep(75 * time.Millisecond)
				}
			}, nil)
			attempt := &backgroundCheckpointAttempt{copy: &walCopyAttempt{pressure: true}}
			result := s.convergeBackfillPacedWithSmallRemainder(ctx, db, attempt, walReclaimPressureSmallFrames)
			t.Logf("kind=%s copy_setup_budget=%s controlled_cancel_requested=%v actual_first_tuple=%+v observed=%v stop=%s passes=%d context=%v", kind, copySetupBudget, controlledCancel.Load(), first, observed.Load(), result.stop, result.passes, ctx.Err())
			if kind == "cancelled" {
				require.True(t, controlledCancel.Load(), "the error-free full-copy cancellation boundary was not reached")
				require.ErrorIs(t, ctx.Err(), context.Canceled, "fixture setup expiry is not the intended cancellation witness")
			}
			require.Nil(t, result.slowTail, "invalid copy earned adaptive entitlement")
			require.NotEqual(t, "slow_tail_plateau", result.stop)
			if kind == "no_actual_copy" {
				require.False(t, observed.Load())
				require.Zero(t, result.passes)
			} else {
				require.True(t, observed.Load(), "the actual convergence copy premise was not reached")
				if kind == "incomplete" {
					require.Less(t, first.CheckpointedFrames, first.WALFrames)
				} else {
					require.Zero(t, first.Busy)
					require.Positive(t, first.WALFrames)
					require.Equal(t, first.WALFrames, first.CheckpointedFrames)
				}
			}
		})
	}
}
