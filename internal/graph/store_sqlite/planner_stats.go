package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
)

// plannerStatsAnalysisLimit bounds how many entries ANALYZE samples per
// named graph index. The graph store only needs coarse relative cardinalities:
// without any sqlite_stat1 rows the planner costs alternative indexes by
// IN-probe count alone, and on a 480k-node workspace that served the hottest
// file projection from nodes_by_kind instead of the selective file index.
const plannerStatsAnalysisLimit = 1000

// plannerStatsIndexQuery is the synchronous, query-plan-critical subset of the
// graph indexes. Approximate ANALYZE still counts B-tree pages, so sampling all
// 17 named indexes traversed about 1.64 GiB and consumed 35 seconds on a cold
// production store. Most graph indexes either have no competing left-prefix
// access path or are explicitly selected with INDEXED BY; statistics do not
// change those plans. The exceptions below participate in real planner choices:
// node lookup/join order, edge-kind join order, and exact-site lookups where
// edges_by_from_line must beat the from_id-leading table key.
//
// Keep this list paired with the EXPLAIN plan locks in
// planner_stats_checkpoint_test.go. A new index belongs here only when a plan
// test demonstrates that its statistics change a production query choice.
// plannerStatsIndexQuery lists the planner indexes present in the schema:
// every index plannerStatsIndexProbes names
// (TestPlannerStatsCoversEveryIndexOnNodesAndEdges fails when the schema gains
// an index on nodes or edges that is in neither that map nor
// plannerStatsIndexesOutside).
var plannerStatsIndexQuery = buildPlannerStatsIndexQuery()

// plannerStatsIndexesOutside are the indexes on nodes and edges the statistics
// check leaves out, each with the reason its choice does not rest on a
// statistics row: no other index shares its leading columns (the planner takes
// it by shape), or it is partial with a predicate no read that takes a list of
// keys implies. Every other index is a planner index (plannerStatsIndexProbes):
// it competes with another on a left prefix, and a missing row there hands the
// choice to the default cost model.
var plannerStatsIndexesOutside = map[string]string{
	"edges_by_file":                 "(file_path, kind): only edges_by_file_generation shares file_path, and the default model prefers the one that keys more of a statement",
	"edges_by_file_generation":      "(file_path, view_gen): the lazy index, see edges_by_file",
	"edges_by_from":                 "(view_gen, from_id, kind): its rivals edges_by_from_line and edges_by_from_line_kind carry the rows",
	"edges_by_to":                   "(view_gen, to_id, kind): no other index leads with to_id",
	"nodes_by_qual":                 "partial, qual_name <> '': the qualified-name reads only",
	"nodes_missing_workspace_slugs": "partial, workspace_id = '' OR project_id = '': the slug backfill only",
	"nodes_repo_files":              "partial, kind = 'file': the repository file listing only",
	"edges_by_unresolved":           "partial, is_unresolved = 1: the resolver's unresolved scan only",
	"edges_fnvalue_prefixed":        "partial, to_id LIKE '%::unresolved::fnvalue::%': the fnvalue pass only",
	"edges_external":                "partial, the external-call target predicate: the external-call synthesizer only",
}

func buildPlannerStatsIndexQuery() string {
	names := make([]string, 0, len(plannerStatsIndexProbes))
	for name := range plannerStatsIndexProbes {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]string, 0, len(names))
	for _, name := range names {
		values = append(values, "('"+name+"')")
	}
	return `
WITH critical(name) AS (VALUES
  ` + strings.Join(values, ",\n  ") + `
)
SELECT schema_index.name
FROM sqlite_schema AS schema_index
JOIN critical ON critical.name = schema_index.name
WHERE schema_index.type = 'index'
  AND schema_index.sql IS NOT NULL
ORDER BY schema_index.name`
}

// plannerStatsTableExistsQuery probes the catalog: sqlite_stat1 is created by
// the first ANALYZE and does not exist before it.
const plannerStatsTableExistsQuery = `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_stat1')`

