package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A durable sync slower than the interval between actual writes needs a
// bounded quiescent completion opportunity, not another copy of the new tail.
func TestUrgentReclaimCompletesSlowSyncWithContinuousWrites(t *testing.T) {
	for _, kind := range []string{"short", "pressure"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			var writes, syncs, worstGate, worstSQL atomic.Int64
			committed := make(chan struct{}, 1)
			producer := make(chan error, 1)
			go func() {
				for {
					select {
					case <-ctx.Done():
						producer <- nil
						return
					case <-time.After(150 * time.Millisecond):
					}
					wctx, stop := context.WithTimeout(ctx, 2*time.Second)
					before := time.Now()
					err := s.writeMu.LockContext(wctx)
					gate := time.Since(before)
					stage := "gate"
					var sqlTime time.Duration
					if err == nil {
						stage = "SQL"
						before = time.Now()
						_, err = s.writerDB.ExecContext(wctx, `UPDATE wal_churn SET payload = ? WHERE id = 1`, fmt.Sprintf("urgent-%d", writes.Load()+1))
						sqlTime = time.Since(before)
						s.writeMu.Unlock()
					}
					stop()
					for prev := worstGate.Load(); int64(gate) > prev && !worstGate.CompareAndSwap(prev, int64(gate)); prev = worstGate.Load() {
					}
					for prev := worstSQL.Load(); int64(sqlTime) > prev && !worstSQL.CompareAndSwap(prev, int64(sqlTime)); prev = worstSQL.Load() {
					}
					if err != nil {
						if ctx.Err() != nil {
							producer <- nil
						} else {
							producer <- fmt.Errorf("foreground %s (gate=%s SQL=%s): %w", stage, gate, sqlTime, err)
						}
						return
					}
					writes.Add(1)
					select {
					case committed <- struct{}{}:
					default:
					}
				}
			}()
			var joinOnce sync.Once
			var producerErr error
			join := func() {
				joinOnce.Do(func() {
					cancel()
					select {
					case producerErr = <-producer:
					case <-time.After(3 * time.Second):
						producerErr = fmt.Errorf("foreground producer did not join")
					}
				})
			}
			t.Cleanup(join)
			started := time.Now()
			resets := 0
			adaptiveAttempts := 0
			var aggregateMax time.Duration
			for ctx.Err() == nil && resets < 2 {
				observeDelayedReclaimSync(t, db, func(file int) {
					if file == vfsFileWAL {
						syncs.Add(1)
						time.Sleep(300 * time.Millisecond)
					}
				})
				res := &walReclaimResult{urgent: true}
				done := false
				if kind == "pressure" {
					done = s.reclaimWALPressureReset(ctx, db, res) == nil
				} else {
					done, _ = s.reclaimWALResetHold(ctx, walReclaimConfig{thresholdBytes: 1}, db, res, false, false)
				}
				if res.adaptiveUsed {
					adaptiveAttempts++
					aggregateMax = max(aggregateMax, res.writerSpent)
					require.LessOrEqual(t, res.writerSpent, walReclaimMaxWriterHold)
				}
				if !done {
					continue
				}
				resets++
				before := writes.Load()
				for writes.Load() == before && ctx.Err() == nil {
					select {
					case <-committed:
					case <-ctx.Done():
					}
				}
			}
			active := ctx.Err() == nil
			join()
			t.Logf("resets=%d writes=%d WAL_syncs=%d adaptive_attempts=%d aggregate_hold_max=%s worst_gate=%s worst_SQL=%s elapsed=%s producer_error=%v", resets, writes.Load(), syncs.Load(), adaptiveAttempts, aggregateMax, time.Duration(worstGate.Load()), time.Duration(worstSQL.Load()), time.Since(started), producerErr)
			require.NoError(t, producerErr)
			require.True(t, active, "urgent reset never caught the growing tail before its finite deadline")
			require.Equal(t, 2, resets)
			require.Positive(t, adaptiveAttempts)
			require.GreaterOrEqual(t, writes.Load(), int64(3))
			require.GreaterOrEqual(t, syncs.Load(), int64(2))
			require.Less(t, time.Duration(worstGate.Load()), 2*time.Second)
			var payload string
			require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			require.Equal(t, fmt.Sprintf("urgent-%d", writes.Load()), payload)
		})
	}
}

