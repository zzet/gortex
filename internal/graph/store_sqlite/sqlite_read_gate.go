package store_sqlite

import (
	"context"
	"database/sql/driver"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	modernsqlite "modernc.org/sqlite"
)

// sqliteReadGate is the admission point every physical connection of the
// store's read pool passes through before it starts SQL. It exists for one
// client: the bounded WAL reclaim, which must briefly guarantee that none of
// the store's own readers holds a WAL read mark so that a RESTART/TRUNCATE
// checkpoint can reset the log.
//
// SQLite resets the WAL only when no reader is using it. PASSIVE checkpoints
// backfill frames but never reset, and a daemon that always has some reader in
// flight therefore grows the -wal file without bound: journal_size_limit is
// applied only at a reset. Closing this gate stops NEW read work from starting
// on the pool, waits (bounded) for the reads already in flight to release their
// connections, and reopens it — so the reclaim can run a TRUNCATE against a
// log no pool reader holds.
//
// Accounting is per physical connection. A connection becomes "active" at the
// first SQL it starts after being handed out (prepare, query, exec, begin, or a
// cached prepared statement's execution) and stops being active when
// database/sql returns it to the pool (driver.Validator.IsValid runs there) or
// closes it. That is exactly the interval during which the connection can hold
// a WAL read lock: an open *sql.Rows or *sql.Tx keeps the connection checked
// out, and a returned connection holds no statement or transaction.
//
// Once a connection is active it never blocks on the gate again until it is
// released: a read transaction already under way is never paused half-way,
// only the start of new work is. The cost on the open path is one atomic load
// per statement start plus one uncontended mutex per connection checkout.
type sqliteReadGate struct {
	// closed is set and cleared only under mu; the atomic lets the reclaim's
	// observers read it without the lock.
	closed atomic.Bool

	mu sync.Mutex
	// active counts connections that started SQL and have not been released.
	active int
	// epoch is the admission epoch a connection records when it becomes
	// active; activeByEpoch counts active connections per epoch. The WAL
	// reclaim advances the epoch at a point after which new read
	// transactions cannot pin the log, and then waits only for the
	// connections admitted before it (see waitOlder).
	epoch         uint64
	activeByEpoch map[uint64]int
	// changed is closed (and replaced) whenever a connection is released
	// while a waitOlder is parked on it.
	changed chan struct{}
	// conns is the set of active gated connections (their identity for the
	// reclaim's blocked-reader warning).
	conns map[*gatedConn]struct{}
	// reopen is closed when a quiesce ends; waiters park on it.
	reopen chan struct{}
	// drained is closed when active reaches zero while the gate is closed.
	drained chan struct{}

	// Waiter accounting: how long a reader was held at the gate. Exported via
	// the reclaim statistics so the reader-visible pause is measured, not
	// inferred from the reclaim's own timings.
	waits       atomic.Int64
	waitNanos   atomic.Int64
	maxWaitNano atomic.Int64

	// Read-transaction accounting: the wall time from a pooled connection's
	// first statement to its return to the pool (ReaderWaitMark.ReadTxn*).
	readTxns  atomic.Int64
	readNanos atomic.Int64

	// Page-cache accounting, summed over the pool's connections: each read
	// transaction's SQLITE_DBSTATUS_CACHE_HIT / _MISS / _SPILL, read with
	// reset when the connection returns to the pool (ReaderWaitMark.Cache*).
	cacheHits   atomic.Int64
	cacheMisses atomic.Int64
	cacheSpills atomic.Int64
}

func newSQLiteReadGate() *sqliteReadGate {
	return &sqliteReadGate{}
}

// sqliteReadGateEnabled is the kill switch for the read-pool wrapper:
// GORTEX_SQLITE_READ_GATE=off (or 0/false) opens the read pool on the bare
// driver. The WAL reclaim then still runs, but without reader quiescence, so
// it resets the log only when the pool's readers happen to be idle. The bare
// pool is also what a caller needs for modernc helpers that type-assert the
// driver connection through (*sql.Conn).Raw, such as sqlite.Limit.
func sqliteReadGateEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_SQLITE_READ_GATE"))) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

// enter admits one connection's first SQL. It returns promptly while the gate
// is open; while a quiesce holds it closed it waits for the reopen or for ctx.
func (g *sqliteReadGate) enter(ctx context.Context) error {
	_, err := g.enterEpoch(ctx)
	return err
}

