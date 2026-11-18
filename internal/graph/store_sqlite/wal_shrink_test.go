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

// writerJournalLimit reads journal_size_limit on the store's active write
// connection (the writer, or the pinned connection of a bulk window).
func writerJournalLimit(t *testing.T, s *Store) int64 {
	t.Helper()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	conn, release, err := s.activeWriteConnLocked(t.Context())
	require.NoError(t, err)
	defer release()
	var limit int64
	require.NoError(t, conn.QueryRowContext(t.Context(), `PRAGMA journal_size_limit`).Scan(&limit))
	return limit
}

// The live reclaim's final TRUNCATE of a 57.6 GB log held the writer 37.3 s:
// an ftruncate cannot be interrupted. A big log is reset in place (the file
// keeps its size under the writer hold) and then shrunk in slices, each one
// ftruncate of at most the slice, with the writer released between them and a
// writer committing throughout. The truncation seam charges 2 ms per MiB freed
// (≈500 MB/s, slower than the live 1.5 GB/s) so a single whole-file truncate
// would hold the writer for most of a second.
func TestWALReclaimShrinksABigLogInBoundedSlices(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	// The case asks for the reset itself, so it must own the one background
	// checkpoint slot. With the reclaim loop off, the store's pressure PASSIVE
	// starts at its 5 s poll while the log is still growing under the pinned
	// reader; on a slow host that pass is still running when the reset is
	// asked for, and the reset skips (checkpoint_in_flight).
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "0")
	const sliceCap = 32 << 20
	prevInPlace, prevCap, prevTruncate := walShrinkInPlaceBytes, walShrinkSliceCap, walShrinkTruncate
	walShrinkInPlaceBytes, walShrinkSliceCap = 128<<20, sliceCap
	var mu sync.Mutex
	var freed []int64
	walShrinkTruncate = func(path string, size int64) error {
		before := walFileSize(path)
		mu.Lock()
		freed = append(freed, before-size)
		mu.Unlock()
		time.Sleep(time.Duration((before-size)>>20) * 2 * time.Millisecond)
		return prevTruncate(path, size)
	}
	t.Cleanup(func() {
		walShrinkInPlaceBytes, walShrinkSliceCap, walShrinkTruncate = prevInPlace, prevCap, prevTruncate
	})

	s, path := openWALReclaimStore(t)
	seedWALChurnTable(t, s)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()

	// A big log: a reader pinned at the start keeps the log from restarting
	// while ~1 MiB transactions land.
	pinned, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var rows int
	require.NoError(t, pinned.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&rows))
	growWAL(t, s, 320)
	require.NoError(t, pinned.Rollback())
	walPath := path + "-wal"
	sizeBefore := walFileSize(walPath)
	t.Logf("log before: %.0f MiB", float64(sizeBefore)/(1<<20))
	require.Greater(t, sizeBefore, walShrinkInPlaceBytes, "precondition: a log above the in-place size")
	require.Equal(t, int64(walShrinkKeepBytes), writerJournalLimit(t, s))

	cfg := walReclaimConfig{thresholdBytes: 16 << 20, drainDeadline: defaultWALReclaimDrainDeadline, truncateBudget: walReclaimTruncateBudget, readerWait: defaultWALReclaimReaderWait}
	res := s.reclaimWALOnce(cfg, ckpt, walPath)
	t.Logf("reset: outcome=%s reason=%q writer_hold=%s bytes_after=%.0f MiB", res.outcome, res.reason, res.writerHold, float64(walFileSize(walPath))/(1<<20))
	require.Equal(t, walReclaimReset, res.outcome, "reason=%q", res.reason)
	require.Equal(t, sizeBefore, walFileSize(walPath), "the reset must not truncate the big file under the writer hold")
	require.Empty(t, freed)
	require.Less(t, res.writerHold, walReclaimMaxWriterHold)
	snap, ok := readWALIndexSnapshot(path)
	require.True(t, ok)
	require.LessOrEqual(t, snap.MxFrame, uint32(walShrinkMaxRestartFrames), "the log restarted at its first frame")
	require.Equal(t, int64(-1), writerJournalLimit(t, s), "no later commit may truncate the big file in one call")
	require.True(t, s.walShrinkNeeded())
	// A natural restart: the marker's frames backfilled, no reader, and the
	// application writer commits. The log restarts again, and the commit must
	// not truncate the big file (journal_size_limit would, in one call).
	_, err = checkpointWALOnceOn(context.Background(), ckpt, "PASSIVE")
	require.NoError(t, err)
	require.NoError(t, churnWriteOnce(s, 0))
	require.Equal(t, sizeBefore, walFileSize(walPath), "a commit truncated the big file in one call")

	// A writer commits throughout the shrink.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var commits atomic.Int64
	var maxWait atomic.Int64
	var writeErr atomic.Value
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			if err := churnWriteOnce(s, k); err != nil {
				writeErr.Store(err)
				return
			}
			if d := int64(time.Since(start)); d > maxWait.Load() {
				maxWait.Store(d)
			}
			commits.Add(1)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	slices, shrunk, err := s.shrinkWAL(context.Background(), ckpt)
	close(stop)
	wg.Wait()
	require.NoError(t, err)
	if v := writeErr.Load(); v != nil {
		t.Fatalf("writer failed during the shrink: %v", v)
	}
	st := s.WALReclaimStats()
	t.Logf("shrink: slices=%d shrunk=%.0f MiB max_slice_hold=%s commits=%d max_commit=%s wal_after=%.0f MiB",
		slices, float64(shrunk)/(1<<20), st.ShrinkSliceHoldMax, commits.Load(), time.Duration(maxWait.Load()), float64(walFileSize(walPath))/(1<<20))
	mu.Lock()
	for i, b := range freed {
		require.LessOrEqual(t, b, int64(sliceCap), "slice %d truncated %d bytes, over the slice", i, b)
	}
	calls := len(freed)
	mu.Unlock()
	require.GreaterOrEqual(t, calls, int((sizeBefore-walShrinkKeepBytes)/sliceCap), "the file came down in slices")
	require.LessOrEqual(t, walFileSize(walPath), int64(walShrinkKeepBytes)+int64(sliceCap), "the file is back near journal_size_limit")
	require.Less(t, st.ShrinkSliceHoldMax, 400*time.Millisecond, "one slice held the writer too long")
	require.Positive(t, commits.Load())
	require.False(t, s.walShrinkNeeded())
	require.Equal(t, int64(walShrinkKeepBytes), writerJournalLimit(t, s), "the writer's journal_size_limit is restored")
	require.Equal(t, int64(1), st.ShrinkInPlaceResets)

	// Nothing was lost: the database is intact and every committed row
	// survives a close and reopen.
	var check string
	require.NoError(t, s.db.QueryRow(`PRAGMA quick_check`).Scan(&check))
	require.Equal(t, "ok", check)
	require.NoError(t, s.Close())
	reopened, err := openPristine(t, path)
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	var after int
	require.NoError(t, reopened.db.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&after))
	require.Equal(t, rows, after)
	require.NoError(t, reopened.db.QueryRow(`PRAGMA quick_check`).Scan(&check))
	require.Equal(t, "ok", check)
}

