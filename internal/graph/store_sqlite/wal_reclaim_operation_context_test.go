package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
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

// Windows checkpoints can spend longer than the reset hold in an OS sync.
// Repeatedly interrupting immediately after that sync loses the copied
// frontier, so even genuine writer gaps cannot complete a reset.
func TestReclaimSlowFinalBackfillMakesProgressBetweenWriterBursts(t *testing.T) {
	s, db := finalBackfillFixture(t)
	var syncs atomic.Int64
	delaySync := func(kind int) {
		if kind == vfsFileWAL {
			syncs.Add(1)
			time.Sleep(300 * time.Millisecond)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var writes, worstWrite atomic.Int64
	written := make(chan struct{}, 1)
	producer := make(chan error, 1)
	started := time.Now()
	go func() {
		for {
			select {
			case <-ctx.Done():
				producer <- nil
				return
			case <-time.After(20 * time.Millisecond):
			}
			// The writer remains active throughout acceptance: 250ms bursts,
			// then 400ms gaps, with real SQL commits in each burst.
			if time.Since(started)%(650*time.Millisecond) >= 250*time.Millisecond {
				continue
			}
			wctx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
			before := time.Now()
			err := s.writeMu.LockContext(wctx)
			if err == nil {
				_, err = s.writerDB.ExecContext(wctx, `UPDATE wal_churn SET payload = ? WHERE id = 1`, fmt.Sprintf("burst-%d", writes.Load()+1))
				s.writeMu.Unlock()
			}
			stop()
			elapsed := time.Since(before).Nanoseconds()
			for prev := worstWrite.Load(); elapsed > prev && !worstWrite.CompareAndSwap(prev, elapsed); prev = worstWrite.Load() {
			}
			if err != nil {
				if ctx.Err() != nil {
					producer <- nil
				} else {
					producer <- err
				}
				return
			}
			writes.Add(1)
			select {
			case written <- struct{}{}:
			default:
			}
		}
	}()
	resets := 0
	// Cancellation can discard a connection. Rebind the TLS-scoped delay
	// each round so the old implementation cannot evade the slow-sync
	// condition by opening a replacement checkpoint connection.
	for ctx.Err() == nil && resets < 2 {
		observeDelayedReclaimSync(t, db, delaySync)
		res := &walReclaimResult{urgent: true}
		done, _ := s.reclaimWALResetHold(ctx, walReclaimConfig{thresholdBytes: 1}, db, res, false, false)
		if done {
			resets++
			// Require a subsequent foreground commit before another reset;
			// repeatedly truncating the already-empty log proves no progress.
			before := writes.Load()
			for writes.Load() == before && ctx.Err() == nil {
				select {
				case <-written:
				case <-ctx.Done():
				}
			}
		}
	}
	activeAtCompletion := ctx.Err() == nil
	cancel()
	require.NoError(t, <-producer)
	t.Logf("resets=%d foreground_writes=%d actual_wal_syncs=%d worst_foreground_gate_and_sql=%s elapsed=%s", resets, writes.Load(), syncs.Load(), time.Duration(worstWrite.Load()), time.Since(started))
	require.True(t, activeAtCompletion, "slow syncs prevented reset progress before the finite operation deadline")
	require.Equal(t, 2, resets)
	require.Positive(t, writes.Load())
	require.GreaterOrEqual(t, syncs.Load(), int64(2))
	require.Less(t, time.Duration(worstWrite.Load()), 100*time.Millisecond)
	var payload string
	require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
	require.Equal(t, fmt.Sprintf("burst-%d", writes.Load()), payload)
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

func TestReclaimSlowFinalBackfillRefusesContinuousWrites(t *testing.T) {
	s, db := finalBackfillFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	var writes, worst atomic.Int64
	done := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-time.After(20 * time.Millisecond):
			}
			wctx, end := context.WithTimeout(ctx, 100*time.Millisecond)
			start := time.Now()
			err := s.writeMu.LockContext(wctx)
			if err == nil {
				_, err = s.writerDB.ExecContext(wctx, `UPDATE wal_churn SET payload = ? WHERE id = 1`, fmt.Sprintf("continuous-%d", writes.Load()+1))
				s.writeMu.Unlock()
			}
			end()
			elapsed := time.Since(start).Nanoseconds()
			for prev := worst.Load(); elapsed > prev && !worst.CompareAndSwap(prev, elapsed); prev = worst.Load() {
			}
			if err != nil {
				if ctx.Err() != nil {
					done <- nil
				} else {
					done <- err
				}
				return
			}
			writes.Add(1)
		}
	}()
	var backfill uint32
	for range 3 {
		observeDelayedReclaimSync(t, db, func(kind int) {
			if kind == vfsFileWAL {
				time.Sleep(300 * time.Millisecond)
			}
		})
		res := &walReclaimResult{urgent: true}
		reset, _ := s.reclaimWALResetHold(ctx, walReclaimConfig{thresholdBytes: 1}, db, res, false, false)
		if reset {
			cancel()
			require.NoError(t, <-done)
			t.Fatal("slow copied frontier admitted a reset despite continuing writes")
		}
		if snap, ok := readWALIndexSnapshot(s.dbPath); ok {
			backfill = max(backfill, snap.NBackfill)
		}
	}
	cancel()
	require.NoError(t, <-done)
	require.Greater(t, writes.Load(), int64(10))
	require.Positive(t, backfill, "completed slow copies discarded their durable frontier")
	require.Less(t, time.Duration(worst.Load()), 100*time.Millisecond)
	t.Logf("continuous_writes=%d retained_backfill=%d worst_foreground_gate_and_sql=%s", writes.Load(), backfill, time.Duration(worst.Load()))
}
