package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"
)

// Writer-maintained node and edge counts per generation.
//
// NodeCount and EdgeCount used to count the generation's rows through its
// generation index on every call — O(rows): on the live store one 6-minute
// window spent 16 s of CPU in EdgeCount alone. The counts are now kept in
// generation_row_counts by triggers on nodes and edges, so every write path
// (AddBatch, evictions, sweeps, copies, flattens, raw SQL alike) updates them in
// the same transaction as the rows, with no Go path to forget.
//
// Version-neutral: the table, its state row and the six triggers are created
// lazily after Open (the lazy index builder's loop), not by the schema DDL, so
// no schema version moves. An older binary opening the store keeps the triggers
// firing (they are in the database), so the counts stay exact even while it
// does not read them. A table rebuild (a schema migration that recreates nodes
// or edges) drops the triggers; the next install notices and re-seeds.
//
// Seeding without a long writer hold: the triggers are created and the counts
// zeroed in one short write transaction; a read transaction is pinned on the
// snapshot that transaction produced before the writer is released (no write
// can commit in between); the per-generation counts of that snapshot are then
// taken with no writer held and ADDED to the counters, which by then carry
// exactly the changes committed after the snapshot. Until the seed lands the
// readers keep the exact COUNT(*).
//
// CheckRowCounters compares the counters with an exact recount in one read
// snapshot and, when asked, repairs a drift by adding the snapshot's
// difference (daemon status --exact).

const (
	rowCountsTable          = "generation_row_counts"
	rowCountsState          = "generation_row_counts_state"
	symbolFTSRevisionsTable = "symbol_fts_generation_revisions"
)

var rowCounterDDL = []string{
	`CREATE TABLE IF NOT EXISTS ` + rowCountsTable + ` (
    view_gen INTEGER PRIMARY KEY,
    nodes    INTEGER NOT NULL DEFAULT 0,
    edges    INTEGER NOT NULL DEFAULT 0
)`,
	`CREATE TABLE IF NOT EXISTS ` + rowCountsState + ` (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    seeded       INTEGER NOT NULL,
    installed_at INTEGER NOT NULL,
    seeded_at    INTEGER NOT NULL DEFAULT 0
)`,
	// symbol_fts_generation_revisions moves a generation's revision on every
	// write of its symbol FTS ownership rows (store_fts_stats.go keys its
	// incremental prefix counts on it). Every symbol FTS document write
	// writes its ownership row in the same transaction.
	`CREATE TABLE IF NOT EXISTS ` + symbolFTSRevisionsTable + ` (
    view_gen INTEGER PRIMARY KEY,
    rev      INTEGER NOT NULL
)`,
}

// rowCounterTriggers are the six triggers, by name. Each adds ±1 to the row's
// generation, creating the counter row on first use.
var rowCounterTriggers = func() map[string]string {
	bump := func(gen, column, delta string) string {
		return `INSERT INTO ` + rowCountsTable + `(view_gen, ` + column + `) VALUES (` + gen + `, ` + delta +
			`) ON CONFLICT(view_gen) DO UPDATE SET ` + column + ` = ` + column + ` + (` + delta + `);`
	}
	out := map[string]string{}
	for _, t := range []struct{ table, column string }{{"nodes", "nodes"}, {"edges", "edges"}} {
		out[rowCountsTable+"_"+t.table+"_insert"] = `CREATE TRIGGER ` + rowCountsTable + `_` + t.table + `_insert AFTER INSERT ON ` + t.table +
			` BEGIN ` + bump("NEW.view_gen", t.column, "1") + ` END`
		out[rowCountsTable+"_"+t.table+"_delete"] = `CREATE TRIGGER ` + rowCountsTable + `_` + t.table + `_delete AFTER DELETE ON ` + t.table +
			` BEGIN ` + bump("OLD.view_gen", t.column, "-1") + ` END`
		out[rowCountsTable+"_"+t.table+"_move"] = `CREATE TRIGGER ` + rowCountsTable + `_` + t.table + `_move AFTER UPDATE OF view_gen ON ` + t.table +
			` WHEN OLD.view_gen IS NOT NEW.view_gen BEGIN ` + bump("OLD.view_gen", t.column, "-1") + ` ` + bump("NEW.view_gen", t.column, "1") + ` END`
	}
	for _, t := range []struct{ event, row string }{{"insert", "NEW"}, {"delete", "OLD"}} {
		name := symbolFTSRevisionsTable + "_" + t.event
		out[name] = `CREATE TRIGGER ` + name + ` AFTER ` + strings.ToUpper(t.event) + ` ON symbol_fts_rowid BEGIN ` +
			`INSERT INTO ` + symbolFTSRevisionsTable + `(view_gen, rev) VALUES (` + t.row + `.view_gen, 1) ` +
			`ON CONFLICT(view_gen) DO UPDATE SET rev = rev + 1; END`
	}
	name := symbolFTSRevisionsTable + "_update"
	out[name] = `CREATE TRIGGER ` + name + ` AFTER UPDATE ON symbol_fts_rowid BEGIN ` +
		`INSERT INTO ` + symbolFTSRevisionsTable + `(view_gen, rev) VALUES (OLD.view_gen, 1) ON CONFLICT(view_gen) DO UPDATE SET rev = rev + 1; ` +
		`INSERT INTO ` + symbolFTSRevisionsTable + `(view_gen, rev) VALUES (NEW.view_gen, 1) ON CONFLICT(view_gen) DO UPDATE SET rev = rev + 1; END`
	return out
}()

