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

// interruptOnCancellation cancels the caller only after the selected retirement
// operation is entered, then returns the driver's real bare SQLITE_INTERRUPT.
// Admission and connection setup therefore have no short elapsed-time premise.
type interruptOnCancellation struct {
	driver.Conn
	raw        error
	begin      bool // a BEGIN; otherwise the first DELETE
	skipBegins int
	cancel     context.CancelFunc
	hit        *atomic.Int32
}

func (c *interruptOnCancellation) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.begin {
		if c.skipBegins > 0 {
			c.skipBegins--
			return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
		}
		c.hit.Add(1)
		c.cancel()
		<-ctx.Done()
		return nil, c.raw
	}
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *interruptOnCancellation) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if !c.begin && strings.HasPrefix(strings.TrimSpace(query), "DELETE") {
		c.hit.Add(1)
		c.cancel()
		<-ctx.Done()
		return nil, c.raw
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *interruptOnCancellation) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

type interruptOnCancellationConnector struct {
	driver.Connector
	raw        error
	begin      bool
	skipBegins int
	cancel     context.CancelFunc
	hit        *atomic.Int32
}

func (c interruptOnCancellationConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &interruptOnCancellation{Conn: conn, raw: c.raw, begin: c.begin, skipBegins: c.skipBegins, cancel: c.cancel, hit: c.hit}, nil
}

// A retirement canceled while the driver runs a BEGIN or one of its statements
// reports the caller's cancellation rather than the bare SQLite interrupt:
// the stop is counted as a preemption rather than an error, and no storage
// failure is recorded against the generation. This exercises actual operation
// boundaries without requiring preflight to finish before a 20 ms timer. The
// direct contextInterrupt control below separately covers expired deadlines.
func TestRetirementInterruptedByCancellationYields(t *testing.T) {
	for _, tc := range []struct {
		name       string
		begin      bool
		skipBegins int // the first BEGIN fences; the next begins the payload sweep
	}{
		{name: "fence_begin", begin: true},
		{name: "sweep_begin", begin: true, skipBegins: 1},
		{name: "sweep_statement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, id := quantumPayloadFixture(t)
			analysis := buildMinimalAnalysisGeneration(t, s.AtGeneration(id), "interrupt", 12, true)
			require.NoError(t, s.waitMaintenanceIdle(t.Context()))
			// The checkpoint loop and the builders it starts read the writer
			// pool without the writer lock; stopped and joined, they cannot
			// race its replacement. Close finds the loop already stopped.
			s.stopCheckpointLoop()
			rowsBefore := make(map[string]int)
			for _, table := range payloadGenerationTables() {
				rowsBefore[table] = countAtGeneration(t, s, table, id)
			}
			var analysisNodesBefore int
			require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM analysis_nodes WHERE generation_id=?`, analysis).Scan(&analysisNodesBefore))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			base, err := sqlite.NewConnector(sqliteWriterDSN(s.Path()))
			require.NoError(t, err)
			var hit atomic.Int32
			writer := sql.OpenDB(interruptOnCancellationConnector{Connector: base, raw: driverInterrupt(t, s.Path()), begin: tc.begin, skipBegins: tc.skipBegins, cancel: cancel, hit: &hit})
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

			before := viewmetrics.Read()
			err = s.RetirePayloadGeneration(ctx, id, nil)
			after := viewmetrics.Read()
			require.EqualValues(t, 1, hit.Load(), "the %s was not the operation interrupted", tc.name)
			require.ErrorIs(t, err, context.Canceled)
			require.False(t, isSQLiteInterrupt(err), "the interrupt must read as the caller's cancellation: %v", err)
			for table, count := range rowsBefore {
				require.Equal(t, count, countAtGeneration(t, s, table, id), table)
			}
			var analysisNodesAfter int
			require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM analysis_nodes WHERE generation_id=?`, analysis).Scan(&analysisNodesAfter))
			require.Equal(t, analysisNodesBefore, analysisNodesAfter)
			_, found, err := s.Catalog().GetViewGeneration(t.Context(), id)
			require.NoError(t, err)
			require.True(t, found, "cancellation must not remove the catalog")
			require.Zero(t, retireRefusalDelta(before, after, viewmetrics.RefusedError), "cancellation was counted as an error")
			require.Positive(t, retireRefusalDelta(before, after, viewmetrics.RetirePreempted))
			for _, failure := range s.StorageFailures() {
				require.NotEqual(t, id, failure.GenerationID, "cancellation was recorded as a storage failure: %s", failure.Reason)
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