func TestWALAdaptiveWriterBudgetIsSingleAndBounded(t *testing.T) {
	for _, kind := range []string{"nonurgent", "last_resort", "no_witness", "exhausted", "eligible"} {
		t.Run(kind, func(t *testing.T) {
			r := &walReclaimResult{urgent: true, writerSpent: 350 * time.Millisecond, slowTail: &walReclaimSlowTail{copyElapsed: time.Second}}
			switch kind {
			case "nonurgent":
				r.urgent = false
			case "last_resort":
				r.lastResort = true
			case "no_witness":
				r.slowTail = nil
			case "exhausted":
				r.writerSpent = walReclaimMaxWriterHold
			}
			budget := r.takeAdaptiveWriterBudget()
			if kind != "eligible" {
				require.Zero(t, budget)
				return
			}
			require.Equal(t, walReclaimMaxWriterHold-r.writerSpent, budget)
			require.Equal(t, budget, r.takeAdaptiveWriterBudget(), "a proposal consumed credit before an actual copy")
			require.True(t, r.beginAdaptiveWriterCopy(budget))
			require.Zero(t, r.takeAdaptiveWriterBudget(), "another helper round renewed adaptive credit after a copy began")
		})
	}
}

func TestWALAdaptiveEntryKeepsStateAndCancellationFences(t *testing.T) {
	for _, kind := range []string{"new_wal", "bulk", "lane", "cancelled", "last_resort_after_adaptive", "pinned_reader"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			frontier, ok := readWALReclaimFrontier(s.dbPath)
			require.True(t, ok)
			r := &walReclaimResult{urgent: true, slowTail: &walReclaimSlowTail{salt: frontier.salt, covered: frontier.backfill, copyElapsed: 300 * time.Millisecond}}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			switch kind {
			case "new_wal":
				_, err := checkpointWALOnceOn(ctx, db, "TRUNCATE")
				require.NoError(t, err)
				s.writeMu.Lock()
				_, err = s.writerDB.ExecContext(ctx, `UPDATE wal_churn SET payload = 'new-WAL' WHERE id = 1`)
				s.writeMu.Unlock()
				require.NoError(t, err)
			case "bulk":
				engaged, err := s.BeginGenerationBulkLoad(77)
				require.NoError(t, err)
				require.True(t, engaged)
				t.Cleanup(func() { _ = s.EndGenerationBulkLoadFor(77) })
			case "lane":
				lane := &fakeBuildLane{}
				lane.install(s)
				lane.held.Store(true)
				t.Cleanup(func() { lane.held.Store(false) })
			case "cancelled":
				cancel()
			case "last_resort_after_adaptive":
				r.adaptiveUsed, r.lastResort = true, true
				err := s.reclaimWALInLane(ctx, walReclaimConfig{}, db, r)
				require.ErrorIs(t, err, errWALReclaimReadersInFlight)
				require.Zero(t, r.writerSpent)
				return
			case "pinned_reader":
				reader, err := s.db.BeginTx(ctx, nil)
				require.NoError(t, err)
				t.Cleanup(func() { _ = reader.Rollback() })
				var payload string
				require.NoError(t, reader.QueryRowContext(ctx, `SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
				s.writeMu.Lock()
				_, err = s.writerDB.ExecContext(ctx, `UPDATE wal_churn SET payload = 'after-reader' WHERE id = 1`)
				s.writeMu.Unlock()
				require.NoError(t, err)
				r.slowTail = nil
				observeDelayedReclaimSync(t, db, func(file int) {
					if file == vfsFileWAL {
						time.Sleep(300 * time.Millisecond)
					}
				})
				done, _ := s.reclaimWALResetHold(ctx, walReclaimConfig{}, db, r, false, false)
				require.False(t, done)
				require.False(t, r.adaptiveUsed, "a pinned read mark qualified as a completed slow copy")
				return
			}
			budget := r.takeAdaptiveWriterBudget()
			require.Positive(t, budget)
			done, err := s.reclaimWALResetHoldOnce(ctx, walReclaimConfig{}, db, r, false, true, budget)
			require.False(t, done)
			if kind == "cancelled" {
				require.True(t, errors.Is(err, context.Canceled))
			}
			require.Less(t, r.writerSpent, walReclaimResetHold)
		})
	}
}

func TestWALAdaptiveCreditRefusesANewTailAfterTimerRelease(t *testing.T) {
	assertAdaptiveCreditCommittedTailRefusal(t, 0)
}

// The original hold deadline can expire while a real WAL sync continues. A
// fresh admission slice may use only its unused aggregate credit; a committed
// foreground tail must still be refused by the current-frontier fence.
func TestWALAdaptiveCreditRefusesCommittedTailAfterHoldExpiry(t *testing.T) {
	assertAdaptiveCreditCommittedTailRefusal(t, 150*time.Millisecond)
}

func assertAdaptiveCreditCommittedTailRefusal(t *testing.T, syncDelay time.Duration) {
	t.Helper()
	s, db := finalBackfillFixture(t)
	var delayedSyncs atomic.Int64
	if syncDelay > 0 {
		observeDelayedReclaimSync(t, db, func(kind int) {
			if kind == vfsFileWAL {
				delayedSyncs.Add(1)
				time.Sleep(syncDelay)
			}
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	completedCopy := make(chan struct{})
	written := make(chan error, 1)
	var once sync.Once
	walCheckpointCallObserver = func(mode string, _ time.Time, _ time.Duration) {
		if mode == "PASSIVE" {
			once.Do(func() { close(completedCopy) })
			select {
			case <-written:
			case <-ctx.Done():
			}
		}
	}
	t.Cleanup(func() { walCheckpointCallObserver = nil })
	writerDone := make(chan error, 1)
	go func() {
		select {
		case <-completedCopy:
		case <-ctx.Done():
			writerDone <- ctx.Err()
			return
		}
		err := s.writeMu.LockContext(ctx)
		if err == nil {
			_, err = s.writerDB.ExecContext(ctx, `UPDATE wal_churn SET payload = 'after-adaptive-copy' WHERE id = 1`)
			s.writeMu.Unlock()
		}
		written <- err
		writerDone <- err
	}()
	var joinOnce sync.Once
	var writeErr error
	join := func() { joinOnce.Do(func() { cancel(); writeErr = <-writerDone }) }
	t.Cleanup(join)
	s.writeMu.Lock()
	held := time.Now()
	writer := newWALReclaimWriterCredit(s, held)
	defer writer.release()
	// A declared longer slice is still subject to the same live-state fence.
	// The real commit is forced after timer release, before readmission.
	budget := walReclaimResetHold + 40*time.Millisecond
	holdCtx, stop := context.WithTimeout(ctx, budget)
	defer stop()
	_, err := writer.passive(holdCtx, ctx, db, budget-walReclaimResetHold/2, false)
	require.ErrorIs(t, err, errWALReclaimReadersInFlight)
	// The returned frontier-fence error is authoritative; the original hold
	// context may legitimately have expired before a fresh admission slice.
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	if syncDelay > 0 {
		require.Positive(t, delayedSyncs.Load(), "the controlled WAL sync was not exercised")
		require.True(t, writer.readmitted, "the expired hold did not use unused credit for fresh admission")
		require.NotNil(t, writer.resetCtx, "fresh admission did not retain its effective scope")
	}
	writer.release()
	join()
	require.NoError(t, writeErr)
	frontier, ok := readWALReclaimFrontier(s.dbPath)
	require.True(t, ok)
	require.Greater(t, frontier.mx, frontier.backfill)
	var payload string
	require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
	require.Equal(t, "after-adaptive-copy", payload)
}