// rowCountersEnabled is the kill switch: GORTEX_SQLITE_ROW_COUNTERS=0 keeps
// the exact counts and installs nothing.
func rowCountersEnabled() bool {
	v := strings.TrimSpace(os.Getenv("GORTEX_SQLITE_ROW_COUNTERS"))
	return v != "0" && !strings.EqualFold(v, "false")
}

// rowCountersInstalled reports whether the state says seeded and every
// trigger exists. A read on the pool.
func (s *Store) rowCountersInstalled(ctx context.Context) (bool, error) {
	var seeded int
	err := s.db.QueryRowContext(ctx, `SELECT seeded FROM `+rowCountsState+` WHERE id = 1`).Scan(&seeded)
	if errors.Is(err, sql.ErrNoRows) || isNoSuchTableErr(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if seeded != 1 {
		return false, nil
	}
	var n int
	names := make([]any, 0, len(rowCounterTriggers))
	for name := range rowCounterTriggers {
		names = append(names, name)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type = 'trigger' AND name IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")+`)`, names...).Scan(&n); err != nil {
		return false, err
	}
	return n == len(rowCounterTriggers), nil
}

func isNoSuchTableErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// RowCountersReady reports whether NodeCount and EdgeCount read the counters.
func (s *Store) RowCountersReady() bool {
	return !s.coreless() && s.rowCountersReady.Load()
}

// EnsureRowCounters installs and seeds the counters if they are not already
// (see the file comment). The writer is held twice, briefly: to create the
// triggers and pin the seed snapshot, and to add the seed.
func (s *Store) EnsureRowCounters(ctx context.Context) error {
	if s.coreless() || !rowCountersEnabled() {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.rowCountersInstall.Lock()
	defer s.rowCountersInstall.Unlock()
	// Background work: it never starts while an edit cycle holds the build
	// lane, and its seed count (seconds of reads on a large store) is
	// cancelled when one starts; the lazy loop retries at its next poll.
	if s.cycleYieldEnabled() && s.buildLaneBusy() {
		return errRowCountersEditCycle
	}
	ctx, cancelOnCycle := context.WithCancel(ctx)
	defer cancelOnCycle()
	stopWatch, yielded := s.cancelOnEditCycle(cancelOnCycle)
	defer stopWatch()
	defer func() {
		if yielded.Load() {
			s.walReclaim.cycle.yields.Add(1)
		}
	}()
	ok, err := s.rowCountersInstalled(ctx)
	if err != nil {
		return err
	}
	if ok {
		s.rowCountersReady.Store(true)
		return nil
	}
	s.rowCountersReady.Store(false)
	started := time.Now()
	// Reserve the read connection before taking the writer. Acquiring it
	// while holding the gate would wait behind long pool readers and stall
	// mutations. BeginTx and its first read still pin the seed snapshot only
	// after the trigger transaction commits, under the writer below.
	if rowCountersBeforeReadConnHook != nil {
		rowCountersBeforeReadConnHook()
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// 1. Triggers and zeroed counters, then the seed snapshot, under the
	// writer.
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	locked := true
	unlock := func() {
		if locked {
			locked = false
			s.writeMu.Unlock()
		}
	}
	defer unlock()
	if s.bulkConn != nil {
		return fmt.Errorf("row counters: a bulk window is open")
	}
	tx, releaseInstall, err := s.beginMaintenanceWriteLockedWithin(ctx, rowCountersWriteWait)
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			releaseInstall()
		}
	}()
	for _, ddl := range rowCounterDDL {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("row counters: %w", err)
		}
	}
	for _, name := range sortedTriggerNames() {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+name); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, rowCounterTriggers[name]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("row counters: create %s: %w", name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+rowCountsTable); err != nil {
		_ = tx.Rollback()
		return err
	}
	// Every generation that already owns symbol documents gets a revision
	// row, so a later write to it reads as a change of that generation and
	// never as a new one (index seeks per generation, not a scan).
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO `+symbolFTSRevisionsTable+`(view_gen, rev)
WITH RECURSIVE g(v) AS (
  SELECT (SELECT min(view_gen) FROM symbol_fts_rowid)
  UNION ALL
  SELECT (SELECT min(view_gen) FROM symbol_fts_rowid WHERE view_gen > g.v) FROM g WHERE g.v IS NOT NULL
)
SELECT v, 1 FROM g WHERE v IS NOT NULL`); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("row counters: seed symbol fts revisions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO `+rowCountsState+`(id, seeded, installed_at, seeded_at) VALUES (1, 0, ?, 0)`,
		time.Now().UnixNano()); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The writer connection goes back to the pool before the gate is
	// released: the pool has one, and the next writer (a bulk window
	// opening) needs it.
	installed = true
	releaseInstall()
	snap, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer snap.Rollback() //nolint:errcheck // read-only snapshot
	// The first read starts the snapshot: it must happen before any writer
	// can commit, i.e. while the gate is still held.
	var seededFlag int
	if err := snap.QueryRowContext(ctx, `SELECT seeded FROM `+rowCountsState+` WHERE id = 1`).Scan(&seededFlag); err != nil {
		return err
	}
	unlock()
	if rowCountersAfterPinHook != nil {
		rowCountersAfterPinHook()
	}

	// 2. The seed counts, with no writer held.
	seed, err := exactGenerationCounts(ctx, snap)
	if err != nil {
		return err
	}
	_ = snap.Rollback()
	countedAt := time.Since(started)

	// 3. Add the seed.
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	if s.bulkConn != nil {
		// A window opened while the seed was counted: its rows are not in
		// the snapshot's counts as the triggers saw them only partly. Give
		// up; the next poll installs from scratch (seeded is still 0).
		return errRowCountersBulkWindow
	}
	wtx, releaseSeed, err := s.beginMaintenanceWriteLockedWithin(ctx, rowCountersWriteWait)
	if err != nil {
		return err
	}
	defer releaseSeed()
	defer wtx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	if err := addGenerationCountsTx(ctx, wtx, seed); err != nil {
		return err
	}
	if _, err := wtx.ExecContext(ctx, `UPDATE `+rowCountsState+` SET seeded = 1, seeded_at = ? WHERE id = 1`, time.Now().UnixNano()); err != nil {
		return err
	}
	if err := wtx.Commit(); err != nil {
		return err
	}
	s.rowCountersReady.Store(true)
	var nodes, edges int64
	for _, c := range seed {
		nodes += c.nodes
		edges += c.edges
	}
	log.Printf("store_sqlite: row counters seeded generations=%d nodes=%d edges=%d count=%s elapsed=%s",
		len(seed), nodes, edges, countedAt.Round(time.Millisecond), time.Since(started).Round(time.Millisecond))
	return nil
}

