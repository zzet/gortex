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

// writerJournalLimit reads journal_size_limit on the store's writer connection.
func writerJournalLimit(t *testing.T, s *Store) int64 {
	t.Helper()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var limit int64
	require.NoError(t, s.writerDB.QueryRow(`PRAGMA journal_size_limit`).Scan(&limit))
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
