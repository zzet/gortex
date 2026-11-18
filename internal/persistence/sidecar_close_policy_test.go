package persistence

import (
	"database/sql"
	"database/sql/driver"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/sqliteconn"
)

func requireSidecarNoCloseCheckpoint(t *testing.T, conn *sql.Conn) {
	t.Helper()
	require.NoError(t, conn.Raw(func(dc any) error {
		mode, err := sqliteconn.NoCloseCheckpointMode(dc.(driver.Conn), -1)
		require.Equal(t, int32(1), mode)
		return err
	}))
}

func TestSidecarNoCloseCheckpointCoversPhysicalConnectionsAndReplacement(t *testing.T) {
	sc, err := OpenSidecar(filepath.Join(t.TempDir(), "sidecar.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sc.Close()) })
	sc.db.SetMaxOpenConns(3)
	sc.db.SetMaxIdleConns(3)
	var conns []*sql.Conn
	for range 3 {
		conn, err := sc.db.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		requireSidecarNoCloseCheckpoint(t, conn)
		conns = append(conns, conn)
	}
	require.Equal(t, 3, sc.db.Stats().OpenConnections)
	// Clearing the old connection's policy before discarding it proves a new
	// admission restores the setting, independently of allocator pointer reuse.
	require.NoError(t, conns[0].Raw(func(dc any) error {
		mode, err := sqliteconn.NoCloseCheckpointMode(dc.(driver.Conn), 0)
		require.Zero(t, mode)
		return err
	}))
	require.ErrorIs(t, conns[0].Raw(func(any) error { return driver.ErrBadConn }), driver.ErrBadConn)
	_ = conns[0].Close() // ErrBadConn already discarded this connection.
	for _, conn := range conns[1:] {
		require.NoError(t, conn.Close())
	}
	sc.db.SetMaxIdleConns(0)
	sc.db.SetMaxIdleConns(1)
	replacement, err := sc.db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = replacement.Close() })
	requireSidecarNoCloseCheckpoint(t, replacement)
	require.NoError(t, replacement.Close())
}

func TestSidecarLastClosePreservesCommittedNotesMemoriesWithoutCheckpoint(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "no-implicit-checkpoint", false: "SQLite-default-control"}[enabled], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "sidecar.sqlite")
			sc, err := OpenSidecar(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sc.Close()) })
			sc.db.SetMaxOpenConns(1)
			sc.db.SetMaxIdleConns(1)
			_, err = sc.db.Exec(`PRAGMA wal_autocheckpoint=0`)
			require.NoError(t, err)
			var busy, frames, checkpointed int
			require.NoError(t, sc.db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed))
			require.Zero(t, busy)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			note := NoteEntry{ID: "retained-note", Body: strings.Repeat("note", 8192), Tags: []string{"retained"}, AutoLinks: []string{"symbol"}}
			memory := MemoryEntry{ID: "retained-memory", Body: strings.Repeat("memory", 8192), Title: "Retained", Kind: "decision", SymbolIDs: []string{"symbol"}, FilePaths: []string{"file.go"}, AutoLinks: []string{"related"}, Tags: []string{"retained"}}
			require.NoError(t, sc.UpsertNote("repo", note))
			require.NoError(t, sc.UpsertMemory("repo", memory))
			wal, err := os.ReadFile(path + "-wal")
			require.NoError(t, err)
			require.Greater(t, len(wal), 32)
			if !enabled {
				conn, err := sc.db.Conn(t.Context())
				require.NoError(t, err)
				require.NoError(t, conn.Raw(func(dc any) error { _, err := sqliteconn.NoCloseCheckpointMode(dc.(driver.Conn), 0); return err }))
				require.NoError(t, conn.Close())
			}
			require.NoError(t, CloseSidecar(path))
			require.Zero(t, sc.db.Stats().OpenConnections, "physical handles must actually close")
			var ignored int
			require.Error(t, sc.db.QueryRow(`SELECT 1`).Scan(&ignored), "closed owner remained usable")
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			if enabled {
				require.Equal(t, before, after, "last close secretly copied committed WAL frames")
				preserved, err := os.ReadFile(path + "-wal")
				require.NoError(t, err)
				require.Equal(t, wal, preserved, "last close changed/deleted committed WAL")
			} else {
				require.NotEqual(t, before, after, "control did not exercise SQLite's ordinary last-close checkpoint")
			}
			reopened, err := OpenSidecar(path)
			require.NoError(t, err)
			require.NotSame(t, sc, reopened, "closed owner remained in shared cache")
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			notes, err := reopened.LoadNotesRows("repo")
			require.NoError(t, err)
			require.Len(t, notes, 1)
			require.Equal(t, note, notes[0])
			memories, err := reopened.LoadMemoriesRows("repo")
			require.NoError(t, err)
			require.Len(t, memories, 1)
			require.Equal(t, memory, memories[0])
			require.NoError(t, CloseSidecar(path))
			require.NoError(t, CloseSidecar(path))
			require.Zero(t, reopened.db.Stats().OpenConnections)
			// Windows refuses this when an actual SQLite handle remains open; POSIX
			// also checks pool closure above rather than treating unlink as proof.
			require.NoError(t, os.RemoveAll(dir))
		})
	}
}