// nodesGoReceiverTypePredicate mirrors the WHERE clause of the
// nodes_go_receiver_type DDL in bulk_load.go (bulkAlwaysLiveIndexes).
const nodesGoReceiverTypePredicate = `language = 'go' AND kind IN ('type', 'interface') AND name <> '' AND file_path <> ''`

// plannerStatsSuspectRows bounds the believed cardinality that is worth a
// synchronous verification count on Open.
//
// Only a believed cardinality of a few rows can invert a join order: the
// literal zero row ANALYZE writes for an empty partial index, or a natural row
// captured while the index held 1-3 entries that later grew. Plans are correct
// from a believed count in the single digits on every fixture measured here,
// and well before 64. Anything larger is ordinary staleness, which must not
// buy a synchronous whole-index ANALYZE (about 1.8 GiB of index pages on a
// 12 GB store) at every daemon start. Keeping statistics fresh as a store
// grows at runtime is a separate follow-up; this rule exists only to catch the
// misplanning regime.
const plannerStatsSuspectRows = 64

// plannerStatsIndexProbe describes how to interrogate one critical index. For
// a non-partial index "does it hold entries" is just "is the owning table
// non-empty"; for a partial index every question must repeat the index's own
// predicate and read through INDEXED BY, so the answer is the index's
// population rather than the table's.
//
// The predicate is stored once and BOTH the existence probe and the bounded
// verification count are derived from it, so the two can never drift apart.
type plannerStatsIndexProbe struct {
	// table is the relation the index is defined on.
	table string
	// predicate is the partial index's WHERE clause, verbatim from its DDL.
	// Empty for non-partial indexes.
	predicate string
	// partial marks the indexes whose DDL carries a WHERE clause. Only these
	// can be legitimately empty over a non-empty table, which is exactly the
	// state that must never be recorded as a zero stat row.
	partial bool
}

// existsQuery builds the single-column EXISTS probe returning 0/1.
func (p plannerStatsIndexProbe) existsQuery(index string) string {
	if !p.partial {
		return `SELECT EXISTS(SELECT 1 FROM ` + p.table + `)`
	}
	return `SELECT EXISTS(SELECT 1 FROM ` + p.table + ` INDEXED BY ` + quoteSQLiteIdentifier(index) +
		` WHERE ` + p.predicate + `)`
}

// countQuery builds a bounded count of the entries the partial index actually
// holds. The count is NOT index-only: language, kind and file_path are not
// index columns, so satisfying the predicate probes the WITHOUT ROWID nodes
// table once per index entry. The LIMIT is what makes it affordable on every
// Open — the caller binds at most 2*plannerStatsSuspectRows+1, so the scan
// stops after a couple of hundred table probes no matter how large the index
// is. It is also the only way to tell a truthful small stat row apart from the
// poisoned '0 0 0 0 0' row that flips the receiver-rebind join order
// (issue #651).
func (p plannerStatsIndexProbe) countQuery(index string) string {
	return `SELECT count(*) FROM (SELECT 1 FROM ` + p.table + ` INDEXED BY ` + quoteSQLiteIdentifier(index) +
		` WHERE ` + p.predicate + ` LIMIT ?)`
}

// plannerStatsIndexProbes must stay paired with the index DDL in bulk_load.go
// (bulkDroppableIndexes / bulkAlwaysLiveIndexes) and with the critical list in
// plannerStatsIndexQuery above. A drifted predicate fails loudly rather than
// silently: SQLite rejects INDEXED BY when the partial index cannot serve the
// stated WHERE clause, so the probe errors instead of reporting a wrong
// population.
var plannerStatsIndexProbes = map[string]plannerStatsIndexProbe{
	// Non-partial edge indexes.
	"edges_by_from_line":      {table: "edges"},
	"edges_by_from_line_kind": {table: "edges"},
	"edges_by_generation":     {table: "edges"},
	"edges_by_kind":           {table: "edges"},
	// Non-partial node indexes.
	"nodes_by_file":       {table: "nodes"},
	"nodes_by_generation": {table: "nodes"},
	"nodes_by_kind":       {table: "nodes"},
	"nodes_by_name":       {table: "nodes"},
	"nodes_by_repo":       {table: "nodes"},
	"nodes_by_repo_kind":  {table: "nodes"},
	// Partial node indexes: every question is asked through the index with
	// its own predicate.
	"nodes_by_repo_language_name": {
		table:     "nodes",
		predicate: `name <> ''`,
		partial:   true,
	},
	"nodes_go_receiver_type": {
		table:     "nodes",
		predicate: nodesGoReceiverTypePredicate,
		partial:   true,
	},
}

