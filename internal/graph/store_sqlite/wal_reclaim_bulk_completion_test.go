package store_sqlite

import (
	"context"
	"database/sql"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Admission, reader turnover and cancellation run through the real checkpoint
// lease policy; setting a result flag alone would not prove the bulk boundary.
func TestWALBulkCompletionAdmissionAndReaderTurnover(t *testing.T) {
	for _, kind := range []string{"below_ceiling", "busy_lane", "reader_turnover", "lane_cancellation"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			engaged, err := s.BeginGenerationBulkLoad(77)
			require.NoError(t, err)
			require.True(t, engaged)
			defer func() { require.NoError(t, s.EndGenerationBulkLoadFor(77)) }()
			reader, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			defer func() { _ = reader.Rollback() }()
			var payload string
			require.NoError(t, reader.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			s.writeMu.Lock()
			tx, err := s.beginWriteContext(t.Context())
			if err == nil {
				_, err = tx.Exec(`UPDATE wal_churn SET payload = 'bulk-after-reader' WHERE id = 1`)
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
			s.writeMu.Unlock()
			require.NoError(t, err)
			size := walFileSize(s.dbPath + "-wal")
			require.Positive(t, size)
			cfg := walReclaimConfig{thresholdBytes: size, ceilingBytes: size / 2, readerWait: time.Second, truncateBudget: walReclaimTruncateBudget}
			var busy atomic.Bool
			if kind == "busy_lane" || kind == "lane_cancellation" {
				busy.Store(kind == "busy_lane")
				s.SetBuildLaneBusy(busy.Load)
			}
			if kind == "below_ceiling" {
				cfg.ceilingBytes = size + 1
			}
			if kind == "below_ceiling" || kind == "busy_lane" {
				res := s.reclaimWALAttempt(cfg, db, s.dbPath+"-wal")
				require.Equal(t, walReclaimSkipped, res.outcome)
				require.False(t, res.bulkCompletion)
				require.False(t, res.leaseOverride)
				require.Zero(t, res.writerHold)
				require.Zero(t, s.WALReclaimStats().LastResortRuns)
				return
			}
			// This qualifying bulk WAL is below the separate four-times-ceiling mark.
			// Urgency does not affect lease admission, but keeps waiting writes from
			// abandoning the reader-turnover completion we are exercising.
			cfg.thresholdBytes = 1
			heldCopy := make(chan struct{})
			var once sync.Once
			walCheckpointCallObserver = func(mode string, _ time.Time, _ time.Duration) {
				if mode == "PASSIVE" && s.writeMu.held() && bulkCompletionHeldCaller() {
					once.Do(func() { close(heldCopy) })
				}
			}
			defer func() { walCheckpointCallObserver = nil }()
			done := make(chan walReclaimResult, 1)
			go func() { done <- s.reclaimWALAttempt(cfg, db, s.dbPath+"-wal") }()
			// Always unblock and join the owned attempt before closing the store.
			joined := false
			defer func() {
				_ = reader.Rollback()
				if !joined {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("bulk completion did not join")
					}
				}
			}()
			select {
			case <-heldCopy:
			case <-time.After(5 * time.Second):
				t.Fatal("bulk completion never reached its held copy")
			}
			require.False(t, s.readGate.closed.Load(), "bulk completion must keep admitting readers")
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			var count int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM wal_churn`).Scan(&count))
			require.Positive(t, count)
			if kind == "lane_cancellation" {
				busy.Store(true)
			} else {
				require.NoError(t, reader.Rollback())
			}
			var res walReclaimResult
			select {
			case res = <-done:
				joined = true
			case <-time.After(5 * time.Second):
				t.Fatal("bulk completion did not finish")
			}
			require.True(t, res.bulkCompletion)
			require.True(t, res.leaseOverride)
			require.EqualValues(t, 1, s.WALReclaimStats().LeaseOverrides)
			require.False(t, res.lastResort)
			require.Zero(t, s.WALReclaimStats().LastResortRuns, "bulk completion is not a four-times-ceiling event")
			require.False(t, res.pauseClosed)
			require.Zero(t, res.pause)
			require.Less(t, res.writerHold, walReclaimMaxWriterHold+100*time.Millisecond)
			require.True(t, s.writeMu.TryLock(), "completion retained the application writer")
			s.writeMu.Unlock()
			if kind == "lane_cancellation" {
				require.Equal(t, walReclaimSkipped, res.outcome)
				require.Equal(t, "build_lane_busy", res.reason)
				require.False(t, res.openGate)
				require.Less(t, res.writerHold, time.Second)
			} else {
				require.Equal(t, walReclaimReset, res.outcome, res.reason)
				require.True(t, res.openGate)
				require.True(t, walLogIsReset(s.dbPath, s.dbPath+"-wal"))
			}
			_ = reader.Rollback()
			require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			require.Equal(t, "bulk-after-reader", payload)
		})
	}
}

// A pinned bulk connection without an admitted lease override still refuses
// the writer step; it cannot borrow the new completion policy.
func TestWALBulkCompletionRequiresLeaseOverride(t *testing.T) {
	s, db := finalBackfillFixture(t)
	engaged, err := s.BeginGenerationBulkLoad(77)
	require.NoError(t, err)
	require.True(t, engaged)
	defer func() { require.NoError(t, s.EndGenerationBulkLoadFor(77)) }()
	res := &walReclaimResult{urgent: true}
	_, err = s.reclaimWALResetHold(context.Background(), walReclaimConfig{}, db, res, false, false)
	require.ErrorIs(t, err, errWALCheckpointDeferredBulk)
	require.False(t, res.bulkCompletion)
	require.True(t, s.writeMu.TryLock())
	s.writeMu.Unlock()
}

func TestWALBulkCompletionBoundsPinnedReaderAndParentCancellation(t *testing.T) {
	for _, kind := range []string{"pinned_reader", "parent_cancel", "ordinary"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			reader, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			defer func() { _ = reader.Rollback() }()
			var payload string
			require.NoError(t, reader.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			s.writeMu.Lock()
			_, err = s.writerDB.Exec(`UPDATE wal_churn SET payload = 'pinned-tail' WHERE id = 1`)
			s.writeMu.Unlock()
			require.NoError(t, err)
			cfg := walReclaimConfig{thresholdBytes: 1, readerWait: 100 * time.Millisecond, truncateBudget: walReclaimTruncateBudget}
			res := &walReclaimResult{urgent: true, bulkCompletion: kind != "ordinary", leaseOverride: kind != "ordinary"}
			entered := make(chan struct{})
			var once sync.Once
			walCheckpointCallObserver = func(mode string, _ time.Time, _ time.Duration) {
				if mode == "PASSIVE" {
					once.Do(func() { close(entered) })
				}
			}
			defer func() { walCheckpointCallObserver = nil }()
			ctx, cancel := context.WithCancel(t.Context())
			if kind == "ordinary" {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
			}
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- s.reclaimWALInLane(ctx, cfg, db, res) }()
			joined := false
			defer func() {
				cancel()
				_ = reader.Rollback()
				if !joined {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("pinned-reader attempt did not join")
					}
				}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("attempt never reached checkpoint")
			}
			if kind == "parent_cancel" {
				cancel()
			}
			select {
			case err = <-done:
				joined = true
			case <-time.After(5 * time.Second):
				t.Fatal("pinned-reader attempt exceeded completion cap")
			}
			require.Error(t, err)
			require.False(t, res.openGate)
			require.False(t, res.pauseClosed)
			require.Zero(t, res.pause)
			require.False(t, s.readGate.closed.Load())
			require.Zero(t, s.WALReclaimStats().LastResortRuns)
			limit := walReclaimMaxWriterHold + 100*time.Millisecond
			if kind == "ordinary" {
				limit = walReclaimResetHold + 100*time.Millisecond
			}
			if kind == "parent_cancel" {
				limit = time.Second
			}
			require.Less(t, res.writerHold, limit)
			if kind == "pinned_reader" {
				require.Greater(t, res.writerHold, walReclaimResetHold, "bulk completion never waited for the pinned reader")
			}
			require.True(t, s.writeMu.TryLock(), "refused reset retained writer")
			s.writeMu.Unlock()
			require.NoError(t, reader.Rollback())
			require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			require.Equal(t, "pinned-tail", payload)
		})
	}
}

// The physical write gate may also be owned by maintenance. Witness the
// current callback's completion stack, rather than merely any holder.
func bulkCompletionHeldCaller() bool {
	var pcs [16]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, ".reclaimWALInLane") {
			return true
		}
		if !more {
			return false
		}
	}
}
