package store_sqlite

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// quietWindowReclaimer stands in for the live reclaim's failure mode: it
// resets the log only after the -wal file has not grown for a quiet window
// (the live attempts' capped backfill failed whenever writes kept landing).
// It never takes the writer while one is in use, like the real reclaim.
func quietWindowReclaimer(t *testing.T, s *Store, walPath string, quiet time.Duration, stop <-chan struct{}) (resets *atomic.Int64, done func()) {
	t.Helper()
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	resets = &atomic.Int64{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer ckpt.Close()
		lastSize, lastChange := walFileSize(walPath), time.Now()
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			size := walFileSize(walPath)
			if size != lastSize {
				lastSize, lastChange = size, time.Now()
				continue
			}
			if size == 0 || time.Since(lastChange) < quiet {
				continue
			}
			s.writeMu.Lock()
			_, err := checkpointWALOnceOn(context.Background(), ckpt, "TRUNCATE")
			s.writeMu.Unlock()
			if err == nil {
				resets.Add(1)
			}
			lastSize, lastChange = walFileSize(walPath), time.Now()
		}
	}()
	return resets, wg.Wait
}

// A retirement sweep whose chunks follow each other back to back leaves the
// reclaim no quiet window, so the WAL grows for as long as the sweep runs.
// With the wait, a chunk that finds the log over the ceiling nudges the
// reclaim and pauses (holding nothing) until the log is back under it, so the
// log stays near the ceiling; a wait that runs out proceeds anyway, and the
// episode is logged once.
func TestRetirementWaitsForTheReclaimOverTheWALCeiling(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0") // the stand-in reclaimer below
	logs := captureReclaimLog(t)
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)
	mustExec(t, store, `CREATE TABLE retire_churn(id INTEGER PRIMARY KEY, payload BLOB)`)
	mustExec(t, store, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 2000)
		INSERT INTO retire_churn(id, payload) SELECT i, randomblob(1024) FROM n`)
	_, err := store.writerDB.Exec(`UPDATE view_generations SET state = ? WHERE generation_id = ?`, string(ViewGenerationRetiring), generationID)
	require.NoError(t, err)

	const ceiling = 12 << 20
	store.walReclaim.update(func(st *WALReclaimStats) {
		st.ThresholdBytes = 4 << 20
		st.CeilingBytes = ceiling
	})
	prevMax, prevPoll := retirementWALWaitMax, retirementWALWaitPoll
	retirementWALWaitMax, retirementWALWaitPoll = 5*time.Second, 10*time.Millisecond
	t.Cleanup(func() { retirementWALWaitMax, retirementWALWaitPoll = prevMax, prevPoll })

	walPath := store.dbPath + "-wal"
	stop := make(chan struct{})
	quiet := 150 * time.Millisecond
	if raceDetectorOn {
		quiet = 1500 * time.Millisecond // chunks run several times slower
	}
	resets, joinReclaimer := quietWindowReclaimer(t, store, walPath, quiet, stop)
	var maxWAL atomic.Int64
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			if size := walFileSize(walPath); size > maxWAL.Load() {
				maxWAL.Store(size)
			}
		}
	}()

	// A chunk rewrites a quarter of the churn table (about 0.5 MiB of WAL)
	// and "removes" one row until 120 chunks have run: 60 MiB of log with
	// no reset in between if nothing pauses the sweep.
	chunks := 0
	chunk := func(ctx context.Context, tx *sql.Tx) (int64, error) {
		if chunks >= 120 {
			return 0, nil
		}
		chunks++
		_, err := tx.ExecContext(ctx, `UPDATE retire_churn SET payload = randomblob(1024) WHERE id % 4 = ?`, chunks%4)
		return 1, err
	}
	started := time.Now()
	require.NoError(t, store.deletePayloadChunks(context.Background(), generationID, chunk, nil))
	elapsed := time.Since(started)
	close(stop)
	joinReclaimer()
	sampler.Wait()

	st := store.WALReclaimStats()
	out := logs.String()
	t.Logf("retirement: chunks=%d elapsed=%s wal_max=%.1fMiB ceiling=%dMiB reclaimer_resets=%d waits=%d timeouts=%d",
		chunks, elapsed.Round(time.Millisecond), float64(maxWAL.Load())/(1<<20), ceiling>>20, resets.Load(), st.RetirementWaits, st.RetirementWaitTimeouts)
	require.Equal(t, 120, chunks, "retirement must always finish")
	require.Positive(t, st.RetirementWaits)
	require.Positive(t, resets.Load(), "the waits must give the reclaim its window")
	require.LessOrEqual(t, maxWAL.Load(), int64(ceiling+4<<20), "the WAL passed the ceiling under retirement")
	require.Contains(t, out, "retirement waiting for the wal reclaim")
	require.LessOrEqual(t, strings.Count(out, "retirement waiting for the wal reclaim"), int(st.RetirementWaits), "one line per episode, not per poll")
}

// A wait that runs out proceeds: a reclaim that never resets delays each
// chunk by the bound and the sweep still finishes.
func TestRetirementWaitProceedsWhenTheReclaimCannotReset(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)
	_, err := store.writerDB.Exec(`UPDATE view_generations SET state = ? WHERE generation_id = ?`, string(ViewGenerationRetiring), generationID)
	require.NoError(t, err)
	store.walReclaim.update(func(st *WALReclaimStats) { st.CeilingBytes = 1 }) // always over
	prevMax := retirementWALWaitMax
	retirementWALWaitMax = 50 * time.Millisecond
	t.Cleanup(func() { retirementWALWaitMax = prevMax })

	chunks := 0
	chunk := func(ctx context.Context, tx *sql.Tx) (int64, error) {
		if chunks >= 3 {
			return 0, nil
		}
		chunks++
		_, err := tx.ExecContext(ctx, `UPDATE view_generations SET state = state WHERE generation_id = ?`, generationID)
		return 1, err
	}
	started := time.Now()
	require.NoError(t, store.deletePayloadChunks(context.Background(), generationID, chunk, nil))
	st := store.WALReclaimStats()
	require.Equal(t, 3, chunks)
	require.Equal(t, int64(4), st.RetirementWaitTimeouts, "every chunk (and the closing empty one) waited out the bound")
	require.GreaterOrEqual(t, time.Since(started), 4*50*time.Millisecond)
}

// The sweep waits at the reclaim threshold, not at four times it: a 4 MiB
// mark under a 32 MiB ceiling holds a chunk once the log is at 20 MiB.
func TestRetirementWaitsAtTheReclaimThreshold(t *testing.T) {
	require.Equal(t, int64(256<<20), retirementWALMark(256<<20, 2<<30))
	require.Equal(t, int64(3<<20), retirementWALMark(4<<20, 3<<20), "the ceiling when it is lower")
	require.Equal(t, int64(2<<30), retirementWALMark(0, 2<<30), "no threshold: the ceiling")

	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0") // no reclaim to bring the log down
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 20)
	size := walFileSize(path + "-wal")
	require.Greater(t, size, int64(16<<20), "precondition: the log is over the mark")
	require.Less(t, size, int64(32<<20), "precondition: the log is under the ceiling")
	s.walReclaim.update(func(st *WALReclaimStats) { st.ThresholdBytes, st.CeilingBytes = 4<<20, 32<<20 })
	prev := retirementWALWaitMax
	retirementWALWaitMax = 150 * time.Millisecond
	t.Cleanup(func() { retirementWALWaitMax = prev })

	var episode retirementWALEpisode
	started := time.Now()
	s.awaitWALUnderCeiling(context.Background(), 7, &episode)
	require.GreaterOrEqual(t, time.Since(started), 150*time.Millisecond, "the chunk did not wait")
	require.Equal(t, int64(1), s.WALReclaimStats().RetirementWaits)
	require.True(t, episode.active)
}

// With the daemon's marks (threshold 256 MiB, ceiling 2 GiB): a sweep that is
// running when the log passes 256 MiB stops at its next chunk, so the log it
// leaves is the mark plus at most one chunk; the sweep then waits and still
// finishes. The reclaim loop is off (named override), so nothing resets the
// log underneath, and the wait is shortened (named override: 100 ms, 30 s in
// production) so the test does not sit in it.
func TestARunningRetirementStopsAtTheMarkWithTheDaemonDefaults(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	logs := captureReclaimLog(t)
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)
	mustExec(t, store, `CREATE TABLE retire_churn(id INTEGER PRIMARY KEY, payload BLOB)`)
	mustExec(t, store, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 2000)
		INSERT INTO retire_churn(id, payload) SELECT i, randomblob(1024) FROM n`)
	_, err := store.writerDB.Exec(`UPDATE view_generations SET state = ? WHERE generation_id = ?`, string(ViewGenerationRetiring), generationID)
	require.NoError(t, err)
	// The daemon's defaults, installed by hand: the loop is off (above) so
	// that no reset ends a wait under the test, and a disabled reclaim
	// resolves to no threshold and no ceiling (under which retirement does
	// not wait: no reclaim would come).
	cfg := walReclaimConfig{thresholdBytes: defaultWALReclaimThresholdBytes, ceilingBytes: walReclaimCeiling(defaultWALReclaimThresholdBytes, 0)}
	store.walReclaim.update(func(st *WALReclaimStats) { st.ThresholdBytes, st.CeilingBytes = cfg.thresholdBytes, cfg.ceilingBytes })
	mark := retirementWALMark(cfg.thresholdBytes, cfg.ceilingBytes)
	require.Equal(t, int64(256<<20), mark)
	prevMax := retirementWALWaitMax
	retirementWALWaitMax = 100 * time.Millisecond
	t.Cleanup(func() { retirementWALWaitMax = prevMax })

	// The log to just under the mark, then a sweep whose chunks each add
	// about 0.5 MiB: it crosses the mark in its middle.
	for i := 0; store.retirementLogBytes() < mark-8<<20; i++ {
		mustExec(t, store, `UPDATE retire_churn SET payload = randomblob(1024) WHERE id % 2 = `+strconv.Itoa(i%2))
	}
	var firstWaitAt int64
	chunks, perChunk, prevBefore := 0, int64(0), int64(0)
	chunk := func(ctx context.Context, tx *sql.Tx) (int64, error) {
		if chunks >= 40 {
			return 0, nil
		}
		// A chunk's frames reach the log at its commit, after this callback:
		// the log a chunk adds is the growth from one chunk's start to the
		// next's (nothing resets it: the loop is off).
		before := store.retirementLogBytes()
		if chunks > 0 {
			perChunk = max(perChunk, before-prevBefore)
		}
		prevBefore = before
		if firstWaitAt == 0 && store.WALReclaimStats().RetirementWaits > 0 {
			firstWaitAt = before
		}
		chunks++
		_, err := tx.ExecContext(ctx, `UPDATE retire_churn SET payload = randomblob(1024) WHERE id % 4 = ?`, chunks%4)
		return 1, err
	}
	require.NoError(t, store.deletePayloadChunks(context.Background(), generationID, chunk, nil))
	st := store.WALReclaimStats()
	t.Logf("sweep: chunks=%d waits=%d log_at_first_wait=%.1fMiB mark=%dMiB largest_chunk=%.2fMiB",
		chunks, st.RetirementWaits, float64(firstWaitAt)/(1<<20), mark>>20, float64(perChunk)/(1<<20))
	require.Equal(t, 40, chunks, "retirement must always finish")
	require.Positive(t, st.RetirementWaits, "the sweep never stopped at the mark")
	require.Contains(t, logs.String(), "retirement waiting for the wal reclaim")
	require.LessOrEqual(t, firstWaitAt, mark+2*perChunk+(1<<20), "the sweep ran past the mark by more than a chunk")
}
