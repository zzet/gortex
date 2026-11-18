package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && v > 0 {
		return v
	}
	return def
}

// TestWALReclaimBoundsTheLogUnderAContinuousWriteStream is the crash window's
// workload at test scale. A large store (default 1.5 GB) so that backfill
// copies land on scattered pages and run at the live store's copy rate (10–33
// MB/s measured here against 21 MB/s for the live close) rather than the
// hundreds of MB/s a small cached file allows; one writer producing
// generation rows and rewriting scattered pages at a steady WAL rate (default
// 7 MB/s); a retirement deleting older generations in 1000-row chunks back to
// back; generation bulk windows (default 40 s of every 60 s) that hold the
// checkpoint lease the way the live builds' bulk loads did; CPU-bound
// goroutines standing in for the builds' compute (default 2); rotating
// readers holding 100–400 ms and one 3 s read every 15 s; the reclaim at the daemon's own cadence (5 s poll, 5 s initial
// backoff). Run with GOMAXPROCS=1:
//
//	GOMAXPROCS=1 GORTEX_TEST_WAL_STREAM_SECONDS=300 go test -run TestWALReclaimBoundsTheLogUnderAContinuousWriteStream ./internal/graph/store_sqlite/
//
// The log must reset at least once in every full minute and never exceed
// twice its ceiling. Knobs: _MB_S (write rate), _THRESHOLD_MB, _CEILING_MB,
// _DIR (store directory). Skipped unless _SECONDS is set: it is a
// multi-minute run.
func TestWALReclaimBoundsTheLogUnderAContinuousWriteStream(t *testing.T) {
	seconds := envInt("GORTEX_TEST_WAL_STREAM_SECONDS", 0)
	if seconds == 0 {
		t.Skip("GORTEX_TEST_WAL_STREAM_SECONDS not set (multi-minute run)")
	}
	rateMB := envInt("GORTEX_TEST_WAL_STREAM_MB_S", 7)
	thresholdMB := envInt("GORTEX_TEST_WAL_STREAM_THRESHOLD_MB", 32)
	ceilingMB := envInt("GORTEX_TEST_WAL_STREAM_CEILING_MB", 8*thresholdMB)
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", strconv.Itoa(thresholdMB))
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", strconv.Itoa(ceilingMB))
	prevFloor := walReclaimCeilingFloor
	walReclaimCeilingFloor = 0
	t.Cleanup(func() { walReclaimCeilingFloor = prevFloor })
	logs := captureReclaimLog(t)

	dir := strings.TrimSpace(os.Getenv("GORTEX_TEST_WAL_STREAM_DIR"))
	if dir == "" {
		dir = t.TempDir()
	} else {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	path := filepath.Join(dir, fmt.Sprintf("wal-stream-%d.sqlite", time.Now().UnixNano()))
	t.Cleanup(func() {
		for _, sfx := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(path + sfx)
		}
	})
	s, err := openPristine(t, path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	_, err = s.writerDB.Exec(`CREATE TABLE gen_rows(rid INTEGER PRIMARY KEY, view_gen INTEGER NOT NULL, id INTEGER NOT NULL, payload BLOB)`)
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`CREATE INDEX gen_rows_by_gen ON gen_rows(view_gen, id)`)
	require.NoError(t, err)
	// The large table the scattered rewrites land on: one ~3 KB row per page.
	bigMB := envInt("GORTEX_TEST_WAL_STREAM_DB_MB", 1500)
	bigRows := bigMB << 20 / 4096
	_, err = s.writerDB.Exec(`CREATE TABLE big(id INTEGER PRIMARY KEY, payload BLOB)`)
	require.NoError(t, err)
	built := time.Now()
	for done := 0; done < bigRows; done += 10000 {
		_, err = s.writerDB.Exec(`INSERT INTO big(payload) SELECT randomblob(3000) FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 10000) SELECT x FROM c)`)
		require.NoError(t, err)
		_, err = s.writerDB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		require.NoError(t, err)
	}
	t.Logf("setup: db=%.0fMiB rows=%d in %s", float64(walFileSize(path))/(1<<20), bigRows, time.Since(built).Round(time.Second))
	bulkEvery := time.Duration(envInt("GORTEX_TEST_WAL_STREAM_BULK_EVERY_S", 60)) * time.Second
	bulkHold := time.Duration(envInt("GORTEX_TEST_WAL_STREAM_BULK_HOLD_S", 40)) * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	var failed atomic.Value
	write := func(fn func(tx *sql.Tx) error) error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		tx, err := s.beginWriteContext(ctx)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	var gen atomic.Int64
	var appended atomic.Int64 // WAL bytes appended, across resets
	start := time.Now()

	wg.Add(1)
	go func() { // generation builder, paced to the target WAL rate
		defer wg.Done()
		for ctx.Err() == nil {
			g := gen.Add(1)
			for batch := 0; batch < 8 && ctx.Err() == nil; batch++ {
				for ctx.Err() == nil && float64(appended.Load()) > float64(rateMB<<20)*time.Since(start).Seconds() {
					time.Sleep(5 * time.Millisecond)
				}
				err := write(func(tx *sql.Tx) error {
					if _, err := tx.Exec(`INSERT INTO gen_rows(view_gen, id, payload)
						WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 1000)
						SELECT ?, ? * 1000 + x, randomblob(300 + abs(random()) % 400) FROM c`, g, batch); err != nil {
						return err
					}
					_, err := tx.Exec(`UPDATE big SET payload = randomblob(3000) WHERE id IN (
						SELECT 1 + abs(random()) % ? FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 200) SELECT x FROM c))`, bigRows)
					return err
				})
				if err != nil && ctx.Err() == nil {
					failed.Store(err)
					cancel()
					return
				}
			}
		}
	}()
	wg.Add(1)
	go func() { // retirement: 1000-row chunks back to back, generations older than current-3
		defer wg.Done()
		const q = `DELETE FROM gen_rows WHERE view_gen = ? AND id IN (
    SELECT id FROM gen_rows WHERE view_gen > 0 AND view_gen = ? LIMIT ?)`
		for ctx.Err() == nil {
			var oldest int64
			if err := s.db.QueryRowContext(ctx, `SELECT coalesce(min(view_gen), 0) FROM gen_rows`).Scan(&oldest); err != nil {
				if ctx.Err() == nil {
					failed.Store(err)
					cancel()
				}
				return
			}
			if oldest == 0 || oldest >= gen.Load()-3 {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			for ctx.Err() == nil {
				var removed int64
				err := write(func(tx *sql.Tx) error {
					res, err := tx.Exec(q, oldest, oldest, 1000)
					if err != nil {
						return err
					}
					removed, err = res.RowsAffected()
					return err
				})
				if err != nil {
					if ctx.Err() == nil {
						failed.Store(err)
						cancel()
					}
					return
				}
				if removed == 0 {
					break
				}
			}
		}
	}()
	// Rotating readers, each holding its read transaction 100–400 ms (the
	// daemon's queries).
	readerMinMS := envInt("GORTEX_TEST_WAL_STREAM_READER_MIN_MS", 100)
	readerSpanMS := envInt("GORTEX_TEST_WAL_STREAM_READER_SPAN_MS", 300)
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func(seed int64) { // rotating readers
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for ctx.Err() == nil {
				tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
				if err != nil {
					return
				}
				var n int64
				_ = tx.QueryRow(`SELECT count(*) FROM gen_rows WHERE view_gen = ?`, gen.Load()-int64(rng.Intn(3))).Scan(&n)
				select {
				case <-ctx.Done():
				case <-time.After(time.Duration(readerMinMS+rng.Intn(readerSpanMS)) * time.Millisecond):
				}
				_ = tx.Rollback()
			}
		}(int64(r))
	}
	// The builds' compute: at GOMAXPROCS=1 parsing and resolving take the
	// processor from the writer and from every checkpoint copy, as they did
	// in the crash window.
	burners := envInt("GORTEX_TEST_WAL_STREAM_CPU_BURNERS", 2)
	var burned atomic.Uint64
	for b := 0; b < burners; b++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x := uint64(b + 1)
			for ctx.Err() == nil {
				for i := 0; i < 1<<16; i++ {
					x ^= x << 13
					x ^= x >> 7
					x ^= x << 17
				}
				burned.Add(x & 1)
			}
		}()
	}
	// An occasional long read (an analysis query): attempts that meet it
	// fail on the hold cap after making progress, which must not push the
	// next attempt minutes away.
	longEvery := time.Duration(envInt("GORTEX_TEST_WAL_STREAM_LONG_READ_EVERY_S", 15)) * time.Second
	longHold := time.Duration(envInt("GORTEX_TEST_WAL_STREAM_LONG_READ_MS", 3000)) * time.Millisecond
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(longEvery - longHold):
			}
			tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				return
			}
			var n int64
			_ = tx.QueryRow(`SELECT count(*) FROM gen_rows`).Scan(&n)
			select {
			case <-ctx.Done():
			case <-time.After(longHold):
			}
			_ = tx.Rollback()
		}
	}()
	var bulkWindows atomic.Int64
	wg.Add(1)
	go func() { // generation bulk windows: real ones, holding the checkpoint lease; the writes inside go through the window's pinned connection
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(bulkEvery - bulkHold):
			}
			id := 1000 + bulkWindows.Load()
			engaged, err := s.BeginGenerationBulkLoad(id)
			if err != nil || !engaged {
				continue
			}
			bulkWindows.Add(1)
			select {
			case <-ctx.Done():
			case <-time.After(bulkHold):
			}
			if err := s.EndGenerationBulkLoadFor(id); err != nil && ctx.Err() == nil {
				failed.Store(err)
				cancel()
				return
			}
		}
	}()
	var maxWAL atomic.Int64
	var resetsMu sync.Mutex
	var resets []time.Duration
	wg.Add(1)
	go func() { // sampler: WAL size, appended bytes, resets (mxFrame moving backwards)
		defer wg.Done()
		walPath := path + "-wal"
		var lastMx uint32
		for ctx.Err() == nil {
			if size := walFileSize(walPath); size > maxWAL.Load() {
				maxWAL.Store(size)
				if size > int64(3*ceilingMB)<<20 {
					// Far past the bound: stop before the log eats the disk.
					cancel()
				}
			}
			if mark := readWALWriteMark(path); mark.Valid {
				frame := int64(mark.PageSize) + 24
				switch {
				case mark.MxFrame < lastMx:
					resetsMu.Lock()
					resets = append(resets, time.Since(start))
					resetsMu.Unlock()
					appended.Add(int64(mark.MxFrame) * frame)
				case mark.MxFrame > lastMx:
					appended.Add(int64(mark.MxFrame-lastMx) * frame)
				}
				lastMx = mark.MxFrame
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	nextReport := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		time.Sleep(200 * time.Millisecond)
		if time.Now().After(nextReport) {
			nextReport = time.Now().Add(30 * time.Second)
			resetsMu.Lock()
			n := len(resets)
			resetsMu.Unlock()
			t.Logf("  %s wal=%.0fMiB max=%.0fMiB resets=%d written=%.0fMiB", time.Since(start).Round(time.Second),
				float64(walFileSize(path+"-wal"))/(1<<20), float64(maxWAL.Load())/(1<<20), n, float64(appended.Load())/(1<<20))
		}
	}
	runaway := ctx.Err() != nil && time.Now().Before(deadline)
	cancel()
	wg.Wait()
	if runaway && failed.Load() == nil {
		t.Logf("stopped early: the WAL passed three times its ceiling")
	}
	if err, _ := failed.Load().(error); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	st := s.WALReclaimStats()
	out := logs.String()
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "wal reclaim") {
			lines = append(lines, line)
		}
	}
	for _, line := range lines {
		t.Log(line)
	}
	resetsMu.Lock()
	defer resetsMu.Unlock()
	rate := float64(appended.Load()) / (1 << 20) / elapsed.Seconds()
	t.Logf("stream: %s at %.1f MB/s written (target %d), threshold=%dMiB ceiling=%dMiB bulk_windows=%d wal_max=%.0fMiB resets=%d at %v; reclaim attempts=%d resets=%d deferrals=%d skips=%d writer_hold_max=%s",
		elapsed.Round(time.Second), rate, rateMB, thresholdMB, ceilingMB, bulkWindows.Load(), float64(maxWAL.Load())/(1<<20), len(resets), roundDurations(resets),
		st.Attempts, st.Resets, st.Deferrals, st.Skips, st.WriterHoldMax)

	require.LessOrEqual(t, maxWAL.Load(), int64(2*ceilingMB)<<20, "the WAL passed twice its ceiling")
	// The mechanism, not only the outcome: the writer step never ran out of
	// time copying (the passes before it converged), and no attempt was ever
	// scheduled more than the pressure cap away.
	require.NotContains(t, out, "backfill_failed error=context deadline exceeded", "a writer step ran out of time copying the backlog")
	for _, line := range lines {
		if i := strings.Index(line, "next_attempt_in="); i >= 0 {
			field := strings.Fields(line[i+len("next_attempt_in="):])[0]
			if d, err := time.ParseDuration(field); err == nil {
				require.LessOrEqualf(t, d, walReclaimPressureBackoffMax, "an attempt was scheduled %s away: %s", d, line)
			}
		}
	}
	for minute := 0; minute < int(elapsed/time.Minute); minute++ {
		lo, hi := time.Duration(minute)*time.Minute, time.Duration(minute+1)*time.Minute
		found := false
		for _, at := range resets {
			if at >= lo && at < hi {
				found = true
				break
			}
		}
		require.Truef(t, found, "no reset in minute %d (resets at %v)", minute+1, roundDurations(resets))
	}
	require.Greater(t, rate, 0.5*float64(rateMB), "the stream did not reach half its target rate; the run proves nothing")
}

