package store_sqlite

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The cost of building symbol_fts_rowid_by_generation (view_gen, fts_rowid)
// over an existing sidecar, per 100,000 rows: its time and the WAL bytes it
// writes. Set A1_INDEX_COST=1 to run it (a measurement): the sidecar grows by
// 100,000 rows at a time to 500,000, and after each step the index is dropped,
// the WAL truncated, and the index built again on one writer connection with
// the automatic checkpoint off, so the WAL holds exactly what the build wrote.
func TestSymbolFTSRowidGenerationIndexBuildCost(t *testing.T) {
	if os.Getenv("A1_INDEX_COST") != "1" {
		t.Skip("set A1_INDEX_COST=1")
	}
	ctx := context.Background()
	store := openPayloadStore(t)
	conn, err := store.writerDB.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	walSize := func() int64 {
		fi, err := os.Stat(store.dbPath + "-wal")
		if err != nil {
			return 0
		}
		return fi.Size()
	}
	const step = 100_000
	for total := step; total <= 5*step; total += step {
		_, err := conn.ExecContext(ctx, `WITH RECURSIVE c(x) AS (SELECT ? UNION ALL SELECT x + 1 FROM c WHERE x < ?)
INSERT INTO symbol_fts_rowid (view_gen, node_id, repo_prefix, fts_rowid)
SELECT 1 + (x % 97), 'repo/internal/pkg' || (x % 311) || '/file' || (x % 1009) || '.go::Symbol' || x, 'repo', 1000000 + x FROM c`, total-step+1, total)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `DROP INDEX IF EXISTS symbol_fts_rowid_by_generation`)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `PRAGMA wal_autocheckpoint=0`)
		require.NoError(t, err)
		before := walSize()
		started := time.Now()
		_, err = conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS symbol_fts_rowid_by_generation ON symbol_fts_rowid(view_gen, fts_rowid)`)
		require.NoError(t, err)
		elapsed := time.Since(started)
		wal := walSize() - before
		_, err = conn.ExecContext(ctx, `PRAGMA wal_autocheckpoint=1000`)
		require.NoError(t, err)
		t.Logf("sidecar %7d rows: index build %8.1f ms (%.1f ms per 100k), WAL %9d bytes (%.1f MB per 100k)",
			total, float64(elapsed.Microseconds())/1000, float64(elapsed.Microseconds())/1000*step/float64(total),
			wal, float64(wal)/1e6*step/float64(total))
	}
}