// maintenanceWriteWait bounds the wait for the writer connection of a
// maintenance write taken under writeMu. The writer pool holds one
// connection; a generation bulk window pins it, and only the window's end —
// which needs writeMu — returns it, so an unbounded wait there is a deadlock.
const maintenanceWriteWait = 10 * time.Second

// rowCountersWriteWait bounds the counters' own connection waits: they are
// background work and retry at the next poll.
const rowCountersWriteWait = 500 * time.Millisecond

// beginMaintenanceWriteLocked starts a write transaction for a maintenance
// write the caller makes under writeMu: on the pinned bulk connection while a
// window is open (the pool's only connection is that one), else on a writer
// connection acquired within maintenanceWriteWait. release returns the
// connection after the transaction ends.
func (s *Store) beginMaintenanceWriteLocked(ctx context.Context) (*sql.Tx, func(), error) {
	return s.beginMaintenanceWriteLockedWithin(ctx, maintenanceWriteWait)
}

// beginMaintenanceWriteLockedWithin is beginMaintenanceWriteLocked with the
// connection wait bounded by wait.
func (s *Store) beginMaintenanceWriteLockedWithin(ctx context.Context, wait time.Duration) (*sql.Tx, func(), error) {
	wctx, cancel := context.WithTimeout(ctx, wait)
	conn, release, err := s.activeWriteConnLocked(wctx)
	cancel()
	if err != nil {
		return nil, func() {}, fmt.Errorf("maintenance write: writer connection: %w", err)
	}
	tx, err := s.beginWriteOnContext(ctx, conn)
	if err != nil {
		release()
		return nil, func() {}, err
	}
	return tx, release, nil
}

