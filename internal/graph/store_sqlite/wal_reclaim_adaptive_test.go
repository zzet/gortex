package store_sqlite

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

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
			beforeFrontier, ok := readWALReclaimFrontier(s.dbPath)
			require.True(t, ok)
			beforeIO := vfsIOMark()
			var checkpointCalls atomic.Int64
			oldObserver := walCheckpointCallObserver
			walCheckpointCallObserver = func(string, time.Time, time.Duration) { checkpointCalls.Add(1) }
			t.Cleanup(func() { walCheckpointCallObserver = oldObserver })
			done, err := s.reclaimWALResetHoldOnce(ctx, walReclaimConfig{}, db, r, false, true, budget)
			walCheckpointCallObserver = oldObserver
			require.False(t, done)
			switch kind {
			case "new_wal":
				require.ErrorIs(t, err, errWALReclaimReadersInFlight)
			case "bulk":
				require.ErrorIs(t, err, errWALCheckpointDeferredBulk)
			case "lane":
				require.ErrorIs(t, err, errWALReclaimEditBurst)
			case "cancelled":
				require.ErrorIs(t, err, context.Canceled)
			}
			// Rejected entry must not consume the adaptive opportunity or run
			// SQL. Wall time also includes scheduling and the release callback;
			// it does not identify whether the copy/reset boundary was crossed.
			require.False(t, r.adaptiveUsed)
			require.Zero(t, r.adaptiveBudget)
			require.Zero(t, r.adaptiveHold)
			require.Nil(t, r.resetReaders)
			require.Zero(t, checkpointCalls.Load())
			require.Zero(t, vfsIOMark().Since(beforeIO).OtherMainWriteBytes)
			afterFrontier, ok := readWALReclaimFrontier(s.dbPath)
			require.True(t, ok)
			require.Equal(t, beforeFrontier, afterFrontier)
			require.True(t, s.writeMu.TryLock(), "rejected entry must release the actual writer")
			s.writeMu.Unlock()
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
	s, db := finalBackfillFixture(t)
	stopStoreBackgroundReaders(s)
	synctest.Test(t, func(t *testing.T) {
		assertAdaptiveCreditCommittedTailRefusalOn(t, s, db, 150*time.Millisecond)
	})
}

func assertAdaptiveCreditCommittedTailRefusal(t *testing.T, syncDelay time.Duration) {
	t.Helper()
	s, db := finalBackfillFixture(t)
	assertAdaptiveCreditCommittedTailRefusalOn(t, s, db, syncDelay)
}

