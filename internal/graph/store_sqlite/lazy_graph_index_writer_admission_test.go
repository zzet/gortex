package store_sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLazyIndexBuildDoesNotWaitForTheReadPoolWhileHoldingTheWriter(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "missing index"
		if existing {
			name = "existing index with unknown cache"
		}
		t.Run(name, func(t *testing.T) { testLazyIndexBuildWithPinnedReaders(t, existing) })
	}
}

func testLazyIndexBuildWithPinnedReaders(t *testing.T, existing bool) {
	t.Helper()
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	s, _ := openWALReclaimStore(t)
	t.Cleanup(func() { _ = s.Close() })
	s.stopCheckpointLoop()
	s.writeMu.Lock()
	_, err := s.writerDB.Exec(`DROP INDEX IF EXISTS ` + edgesByFileGenerationIndexName)
	s.forgetFileGenerationIndex()
	s.writeMu.Unlock()
	require.NoError(t, err)
	if existing {
		s.writeMu.Lock()
		_, err = s.writerDB.Exec(edgesByFileGenerationIndexDDL)
		s.writeMu.Unlock()
		require.NoError(t, err)
	}

	// Leave SQLite's writer free but occupy every application reader. A
	// metadata probe through s.db cannot acquire a connection until release.
	var readers []*sql.Conn
	release := func() {
		for _, conn := range readers {
			_ = conn.Close()
		}
		readers = nil
	}
	t.Cleanup(release)
	for range s.db.Stats().MaxOpenConnections {
		conn, err := s.db.Conn(t.Context())
		require.NoError(t, err)
		readers = append(readers, conn)
		var rows int
		require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT count(*) FROM edges`).Scan(&rows))
	}
	require.NotEmpty(t, readers)
	require.Equal(t, s.db.Stats().MaxOpenConnections, s.db.Stats().InUse)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.buildLazyIndexOnce(ctx) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(200 * time.Millisecond):
		release()
		<-done
		t.Fatal("the lazy index builder held the writer while waiting for an application reader")
	}
	require.Equal(t, lazyIndexPresent, s.fileGenerationIndex.Load())
	if existing {
		require.Zero(t, s.lazyIndex.builds.Load(), "an existing index must not be rebuilt")
	} else {
		require.EqualValues(t, 1, s.lazyIndex.builds.Load())
	}
	require.NoError(t, s.writeMu.LockContext(ctx))
	var present bool
	err = s.writerDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name = 'edges_by_file_generation' AND type = 'index')`).Scan(&present)
	if err == nil {
		_, err = s.writerDB.ExecContext(ctx, `CREATE TABLE lazy_index_foreground_commit (id INTEGER)`)
	}
	s.writeMu.Unlock()
	require.NoError(t, err, "the actual foreground SQL commit must work while all readers remain occupied")
	require.True(t, present, "the index must be committed in SQLite, not only cached")
}

func TestLazyIndexMetadataCancellationReleasesTheWriter(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	s, _ := openWALReclaimStore(t)
	t.Cleanup(func() { _ = s.Close() })
	s.stopCheckpointLoop()
	s.forgetFileGenerationIndex()
	// Occupy the writer connection independently of the Go gate. The
	// metadata query must honor the caller's cancellation and release both
	// admission gates without attempting an index build.
	writer, err := s.writerDB.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = writer.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.buildLazyIndexOnce(ctx), context.DeadlineExceeded)
	require.Zero(t, s.lazyIndex.builds.Load())
	require.Equal(t, lazyIndexUnknown, s.fileGenerationIndex.Load())
	require.True(t, s.writeMu.TryLock(), "cancelled metadata probe retained the writer")
	s.writeMu.Unlock()
	require.True(t, s.maintenanceGate.TryLock(), "cancelled metadata probe retained maintenance admission")
	s.maintenanceGate.Unlock()
}