// errRowCountersBulkWindow defers the counters' install while a generation
// bulk window is open; the next poll installs them again from scratch.
var errRowCountersBulkWindow = errors.New("row counters: a bulk window is open")

// errRowCountersEditCycle defers the install while an edit cycle holds the
// build lane.
var errRowCountersEditCycle = errors.New("row counters: an edit cycle holds the build lane")

// rowCountersBeforeReadConnHook parks seed reader admission in deterministic
// pool-saturation fixtures. nil in production.
var rowCountersBeforeReadConnHook func()

// rowCountersAfterPinHook runs right after the seed snapshot is pinned and
// the writer released (a seam for the case that commits a write there).
var rowCountersAfterPinHook func()

type generationCounts struct{ nodes, edges int64 }

func exactGenerationCounts(ctx context.Context, tx *sql.Tx) (map[int64]generationCounts, error) {
	out := map[int64]generationCounts{}
	for _, q := range []struct {
		sql   string
		edges bool
	}{
		{`SELECT view_gen, count(*) FROM nodes INDEXED BY nodes_by_generation GROUP BY view_gen`, false},
		{`SELECT view_gen, count(*) FROM edges INDEXED BY edges_by_generation GROUP BY view_gen`, true},
	} {
		rows, err := tx.QueryContext(ctx, q.sql)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var gen, n int64
			if err := rows.Scan(&gen, &n); err != nil {
				_ = rows.Close()
				return nil, err
			}
			c := out[gen]
			if q.edges {
				c.edges = n
			} else {
				c.nodes = n
			}
			out[gen] = c
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func addGenerationCountsTx(ctx context.Context, tx *sql.Tx, delta map[int64]generationCounts) error {
	gens := make([]int64, 0, len(delta))
	for g := range delta {
		gens = append(gens, g)
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i] < gens[j] })
	for _, g := range gens {
		c := delta[g]
		if c.nodes == 0 && c.edges == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+rowCountsTable+`(view_gen, nodes, edges) VALUES (?, ?, ?)
  ON CONFLICT(view_gen) DO UPDATE SET nodes = nodes + excluded.nodes, edges = edges + excluded.edges`, g, c.nodes, c.edges); err != nil {
			return err
		}
	}
	return nil
}

