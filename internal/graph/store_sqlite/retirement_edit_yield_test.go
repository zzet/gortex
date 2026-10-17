package store_sqlite

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// editWaits runs `edits` edit-path mutations beside a running retirement:
// each announces itself (AnnounceWrite), then commits `writes` separate
// transactions through the write gate — the receipt row, the delta payload,
// the publish — and records how long it waited for the gate in total.
func editWaits(t *testing.T, s *Store, edits, writes int, gap time.Duration) []time.Duration {
	t.Helper()
	var out []time.Duration
	for e := 0; e < edits; e++ {
		time.Sleep(gap)
		release := s.AnnounceWrite()
		var waited time.Duration
		for w := 0; w < writes; w++ {
			start := time.Now()
			s.writeMu.Lock()
			waited += time.Since(start)
			_, err := s.writerDB.Exec(`INSERT INTO edit_rows(v) VALUES (?)`, e*writes+w)
			s.writeMu.Unlock()
			require.NoError(t, err)
		}
		release()
		out = append(out, waited)
	}
	return out
}

func percentile(ds []time.Duration, p float64) time.Duration {
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[int(float64(len(sorted)-1)*p)]
}

// An edit's writes take precedence over retirement chunks: a chunk boundary
// that finds an announced edit mutation waits for it, so a three-write edit
// waits at most for the one chunk already in flight when it arrived — not for
// a chunk before each of its writes — and the retirement still finishes.
func TestRetirementYieldsTheWriteGateToEditWriters(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)
	mustExec(t, store, `CREATE TABLE edit_rows(id INTEGER PRIMARY KEY, v INTEGER)`)
	_, err := store.writerDB.Exec(`UPDATE view_generations SET state = ? WHERE generation_id = ?`, string(ViewGenerationRetiring), generationID)
	require.NoError(t, err)

	const chunkCost = 60 * time.Millisecond
	chunks := 0
	var mu sync.Mutex
	chunk := func(ctx context.Context, tx *sql.Tx) (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		if chunks >= 60 {
			return 0, nil
		}
		chunks++
		time.Sleep(chunkCost) // a chunk's delete, holding the write gate
		_, err := tx.ExecContext(ctx, `UPDATE view_generations SET state = state WHERE generation_id = ?`, generationID)
		return 1, err
	}
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- store.deletePayloadChunks(context.Background(), generationID, chunk, nil) }()
	waits := editWaits(t, store, 8, 3, 170*time.Millisecond)
	require.NoError(t, <-done)
	st := store.WALReclaimStats()
	t.Logf("edit waits (3 writes each) beside %s retirement chunks: p50=%s max=%s all=%v; retirement %d chunks in %s, yields=%d timeouts=%d",
		chunkCost, percentile(waits, 0.5), percentile(waits, 1), waits, chunks, time.Since(started).Round(time.Millisecond),
		st.RetirementEditYields, st.RetirementEditYieldTimeouts)
	require.Equal(t, 60, chunks, "retirement must finish")
	require.Positive(t, st.RetirementEditYields)
	require.Less(t, percentile(waits, 1), chunkCost+chunkCost/2, "an edit waited for more than the chunk in flight")
}
