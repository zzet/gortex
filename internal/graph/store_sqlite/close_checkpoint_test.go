package store_sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The fake connection observes the real checkpoint call without slow disks or
// sleeps. Both database/sql pools are opened before Close so cleanup assertions
// cannot pass merely because no driver connection was created.
type closeCheckpointProbe struct {
	queryErr error
	busy     int64
	queries  atomic.Int32
	closed   atomic.Int32
	deadline atomic.Bool
}

type closeCheckpointConnector struct{ probe *closeCheckpointProbe }
type closeCheckpointDriver struct{}
type closeCheckpointConn struct{ probe *closeCheckpointProbe }
type closeCheckpointRows struct {
	busy int64
	sent bool
}

func (c closeCheckpointConnector) Connect(context.Context) (driver.Conn, error) {
	return &closeCheckpointConn{probe: c.probe}, nil
}
func (closeCheckpointConnector) Driver() driver.Driver { return closeCheckpointDriver{} }
func (closeCheckpointDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("checkpoint fixture requires its connector")
}
func (c *closeCheckpointConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected checkpoint fixture prepare")
}
func (c *closeCheckpointConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected checkpoint fixture transaction")
}
func (c *closeCheckpointConn) Close() error { c.probe.closed.Add(1); return nil }
func (c *closeCheckpointConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(strings.ToLower(query), "wal_checkpoint") {
		return nil, fmt.Errorf("unexpected checkpoint fixture query: %s", query)
	}
	c.probe.queries.Add(1)
	if _, ok := ctx.Deadline(); ok {
		c.probe.deadline.Store(true)
	}
	if c.probe.queryErr != nil {
		return nil, c.probe.queryErr
	}
	return &closeCheckpointRows{busy: c.probe.busy}, nil
}
func (*closeCheckpointRows) Columns() []string { return []string{"busy", "log", "checkpointed"} }
func (*closeCheckpointRows) Close() error      { return nil }
func (r *closeCheckpointRows) Next(values []driver.Value) error {
	if r.sent {
		return io.EOF
	}
	r.sent = true
	values[0], values[1], values[2] = r.busy, int64(0), int64(0)
	return nil
}

func newCloseCheckpointFixture(t *testing.T, checkpointErr error, busy int64) (*Store, *closeCheckpointProbe, *closeCheckpointProbe) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	// This source-verified idempotent join leaves no background checkpoint
	// using the original pools while the fixture replaces them.
	s.stopCheckpointLoop()
	if err := closeSQLitePools(s.db, s.writerDB); err != nil {
		t.Fatal(err)
	}
	reader := &closeCheckpointProbe{}
	writer := &closeCheckpointProbe{queryErr: checkpointErr, busy: busy}
	s.db = sql.OpenDB(closeCheckpointConnector{probe: reader})
	s.writerDB = sql.OpenDB(closeCheckpointConnector{probe: writer})
	t.Cleanup(func() { _ = closeSQLitePools(s.db, s.writerDB) })
	for _, db := range []*sql.DB{s.db, s.writerDB} {
		if err := db.PingContext(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return s, reader, writer
}

func assertCloseCheckpointPoolsClosed(t *testing.T, s *Store, reader, writer *closeCheckpointProbe) {
	t.Helper()
	for name, db := range map[string]*sql.DB{"reader": s.db, "writer": s.writerDB} {
		if err := db.PingContext(context.Background()); err == nil {
			t.Errorf("%s pool remains usable after Close", name)
		}
		if open := db.Stats().OpenConnections; open != 0 {
			t.Errorf("%s pool retains %d connections", name, open)
		}
	}
	if reader.closed.Load() != 1 || writer.closed.Load() != 1 {
		t.Errorf("driver closes: reader=%d writer=%d, want one each", reader.closed.Load(), writer.closed.Load())
	}
}

func TestCloseCheckpointDoesNotUseInteractiveDeadline(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT", "full")
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S", "")
	s, reader, writer := newCloseCheckpointFixture(t, nil, 0)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if writer.queries.Load() != 1 || reader.queries.Load() != 0 {
		t.Fatalf("checkpoint queries: writer=%d reader=%d", writer.queries.Load(), reader.queries.Load())
	}
	if writer.deadline.Load() {
		t.Error("ordinary Close imposed the interactive checkpoint deadline on successful shutdown I/O")
	}
	assertCloseCheckpointPoolsClosed(t, s, reader, writer)
}

func TestExplicitCheckpointWALKeepsInteractiveDeadline(t *testing.T) {
	s, reader, writer := newCloseCheckpointFixture(t, nil, 0)
	if err := s.CheckpointWAL(); err != nil {
		t.Fatal(err)
	}
	if writer.queries.Load() != 1 || reader.queries.Load() != 0 {
		t.Fatalf("checkpoint queries: writer=%d reader=%d", writer.queries.Load(), reader.queries.Load())
	}
	if !writer.deadline.Load() {
		t.Error("explicit CheckpointWAL lost its interactive deadline")
	}
}

func TestCloseCheckpointErrorStillClosesBothPools(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT", "full")
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S", "")
	want := errors.New("checkpoint driver failure")
	s, reader, writer := newCloseCheckpointFixture(t, want, 0)
	if err := s.Close(); !errors.Is(err, want) {
		t.Fatalf("Close error = %v, want original checkpoint failure", err)
	}
	if writer.queries.Load() != 1 {
		t.Fatalf("checkpoint queries=%d, want one non-retryable failure", writer.queries.Load())
	}
	assertCloseCheckpointPoolsClosed(t, s, reader, writer)
}

func TestCloseCheckpointRetainsContentionRetryBudget(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT", "full")
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S", "")
	s, reader, writer := newCloseCheckpointFixture(t, nil, 1)
	// This expires the existing contention repetition budget deterministically;
	// it does not sleep or impose a deadline on a single successful I/O call.
	s.busyRetryTimeout = time.Nanosecond
	if err := s.Close(); !errors.Is(err, errSQLiteBusyRetryExhausted) {
		t.Fatalf("Close error = %v, want bounded contention exhaustion", err)
	}
	if writer.queries.Load() == 0 {
		t.Fatal("contention prerequisite: the checkpoint driver was not called")
	}
	assertCloseCheckpointPoolsClosed(t, s, reader, writer)
}

// The standalone bulk window's final TRUNCATE syncs every frame written at
// synchronous=OFF. It is a durability boundary like Close, so slow but
// successful I/O must not be cut off by the interactive deadline.
func TestBulkCheckpointDoesNotUseInteractiveDeadline(t *testing.T) {
	s, reader, writer := newCloseCheckpointFixture(t, nil, 0)
	if err := s.checkpointBulkWAL(); err != nil {
		t.Fatal(err)
	}
	if writer.queries.Load() != 1 || reader.queries.Load() != 0 {
		t.Fatalf("checkpoint queries: writer=%d reader=%d", writer.queries.Load(), reader.queries.Load())
	}
	if writer.deadline.Load() {
		t.Error("bulk durability checkpoint imposed the interactive checkpoint deadline on successful I/O")
	}
}

func TestBulkCheckpointRetainsContentionRetryBudget(t *testing.T) {
	s, _, writer := newCloseCheckpointFixture(t, nil, 1)
	s.busyRetryTimeout = time.Nanosecond
	if err := s.checkpointBulkWAL(); !errors.Is(err, errSQLiteBusyRetryExhausted) {
		t.Fatalf("bulk checkpoint error = %v, want bounded contention exhaustion", err)
	}
	if writer.queries.Load() == 0 {
		t.Fatal("contention prerequisite: the checkpoint driver was not called")
	}
}