// enterEpoch is enter reporting the admission epoch the caller must hand back
// to leaveEpoch.
func (g *sqliteReadGate) enterEpoch(ctx context.Context) (uint64, error) {
	if g == nil {
		return 0, nil
	}
	var waitStart time.Time
	for {
		g.mu.Lock()
		if !g.closed.Load() {
			g.active++
			epoch := g.epoch
			if g.activeByEpoch == nil {
				g.activeByEpoch = map[uint64]int{}
			}
			g.activeByEpoch[epoch]++
			g.mu.Unlock()
			if !waitStart.IsZero() {
				g.recordWait(time.Since(waitStart))
			}
			return epoch, nil
		}
		reopen := g.reopen
		g.mu.Unlock()
		if waitStart.IsZero() {
			waitStart = time.Now()
		}
		if ctx == nil {
			<-reopen
			continue
		}
		select {
		case <-reopen:
		case <-ctx.Done():
			g.recordWait(time.Since(waitStart))
			return 0, ctx.Err()
		}
	}
}

func (g *sqliteReadGate) recordWait(d time.Duration) {
	g.waits.Add(1)
	g.waitNanos.Add(int64(d))
	for {
		cur := g.maxWaitNano.Load()
		if int64(d) <= cur || g.maxWaitNano.CompareAndSwap(cur, int64(d)) {
			return
		}
	}
}

// leave releases one active connection admitted in the oldest epoch that
// still has one (callers that did not keep their epoch).
func (g *sqliteReadGate) leave() {
	if g == nil {
		return
	}
	g.mu.Lock()
	oldest, found := uint64(0), false
	for epoch, n := range g.activeByEpoch {
		if n > 0 && (!found || epoch < oldest) {
			oldest, found = epoch, true
		}
	}
	g.releaseLocked(oldest, found)
	g.mu.Unlock()
}

// leaveEpoch releases one active connection admitted in epoch.
func (g *sqliteReadGate) leaveEpoch(epoch uint64) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.releaseLocked(epoch, g.activeByEpoch[epoch] > 0)
	g.mu.Unlock()
}

func (g *sqliteReadGate) releaseLocked(epoch uint64, counted bool) {
	if g.active > 0 {
		g.active--
	}
	if counted {
		if g.activeByEpoch[epoch]--; g.activeByEpoch[epoch] <= 0 {
			delete(g.activeByEpoch, epoch)
		}
	}
	if g.active == 0 && g.drained != nil {
		close(g.drained)
		g.drained = nil
	}
	if g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
}

// advance starts a new admission epoch and returns it: every connection
// active now is "older" than the returned epoch, every one admitted from now
// on is not.
func (g *sqliteReadGate) advance() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.epoch++
	return g.epoch
}

// olderLocked counts active connections admitted before epoch.
func (g *sqliteReadGate) olderLocked(epoch uint64) int {
	n := 0
	for e, c := range g.activeByEpoch {
		if e < epoch {
			n += c
		}
	}
	return n
}

// waitOlder waits, with the gate left OPEN, until no connection admitted
// before epoch is still active, the deadline passes, or ctx ends. New readers
// are never held. It reports how many older connections were still active
// when it gave up.
func (g *sqliteReadGate) waitOlder(ctx context.Context, epoch uint64, deadline time.Time) (int, error) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		g.mu.Lock()
		older := g.olderLocked(epoch)
		if older == 0 {
			g.mu.Unlock()
			return 0, nil
		}
		if g.changed == nil {
			g.changed = make(chan struct{})
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			g.mu.Lock()
			older = g.olderLocked(epoch)
			g.mu.Unlock()
			if older == 0 {
				return 0, nil
			}
			return older, errWALReclaimReadersInFlight
		case <-ctx.Done():
			return g.olderCount(epoch), ctx.Err()
		}
	}
}

func (g *sqliteReadGate) olderCount(epoch uint64) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.olderLocked(epoch)
}

var errReadGateBusy = errors.New("store_sqlite: read gate already quiescing")

// quiesce closes the gate and waits until no pool connection is active, the
// deadline passes, or ctx ends. On success it returns the reopen function the
// caller MUST call (the gate stays closed until then). On failure the gate is
// already reopened and the error says why. Only one quiesce may be open at a
// time; the WAL reclaim is its sole caller and is serialised by the
// maintenance lane, so a second concurrent caller is refused rather than
// queued.
func (g *sqliteReadGate) quiesce(ctx context.Context, deadline time.Time) (reopen func(), inFlight int, err error) {
	g.mu.Lock()
	if g.closed.Load() {
		g.mu.Unlock()
		return nil, 0, errReadGateBusy
	}
	g.closed.Store(true)
	g.reopen = make(chan struct{})
	var once sync.Once
	reopen = func() {
		once.Do(func() {
			g.mu.Lock()
			g.closed.Store(false)
			g.drained = nil
			close(g.reopen)
			g.mu.Unlock()
		})
	}
	if g.active == 0 {
		g.mu.Unlock()
		return reopen, 0, nil
	}
	drained := make(chan struct{})
	g.drained = drained
	g.mu.Unlock()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-drained:
		return reopen, 0, nil
	case <-timer.C:
		err = errWALReclaimReadersInFlight
	case <-ctx.Done():
		err = ctx.Err()
	}
	g.mu.Lock()
	inFlight = g.active
	g.mu.Unlock()
	reopen()
	return nil, inFlight, err
}

