package store_sqlite

import (
	"database/sql"
	"database/sql/driver"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/sqliteconn"
	sqlite "modernc.org/sqlite"
)

func requireNoCloseCheckpoint(t *testing.T, conn *sql.Conn) {
	t.Helper()
	require.NoError(t, conn.Raw(func(dc any) error {
		if gated, ok := dc.(*gatedConn); ok {
			dc = gated.inner
		}
		value, err := sqliteconn.NoCloseCheckpointMode(dc.(driver.Conn), -1)
		if err != nil {
			return err
		}
		require.Equal(t, int32(1), value)
		return nil
	}))
	var registered int
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT gortex_outline_via_is(?, ?)`, `{"via":"spring.Bean"}`, "spring.Bean").Scan(&registered))
	require.Equal(t, 1, registered, "connector lost the default driver's registered function")
}

func TestSQLiteNoCloseCheckpointCoversPhysicalPoolsAndReplacements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	s.stopCheckpointLoop()
	nilGate, err := openSQLiteReadPool(path, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, nilGate.Close()) })
	passive, err := openSQLiteStoreDB(sqliteCheckpointDSN(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, passive.Close()) })
	reclaim, err := openWALReclaimCheckpointDB(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reclaim.Close()) })
	configureWriterPool(passive)
	inner, err := sqlite.NewConnector(sqliteWriterDSN(path))
	require.NoError(t, err)
	require.Same(t, inner.Driver(), s.writerDB.Driver())
	for name, db := range map[string]*sql.DB{"writer": s.writerDB, "readers": s.db, "nil-gate-readers": nilGate, "passive": passive, "reclaim": reclaim} {
		t.Run(name, func(t *testing.T) {
			count := 1
			if name == "readers" || name == "nil-gate-readers" {
				count = db.Stats().MaxOpenConnections
			}
			var conns []*sql.Conn
			for range count {
				conn, err := db.Conn(t.Context())
				require.NoError(t, err)
				t.Cleanup(func() { _ = conn.Close() })
				conns = append(conns, conn)
				requireNoCloseCheckpoint(t, conn)
			}
			for _, conn := range conns {
				require.NoError(t, conn.Close())
			}
			// Deliberately clear the flag on the old connection. A value of 1
			// afterwards proves setup ran on a replacement, without pointer-ID
			// assumptions (allocators can reuse a freed address).
			conn, err := db.Conn(t.Context())
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			require.NoError(t, conn.Raw(func(dc any) error {
				if gated, ok := dc.(*gatedConn); ok {
					dc = gated.inner
				}
				value, err := sqliteconn.NoCloseCheckpointMode(dc.(driver.Conn), 0)
				require.Equal(t, int32(0), value)
				return err
			}))
			err = conn.Raw(func(any) error { return driver.ErrBadConn })
			require.ErrorIs(t, err, driver.ErrBadConn)
			_ = conn.Close()
			// Discard the other idle read connections too, as the planner-stats
			// refresh does; none of them may mask the replacement being tested.
			db.SetMaxIdleConns(0)
			db.SetMaxIdleConns(1)
			conn, err = db.Conn(t.Context())
			require.NoError(t, err)
			requireNoCloseCheckpoint(t, conn)
			require.NoError(t, conn.Close())
		})
	}
}

func TestSQLiteNoCloseCheckpointLastConnectionPreservesWALAndReopens(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "no-implicit-checkpoint", false: "SQLite-default-control"}[enabled], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "last.sqlite")
			db, err := openSQLiteStoreDB(sqliteWriterDSN(path))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			configureWriterPool(db)
			_, err = db.Exec(`CREATE TABLE retained(id INTEGER PRIMARY KEY, payload BLOB); PRAGMA wal_checkpoint(TRUNCATE)`)
			require.NoError(t, err)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO retained VALUES(1, zeroblob(32768))`)
			require.NoError(t, err)
			wal, err := os.ReadFile(path + "-wal")
			require.NoError(t, err)
			require.Greater(t, len(wal), 32)
			if !enabled {
				conn, err := db.Conn(t.Context())
				require.NoError(t, err)
				t.Cleanup(func() { _ = conn.Close() })
				require.NoError(t, conn.Raw(func(dc any) error { _, err := sqliteconn.NoCloseCheckpointMode(dc.(driver.Conn), 0); return err }))
				require.NoError(t, conn.Close())
			}
			require.NoError(t, db.Close()) // The sole actual physical connection.
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			if enabled {
				require.Equal(t, before, after, "last connection secretly copied WAL frames")
				preserved, err := os.ReadFile(path + "-wal")
				require.NoError(t, err)
				require.Equal(t, wal, preserved)
			} else {
				require.NotEqual(t, before, after, "control did not exercise SQLite's ordinary last-close checkpoint")
			}
			reopened, err := openSQLiteStoreDB(sqliteReaderDSN(path))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			var bytes int
			require.NoError(t, reopened.QueryRow(`SELECT length(payload) FROM retained WHERE id=1`).Scan(&bytes))
			require.Equal(t, 32768, bytes, "committed data did not survive the closed WAL/reopen boundary")
		})
	}
}