func assertAdaptiveCreditCommittedTailRefusalOn(t *testing.T, s *Store, db *sql.DB, syncDelay time.Duration) {
	t.Helper()
	budget := walReclaimResetHold + 40*time.Millisecond
	copyCredit := budget - walReclaimResetHold/2
	if syncDelay > 0 {
		// Fake-time return delay outlasts the hold without charging real I/O.
		copyCredit = walReclaimResetHold / 2
		syncDelay += budget
	}
	var delayedSyncs atomic.Int64
	if syncDelay > 0 {
		pauseReclaimCopyReturn(t, func() {
			delayedSyncs.Add(1)
			time.Sleep(syncDelay)
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	completedCopies := make(chan struct{}, 2)
	written := make(chan error, 1)
	walCheckpointCallObserver = func(mode string, _ time.Time, _ time.Duration) {
		if mode == "PASSIVE" {
			select {
			case completedCopies <- struct{}{}:
			case <-ctx.Done():
				return
			}
			select {
			case <-written:
			case <-ctx.Done():
			}
		}
	}
	t.Cleanup(func() { walCheckpointCallObserver = nil })
	writerDone := make(chan error, 1)
	go func() {
		writes := 1
		if syncDelay > 0 {
			writes = 2
		}
		for ordinal := 0; ordinal < writes; ordinal++ {
			select {
			case <-completedCopies:
			case <-ctx.Done():
				writerDone <- ctx.Err()
				return
			}
			err := s.writeMu.LockContext(ctx)
			if err == nil {
				payload := "after-adaptive-copy"
				if ordinal+1 < writes {
					payload = "first-adaptive-tail"
				}
				_, err = s.writerDB.ExecContext(ctx, `UPDATE wal_churn SET payload = ? WHERE id = 1`, payload)
				s.writeMu.Unlock()
			}
			written <- err
			if err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
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
	holdCtx, stop := context.WithTimeout(ctx, budget)
	defer stop()
	_, err := writer.passive(holdCtx, ctx, db, copyCredit, false)
	require.ErrorIs(t, err, errWALReclaimReadersInFlight)
	// The returned frontier-fence error is authoritative; the original hold
	// context may legitimately have expired before a fresh admission slice.
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	if syncDelay > 0 {
		require.Equal(t, int64(2), delayedSyncs.Load(), "only one additional real PASSIVE may finish a committed tail")
		require.True(t, writer.tailCopyTried)
		require.LessOrEqual(t, writer.spent, budget)
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

// A scheduler pause after writer admission cannot authorize a stale frontier.
// Fake time makes the distinction between elapsed hold accounting and actual
// adaptive-copy admission deterministic without delaying the host.
func TestWALAdaptiveNewFrontierFenceAfterDelayedEntry(t *testing.T) {
	s, db := finalBackfillFixture(t)
	stopStoreBackgroundReaders(s)
	frontier, ok := readWALReclaimFrontier(s.dbPath)
	require.True(t, ok)
	_, err := checkpointWALOnceOn(t.Context(), db, "TRUNCATE")
	require.NoError(t, err)
	s.writeMu.Lock()
	_, err = s.writerDB.ExecContext(t.Context(), `UPDATE wal_churn SET payload = 'new-WAL-delayed' WHERE id = 1`)
	s.writeMu.Unlock()
	require.NoError(t, err)
	var calls atomic.Int64
	oldObserver := walCheckpointCallObserver
	walCheckpointCallObserver = func(mode string, _ time.Time, _ time.Duration) {
		if mode == "PASSIVE" {
			calls.Add(1)
		}
	}
	t.Cleanup(func() { walCheckpointCallObserver = oldObserver })
	oldHook := walIdleResetHook
	walIdleResetHook = func() { time.Sleep(75 * time.Millisecond) }
	t.Cleanup(func() { walIdleResetHook = oldHook })
	synctest.Test(t, func(t *testing.T) {
		r := &walReclaimResult{urgent: true, slowTail: &walReclaimSlowTail{salt: frontier.salt, covered: frontier.backfill, copyElapsed: 300 * time.Millisecond}}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		beforeIO := vfsIOMark()
		done, err := s.reclaimWALResetHoldOnce(ctx, walReclaimConfig{}, db, r, false, true, r.takeAdaptiveWriterBudget())
		t.Logf("done=%t err=%v adaptive_used=%t PASSIVE_calls=%d main_write_bytes=%d spent=%s", done, err, r.adaptiveUsed, calls.Load(), vfsIOMark().Since(beforeIO).OtherMainWriteBytes, r.writerSpent)
		require.False(t, done)
		require.ErrorIs(t, err, errWALReclaimReadersInFlight)
		require.False(t, r.adaptiveUsed)
		require.Zero(t, r.adaptiveBudget)
		require.Zero(t, r.adaptiveHold)
		require.Nil(t, r.resetReaders)
		require.Zero(t, calls.Load())
		require.Zero(t, vfsIOMark().Since(beforeIO).OtherMainWriteBytes)
		require.Equal(t, 75*time.Millisecond, r.writerSpent, "elapsed hold accounting still includes the admission pause")
		require.True(t, s.writeMu.TryLock(), "stale frontier must release the actual writer")
		s.writeMu.Unlock()
	})
}