// activeCount reports the connections currently past the gate (tests).
func (g *sqliteReadGate) activeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}

// gatedConnector wraps the driver connector of the read pool so every
// physical connection it opens is a gatedConn.
type gatedConnector struct {
	inner driver.Connector
	gate  *sqliteReadGate
}

func (c gatedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &gatedConn{inner: conn, gate: c.gate}, nil
}

func (c gatedConnector) Driver() driver.Driver { return c.inner.Driver() }

// gatedConn forwards every driver interface the modernc connection implements
// and database/sql probes for (ConnBeginTx, ConnPrepareContext, ExecerContext,
// QueryerContext, Pinger, SessionResetter, Validator), entering the gate
// before the first SQL after each checkout. database/sql serialises every call
// on one connection under its driverConn lock, so entered needs no ordering
// beyond the atomic that keeps the race detector honest.
type gatedConn struct {
	inner   driver.Conn
	gate    *sqliteReadGate
	entered atomic.Bool
	// epoch is the admission epoch of the current active interval; written
	// before entered is set and read after it is cleared.
	epoch uint64
	// since (UnixNano) is when the current active interval began and label
	// the statement it most recently started: the identity the WAL reclaim
	// logs for a reader that outlives its attempts.
	since atomic.Int64
	label atomic.Value // string
}

// noteStatement records the statement a connection is running.
func (c *gatedConn) noteStatement(query string) {
	if len(query) > readerLabelMax {
		query = query[:readerLabelMax]
	}
	c.label.Store(strings.Join(strings.Fields(query), " "))
}

// readerLabelMax bounds the logged statement text.
const readerLabelMax = 160

var (
	_ driver.Conn               = (*gatedConn)(nil)
	_ driver.ConnBeginTx        = (*gatedConn)(nil)
	_ driver.ConnPrepareContext = (*gatedConn)(nil)
	_ driver.ExecerContext      = (*gatedConn)(nil)
	_ driver.QueryerContext     = (*gatedConn)(nil)
	_ driver.Pinger             = (*gatedConn)(nil)
	_ driver.SessionResetter    = (*gatedConn)(nil)
	_ driver.Validator          = (*gatedConn)(nil)
	_ driver.Stmt               = (*gatedStmt)(nil)
	_ driver.StmtExecContext    = (*gatedStmt)(nil)
	_ driver.StmtQueryContext   = (*gatedStmt)(nil)
)

func (c *gatedConn) enter(ctx context.Context) error {
	if c.entered.Load() {
		return nil
	}
	epoch, err := c.gate.enterEpoch(ctx)
	if err != nil {
		return err
	}
	c.epoch = epoch
	c.since.Store(time.Now().UnixNano())
	c.gate.track(c)
	c.entered.Store(true)
	return nil
}

func (c *gatedConn) release() {
	if c.entered.CompareAndSwap(true, false) {
		if since := c.since.Load(); since > 0 {
			c.gate.readTxns.Add(1)
			c.gate.readNanos.Add(time.Now().UnixNano() - since)
		}
		c.gate.collectCacheCounters(c.inner)
		c.gate.untrack(c)
		c.gate.leaveEpoch(c.epoch)
	}
}

func (c *gatedConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *gatedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	pc, ok := c.inner.(driver.ConnPrepareContext)
	if !ok {
		return nil, errGatedDriverMissing
	}
	if err := c.enter(ctx); err != nil {
		return nil, err
	}
	c.noteStatement(query)
	st, err := pc.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &gatedStmt{inner: st, conn: c, query: query}, nil
}

func (c *gatedConn) Close() error {
	c.release()
	return c.inner.Close()
}

// Begin satisfies driver.Conn; database/sql itself always calls BeginTx.
func (c *gatedConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *gatedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	bt, ok := c.inner.(driver.ConnBeginTx)
	if !ok {
		return nil, errGatedDriverMissing
	}
	if err := c.enter(ctx); err != nil {
		return nil, err
	}
	c.noteStatement("BEGIN")
	return bt.BeginTx(ctx, opts)
}

func (c *gatedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	ec, ok := c.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if err := c.enter(ctx); err != nil {
		return nil, err
	}
	c.noteStatement(query)
	return ec.ExecContext(ctx, query, args)
}

func (c *gatedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	qc, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if err := c.enter(ctx); err != nil {
		return nil, err
	}
	c.noteStatement(query)
	return qc.QueryContext(ctx, query, args)
}