func TestSQLiteNoCloseCheckpointKeepsExplicitCloseAndRealFailureDurability(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT", "full")
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S", "")
	for _, failCheckpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit-drain", true: "checkpoint-error"}[failCheckpoint], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "close.sqlite")
			s, err := Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() })
			s.stopCheckpointLoop()
			s.stopMaintenanceLane()
			require.NoError(t, s.CheckpointWAL())
			require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "durable", Kind: graph.KindFunction, Name: "Durable"}}, nil))
			if failCheckpoint {
				require.NoError(t, s.writerDB.Close())
			}
			err = s.Close()
			if failCheckpoint {
				require.ErrorContains(t, err, "database is closed")
			} else {
				require.NoError(t, err)
				require.Zero(t, walFileSize(path+"-wal"), "explicit TRUNCATE was silently removed")
			}
			require.Error(t, s.db.Ping())
			require.Error(t, s.writerDB.Ping())
			reopened, err := openSQLiteStoreDB(sqliteReaderDSN(path))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			var name string
			require.NoError(t, reopened.QueryRow(`SELECT name FROM nodes WHERE id='durable' AND view_gen=0`).Scan(&name))
			require.Equal(t, "Durable", name)
		})
	}
}

func TestSQLiteNoCloseCheckpointMemoryAndProbeScopes(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		s, err := Open(":memory:")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		require.Same(t, s.db, s.writerDB)
		conn, err := s.db.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		requireNoCloseCheckpoint(t, conn)
		require.NoError(t, conn.Close())
		require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "memory", Kind: graph.KindFunction}}, nil))
		require.NotNil(t, s.GetNode("memory"))
	})
	t.Run("noncreating-and-nonmutating-probes", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing.sqlite")
		states, err := ReadRepoIndexStates(missing)
		require.NoError(t, err)
		require.Empty(t, states)
		require.NoError(t, checkSchemaDowngrade(missing, currentSchemaVersion))
		_, err = os.Stat(missing)
		require.True(t, os.IsNotExist(err))
		path := filepath.Join(t.TempDir(), "probe.sqlite")
		db, err := openSQLiteStoreDB(sqliteWriterDSN(path))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		_, err = db.Exec(`CREATE TABLE probe_only(x); PRAGMA wal_checkpoint(TRUNCATE); INSERT INTO probe_only VALUES(1)`)
		require.NoError(t, err)
		require.NoError(t, db.Close())
		before := schemaStoreFiles(t, path)
		states, err = ReadRepoIndexStates(path)
		require.NoError(t, err)
		require.Empty(t, states)
		require.NoError(t, checkSchemaDowngrade(path, currentSchemaVersion))
		after := schemaStoreFiles(t, path)
		require.Equal(t, before[""], after[""])
		require.Equal(t, before["-wal"], after["-wal"])
	})
}
