package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// An announced edit owns all three of its transactions before retirement
// starts the next chunk. Channels order the in-flight chunk and announcement;
// no measured latency or synthetic chunk duration decides correctness.
func TestRetirementYieldsTheWriteGateToEditWriters(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)
	mustExec(t, store, `CREATE TABLE edit_rows(id INTEGER PRIMARY KEY, v INTEGER)`)
	_, err := store.writerDB.Exec(`UPDATE view_generations SET state = ? WHERE generation_id = ?`, string(ViewGenerationRetiring), generationID)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	firstStarted, releaseFirst := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	var wg sync.WaitGroup
	defer func() { cancel(); unpark(); wg.Wait() }()
	var committed atomic.Int32
	var eventsMu sync.Mutex
	var events []string
	record := func(event string) {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
	}
	chunks := 0
	chunk := func(ctx context.Context, tx *sql.Tx) (int64, error) {
		chunks++
		switch chunks {
		case 1:
			record("first chunk")
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		case 2:
			if committed.Load() != 3 {
				return 0, fmt.Errorf("retirement resumed before all edit transactions committed")
			}
			record("second chunk")
		default:
			return 0, nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE view_generations SET state = state WHERE generation_id = ?`, generationID)
		return 1, err
	}
	done := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		done <- store.deletePayloadChunks(ctx, generationID, chunk, nil)
	}()
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatal("retirement did not enter its first chunk")
	}
	release := store.AnnounceWrite()
	defer release()
	unpark()
	// Observe the real yield before starting the edit's three transactions.
	// This timeout bounds a broken test; it is not an elapsed-time assertion.
	require.Eventually(t, func() bool { return store.WALReclaimStats().RetirementEditYields > 0 }, 30*time.Second, time.Millisecond)
	for i := 0; i < 3; i++ {
		require.NoError(t, store.writeMu.LockContext(ctx))
		_, err := store.writerDB.ExecContext(ctx, `INSERT INTO edit_rows(v) VALUES (?)`, i)
		if err == nil {
			committed.Add(1)
			record(fmt.Sprintf("edit %d", i))
		}
		store.writeMu.Unlock()
		require.NoError(t, err)
	}
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("retirement did not resume after the edit")
	}
	wg.Wait()
	require.Equal(t, 3, chunks, "retirement must finish its two productive chunks")
	require.Equal(t, []string{"first chunk", "edit 0", "edit 1", "edit 2", "second chunk"}, events)
	var rows int
	require.NoError(t, store.db.QueryRowContext(ctx, `SELECT count(*) FROM edit_rows`).Scan(&rows))
	require.Equal(t, 3, rows)
	require.Positive(t, store.WALReclaimStats().RetirementEditYields)
}
