package store_sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"modernc.org/libc"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// driverInterrupt returns the driver's bare SQLITE_INTERRUPT, the error it
// reports for a BEGIN interrupted when its context ends, and for the next
// step of a query whose context ended as it returned its first row. Both
// windows are too narrow to hit on demand, so the same driver error is taken
// from a statement that runs until it is interrupted.
func driverInterrupt(t *testing.T, path string) error {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteWriterDSN(path))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	raw := conn.Raw(func(dc any) error {
		h, _, ok := driverConnHandles(dc)
		if !ok {
			return fmt.Errorf("unrecognised driver connection %T", dc)
		}
		stop := make(chan struct{})
		interrupted := make(chan struct{})
		go func() {
			defer close(interrupted)
			tls := libc.NewTLS()
			defer tls.Close()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					// Repeated: SQLite discards an interrupt that lands
					// before the statement starts.
					sqlite3.Xsqlite3_interrupt(tls, h)
				}
			}
		}()
		_, err := dc.(driver.ExecerContext).ExecContext(context.Background(),
			`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c) SELECT count(*) FROM c`, nil)
		close(stop)
		<-interrupted
		return err
	})
	require.True(t, isSQLiteInterrupt(raw), "the driver must report the bare interrupt: %v", raw)
	return raw
}

// interruptAtDeadline is a writer connection that fails one operation of the
// retirement quantum the way the driver does when the quantum's budget ends
// under it: it waits for the operation's context to end, then returns the
// bare SQLITE_INTERRUPT.
type interruptAtDeadline struct {
	driver.Conn
	raw   error
	begin bool // the BEGIN; otherwise the first DELETE
	hit   *atomic.Int32
}

func (c *interruptAtDeadline) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.begin {
		c.hit.Add(1)
		<-ctx.Done()
		return nil, c.raw
	}
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *interruptAtDeadline) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if !c.begin && strings.HasPrefix(strings.TrimSpace(query), "DELETE") {
		c.hit.Add(1)
		<-ctx.Done()
		return nil, c.raw
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *interruptAtDeadline) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

type interruptAtDeadlineConnector struct {
	driver.Connector
	raw   error
	begin bool
	hit   *atomic.Int32
}

func (c interruptAtDeadlineConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &interruptAtDeadline{Conn: conn, raw: c.raw, begin: c.begin, hit: c.hit}, nil
}

// A retirement quantum whose budget ends while the driver runs a BEGIN or
// one of its statements yields: the quantum reports the budget's deadline,
// the stop is counted as a preemption rather than an error, and no storage
// failure is recorded against the generation. The burst resumes a deadline;
// the bare interrupt it was handed before failed the sweep.
func TestRetirementQuantumInterruptedAtItsDeadlineYields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fenced bool // a continuation; otherwise the first quantum, whose fence BEGINs first
		begin  bool
	}{
		{name: "fence_begin", begin: true},
		{name: "sweep_begin", fenced: true, begin: true},
		{name: "sweep_statement", fenced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, id := quantumPayloadFixture(t)
			buildMinimalAnalysisGeneration(t, s.AtGeneration(id), "interrupt", 12, true)
			retire := s.RetirePayloadGenerationQuantum
			if tc.fenced {
				// The first quantum fences the generation on the real writer.
				_, err := s.RetirePayloadGenerationQuantum(t.Context(), id, nil)
				require.ErrorIs(t, err, ErrPayloadSweepBudgetExhausted)
				retire = s.RetirePayloadGenerationQuantumFenced
			}
			require.NoError(t, s.waitMaintenanceIdle(t.Context()))
			// The checkpoint loop and the builders it starts read the writer
			// pool without the writer lock; stopped and joined, they cannot
			// race its replacement. Close finds the loop already stopped.
			s.stopCheckpointLoop()

			base, err := sqlite.NewConnector(sqliteWriterDSN(s.Path()))
			require.NoError(t, err)
			var hit atomic.Int32
			writer := sql.OpenDB(interruptAtDeadlineConnector{Connector: base, raw: driverInterrupt(t, s.Path()), begin: tc.begin, hit: &hit})
			writer.SetMaxOpenConns(1)
			defer func() { _ = writer.Close() }()
			s.writeMu.Lock()
			original := s.writerDB
			s.writerDB = writer
			s.writeMu.Unlock()
			defer func() {
				s.writeMu.Lock()
				s.writerDB = original
				s.writeMu.Unlock()
			}()

			s.retirementQuantumOverride = 20 * time.Millisecond
			before := viewmetrics.Read()
			progress, err := retire(t.Context(), id, nil)
			after := viewmetrics.Read()
			require.EqualValues(t, 1, hit.Load(), "the %s was not the operation interrupted", tc.name)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.False(t, isSQLiteInterrupt(err), "the budget's interrupt must read as its deadline: %v", err)
			require.Zero(t, progress.ChunksCommitted)
			require.False(t, progress.CatalogRemoved)
			require.Zero(t, retireRefusalDelta(before, after, viewmetrics.RefusedError), "a budget yield was counted as an error")
			require.Positive(t, retireRefusalDelta(before, after, viewmetrics.RetirePreempted))
			for _, failure := range s.StorageFailures() {
				require.NotEqual(t, id, failure.GenerationID, "a budget yield was recorded as a storage failure: %s", failure.Reason)
			}
		})
	}
}

// An interrupt no ended context explains is not translated: the driver's
// error reaches the caller unchanged.
func TestInterruptUnderALiveContextKeepsTheDriverError(t *testing.T) {
	s, _ := quantumPayloadFixture(t)
	raw := driverInterrupt(t, s.Path())
	require.Equal(t, raw, contextInterrupt(t.Context(), raw))
	other := errors.New("disk I/O error")
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	require.Equal(t, other, contextInterrupt(expired, other))
	require.ErrorIs(t, contextInterrupt(expired, fmt.Errorf("delete chunk: %w", raw)), context.DeadlineExceeded)
}