// A small log keeps the plain TRUNCATE reset: nothing is left to shrink.
func TestWALReclaimTruncatesASmallLogAtOnce(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	// This direct reset fixture owns the checkpoint attempt; a pressure-triggered
	// background PASSIVE must not acquire its slot while the log is seeded.
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	pinned, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var rows int
	require.NoError(t, pinned.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&rows))
	growWAL(t, s, 40)
	require.NoError(t, pinned.Rollback())
	cfg := walReclaimConfig{thresholdBytes: 16 << 20, drainDeadline: defaultWALReclaimDrainDeadline, truncateBudget: walReclaimTruncateBudget, readerWait: defaultWALReclaimReaderWait}
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	require.Equal(t, walReclaimReset, res.outcome, "reason=%q", res.reason)
	require.Zero(t, walFileSize(path+"-wal"))
	require.False(t, s.walShrinkNeeded())
	require.Equal(t, int64(walShrinkKeepBytes), writerJournalLimit(t, s))
}

// Once a statement's interrupt fired during the call, the driver returns the
// context's error even when SQLite finished the work, so an in-place reset can
// complete just after its deadline. The reset connection's observer runs the
// reset's statements on a context of its own and then ends the reset's
// context, as the hold's deadline or a queued writer would. The reset reads
// from the log, with the writer still held, whether it happened.
//
//   - marker: the RESTART and the marker commit completed; the log restarted,
//     so the reset counts.
//   - restart: only the RESTART completed; the log is fully copied but has not
//     restarted, so the reset gave up. The next commit restarts it, and with
//     the writer's journal_size_limit back at 64 MiB that commit would
//     truncate the file in one call: the limit stays lifted, also through an
//     earlier reset's shrink that finishes first, and the shrink restores it
//     once the log has restarted.
//   - refused: no interrupt; a reader refuses the RESTART of the copied log, so
//     the limit stays lifted as above. A commit lands behind the reader, and a
//     second reset gives up on a log no longer fully copied: the limit still
//     stays lifted, since the kept log has yet to restart.
func TestWALInPlaceResetInterruptedAfterItsWork(t *testing.T) {
	for _, kind := range []string{"marker", "restart", "refused"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
			prevInPlace := walShrinkInPlaceBytes
			walShrinkInPlaceBytes = 1 // every log is reset in place
			var s *Store
			var armed atomic.Bool
			var cancel context.CancelFunc
			var workErr error
			prevObserver := walResetCheckpointConnObserver
			walResetCheckpointConnObserver = func(_ context.Context, conn *sql.Conn) {
				if !armed.CompareAndSwap(true, false) {
					return
				}
				if kind == "marker" {
					var result walCheckpointResult
					workErr = s.restartWALInPlaceOn(context.Background(), conn, &result)
				} else {
					_, workErr = checkpointWALOnceOn(context.Background(), conn, "RESTART")
				}
				cancel()
			}
			t.Cleanup(func() { // after the store closes
				walShrinkInPlaceBytes, walResetCheckpointConnObserver = prevInPlace, prevObserver
			})
			var path string
			s, path = openWALReclaimStore(t)
			t.Cleanup(func() { _ = s.Close() })
			seedWALChurnTable(t, s)
			growWAL(t, s, 4)
			if kind == "refused" {
				// Begun before the copy, the reader holds a read mark at the
				// log's end: the copy completes, and the RESTART waits for it.
				reader, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
				require.NoError(t, err)
				defer func() { _ = reader.Rollback() }()
				var n int
				require.NoError(t, reader.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
			}
			ckpt, err := openWALReclaimCheckpointDB(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = ckpt.Close() })
			_, err = checkpointWALOnceOn(t.Context(), ckpt, "PASSIVE")
			require.NoError(t, err)
			walPath := path + "-wal"
			sizeBefore := walFileSize(walPath)
			resetsBefore := s.WALReclaimStats().ShrinkInPlaceResets

			if kind == "refused" {
				// The pressure reset's RESTART does not wait: Busy at once.
				s.writeMu.Lock()
				_, err = s.resetWALForPressure(t.Context())
				s.writeMu.Unlock()
				require.ErrorIs(t, err, errSQLiteCheckpointIncomplete)
				require.NotNil(t, s.walLimitKept.Load(), "the log is fully copied: it is kept")
				require.Equal(t, int64(-1), writerJournalLimit(t, s))
				require.NoError(t, churnWriteOnce(s, 0))
				s.writeMu.Lock()
				_, err = s.resetWALForPressure(t.Context())
				s.writeMu.Unlock()
				require.ErrorIs(t, err, errSQLiteCheckpointIncomplete)
				after, ok := readWALReclaimFrontier(path)
				require.True(t, ok)
				require.Less(t, after.backfill, after.mx, "precondition: the log is no longer fully copied")
				require.Equal(t, int64(-1), writerJournalLimit(t, s), "the kept log's restart must not truncate the file in one call")
				return
			}

			var ctx context.Context
			ctx, cancel = context.WithCancel(t.Context())
			armed.Store(true)
			s.writeMu.Lock()
			_, err = s.resetWALForReclaim(ctx)
			s.writeMu.Unlock()
			cancel()
			require.NoError(t, workErr)
			require.False(t, armed.Load(), "precondition: the observer ran the reset's statements")

			if kind == "marker" {
				require.NoError(t, err, "the log restarted: the reset counts")
				require.True(t, s.walShrinkPending.Load())
				require.Equal(t, resetsBefore+1, s.WALReclaimStats().ShrinkInPlaceResets)
				require.Equal(t, int64(-1), writerJournalLimit(t, s), "no later commit may truncate the file in one call")
				require.Equal(t, sizeBefore, walFileSize(walPath), "the file is left to the shrink")
				return
			}
			require.ErrorIs(t, err, context.Canceled)
			require.False(t, s.walShrinkPending.Load())
			require.Equal(t, resetsBefore, s.WALReclaimStats().ShrinkInPlaceResets)
			require.Equal(t, int64(-1), writerJournalLimit(t, s), "the commit that restarts the log must not truncate the file in one call")
			// An earlier reset's shrink finishing now cannot cut the kept
			// log's live frames, and must leave the limit to its restart.
			s.walShrinkPending.Store(true)
			_, _, err = s.shrinkWAL(t.Context(), ckpt)
			require.NoError(t, err)
			require.Equal(t, int64(-1), writerJournalLimit(t, s), "the kept log has not restarted")
			require.False(t, s.walShrinkNeeded(), "the log has not restarted yet")
			require.NoError(t, churnWriteOnce(s, 0))
			require.True(t, s.walShrinkNeeded(), "the log restarted: the shrink runs")
			_, _, err = s.shrinkWAL(t.Context(), ckpt)
			require.NoError(t, err)
			require.Equal(t, int64(walShrinkKeepBytes), writerJournalLimit(t, s))
			require.False(t, s.walShrinkNeeded())
		})
	}
}

// SQLite applies journal_size_limit when it prepares the PRAGMA, before the
// driver looks at the context. Inside a bulk window the pinned connection is
// used without a pool checkout that would refuse an ended context first, so a
// reset tried after its deadline lifted the bulk connection's limit to -1,
// returned the context's error, and left no shrink to restore the limit.
func TestWALInPlaceResetOnAnEndedContextLeavesTheLimit(t *testing.T) {
	// Every log is reset in place; restored after the store closes.
	prevInPlace := walShrinkInPlaceBytes
	walShrinkInPlaceBytes = 1
	t.Cleanup(func() { walShrinkInPlaceBytes = prevInPlace })
	s, _ := finalBackfillFixture(t)
	engaged, err := s.BeginGenerationBulkLoad(77)
	require.NoError(t, err)
	require.True(t, engaged)
	defer func() { require.NoError(t, s.EndGenerationBulkLoadFor(77)) }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.writeMu.Lock()
	_, err = s.resetWALForReclaim(ctx)
	s.writeMu.Unlock()
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, int64(walShrinkKeepBytes), writerJournalLimit(t, s), "a reset on an ended context changes no limit")
}