// refreshPlannerStatsLocked recomputes sqlite_stat1 for the named graph
// indexes on the active write connection. Callers hold writeMu. PRAGMA optimize
// is intentionally not used here: it is driven by per-connection query history,
// while the cold pinned connection served writes only; forcing its 0x10000 bit
// would consider every FTS and sidecar table in the database.
func (s *Store) refreshPlannerStatsLocked(ctx context.Context) error {
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return err
	}
	defer release()
	return refreshPlannerStatsOnConn(ctx, conn)
}

// analyzePlannerStatsIndexLocked rebuilds ONE index's statistics under a write
// gate the caller has just taken. It is the unit of work the cooperative
// runtime refresh holds the gate for: the batch above is right for a cold-load
// finalize, which already owns the store, and wrong for a live daemon, where
// the gate sits underneath the reach-topology writer gate, the batch mutation
// gate and a repository lane.
func (s *Store) analyzePlannerStatsIndexLocked(ctx context.Context, name string) (removedStatRow bool, err error) {
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	hasStatTable, err := preparePlannerStatsConn(ctx, conn)
	if err != nil {
		return false, err
	}
	return analyzePlannerStatsIndexOnConn(ctx, conn, name, hasStatTable)
}

// reloadPlannerStatsLocked reloads sqlite_stat1 after a deletion, under a write
// gate the caller has just taken. Best-effort in full: a reload that cannot run
// leaves the deleted row absent for every connection opened from here on, and
// the next successful refresh reloads it for the ones already open.
func (s *Store) reloadPlannerStatsLocked(ctx context.Context) {
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		log.Printf("store_sqlite: planner stats reload failed error=%q", err)
		return
	}
	defer release()
	reloadPlannerStatsOnConn(ctx, conn)
}

func refreshPlannerStatsOnConn(ctx context.Context, conn *sql.Conn) error {
	// Materialize the tiny schema result and close its cursor before issuing
	// ANALYZE on the same physical connection. Re-entering a pinned connection
	// with an open rows cursor can deadlock database/sql.
	indexes, err := plannerStatsIndexesOnConn(ctx, conn)
	if err != nil {
		return err
	}

	hasStatTable, err := preparePlannerStatsConn(ctx, conn)
	if err != nil {
		return err
	}

	removedStatRow := false
	for _, name := range indexes {
		removed, err := analyzePlannerStatsIndexOnConn(ctx, conn, name, hasStatTable)
		if err != nil {
			return err
		}
		removedStatRow = removedStatRow || removed
	}
	if removedStatRow {
		reloadPlannerStatsOnConn(ctx, conn)
	}
	return nil
}

// plannerStatsIndexesOnConn reads the critical-index list through a pinned
// connection, fully materialized so no cursor is open when ANALYZE runs on the
// same physical connection.
func plannerStatsIndexesOnConn(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, plannerStatsIndexQuery)
	if err != nil {
		return nil, err
	}
	var indexes []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		indexes = append(indexes, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return indexes, nil
}

