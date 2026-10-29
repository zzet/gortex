package sqliteconn

import (
	"context"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	sqlite "modernc.org/sqlite"
)

type unsupportedClosePolicyConn struct {
	closes   atomic.Int32
	closeErr error
}

func (*unsupportedClosePolicyConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*unsupportedClosePolicyConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected transaction")
}
func (c *unsupportedClosePolicyConn) Close() error { c.closes.Add(1); return c.closeErr }

type unsupportedClosePolicyConnector struct {
	conn  *unsupportedClosePolicyConn
	calls atomic.Int32
}

func (c *unsupportedClosePolicyConnector) Connect(context.Context) (driver.Conn, error) {
	c.calls.Add(1)
	return c.conn, nil
}
func (*unsupportedClosePolicyConnector) Driver() driver.Driver { return &sqlite.Driver{} }

func TestSQLiteNoCloseCheckpointRefusesUnsupportedConnectionAndRetainsCloseError(t *testing.T) {
	sentinel := errors.New("physical close failed")
	conn := &unsupportedClosePolicyConn{closeErr: sentinel}
	inner := &unsupportedClosePolicyConnector{conn: conn}
	connector := noCloseCheckpointConnector{inner: inner}
	opened, err := connector.Connect(t.Context())
	require.Nil(t, opened)
	require.ErrorContains(t, err, "unsupported physical connection handles")
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, int32(1), conn.closes.Load(), "rejected connection was not actually closed exactly once")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	opened, err = connector.Connect(ctx)
	require.Nil(t, opened)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, int32(1), inner.calls.Load(), "canceled admission opened another physical connection")
}
