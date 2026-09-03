package store_sqlite

import (
	"strings"
	"testing"
)

// Plan locks for the shapes the production-scale SQL sweep found misplanning.
// Each query text mirrors its production site (kept in sync by hand — the
// access path lives in the FROM/WHERE text, and drifting a production query
// without updating its lock is exactly the failure these tests exist to
// catch). Properties, not index names, wherever two indexes would be equally
// correct.
func TestSweepPlanLocks(t *testing.T) {
	s := newPlanLockFixture(t)

	cases := []struct {
		name   string
		query  string
		args   int
		want   []string
		forbid []string
	}{
		{
			// store.go stmtGetNodeByQual: the literal qual_name <> ''
			// conjunct is what admits the partial nodes_by_qual index; a
			// bound parameter alone cannot be proven non-empty and the
			// statement scans all nodes (measured on a production store).
			name:   "get_node_by_qual",
			query:  `SELECT ` + lookupNodeCols + ` FROM nodes WHERE qual_name = ? AND qual_name <> '' AND view_gen = ? ORDER BY id LIMIT 1`,
			args:   2,
			want:   []string{"nodes_by_qual (qual_name=?)"},
			forbid: []string{"SCAN nodes"},
		},
		{
			// edgeExactDeleteByIdentitySQL: the IN-over-JOIN shape drives
			// from the VALUES list into the edges primary key. The prior
			// correlated EXISTS scanned all edges per chunk. The trailing
			// generation binding completes that key rather than filtering
			// after it, so the seek gains a column instead of losing one.
			name:  "edge_exact_identity_delete",
			query: edgeExactDeleteByIdentitySQL("(?,?,?,?,?),(?,?,?,?,?),(?,?,?,?,?)"),
			args:  16,
			want: []string{
				"SEARCH e USING COVERING INDEX sqlite_autoindex_edges_1 (from_id=? AND to_id=? AND kind=? AND file_path=? AND line=? AND view_gen=?)",
			},
			forbid: []string{"SCAN edges", "SCAN e"},
		},
		{
			// lsp_projection ProjectFileNodes: requested_files must drive
			// nodes_by_file; the repo predicate is demoted so the planner
			// cannot read the whole repo per changed-file batch.
			name: "lsp_project_file_nodes",
			query: `
WITH requested_languages(language) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), requested_files(file_path) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT ` + qualifiedNodeColumns("n", lookupNodeColsLight) + `
FROM requested_files AS f
CROSS JOIN nodes AS n
JOIN requested_languages AS l ON l.language = n.language
WHERE n.file_path = f.file_path
  AND +n.repo_prefix = ?
  AND n.kind NOT IN (?, ?)
  AND n.view_gen = ?
ORDER BY n.file_path, n.id`,
			args:   6,
			want:   []string{"SEARCH n USING INDEX nodes_by_file (file_path=?)"},
			forbid: []string{"nodes_by_repo", "SCAN n"},
		},
		{
			// lsp_projection ProjectFileEdges: files drive nodes, nodes
			// drive edges_by_from — never a whole-repo read.
			name: "lsp_project_file_edges",
			query: `
WITH requested_languages(language) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), requested_files(file_path) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT ` + lookupQualifiedEdgeCols + `
FROM requested_files AS f
CROSS JOIN nodes AS n
JOIN requested_languages AS l ON l.language = n.language
CROSS JOIN edges AS e
WHERE n.file_path = f.file_path
  AND e.from_id = n.id
  AND +n.repo_prefix = ?
  AND e.kind NOT IN (?, ?, ?, ?, ?, ?)
  AND n.view_gen = ? AND e.view_gen = n.view_gen
ORDER BY e.from_id, e.to_id, e.kind, e.file_path, e.line`,
			args: 10,
			want: []string{
				"SEARCH n USING INDEX nodes_by_file (file_path=?)",
				"SEARCH e USING INDEX edges_by_from", "(from_id=?",
			},
			forbid: []string{"nodes_by_repo", "SCAN n", "SCAN e"},
		},
		{
			name: "lsp_project_file_edges_by_kinds",
			query: `
WITH requested_languages(language) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), requested_files(file_path) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), requested_kinds(kind) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT ` + lookupQualifiedEdgeCols + `
FROM requested_files AS f
CROSS JOIN nodes AS n
JOIN requested_languages AS l ON l.language = n.language
CROSS JOIN edges AS e
JOIN requested_kinds AS k ON k.kind = e.kind
WHERE n.file_path = f.file_path
  AND e.from_id = n.id
  AND +n.repo_prefix = ?
  AND n.view_gen = ? AND e.view_gen = n.view_gen
ORDER BY e.from_id, e.to_id, e.kind, e.file_path, e.line`,
			args: 5,
			want: []string{
				"SEARCH n USING INDEX nodes_by_file (file_path=?)",
				"SEARCH e USING INDEX edges_by_from", "(from_id=?",
			},
			forbid: []string{"nodes_by_repo", "SCAN n", "SCAN e"},
		},
		{
			// store.go stmtOutEdges: the adjacency read declares a total
			// order so first-match callers agree across backends. It has to
			// stay free — the ORDER BY leads with edges_by_from_line's second
			// column and breaks ties on the rowid that index already carries,
			// so the walk itself satisfies it. A sorter here would tax every
			// out-edge lookup in the daemon.
			name:   "out_edges_ordered",
			query:  `SELECT ` + lookupEdgeCols + ` FROM edges WHERE from_id = ? AND view_gen = ? ORDER BY line, id`,
			args:   2,
			want:   []string{"SEARCH edges USING INDEX edges_by_from_line (from_id=?)"},
			forbid: []string{"SCAN edges", "TEMP B-TREE"},
		},
		{
			// store.go stmtInEdges: same contract on the reverse adjacency,
			// riding edges_by_to's (to_id, kind) prefix plus the rowid.
			name:   "in_edges_ordered",
			query:  `SELECT ` + lookupEdgeCols + ` FROM edges WHERE to_id = ? AND view_gen = ? ORDER BY kind, id`,
			args:   2,
			want:   []string{"SEARCH edges USING INDEX edges_by_to (to_id=?)"},
			forbid: []string{"SCAN edges", "TEMP B-TREE"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainQueryPlan(t, s, tc.query, tc.args)
			joined := strings.Join(plan, "\n")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Fatalf("plan missing %q:\n%s", want, joined)
				}
			}
			for _, forbid := range tc.forbid {
				if strings.Contains(joined, forbid) {
					t.Fatalf("plan contains forbidden %q:\n%s", forbid, joined)
				}
			}
		})
	}
}