// preparePlannerStatsConn binds the sampling budget and answers whether
// sqlite_stat1 exists at all.
//
// analysis_limit is a CONNECTION setting, so it has to be bound on whichever
// connection is about to ANALYZE. The cooperative runtime refresh acquires a
// connection per index, which is exactly why this is a separate step rather
// than a preamble inside the loop above.
//
// The catalog probe is part of the best-effort hygiene described in
// analyzePlannerStatsIndexOnConn, so its failure is logged rather than
// returned: assuming the table exists costs at most a DELETE that matches
// nothing, while failing here would abort a cold load.
func preparePlannerStatsConn(ctx context.Context, conn *sql.Conn) (hasStatTable bool, err error) {
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA analysis_limit=%d`, plannerStatsAnalysisLimit)); err != nil {
		return false, err
	}
	hasStatTable = true
	if err := conn.QueryRowContext(ctx, plannerStatsTableExistsQuery).Scan(&hasStatTable); err != nil {
		log.Printf("store_sqlite: planner stats catalog probe failed error=%q", err)
		hasStatTable = true
	}
	return hasStatTable, nil
}

// analyzePlannerStatsIndexOnConn rebuilds ONE index's statistics, and reports
// whether it removed a stat row instead (which obliges the caller to reload).
//
// ANALYZE on an empty partial index writes a literal '0 0 0 0 0' stat row, and
// a believed-zero cardinality is worse than no statistics at all: with no row
// SQLite falls back to its default cost model, which plans these queries
// correctly, while a zero row convinces the planner the index is free to scan
// and it hoists that relation to the outermost loop (issue #651 turned the
// receiver rebind into O(types * member_of) that way). So never ANALYZE an
// empty partial critical index; drop any row it left behind instead.
//
// Every step of that hygiene is best-effort by design. ANALYZE itself stays a
// hard error as it always was, but a failed probe, a failed DELETE or a failed
// reload must not fail the caller: Store.EndCoordinatedBulkLoad joins the
// batch refresh's error into the cold-load result, and the multi-repo pipeline
// aborts on it. Losing a statistics refinement is a planning regression;
// aborting a cold load loses the index.
func analyzePlannerStatsIndexOnConn(ctx context.Context, conn *sql.Conn, name string, hasStatTable bool) (removedStatRow bool, err error) {
	if spec, known := plannerStatsIndexProbes[name]; known && spec.partial {
		var hasRows bool
		if err := conn.QueryRowContext(ctx, spec.existsQuery(name)).Scan(&hasRows); err != nil {
			// Fall through to the plain ANALYZE: without a usable probe the
			// safest assumption is that the index is populated, which is the
			// branch that behaves exactly as it did before this hygiene
			// existed.
			log.Printf("store_sqlite: planner stats probe failed index=%s error=%q", name, err)
		} else if !hasRows {
			if hasStatTable {
				// Index names are unique across the whole schema, so matching
				// on idx alone is exact today and stays exact if a partial
				// critical index ever lands on edges.
				res, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1 WHERE idx = ?`, name)
				if err != nil {
					log.Printf("store_sqlite: planner stats clear failed index=%s error=%q", name, err)
				} else if affected, err := res.RowsAffected(); err == nil && affected > 0 {
					removedStatRow = true
				}
			}
			return removedStatRow, nil
		}
	}
	if _, err := conn.ExecContext(ctx, `ANALYZE `+quoteSQLiteIdentifier(name)); err != nil {
		return removedStatRow, fmt.Errorf("analyze graph index %s: %w", name, err)
	}
	return removedStatRow, nil
}

// reloadPlannerStatsOnConn makes a direct DELETE from sqlite_stat1 visible.
//
// The deletion is invisible to the connection's cached statistics until they
// are reloaded. ANALYZE on the schema table reloads sqlite_stat1 without
// recomputing anything, so this connection stops planning against the row that
// was just removed. Best-effort, as above.
func reloadPlannerStatsOnConn(ctx context.Context, conn *sql.Conn) {
	if _, err := conn.ExecContext(ctx, `ANALYZE sqlite_schema`); err != nil {
		log.Printf("store_sqlite: planner stats reload failed error=%q", err)
	}
}

func quoteSQLiteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// plannerStatsRepairReason decides whether an already-open store needs its
// graph-index statistics rebuilt, and names why. It repairs rather than merely
// backfills: a store can carry statistics that exist and are actively wrong.
//
// The rules, all cheap catalog / index-only probes:
//
//	R0 sqlite_stat1 absent entirely                          -> "no_stats"
//	R1 a critical index present in the schema has no stat row
//	   while the index actually holds entries                -> "missing:<idx>"
//	R2 a partial critical index carries a stat row believing
//	   at most plannerStatsSuspectRows entries while it holds
//	   more than twice that many (the '0 0 0 0 0' row ANALYZE
//	   writes for an empty partial index is the common case)  -> "tiny_stat:<idx> …"
//	R3 a partial critical index carries a stat row while
//	   holding no entries at all                              -> "stale_stat:<idx>"
//
// R3 is the leftover of an older engine that did ANALYZE an empty partial
// index: the row believes zero (or near zero) and nothing about the index has
// changed, so R2's "holds materially more" test can never fire. Absence is the
// correct state for an empty partial index — SQLite falls back to a sane
// default cost model with no row and believes a zero one — so the repair is a
// deletion, which refreshPlannerStatsOnConn already performs on the same
// no-entries branch. The next Open then converges: no row plus no entries is
// R1's "nothing to do". The extra EXISTS costs one probe per partial critical
// index and is only reached when the believed count is already suspect.
//
// R1 subsumes the older existence check on nodes_by_file / edges_by_from_line_kind
// and extends it to every critical index. R2 is what the older check could not
// see at all: the row was present, so the store looked healthy while the
// receiver-rebind join order stayed inverted (issue #651). R2 runs over every
// partial critical index rather than the receiver index alone — the same
// degenerate row can be written for any of them — but only when the believed
// count is already small enough to invert a join order, so ordinary staleness
// on a large index never buys a synchronous ANALYZE.
//
// The caller still gates on a non-empty nodes table; an empty store has
// nothing to analyze. Errors are never fatal — an unreadable probe reports
// "no repair needed" and the store keeps running on whatever stats it has.
func plannerStatsRepairReason(ctx context.Context, db *sql.DB) (string, bool) {
	var hasTable bool
	// sqlite_stat1 does not exist until the first ANALYZE, so probe the catalog
	// before the table.
	if err := db.QueryRowContext(ctx, plannerStatsTableExistsQuery).Scan(&hasTable); err != nil {
		return "", false
	}
	if !hasTable {
		return "no_stats", true
	}

	// Materialize the critical-index list before probing. The writer pool is a
	// single-connection pool: issuing another query while this cursor is open
	// would deadlock.
	rows, err := db.QueryContext(ctx, plannerStatsIndexQuery)
	if err != nil {
		return "", false
	}
	var indexes []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return "", false
		}
		indexes = append(indexes, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", false
	}
	if err := rows.Close(); err != nil {
		return "", false
	}

	// R1: a critical index that holds entries but has no stat row at all.
	statPresent := make(map[string]bool, len(indexes))
	for _, name := range indexes {
		spec, known := plannerStatsIndexProbes[name]
		if !known {
			continue
		}
		var hasStat bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_stat1 WHERE idx = ?)`, name).Scan(&hasStat); err != nil {
			return "", false
		}
		if hasStat {
			statPresent[name] = true
			continue
		}
		var hasRows bool
		if err := db.QueryRowContext(ctx, spec.existsQuery(name)).Scan(&hasRows); err != nil {
			return "", false
		}
		if hasRows {
			return "missing:" + name, true
		}
	}

	// R2: any critical index whose stat row believes a handful of entries
	// while the index holds materially more. The bounded count reads at most
	// believed*2+1 entries, so a literal zero row costs one index entry and the
	// largest suspect row costs only a couple of hundred. The parser clamps a
	// missing, malformed, or negative leading token to zero, so LIMIT is always
	// positive and can never take SQLite's unbounded -1 meaning.
	for _, name := range indexes {
		spec, known := plannerStatsIndexProbes[name]
		if !known || !statPresent[name] {
			continue
		}
		believed := plannerStatsBelievedRows(ctx, db, name)
		if believed > plannerStatsSuspectRows {
			continue
		}
		// R3 remains partial-only: absence is preferable to a stat row for
		// an empty partial index. A dense index can legitimately describe an
		// empty table, so its zero row must remain stable rather than churn.
		if spec.partial {
			var hasRows bool
			if err := db.QueryRowContext(ctx, spec.existsQuery(name)).Scan(&hasRows); err != nil {
				return "", false
			}
			if !hasRows {
				return "stale_stat:" + name, true
			}
		}
		limit := believed*2 + 1
		countQuery := spec.countQuery(name)
		if !spec.partial {
			countQuery = `SELECT count(*) FROM (SELECT 1 FROM ` + spec.table +
				` INDEXED BY ` + quoteSQLiteIdentifier(name) + ` LIMIT ?)`
		}
		var actual int64
		if err := db.QueryRowContext(ctx, countQuery, limit).Scan(&actual); err != nil {
			return "", false
		}
		if actual > 0 && believed*2 < actual {
			return fmt.Sprintf("tiny_stat:%s believed=%d actual>=%d", name, believed, actual), true
		}
	}
	return "", false
}

// plannerStatsBelievedRows reads the cardinality SQLite currently believes an
// index has: sqlite_stat1.stat is a space-separated list whose first token is
// the estimated row count. A missing or unparsable row reads as zero, which is
// the value that matters — zero is exactly the poisoned state.
//
// sqlite_stat1 carries no UNIQUE constraint, so a hand-edited store can hold
// two rows for one index. Order by the leading count and take the smallest:
// SQLite's CAST reads the numeric prefix of the text, so this is the row that
// would misplan worst, and a duplicate can therefore only trigger a repair,
// never hide one. ANALYZE rewrites the index's rows, so the repair also
// collapses the duplicate.
func plannerStatsBelievedRows(ctx context.Context, db *sql.DB, index string) int64 {
	var stat sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT stat FROM sqlite_stat1 WHERE idx = ? ORDER BY CAST(stat AS INTEGER) LIMIT 1`, index).Scan(&stat); err != nil {
		return 0
	}
	if !stat.Valid {
		return 0
	}
	fields := strings.Fields(stat.String)
	if len(fields) == 0 {
		return 0
	}
	believed, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || believed < 0 {
		return 0
	}
	return believed
}

