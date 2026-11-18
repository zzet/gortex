package store_sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	sqlite "modernc.org/sqlite"
)

// A canceled ExecContext may report cancellation after SQLite applied BEGIN.
// Preserve the actual driver/pool behavior but make that narrow boundary
// deterministic: execute the real BEGIN, then return its ended context. The
// driver's stmt.exec also substitutes ctx.Err after stepping the statement.
type cancelAfterManualBeginConn struct {
	driver.Conn
	cancel context.CancelFunc
	hit    *atomic.Int32
}

func (c *cancelAfterManualBeginConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err == nil && strings.EqualFold(strings.TrimSpace(query), "BEGIN IMMEDIATE") && c.hit.CompareAndSwap(0, 1) {
		c.cancel()
		return nil, ctx.Err()
	}
	return result, err
}
func (c *cancelAfterManualBeginConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}
func (c *cancelAfterManualBeginConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *cancelAfterManualBeginConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}
func (c *cancelAfterManualBeginConn) IsValid() bool {
	return c.Conn.(driver.Validator).IsValid()
}

type cancelAfterManualBeginConnector struct {
	driver.Connector
	cancel context.CancelFunc
	hit    *atomic.Int32
}

func (c cancelAfterManualBeginConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &cancelAfterManualBeginConn{Conn: conn, cancel: c.cancel, hit: c.hit}, nil
}

// Maintenance must not hand a still-open manual transaction back to the sole
// writer pool. The next resolver write uses that same physical connection;
// this was the daemon's "cannot start a transaction within a transaction".
func TestManualWALBeginCancellationLeavesWriterReusable(t *testing.T) {
	for _, name := range []string{"restart", "shrink"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
			s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			require.NoError(t, s.waitMaintenanceIdle(t.Context()))
			s.stopCheckpointLoop()
			const from, oldTo, newTo = "repo/caller.go::Caller", "repo/old.go::Target", "repo/new.go::Target"
			s.AddBatch([]*graph.Node{{ID: from, Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/caller.go", RepoPrefix: "repo"}}, []*graph.Edge{{From: from, To: oldTo, Kind: graph.EdgeCalls, FilePath: "repo/caller.go", Line: 7}})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			base, err := sqlite.NewConnector(sqliteWriterDSN(s.Path()))
			require.NoError(t, err)
			var hit atomic.Int32
			writer := sql.OpenDB(cancelAfterManualBeginConnector{Connector: base, cancel: cancel, hit: &hit})
			writer.SetMaxOpenConns(1)
			writer.SetMaxIdleConns(1)
			defer func() { require.NoError(t, writer.Close()) }()
			s.writeMu.Lock()
			original := s.writerDB
			s.writerDB = writer
			s.writeMu.Unlock()
			defer func() {
				s.writeMu.Lock()
				s.writerDB = original
				s.writeMu.Unlock()
			}()
			before := walFileSize(s.Path() + "-wal")
			if name == "restart" {
				conn, connErr := writer.Conn(t.Context())
				require.NoError(t, connErr)
				var result walCheckpointResult
				err = s.restartWALInPlaceOn(ctx, conn, &result)
				require.NoError(t, conn.Close())
			} else {
				_, err = s.shrinkWALSliceOnce(ctx, writer, 1<<20)
			}
			require.EqualValues(t, 1, hit.Load(), "the real BEGIN must have applied before cancellation was reported")
			require.ErrorIs(t, err, context.Canceled, "cleanup must preserve the original refusal")
			require.Equal(t, before, walFileSize(s.Path()+"-wal"), "the canceled maintenance must not truncate the log")
			require.NotPanics(t, func() {
				s.ReindexEdges([]graph.EdgeReindex{{OldTo: oldTo, Edge: &graph.Edge{From: from, To: newTo, Kind: graph.EdgeCalls, FilePath: "repo/caller.go", Line: 7}}})
			}, "the same sole writer must be outside a transaction before its next resolver write")
			edges := s.GetOutEdges(from)
			require.Len(t, edges, 1)
			require.Equal(t, newTo, edges[0].To)
		})
	}
}
