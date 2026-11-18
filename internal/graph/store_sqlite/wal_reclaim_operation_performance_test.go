//go:build performance

package store_sqlite

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These original timed writer/copy measurements run through the existing
// opt-in performance workflow. The ordinary final-backfill and operation
// controls retain deterministic stale-tail, durable progress and cancellation
// coverage without a sustained writer-throughput or foreground-latency gate.

// Windows checkpoints can spend longer than the reset hold in an OS sync.
// Repeatedly interrupting immediately after that sync loses the copied
// frontier, so even genuine writer gaps cannot complete a reset.
func TestPerformanceReclaimSlowFinalBackfillMakesProgressBetweenWriterBursts(t *testing.T) {
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

func TestPerformanceReclaimSlowFinalBackfillRefusesContinuousWrites(t *testing.T) {
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
			gateElapsed := time.Since(start)
			stage := "gate"
			var sqlElapsed, releaseElapsed time.Duration
			if err == nil {
				stage = "SQL"
				beforeSQL := time.Now()
				_, err = s.writerDB.ExecContext(wctx, `UPDATE wal_churn SET payload = ? WHERE id = 1`, fmt.Sprintf("continuous-%d", writes.Load()+1))
				sqlElapsed = time.Since(beforeSQL)
				beforeRelease := time.Now()
				s.writeMu.Unlock()
				releaseElapsed = time.Since(beforeRelease)
			}
			end()
			elapsed := time.Since(start).Nanoseconds()
			for prev := worst.Load(); elapsed > prev && !worst.CompareAndSwap(prev, elapsed); prev = worst.Load() {
			}
			if err != nil {
				if ctx.Err() != nil {
					done <- nil
				} else {
					done <- fmt.Errorf("foreground %s failed (gate=%s SQL=%s release=%s): %w", stage, gateElapsed, sqlElapsed, releaseElapsed, err)
				}
				return
			}
			writes.Add(1)
		}
	}()
	// Require an actual foreground commit after each completed copy. A
	// coincidental safe gap in a continuous producer is not a stale-tail
	// refusal oracle.
	walCheckpointCallObserver = func(mode string, _ time.Time, _ time.Duration) {
		if mode != "PASSIVE" {
			return
		}
		before := writes.Load()
		for writes.Load() == before && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(time.Millisecond):
			}
		}
	}
	t.Cleanup(func() { walCheckpointCallObserver = nil })
	var backfill uint32
	for range 3 {
		observeDelayedReclaimSync(t, db, func(kind int) {
			if kind == vfsFileWAL {
				time.Sleep(300 * time.Millisecond)
			}
		})
		res := &walReclaimResult{urgent: true}
		// This oracle concerns one copy admission, not the urgent policy
		// that may copy the new tail in a separate bounded adaptive slice.
		reset, _ := s.reclaimWALResetHoldOnce(ctx, walReclaimConfig{thresholdBytes: 1}, db, res, false, false, walReclaimResetHold)
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