// healPlannerStats repairs sqlite_stat1 for populated stores opened without —
// or with actively misleading — graph-index statistics. Cold loads refresh
// stats at coordinated-bulk finalize; a warm restart of a store written before
// the engine kept planner statistics, or one whose receiver-index stat row was
// written while the index was still empty, would otherwise plan badly for the
// rest of its life. Never fatal: a store without stats still answers every
// query, just through the default cost model.
//
// It reports whether sqlite_stat1 was actually rewritten, and why, because the
// rewrite is only visible to the connection that performed it: the caller has
// to recycle the read pool for the repair to reach readers at all, and stamp
// the freshness ledger so the runtime checker knows an ANALYZE already ran.
func healPlannerStats(db *sql.DB) (repaired bool, reason string) {
	ctx := context.Background()
	reason, needsRepair := plannerStatsRepairReason(ctx, db)
	if !needsRepair {
		return false, ""
	}
	var hasNodes bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM nodes)`).Scan(&hasNodes); err != nil || !hasNodes {
		return false, ""
	}
	started := time.Now()
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, ""
	}
	defer conn.Close()
	if err := refreshPlannerStatsOnConn(ctx, conn); err != nil {
		log.Printf("store_sqlite: planner stats heal failed error=%q", err)
		return false, ""
	}
	log.Printf("store_sqlite: planner stats repair reason=%s elapsed=%s", reason, time.Since(started))
	return true, reason
}
