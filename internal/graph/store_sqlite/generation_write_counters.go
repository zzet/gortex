package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
)

// GenerationWriteCounters is the physical write work one generation bulk
// window executed, measured at the writer rather than inferred from what a
// reader returns afterwards.
//
// The two measures answer different questions and are kept apart on purpose:
//
//   - StatementRowChanges is SQLite's own total_changes() delta on the pinned
//     writer connection across the window: every row an INSERT, UPDATE or
//     DELETE statement actually changed there — nodes, edges, sidecars, FTS
//     rowid maps, the FTS virtual tables, masks, producer states and the
//     withdrawal deletes of a fallback context separation alike. It is the
//     "rows the statements executed" count, not a count of rows that survived.
//   - BulkNodeRows and BulkEdgeRows are the node and edge rows the batch
//     writer committed through AddBatch inside the window (its own
//     changed/inserted accounting), the two payload tables the other measure
//     cannot split out.
//
// Neither counts reads, and neither counts rows written outside the window
// (a store without a pinned writer, such as an in-memory one, opens none).
type GenerationWriteCounters struct {
	GenerationID int64
	// WindowOpened reports that a generation bulk window was actually held; a
	// false value means the other fields are zero because nothing was
	// measured, not because nothing was written.
	WindowOpened        bool
	StatementRowChanges int64
	BulkNodeRows        int64
	BulkEdgeRows        int64
	// Batches counts the AddBatch commits the window observed.
	Batches int64
}

type generationWriteCounterKey struct {
	core         *storeCore
	generationID int64
}

type generationWriteCounterEntry struct {
	counters     GenerationWriteCounters
	changesStart int64
	closed       bool
	seq          uint64
}

// generationWriteCounterCap bounds the counters retained for builds whose
// caller never collected them, so an uncollected counter can never grow the
// process without limit. The oldest entry is dropped first.
const generationWriteCounterCap = 256

var generationWriteCounterRegistry = struct {
	sync.Mutex
	seq     uint64
	entries map[generationWriteCounterKey]*generationWriteCounterEntry
}{entries: map[generationWriteCounterKey]*generationWriteCounterEntry{}}

