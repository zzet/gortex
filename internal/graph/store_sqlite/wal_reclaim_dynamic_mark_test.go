package store_sqlite

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// One real pressure attempt begins below the existing 48 MiB completion mark.
// A pinned WAL reader keeps its writer-free round open while checked writes
// cross that mark; the same attempt must admit the existing bounded completion.
func TestWALAttemptPromotesWhenItsLogCrossesLastResortMark(t *testing.T) {
	previousQueryer := walReclaimCreditPassiveQueryerObserver
	var target atomic.Pointer[Store]
	waiting, heldCopy := make(chan struct{}), make(chan struct{})
	var waitOnce, heldOnce sync.Once
	var heldEntered, heldDeadline time.Time
	var heldContextError error
	walReclaimCreditPassiveQueryerObserver = func(ctx context.Context, store *Store, db *sql.DB) (walCheckpointQueryer, func()) {
		queryer, cleanup := walCheckpointQueryer(db), func() {}
		if previousQueryer != nil {
			queryer, cleanup = previousQueryer(ctx, store, db)
		}
		return dynamicMarkEntryQueryer{queryer: queryer, before: func(callCtx context.Context) {
			if store == target.Load() && store.writeMu.held() && store.WALReclaimStats().LastResortRuns > 0 {
				heldOnce.Do(func() {
					heldEntered = time.Now()
					heldDeadline, _ = callCtx.Deadline()
					heldContextError = callCtx.Err()
					close(heldCopy)
				})
			}
		}}, cleanup
	}
	t.Cleanup(func() { walReclaimCreditPassiveQueryerObserver = previousQueryer })
	s, db := finalBackfillFixture(t)
	target.Store(s)
	// Seed close to the mark before the bounded attempt starts. Only a few
	// checked commits after its reader wait are needed to cross it.
	for writes := 0; walFileSize(s.dbPath+"-wal") < 42<<20 && writes < 64; writes++ {
		require.NoError(t, churnWriteOnce(s, writes))
	}
	cfg := walReclaimConfig{thresholdBytes: 16 << 20, ceilingBytes: 12 << 20, readerWait: time.Second, truncateBudget: walReclaimTruncateBudget}
	path := s.dbPath + "-wal"
	pin, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = pin.Rollback() }()
	var count int
	require.NoError(t, pin.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&count))
	growWAL(t, s, 2)
	entryBytes := walFileSize(path)
	require.GreaterOrEqual(t, entryBytes, walPressureMark(cfg))
	require.Less(t, entryBytes, walReclaimLastResortBytes(cfg))
	oldRate := walHoldCopyRate.Swap(0)
	defer walHoldCopyRate.Store(oldRate)
	previousRound := walReclaimRoundObserver
	walReclaimRoundObserver = func(stage string, _ uint64, older int, _ error) {
		if older > 0 && (stage == "lane_post_copy_full_wait_start" || stage == "partial_wait_start") {
			waitOnce.Do(func() { close(waiting) })
		}
	}

	done := make(chan walReclaimResult, 1)
	var owned *backgroundCheckpointAttempt
	joined := false
	defer func() {
		if owned != nil {
			owned.cancel(context.Canceled)
		}
		_ = pin.Rollback()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("owned crossing attempt did not join")
			}
		}
		walReclaimRoundObserver = previousRound
	}()
	go func() { done <- s.reclaimWALAttempt(cfg, db, path) }()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("first attempt never reached its pinned reader round")
	}
	s.backgroundCheckpoint.mu.Lock()
	owned = s.backgroundCheckpoint.active
	s.backgroundCheckpoint.mu.Unlock()
	require.NotNil(t, owned)
	require.Zero(t, s.WALReclaimStats().LastResortRuns)
	// This is actual producer work, not a fabricated size/frontier observation.
	s.writeMu.Lock()
	_, err = s.writerDB.Exec(`UPDATE wal_churn SET payload='crossed-mark' WHERE id=1`)
	s.writeMu.Unlock()
	require.NoError(t, err)
	producerStartedAt := time.Now()
	writes := 0
	for walFileSize(path) < walReclaimLastResortBytes(cfg) && writes < 64 {
		require.NoError(t, churnWriteOnce(s, 2*writes))
		writes++
	}
	grownBytes := walFileSize(path)
	require.GreaterOrEqual(t, grownBytes, walReclaimLastResortBytes(cfg))
	crossingAt := time.Now()
	t.Logf("reader_wait_observed=true entry_bytes=%d grown_bytes=%d checked_writes=%d producer_duration=%s crossing_at=%s", entryBytes, grownBytes, writes+1, time.Since(producerStartedAt), crossingAt.Format(time.RFC3339Nano))
	select {
	case <-heldCopy:
	case result := <-done:
		joined = true
		t.Fatalf("attempt ended before held entry: outcome=%s reason=%s%s", result.outcome.String(), result.reason, result.stampSuffix())
	case <-time.After(time.Second):
		t.Fatalf("same attempt did not admit a held completion after crossing the existing mark; attempt_ctx_error=%v cause=%v", owned.ctx.Err(), context.Cause(owned.ctx))
	}
	t.Logf("held_entry_at=%s held_SQL_deadline=%s held_context_error=%v entry_after_crossing=%s remaining_SQL_context_budget=%s", heldEntered.Format(time.RFC3339Nano), heldDeadline.Format(time.RFC3339Nano), heldContextError, heldEntered.Sub(crossingAt), time.Until(heldDeadline))
	require.NoError(t, heldContextError)
	require.False(t, s.readGate.closed.Load())
	require.NoError(t, pin.Rollback())
	var result walReclaimResult
	select {
	case result = <-done:
		joined = true
	case <-time.After(5 * time.Second):
		t.Fatal("promoted attempt did not finish")
	}
	t.Logf("entry_bytes=%d grown_bytes=%d checked_writes=%d completion_after_crossing=%s pressure=%t last_resort=%t outcome=%s reason=%s aggregate_writer_hold=%s", entryBytes, grownBytes, writes+1, time.Since(crossingAt), result.pressure, result.lastResort, result.outcome.String(), result.reason, result.writerSpent)
	require.True(t, result.pressure)
	require.True(t, result.lastResort)
	require.Equal(t, walReclaimReset, result.outcome, result.reason)
	require.EqualValues(t, 1, s.WALReclaimStats().LastResortRuns)
	require.True(t, walLogIsReset(s.dbPath, path))
	require.LessOrEqual(t, result.writerSpent, walReclaimMaxWriterHold+100*time.Millisecond)
	require.False(t, result.pauseClosed)
	var payload string
	require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id=1`).Scan(&payload))
	require.Equal(t, "crossed-mark", payload)
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&count))
	require.Equal(t, 4000, count)
}

// These are the promotion's held-admission fences, not permission to renew the
// ordinary/adaptive writer budget or borrow another WAL's qualification.
func TestWALPromotedCompletionKeepsCreditAndAdmissionFences(t *testing.T) {
	for _, kind := range []string{"spent", "adaptive", "below_mark", "new_wal", "lane", "cancelled", "remaining_credit"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			current, ok := readWALReclaimFrontier(s.dbPath)
			require.True(t, ok)
			size := walFileSize(s.dbPath + "-wal")
			cfg := walReclaimConfig{thresholdBytes: 1, ceilingBytes: size / 4, readerWait: time.Second, truncateBudget: walReclaimTruncateBudget}
			res := &walReclaimResult{urgent: true, lastResort: true, lastResortPromotion: &current}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			switch kind {
			case "spent":
				res.writerSpent = walReclaimMaxWriterHold
			case "adaptive":
				res.adaptiveUsed = true
			case "below_mark":
				cfg.ceilingBytes = size
			case "new_wal":
				_, err := checkpointWALOnceOn(ctx, db, "TRUNCATE")
				require.NoError(t, err)
				for writes := 0; walFileSize(s.dbPath+"-wal") < walReclaimLastResortBytes(cfg) && writes < 64; writes++ {
					require.NoError(t, churnWriteOnce(s, writes))
				}
				require.GreaterOrEqual(t, walFileSize(s.dbPath+"-wal"), walReclaimLastResortBytes(cfg))
				replacement, known := readWALReclaimFrontier(s.dbPath)
				require.True(t, known)
				require.NotEqual(t, current.salt, replacement.salt)
			case "lane":
				lane := &fakeBuildLane{}
				lane.install(s)
				lane.held.Store(true)
				defer lane.held.Store(false)
			case "cancelled":
				cancel()
			case "remaining_credit":
				pin, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
				require.NoError(t, err)
				defer func() { _ = pin.Rollback() }()
				var payload string
				require.NoError(t, pin.QueryRow(`SELECT payload FROM wal_churn WHERE id=1`).Scan(&payload))
				s.writeMu.Lock()
				_, err = s.writerDB.Exec(`UPDATE wal_churn SET payload='remaining-credit-tail' WHERE id=1`)
				s.writeMu.Unlock()
				require.NoError(t, err)
				res.writerSpent = walReclaimMaxWriterHold - 200*time.Millisecond
			}
			spent := res.writerSpent
			err := s.reclaimWALInLane(ctx, cfg, db, res)
			t.Logf("kind=%s error=%v reason=%s prior_hold=%s aggregate_hold=%s", kind, err, res.reason, spent, res.writerSpent)
			require.Error(t, err)
			require.False(t, res.openGate)
			require.False(t, s.readGate.closed.Load())
			require.LessOrEqual(t, res.writerSpent, walReclaimMaxWriterHold+100*time.Millisecond)
			if kind == "remaining_credit" {
				require.Greater(t, res.writerSpent, spent)
				require.LessOrEqual(t, res.writerHold, 300*time.Millisecond)
			} else {
				require.Less(t, res.writerSpent-spent, walReclaimResetHold)
			}
			if kind == "spent" {
				require.Equal(t, "last_resort_credit_spent", res.reason)
			}
			if kind == "adaptive" {
				require.Equal(t, "adaptive_completion_deferred", res.reason)
			}
			if kind == "new_wal" || kind == "below_mark" {
				require.Equal(t, "last_resort_frontier_changed", res.reason)
			}
			if kind == "lane" {
				require.Equal(t, "build_lane_busy", res.reason)
			}
			require.True(t, s.writeMu.TryLock())
			s.writeMu.Unlock()
		})
	}
}

// Observe the actual query dispatch boundary, before SQL, rather than the
// post-return checkpoint observer. The real query and its context are unchanged.
type dynamicMarkEntryQueryer struct {
	queryer walCheckpointQueryer
	before  func(context.Context)
}

func (q dynamicMarkEntryQueryer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.before(ctx)
	return q.queryer.QueryRowContext(ctx, query, args...)
}
