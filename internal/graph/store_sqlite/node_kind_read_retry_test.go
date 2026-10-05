package store_sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type nodeKindRetryConnector struct{ calls atomic.Int32 }

func (c *nodeKindRetryConnector) Connect(context.Context) (driver.Conn, error) {
	return &nodeKindRetryConn{owner: c}, nil
}
func (c *nodeKindRetryConnector) Driver() driver.Driver { return nodeKindRetryDriver{} }

type nodeKindRetryDriver struct{}

func (nodeKindRetryDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("connector only")
}

type nodeKindRetryConn struct{ owner *nodeKindRetryConnector }

func (*nodeKindRetryConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*nodeKindRetryConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (*nodeKindRetryConn) Close() error                        { return nil }
func (c *nodeKindRetryConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	if c.owner.calls.Add(1) == 1 {
		return nil, driver.ErrBadConn
	}
	return &nodeKindRetryRows{}, nil
}

type nodeKindRetryRows struct{ done bool }

func (*nodeKindRetryRows) Columns() []string {
	return []string{"id", "kind", "file_path", "repo_prefix"}
}
func (*nodeKindRetryRows) Close() error { return nil }
func (r *nodeKindRetryRows) Next(v []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(v, []driver.Value{"repo/f.go::N", "function", "repo/f.go", "repo"})
	return nil
}

func TestNodeKindReadTimingRetainsDatabaseSQLBadConnectionRetries(t *testing.T) {
	run := func(observed bool) (map[string]graph.NodeKindRow, graph.NodeKindReadTiming) {
		connector := &nodeKindRetryConnector{}
		db := sql.OpenDB(gatedConnector{inner: connector, gate: newSQLiteReadGate()})
		defer db.Close()
		s := &Store{db: db}
		var timing graph.NodeKindReadTiming
		ctx := t.Context()
		if observed {
			ctx = graph.WithNodeKindReadObserver(ctx, timing.Add)
		}
		rows, err := s.GetNodeKindsByIDsContext(ctx, []string{"repo/f.go::N"})
		require.NoError(t, err)
		require.EqualValues(t, 2, connector.calls.Load())
		require.Zero(t, db.Stats().InUse)
		return rows, timing
	}
	plain, _ := run(false)
	observed, timing := run(true)
	require.Equal(t, plain, observed)
	require.Equal(t, 2, timing.DriverEntries)
	require.Equal(t, 1, timing.Batches)
	require.Zero(t, timing.Errors)
	require.GreaterOrEqual(t, timing.QueryStart, timing.PreDriver+timing.Gate)
}
