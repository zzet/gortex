package sqliteconn

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPageCacheBytesFollowsConnections: the pinned driver builds SQLite with
// its allocator statistics off, so the page-cache counter is the one memory
// figure SQLite still keeps. Writing 8 MiB through a connection whose cache
// can hold it must raise the process-wide figure by most of that, and closing
// the connection must give it back.
func TestPageCacheBytesFollowsConnections(t *testing.T) {
	const written = 8 << 20
	before, _ := PageCacheBytes()

	db, err := Open(filepath.Join(t.TempDir(), "cache.db"))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	ctx := t.Context()
	_, err = db.ExecContext(ctx, `PRAGMA cache_size=-16384; CREATE TABLE t(v BLOB)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 2048)
		INSERT INTO t SELECT randomblob(4096) FROM n`)
	require.NoError(t, err)

	cached, peak := PageCacheBytes()
	require.GreaterOrEqual(t, cached-before, int64(written/2), "page cache before %d, after writing %d bytes %d", before, written, cached)
	require.GreaterOrEqual(t, peak, cached)

	require.NoError(t, db.Close())
	closed, _ := PageCacheBytes()
	require.LessOrEqual(t, closed, cached-written/2, "closing the only connection kept its page cache: %d -> %d", cached, closed)
}
