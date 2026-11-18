package store_sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The next chunk's rows follow the last chunk's time toward the 100 ms target,
// within [16, 1000] rows and at most 4x per step; a short tail chunk says
// nothing about the rate.
func TestRetirementChunkSizeFollowsTheMeasuredTime(t *testing.T) {
	cases := []struct {
		limit   int
		removed int64
		elapsed time.Duration
		want    int
	}{
		{1000, 1000, 2 * time.Second, 250},      // 50 rows would fit; 4x step bound
		{250, 250, 2 * time.Second, 62},         // again bounded
		{62, 62, 500 * time.Millisecond, 16},    // 12 would fit; floor
		{128, 128, 10 * time.Millisecond, 512},  // fast: grows, 4x bound
		{512, 512, 25 * time.Millisecond, 1000}, // ceiling
		{128, 40, 10 * time.Millisecond, 128},   // tail: unchanged
		{128, 40, 300 * time.Millisecond, 64},   // slow tail: halves
		{200, 200, 100 * time.Millisecond, 200}, // on target
	}
	for _, c := range cases {
		require.Equal(t, c.want, nextSweepBatch(c.limit, c.removed, c.elapsed), "%+v", c)
	}
}

// deletePayloadChunks hands each chunk the learned size and learns from its
// time: chunks that take 5 ms per row shrink until one chunk fits the target.
func TestRetirementChunksShrinkToTheTarget(t *testing.T) {
	store, generationID, handle := beginManifestGeneration(t)
	_ = handle
	ctx := context.Background()
	require.NoError(t, store.PublishPayloadGeneration(ctx, generationID, 9000))
	_, err := store.writerDB.Exec(`UPDATE view_generations SET state = ? WHERE generation_id = ?`, string(ViewGenerationRetiring), generationID)
	require.NoError(t, err)
	store.sweepBatch.Store(1000)
	var limits []int
	var holds []time.Duration
	remaining := 3000
	chunk := func(ctx context.Context, _ *sql.Tx) (int64, error) {
		limit := sweepBatchFrom(ctx)
		limits = append(limits, limit)
		n := min(limit, remaining)
		start := time.Now()
		time.Sleep(time.Duration(n) * 5 * time.Millisecond / 10) // 0.5 ms per row
		holds = append(holds, time.Since(start))
		remaining -= n
		return int64(n), nil
	}
	require.NoError(t, store.deletePayloadChunks(ctx, generationID, chunk, nil))
	t.Logf("limits %v", limits)
	require.Equal(t, 1000, limits[0])
	require.Less(t, limits[len(limits)-2], 1000, "the chunk never shrank")
	last := holds[len(holds)-2]
	require.Less(t, last, 3*payloadSweepChunkTarget, "the chunk did not converge on the target: %v", holds)
	require.True(t, store.WriteWanted() == false)
}

// A chunk deletes at most the WAL budget's worth of rows at the learned bytes
// per row: chunks that each append ~1.5 KB of log per row shrink toward the
// 2 MiB budget's ~1,400 rows even when they are fast.
func TestRetirementChunksAreBoundedByTheWALTheyWrite(t *testing.T) {
	store, generationID, handle := beginManifestGeneration(t)
	_ = handle
	ctx := context.Background()
	require.NoError(t, store.PublishPayloadGeneration(ctx, generationID, 9000))
	_, err := store.writerDB.Exec(`UPDATE view_generations SET state = ? WHERE generation_id = ?`, string(ViewGenerationRetiring), generationID)
	require.NoError(t, err)
	_, err = store.writerDB.Exec(`CREATE TABLE wal_rows (v BLOB)`)
	require.NoError(t, err)
	prevBudget := payloadSweepChunkWALBudget
	payloadSweepChunkWALBudget = 256 << 10 // 256 KiB: ~64 rows of 4 KiB each
	t.Cleanup(func() { payloadSweepChunkWALBudget = prevBudget })
	store.sweepBatch.Store(1000)
	var limits []int
	remaining := 2000
	chunk := func(ctx context.Context, tx *sql.Tx) (int64, error) {
		limit := sweepBatchFrom(ctx)
		limits = append(limits, limit)
		n := min(limit, remaining)
		// Each "row" appends about one page of log, fast.
		for i := 0; i < n; i++ {
			if _, err := tx.ExecContext(ctx, `INSERT INTO wal_rows VALUES (randomblob(3500))`); err != nil {
				return 0, err
			}
		}
		remaining -= n
		return int64(n), nil
	}
	require.NoError(t, store.deletePayloadChunks(ctx, generationID, chunk, nil))
	t.Logf("limits %v", limits)
	require.Equal(t, 1000, limits[0])
	require.Greater(t, len(limits), 3)
	require.LessOrEqual(t, limits[2], 128, "the chunk did not shrink to the WAL budget")
	require.GreaterOrEqual(t, limits[2], payloadSweepMinBatch)
}
