package store_sqlite

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func warmupWALFixture(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	s, db := finalBackfillFixture(t)
	stopStoreBackgroundReaders(s)
	s.walReclaim.update(func(st *WALReclaimStats) {
		st.ThresholdBytes = 1 << 20
		st.CeilingBytes = 32 << 20
	})
	oldPoll := retirementWALWaitPoll
	retirementWALWaitPoll = 10 * time.Millisecond
	t.Cleanup(func() { retirementWALWaitPoll = oldPoll })
	return s, db
}

func startWarmupFixtureReclaimer(t *testing.T, s *Store, ceilingMB string) {
	t.Helper()
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "1")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", ceilingMB)
	oldFloor := walReclaimCeilingFloor
	walReclaimCeilingFloor = 1 << 20
	t.Cleanup(func() { walReclaimCeilingFloor = oldFloor })
	setWALReclaimCadence(t, time.Hour, 10*time.Millisecond, 20*time.Millisecond)
	stop := make(chan struct{})
	s.stopCheckpoint = stop
	join := s.startWALReclaimLoop(s.dbPath + "-wal")
	t.Cleanup(func() {
		close(stop)
		join()
	})
}

// RESTART and the next commit can reuse a correctly reset allocation larger
// than the pressure trigger. Requiring the physical file below that trigger
// would strand a valid warm store even though the next bounded chunk fits.
func TestWarmupWALWaitReusesExistingResetAllocation(t *testing.T) {
	s, db := warmupWALFixture(t)
	_, err := checkpointWALOnceOn(t.Context(), db, "RESTART")
	require.NoError(t, err)
	s.writeMu.Lock()
	_, err = s.writerDB.Exec(`UPDATE wal_churn SET payload='reused-allocation' WHERE id=1`)
	s.writeMu.Unlock()
	require.NoError(t, err)
	physical := walFileSize(s.dbPath + "-wal")
	require.Greater(t, physical, int64(1<<20))
	require.Less(t, s.retirementLogBytes(), int64(1<<20))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, s.AwaitWarmupWAL(ctx))
	require.NoError(t, churnWriteOnce(s, 0))
	require.LessOrEqual(t, walFileSize(s.dbPath+"-wal"), int64(32<<20))
}

func TestWarmupWALWaitKeepsPhysicalHeadroomAfterLogicalReset(t *testing.T) {
	s, db := warmupWALFixture(t)
	_, err := checkpointWALOnceOn(t.Context(), db, "RESTART")
	require.NoError(t, err)
	s.writeMu.Lock()
	_, err = s.writerDB.Exec(`UPDATE wal_churn SET payload='logical-reset-only' WHERE id=1`)
	s.writeMu.Unlock()
	require.NoError(t, err)
	s.walReclaim.update(func(st *WALReclaimStats) { st.CeilingBytes = 8 << 20 })
	before := walFileSize(s.dbPath + "-wal")
	require.Greater(t, before, int64(4<<20))
	require.Less(t, s.retirementLogBytes(), int64(1<<20))
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.AwaitWarmupWAL(ctx), context.DeadlineExceeded)
	require.Equal(t, before, walFileSize(s.dbPath+"-wal"), "logical reset bypassed physical headroom")
	startWarmupFixtureReclaimer(t, s, "8")
	progress, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	require.NoError(t, s.AwaitWarmupWAL(progress), "production nudge must reclaim an otherwise reusable allocation")
	require.LessOrEqual(t, walFileSize(s.dbPath+"-wal"), int64(4<<20))
}

// This is a producer boundary: no next chunk is allocated while an older
// reader prevents progress. Cancellation refuses it; a real reader release
// plus checkpoint admits the next actual bounded write.
func TestWarmupWALWaitGatesNextAllocationUntilReaderProgress(t *testing.T) {
	for _, kind := range []string{"cancel", "deadline", "reader_release"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := warmupWALFixture(t)
			pin, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			defer func() { _ = pin.Rollback() }()
			var count int
			require.NoError(t, pin.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&count))
			require.NoError(t, churnWriteOnce(s, 0))
			require.NoError(t, churnWriteOnce(s, 1))
			require.Greater(t, s.retirementLogBytes(), int64(1<<20))
			before := walFileSize(s.dbPath + "-wal")
			startWarmupFixtureReclaimer(t, s, "32")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 100*time.Millisecond)
				defer stop()
			}
			var allocations atomic.Int64
			done := make(chan error, 1)
			go func() {
				err := s.AwaitWarmupWAL(ctx)
				if err == nil {
					allocations.Add(1)
					err = churnWriteOnce(s, 1)
				}
				done <- err
			}()
			joined := false
			defer func() {
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						t.Error("owned warmup boundary did not join")
					}
				}
			}()
			require.Eventually(t, func() bool { return s.walReclaimRequestedAt.Load() != 0 }, 2*time.Second, time.Millisecond,
				"warmup did not request the existing production reclaim")
			require.Zero(t, allocations.Load())
			require.Equal(t, before, walFileSize(s.dbPath+"-wal"), "refused producer allocated another chunk")
			if kind == "reader_release" {
				require.NoError(t, pin.Rollback())
			} else if kind == "cancel" {
				cancel()
			}
			select {
			case err = <-done:
				joined = true
			case <-time.After(2 * time.Second):
				cancel()
				t.Fatal("warmup boundary did not join")
			}
			if kind == "reader_release" {
				require.NoError(t, err)
				require.EqualValues(t, 1, allocations.Load())
				require.LessOrEqual(t, walFileSize(s.dbPath+"-wal"), int64(32<<20))
			} else {
				want := error(context.Canceled)
				if kind == "deadline" {
					want = context.DeadlineExceeded
				}
				require.ErrorIs(t, err, want)
				require.Zero(t, allocations.Load())
				require.Equal(t, before, walFileSize(s.dbPath+"-wal"))
			}
		})
	}
}