// countFromCounters answers NodeCount / EdgeCount from the counters. ok is
// false when they are not ready (the caller counts exactly).
func (s *Store) countFromCounters(column string) (int, bool) {
	if !s.rowCountersReady.Load() {
		return 0, false
	}
	var n int64
	err := s.db.QueryRow(`SELECT `+column+` FROM `+rowCountsTable+` WHERE view_gen = ?`, s.viewGen).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, true
	}
	if err != nil {
		// A dropped table (an external wipe) or a transient error: fall back
		// to the exact count and stop trusting the counters until the next
		// install verifies them.
		s.rowCountersReady.Store(false)
		return 0, false
	}
	return int(n), true
}

// ensureRowCountersUntilStopped runs EnsureRowCounters cancelled by shutdown.
func (s *Store) ensureRowCountersUntilStopped() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-s.stopCheckpoint:
			cancel()
		case <-finished:
		}
	}()
	return s.EnsureRowCounters(ctx)
}

// RowCounterDrift is one generation whose counter disagrees with its rows.
type RowCounterDrift struct {
	GenerationID             int64
	CounterNodes, ExactNodes int64
	CounterEdges, ExactEdges int64
}

// RowCounterCheck is the outcome of CheckRowCounters.
type RowCounterCheck struct {
	Ready       bool
	Generations int
	Drift       []RowCounterDrift
	Repaired    bool
	Elapsed     time.Duration
}

// CheckRowCounters recounts every generation in one read snapshot and compares
// the counters of that same snapshot. With repair, a drift is corrected by
// adding the snapshot's difference under the writer (the counters already
// carry every change committed after the snapshot). Proportional to the whole
// corpus: a deliberate request (daemon status --exact), never a poll.
func (s *Store) CheckRowCounters(ctx context.Context, repair bool) (RowCounterCheck, error) {
	var out RowCounterCheck
	if s.coreless() {
		return out, errMaintenanceNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	defer func() { out.Elapsed = time.Since(started) }()
	out.Ready = s.rowCountersReady.Load()
	if !out.Ready {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only snapshot
	counters := map[int64]generationCounts{}
	rows, err := tx.QueryContext(ctx, `SELECT view_gen, nodes, edges FROM `+rowCountsTable)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var g int64
		var c generationCounts
		if err := rows.Scan(&g, &c.nodes, &c.edges); err != nil {
			_ = rows.Close()
			return out, err
		}
		counters[g] = c
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return out, err
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	exact, err := exactGenerationCounts(ctx, tx)
	if err != nil {
		return out, err
	}
	_ = tx.Rollback()
	delta := map[int64]generationCounts{}
	for g := range unionGenerations(counters, exact) {
		c, e := counters[g], exact[g]
		if c != e {
			out.Drift = append(out.Drift, RowCounterDrift{GenerationID: g, CounterNodes: c.nodes, ExactNodes: e.nodes, CounterEdges: c.edges, ExactEdges: e.edges})
			delta[g] = generationCounts{nodes: e.nodes - c.nodes, edges: e.edges - c.edges}
		}
	}
	out.Generations = len(exact)
	sort.Slice(out.Drift, func(i, j int) bool { return out.Drift[i].GenerationID < out.Drift[j].GenerationID })
	if len(out.Drift) > 0 {
		log.Printf("store_sqlite: row counters drifted generations=%d first=%+v repair=%t", len(out.Drift), out.Drift[0], repair)
	}
	if !repair || len(delta) == 0 {
		return out, nil
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return out, err
	}
	defer s.writeMu.Unlock()
	wtx, releaseRepair, err := s.beginMaintenanceWriteLocked(ctx)
	if err != nil {
		return out, err
	}
	defer releaseRepair()
	defer wtx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	if err := addGenerationCountsTx(ctx, wtx, delta); err != nil {
		return out, err
	}
	if err := wtx.Commit(); err != nil {
		return out, err
	}
	out.Repaired = true
	return out, nil
}

func unionGenerations(a, b map[int64]generationCounts) map[int64]struct{} {
	out := make(map[int64]struct{}, len(a)+len(b))
	for g := range a {
		out[g] = struct{}{}
	}
	for g := range b {
		out[g] = struct{}{}
	}
	return out
}

func sortedTriggerNames() []string {
	out := make([]string, 0, len(rowCounterTriggers))
	for name := range rowCounterTriggers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