// Ping is not gated: it runs `select 1`, which opens no read transaction on
// the database file and so cannot pin the WAL.
func (c *gatedConn) Ping(ctx context.Context) error {
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *gatedConn) ResetSession(ctx context.Context) error {
	if r, ok := c.inner.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

// IsValid runs when database/sql returns the connection to the pool: the
// connection has no open rows or transaction any more, so it stops counting as
// an in-flight reader here.
func (c *gatedConn) IsValid() bool {
	c.release()
	if v, ok := c.inner.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// gatedStmt re-enters the gate when a statement prepared earlier is executed
// on a connection checked out again (database/sql caches prepared statements
// per connection, so a cached execution never calls back into the conn).
type gatedStmt struct {
	inner driver.Stmt
	conn  *gatedConn
	query string
}

func (s *gatedStmt) Close() error  { return s.inner.Close() }
func (s *gatedStmt) NumInput() int { return s.inner.NumInput() }

// Exec and Query satisfy driver.Stmt; database/sql uses the context forms.
func (s *gatedStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), valuesToNamedValues(args))
}

func (s *gatedStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), valuesToNamedValues(args))
}

func (s *gatedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	ec, ok := s.inner.(driver.StmtExecContext)
	if !ok {
		return nil, errGatedDriverMissing
	}
	if err := s.conn.enter(ctx); err != nil {
		return nil, err
	}
	s.conn.noteStatement(s.query)
	return ec.ExecContext(ctx, args)
}

func (s *gatedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	qc, ok := s.inner.(driver.StmtQueryContext)
	if !ok {
		return nil, errGatedDriverMissing
	}
	if err := s.conn.enter(ctx); err != nil {
		return nil, err
	}
	s.conn.noteStatement(s.query)
	return qc.QueryContext(ctx, args)
}

func valuesToNamedValues(args []driver.Value) []driver.NamedValue {
	named := make([]driver.NamedValue, len(args))
	for i, arg := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: arg}
	}
	return named
}

// errGatedDriverMissing reports a wrapped driver without the context-aware
// interfaces the gate forwards to. The modernc driver implements all of them;
// the error exists so a driver swap fails loudly instead of silently
// bypassing the gate.
var errGatedDriverMissing = errors.New("store_sqlite: read-pool driver lacks a context-aware interface the read gate forwards")

func (g *sqliteReadGate) track(c *gatedConn) {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.conns == nil {
		g.conns = map[*gatedConn]struct{}{}
	}
	g.conns[c] = struct{}{}
	g.mu.Unlock()
}

func (g *sqliteReadGate) untrack(c *gatedConn) {
	if g == nil {
		return
	}
	g.mu.Lock()
	delete(g.conns, c)
	g.mu.Unlock()
}

// activeReader describes one connection past the gate.
type activeReader struct {
	Epoch uint64
	Age   time.Duration
	Label string
}

// oldestOlderThan returns the longest-active connection admitted before
// epoch, and whether there is one.
func (g *sqliteReadGate) oldestOlderThan(epoch uint64, now time.Time) (activeReader, bool) {
	if g == nil {
		return activeReader{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var best activeReader
	found := false
	for c := range g.conns {
		if c.epoch >= epoch {
			continue
		}
		age := now.Sub(time.Unix(0, c.since.Load()))
		if !found || age > best.Age {
			label, _ := c.label.Load().(string)
			best, found = activeReader{Epoch: c.epoch, Age: age, Label: label}, true
		}
	}
	return best, found
}

// collectCacheCounters adds a connection's page-cache counters since its last
// read transaction to the gate's sums and resets them. It runs when the
// connection returns to the pool, with no statement active on it.
//
// What they count: a page SQLite looks up in the connection's page cache,
// found (hit) or read with a pread from the log or the database file (miss),
// or a dirty page written out to make room (spill). A page served from the
// memory map (the first mmap_size bytes of the database file, when its latest
// version is not in the log) bypasses the page cache and is in neither count;
// sqliteMappedPages counts those.
func (g *sqliteReadGate) collectCacheCounters(inner driver.Conn) {
	st, ok := inner.(modernsqlite.DBStatus)
	if !ok {
		return
	}
	if n, _, err := st.Status(modernsqlite.DBStatusCacheHit, true); err == nil {
		g.cacheHits.Add(int64(n))
	}
	if n, _, err := st.Status(modernsqlite.DBStatusCacheMiss, true); err == nil {
		g.cacheMisses.Add(int64(n))
	}
	if n, _, err := st.Status(modernsqlite.DBStatusCacheSpill, true); err == nil {
		g.cacheSpills.Add(int64(n))
	}
}