// TestAdjacencyPlanLocksDuringBulkLoad pins the OTHER regime the adjacency
// reads run in. BeginBulkLoad drops every index in bulkDroppableIndexes for the
// head of a proven cold load — including edges_by_from_line and edges_by_to,
// the two the ordered locks above depend on — and rebuilds them at FlushBulk.
// A read served inside that window therefore cannot get its ORDER BY for free:
// the out-edge lookup falls back to the edges UNIQUE key and the in-edge lookup
// to a full table scan, and both pay a per-call TEMP B-TREE sort.
//
// That is the accepted trade-off, not a regression: the window is a cold load
// with no interactive readers, and per-row index maintenance across a
// multi-hundred-thousand-row drain costs far more than the sorts do. The case
// is here so the degraded plan is a stated expectation — if it ever becomes a
// steady-state plan, the locks above fail first and this one explains why the
// difference exists.
func TestAdjacencyPlanLocksDuringBulkLoad(t *testing.T) {
	s := newPlanLockFixture(t)
	dropBulkLoadIndexes(t, s)

	cases := []struct {
		name   string
		query  string
		want   []string
		forbid []string
	}{
		{
			// stmtOutEdges: from_id still probes the edges UNIQUE key's
			// leading column, so the lookup stays a search; only the order
			// has to be built.
			name:   "out_edges_ordered_bulk_load",
			query:  `SELECT ` + lookupEdgeCols + ` FROM edges WHERE from_id = ? AND view_gen = ? ORDER BY line, id`,
			want:   []string{"SEARCH edges USING INDEX sqlite_autoindex_edges_1 (from_id=?)", "USE TEMP B-TREE FOR ORDER BY"},
			forbid: []string{"SCAN edges"},
		},
		{
			// stmtInEdges: nothing left indexes to_id, so the reverse
			// adjacency degrades all the way to a table scan plus a sort.
			name:  "in_edges_ordered_bulk_load",
			query: `SELECT ` + lookupEdgeCols + ` FROM edges WHERE to_id = ? AND view_gen = ? ORDER BY kind, id`,
			want:  []string{"SCAN edges", "USE TEMP B-TREE FOR ORDER BY"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainQueryPlan(t, s, tc.query, 2)
			joined := strings.Join(plan, "\n")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Fatalf("plan missing %q:\n%s", want, joined)
				}
			}
			for _, forbid := range tc.forbid {
				if strings.Contains(joined, forbid) {
					t.Fatalf("plan contains forbidden %q:\n%s", forbid, joined)
				}
			}
		})
	}
}

