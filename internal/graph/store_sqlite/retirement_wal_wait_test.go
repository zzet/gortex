package store_sqlite

import (
	"context"
	"database/sql"
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
	resets, joinReclaimer := quietWindowReclaimer(t, store, walPath, 150*time.Millisecond, stop)
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