func roundDurations(ds []time.Duration) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Round(time.Second).String()
	}
	return out
}

// A deferral that advanced the backfill retries after the initial backoff
// and restores it; only a failure that moved nothing doubles, and the
// loop's schedule never waits more than the pressure cap.
func TestWALReclaimBackoffDoublesOnlyWithoutProgress(t *testing.T) {
	now := time.Now()
	sched := newWALReclaimSchedule(5*time.Second, min(walReclaimBackoffMax, walReclaimPressureBackoffMax))
	require.Equal(t, 20*time.Second, sched.max, "the loop's cap while the log is over its threshold")
	sched.observeProgress(now, walReclaimDeferred, false)
	sched.observeProgress(now, walReclaimDeferred, false)
	require.Equal(t, 20*time.Second, sched.backoff, "no progress doubles")
	sched.observeProgress(now, walReclaimDeferred, true)
	require.Equal(t, now.Add(5*time.Second), sched.nextAt, "progress retries after the initial backoff")
	require.Equal(t, 5*time.Second, sched.backoff, "progress restores the initial backoff")
	for i := 0; i < 10; i++ {
		sched.observeProgress(now, walReclaimFailed, false)
	}
	require.LessOrEqual(t, sched.nextAt.Sub(now), 20*time.Second, "no attempt is ever more than the cap away")
}

// The writer-free passes stop once the remainder is small (here: a backlog
// with no writes behind it, done in one pass), and stop at once when a
// reader's mark lets no pass copy anything.
func TestWALReclaimConvergenceStopsWhenTheRemainderFitsOrNothingMoves(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, _ := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()

	// A reader pinned before the writes: no pass can copy past its mark.
	pin, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var n int
	require.NoError(t, pin.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	_, _ = checkpointWALOnceOn(context.Background(), ckpt, "PASSIVE")
	growWAL(t, s, 16)
	pinned := s.convergeBackfill(context.Background(), ckpt)
	t.Logf("pinned: %s", pinned)
	require.Equal(t, "no_progress", pinned.stop)
	require.Equal(t, 1, pinned.passes)

	require.NoError(t, pin.Rollback())
	free := s.convergeBackfill(context.Background(), ckpt)
	t.Logf("free: %s", free)
	require.Contains(t, []string{"small", "fits"}, free.stop)
	require.Positive(t, free.copiedFrames)
	require.LessOrEqual(t, free.passes, 2)
}