// dropBulkLoadIndexes reproduces what BeginBulkLoad does to a store's schema.
// BeginBulkLoad itself engages only on a provably empty store, and a plan lock
// needs rows to plan against, so the drops are issued directly from the same
// list production drops by name.
//
// The reader handle is asked for the surviving index set afterwards, which both
// states the precondition and makes the reader notice the schema change before
// anything is EXPLAINed against it.
func dropBulkLoadIndexes(t *testing.T, s *Store) {
	t.Helper()
	dropped := make(map[string]struct{}, len(bulkDroppableIndexes))
	for _, idx := range bulkDroppableIndexes {
		if _, err := s.writerDB.Exec("DROP INDEX IF EXISTS " + idx.name); err != nil {
			t.Fatalf("drop %s: %v", idx.name, err)
		}
		dropped[idx.name] = struct{}{}
	}
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'index'`)
	if err != nil {
		t.Fatalf("read index set: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		if _, ok := dropped[name]; ok {
			t.Fatalf("index %s survived the bulk-load drop; the degraded plan below would not be the one a cold load runs", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index rows: %v", err)
	}
}

// explainPlanTolerant mirrors explainQueryPlan but accepts empty plans:
// prepared DML like a bare INSERT legitimately has no plan rows.
func explainPlanTolerant(t *testing.T, s *Store, query string) []string {
	t.Helper()
	args := make([]any, strings.Count(query, "?"))
	for i := range args {
		args[i] = ""
	}
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain %.60s: %v", query, err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan rows: %v", err)
	}
	return plan
}

// The receiver-rebind batch drives from the TEMP file table (CROSS JOIN pin);
// the temp table is connection-local, so this lock provisions it through the
// same writer path production uses.
func TestSweepPlanLockReceiverRebindBatch(t *testing.T) {
	s := newPlanLockFixture(t)
	conn, err := s.writerDB.Conn(t.Context())
	if err != nil {
		t.Fatalf("writer conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), goMethodReceiverFileTableSQL); err != nil {
		t.Fatalf("create temp table: %v", err)
	}
	rows, err := conn.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+goMethodReceiverCandidatesForFilesSQL,
		baseViewGeneration, baseViewGeneration, baseViewGeneration, baseViewGeneration)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan rows: %v", err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "SCAN f") && !strings.Contains(joined, "go_receiver_rebind_files") {
		t.Fatalf("plan must drive from the temp file table:\n%s", joined)
	}
	if !strings.Contains(joined, "nodes_by_file (file_path=?)") {
		t.Fatalf("plan must probe nodes_by_file per requested file:\n%s", joined)
	}
}

// TestPreparedStatementPlansNeverScanBigTables is the mechanical form of the
// partial-index convention: every statement prepared at Open is EXPLAINed
// and any full scan of the nodes or edges tables fails the build, unless the
// statement is on the explicit allowlist of intentional whole-table reads.
// A future prepared statement that trips the partial-index-vs-bound-parameter
// trap (or any other misplan) fails here without anyone remembering a rule.
func TestPreparedStatementPlansNeverScanBigTables(t *testing.T) {
	s := newPlanLockFixture(t)
	// Intentional whole-table reads, and statements the tiny fixture plans
	// as a scan but a production-scale store verifiably serves from an
	// index (regime flips — each entry names its verification). Adding an
	// entry here is a reviewed decision, not a remembered convention.
	allowlist := []string{
		"COUNT(*)", // whole-table counts are whole-table by intent
		// WHERE repo_prefix = ?: SEARCH nodes_by_repo_kind (repo_prefix=?)
		// on the production snapshot; the 1.2k-row fixture prefers a scan.
		`FROM nodes WHERE repo_prefix = ?`,
		// Repo edges via LIST SUBQUERY + edges_by_from_line probe on the
		// production snapshot; fixture-size regime flip.
		`FROM edges e`,
	}
	if len(s.preparedSQL) == 0 {
		t.Fatal("prepared-statement registry is empty; the fence covers nothing")
	}
	bareScan := func(plan []string) string {
		for _, detail := range plan {
			trimmed := strings.TrimSpace(detail)
			if !strings.HasPrefix(trimmed, "SCAN nodes") && !strings.HasPrefix(trimmed, "SCAN edges") {
				continue
			}
			// A covering-index scan is an index-only read (DISTINCT over a
			// partial index, for example) — only bare table scans fail.
			if strings.Contains(trimmed, "USING") {
				continue
			}
			return trimmed
		}
		return ""
	}
	for _, query := range s.preparedSQL {
		name := query
		if len(name) > 70 {
			name = name[:70]
		}
		allowed := false
		for _, allow := range allowlist {
			if strings.Contains(query, allow) {
				allowed = true
				break
			}
		}
		// The AllNodes/AllEdges exports are whole-table by intent — matched by
		// exact suffix so nothing but the generation residual rides along.
		// Their trailing ORDER BY id is the primary-key walk the scan already
		// performs (nodes is WITHOUT ROWID leading on id; edges' id is the
		// rowid), so it makes the order explicit without adding a sorter —
		// which the no-temp-b-tree assertion below holds them to. The
		// `WHERE view_gen = ?` form is the same export narrowed to the
		// handle's payload view generation: still every row the walk can
		// legally return, still no sorter.
		wholeTable := false
		for _, table := range []string{"nodes", "edges"} {
			for _, tail := range []string{
				"FROM " + table,
				"FROM " + table + " ORDER BY id",
				"FROM " + table + " WHERE view_gen = ?",
				"FROM " + table + " WHERE view_gen = ? ORDER BY id",
			} {
				if strings.HasSuffix(query, tail) {
					wholeTable = true
				}
			}
		}
		if wholeTable {
			for _, detail := range explainPlanTolerant(t, s, query) {
				if strings.Contains(detail, "TEMP B-TREE") {
					t.Errorf("whole-table export sorts instead of walking the primary key:\n  %s\nplan step: %s",
						name, strings.TrimSpace(detail))
				}
			}
			continue
		}
		if allowed {
			continue
		}
		plan := explainPlanTolerant(t, s, query)
		if offender := bareScan(plan); offender != "" {
			t.Errorf("prepared statement scans a big table (%s):\n  %s\nplan:\n%s",
				offender, name, strings.Join(plan, "\n"))
		}
	}
}

// The WARN-class shapes from the sweep, post-fix: enrichment repo readers
// ride their partial indexes via the literal predicate, and each fn-value
// placeholder arm rides its own index standalone (the OR form defeated the
// partial-index prover).
func TestSweepWarnPlanLocks(t *testing.T) {
	s := newPlanLockFixture(t)
	cases := []struct {
		name   string
		query  string
		args   int
		want   []string
		forbid []string
	}{
		{
			name:   "blame_enrichment_by_repo",
			query:  "SELECT node_id FROM blame_enrichment WHERE view_gen = ? AND repo_prefix = ? AND repo_prefix <> \x27\x27",
			args:   2,
			want:   []string{"blame_by_repo (view_gen=? AND repo_prefix=?)"},
			forbid: []string{"SCAN blame_enrichment"},
		},
		{
			name:   "fnvalue_bare_range",
			query:  "SELECT id FROM edges WHERE to_id >= \x27unresolved::fnvalue::\x27 AND to_id < \x27unresolved::fnvalue:;\x27 AND view_gen = ?",
			args:   1,
			want:   []string{"edges_by_to (to_id>? AND to_id<?)"},
			forbid: []string{"SCAN edges USING COVERING INDEX edges_by_unresolved"},
		},
		{
			name:   "fnvalue_prefixed_partial",
			query:  "SELECT id FROM edges WHERE to_id LIKE \x27%::unresolved::fnvalue::%\x27 AND view_gen = ?",
			args:   1,
			want:   []string{"edges_fnvalue_prefixed"},
			forbid: []string{"edges_by_unresolved"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainPlanTolerant(t, s, tc.query)
			joined := strings.Join(plan, "\n")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Fatalf("plan missing %q:\n%s", want, joined)
				}
			}
			for _, forbid := range tc.forbid {
				if strings.Contains(joined, forbid) {
					t.Fatalf("plan contains forbidden %q:\n%s", forbid, joined)
				}
			}
		})
	}
}