func connTotalChanges(ctx context.Context, conn *sql.Conn) (int64, bool) {
	if conn == nil {
		return 0, false
	}
	var n int64
	if err := conn.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// noteGenerationWriteWindowOpen starts the counters for one generation window.
// The caller holds writeMu and has just installed conn as the pinned writer.
func (s *Store) noteGenerationWriteWindowOpen(ctx context.Context, conn *sql.Conn, generationID int64) {
	if s.coreless() || generationID <= baseViewGeneration {
		return
	}
	start, _ := connTotalChanges(ctx, conn)
	reg := &generationWriteCounterRegistry
	reg.Lock()
	defer reg.Unlock()
	reg.seq++
	reg.entries[generationWriteCounterKey{s.storeCore, generationID}] = &generationWriteCounterEntry{
		counters:     GenerationWriteCounters{GenerationID: generationID, WindowOpened: true},
		changesStart: start,
		seq:          reg.seq,
	}
	if len(reg.entries) > generationWriteCounterCap {
		var oldestKey generationWriteCounterKey
		var oldest uint64
		first := true
		for key, entry := range reg.entries {
			if first || entry.seq < oldest {
				oldestKey, oldest, first = key, entry.seq, false
			}
		}
		delete(reg.entries, oldestKey)
	}
}

// noteGenerationBulkRows adds one committed AddBatch to the open window's
// counters. The caller holds writeMu.
func (s *Store) noteGenerationBulkRows(nodeRows, edgeRows int) {
	if s.coreless() || s.generationBulkLoad <= baseViewGeneration {
		return
	}
	reg := &generationWriteCounterRegistry
	reg.Lock()
	defer reg.Unlock()
	entry := reg.entries[generationWriteCounterKey{s.storeCore, s.generationBulkLoad}]
	if entry == nil || entry.closed {
		return
	}
	entry.counters.BulkNodeRows += int64(nodeRows)
	entry.counters.BulkEdgeRows += int64(edgeRows)
	entry.counters.Batches++
}

// noteGenerationWriteWindowClose seals the counters for the window about to
// be released. The caller holds writeMu and conn is still the pinned writer.
func (s *Store) noteGenerationWriteWindowClose(ctx context.Context, conn *sql.Conn, generationID int64) {
	if s.coreless() || generationID <= baseViewGeneration {
		return
	}
	end, ok := connTotalChanges(ctx, conn)
	reg := &generationWriteCounterRegistry
	reg.Lock()
	defer reg.Unlock()
	entry := reg.entries[generationWriteCounterKey{s.storeCore, generationID}]
	if entry == nil || entry.closed {
		return
	}
	if ok && end >= entry.changesStart {
		entry.counters.StatementRowChanges = end - entry.changesStart
	}
	entry.closed = true
}

// TakeGenerationWriteCounters returns and forgets the write counters recorded
// for generationID's bulk window on this database. ok is false when no window
// was recorded for it (none was opened, or the entry was already taken or
// aged out).
func (s *Store) TakeGenerationWriteCounters(generationID int64) (GenerationWriteCounters, bool) {
	if s.coreless() {
		return GenerationWriteCounters{}, false
	}
	reg := &generationWriteCounterRegistry
	reg.Lock()
	defer reg.Unlock()
	key := generationWriteCounterKey{s.storeCore, generationID}
	entry := reg.entries[key]
	if entry == nil {
		return GenerationWriteCounters{}, false
	}
	delete(reg.entries, key)
	return entry.counters, true
}

// GenerationPayloadRowCensus is the physical row population one payload
// generation carries, per generation-keyed table. It reads the rows stamped
// with the generation's own view_gen — never a composed view — so a row the
// generation inherits from a layer beneath it is not counted, and a row
// counted here is a row this generation physically stores.
type GenerationPayloadRowCensus struct {
	GenerationID int64
	// Tables maps each table carrying a view_gen column to its row count for
	// the generation. Tables with zero rows are included.
	Tables map[string]int64
	// NodeFiles is the number of distinct file paths the generation's node
	// rows sit at: the files it materialised payload for.
	NodeFiles int64
	// EdgeFiles is the number of distinct file paths its edge rows sit at.
	EdgeFiles int64
}

// Total is the sum of every table's row count.
func (c GenerationPayloadRowCensus) Total() int64 {
	var total int64
	for _, n := range c.Tables {
		total += n
	}
	return total
}

// GenerationPayloadRowCensus counts the rows one generation physically stores
// in every table that carries a view_gen column, discovered from the live
// schema so a table added later is counted without editing this function. It
// is a diagnostic read (one COUNT per table, each on the table's
// generation-leading key or index) and is never on a build's path.
func (s *Store) GenerationPayloadRowCensus(ctx context.Context, generationID int64) (GenerationPayloadRowCensus, error) {
	census := GenerationPayloadRowCensus{GenerationID: generationID, Tables: map[string]int64{}}
	if s.coreless() {
		return census, fmt.Errorf("%w: a row census needs an open store", ErrCatalogInvalidValue)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT m.name FROM sqlite_master AS m
WHERE m.type = 'table'
  AND EXISTS (SELECT 1 FROM pragma_table_info(m.name) AS c WHERE c.name = 'view_gen')
ORDER BY m.name`)
	if err != nil {
		return census, fmt.Errorf("store_sqlite: list generation-keyed tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return census, err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return census, fmt.Errorf("store_sqlite: list generation-keyed tables: %w", err)
	}
	if err := rows.Close(); err != nil {
		return census, err
	}
	sort.Strings(tables)
	// Restating `view_gen > 0` literally lets SQLite use the partial
	// generation indexes (see generationPayloadEmpty).
	positive := ""
	if generationID > baseViewGeneration {
		positive = " AND view_gen > 0"
	}
	for _, table := range tables {
		var n int64
		// The table name comes from sqlite_master, never from a caller, and is
		// quoted as an identifier.
		q := fmt.Sprintf(`SELECT COUNT(*) FROM "%s" WHERE view_gen = ?%s`, table, positive)
		if err := s.db.QueryRowContext(ctx, q, generationID).Scan(&n); err != nil {
			return census, fmt.Errorf("store_sqlite: count %s rows for generation %d: %w", table, generationID, err)
		}
		census.Tables[table] = n
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT file_path) FROM nodes WHERE view_gen = ?`+positive, generationID).Scan(&census.NodeFiles); err != nil {
		return census, fmt.Errorf("store_sqlite: count node files for generation %d: %w", generationID, err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT file_path) FROM edges WHERE view_gen = ?`+positive, generationID).Scan(&census.EdgeFiles); err != nil {
		return census, fmt.Errorf("store_sqlite: count edge files for generation %d: %w", generationID, err)
	}
	return census, nil
}
