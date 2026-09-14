package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// E2E matrix 1 — the idle / no-op family (handoff section 8, first bullet).
//
// The workload is the one the acceptance brief names: clean and dirty idle
// polling, repeated samples, touch, stage/unstage, a same-tree amend, same-size
// changed bytes with a restored mtime, atomic replacement, and an uncertain
// filesystem identity (identical bytes arriving on a new inode). Each case
// states the acceptance gate it serves and records one row of an outcome table.
//
// What the assertions are, precisely (gate 2 — "an effective no-op does not
// extract/resolve again, mutate unchanged payload or allocate a payload
// generation merely because of polling/selection; … healthy claim/replay paths
// that promise zero catalog DML retain that stronger guarantee"):
//
//   - sqlite_sequence.seq for view_generations does not move. AUTOINCREMENT
//     hands out a number per INSERT and never reuses it, so an allocation that
//     was inserted and rolled back or deleted is still visible here. This is the
//     single strongest external allocation witness. Its ROW is part of the
//     reading: AUTOINCREMENT writes that row on the first insert and never
//     drops it, so an absent row is the positive statement "no generation has
//     ever been allocated in this store" and is reported as such, cross-checked
//     against an empty view_generations table (w8m5ReadCatalog).
//   - the catalog's semantic state is byte-identical across the case. That is
//     state equality, not statement counting: an UPDATE that rewrote a column
//     with the value it already held is invisible from outside the process. It
//     is paired with the process write series of the sustained harness rather
//     than offered as a substitute for it, and heartbeat columns (last_seen,
//     last_accessible, last_selected) are recorded as bounded bookkeeping —
//     which gate 2 explicitly allows — instead of being asserted at zero.
//   - the daemon's own viewmetrics counters agree: no series in the allocation
//     family moves, and the same-tree commit shows the replay series instead.
//   - the BASE PAYLOAD does not move. nodes(view_gen=0) carries one row per
//     symbol; a content change rewrites those rows and an effective no-op must
//     leave them byte-identical. This is the witness the three clauses above
//     are not: on this fixture a real working-tree content change moves none of
//     them (see w8m5Calibrate), so without it "nothing allocated" is the same
//     observation a genuine edit produces and every no-op row is vacuous.
//
// The fixture carries a DEPENDENT CHECKOUT for the same reason the calibration
// exists. Since the consumer gate (internal/indexer/dedicated_base_startup.go,
// `out.Skipped = "no dependent checkout"`) a family whose only checkout is the
// primary publishes no committed base and allocates no generation at all, so
// on a single-checkout fixture every view-catalog witness above is as still for
// a real committed change as it is for a no-op. The dependent is the consumer
// that makes the committed-base lane run; the calibration FAILS if the
// committed half stops allocating, so the dependent cannot be removed quietly.
//
// No clause is asserted on faith. The matrix opens with a CALIBRATION that
// makes a real, view-visible content change — first dirty, then committed — and
// records which witnesses it moved. A no-op row may only rest on a witness the
// calibration actually moved; the rest are recorded as "not asserted" with the
// reason, and a row left with no live clause at all is a SKIP that says so
// rather than a PASS. The two positive controls assert the converse — a real
// change MUST move the payload witness — so re-declaring one of them a no-op
// fails the case instead of passing it.
//
// The test is opt-in (GXW8_TEST_BINARY), drives a private daemon binary over
// the shared isolated fixture, and never addresses the user's daemon, store or
// configuration. The parts that can be wrong without a daemon — the catalog
// differ, the case table, the counter-series names, the outcome table — are
// unit-tested at the bottom of this file and run in an ordinary `go test`.

const (
	// w8m5BinaryEnv is the opt-in gate. It is the same variable W8.1's
	// sustained harness uses: one opt-in switches on every isolated
	// measurement in this package, so an operator cannot accidentally run
	// half of the evidence.
	w8m5BinaryEnv = "GXW8_TEST_BINARY"
	// w8m5BinaryEnvAlt is the spelling the sibling matrix items adopted. It
	// is honoured as well so one export runs every matrix; neither variable
	// has a default, and with both unset every matrix skips by name.
	w8m5BinaryEnvAlt = "GXW8_MATRIX_BINARY"

	w8m5ProbeTimeout   = 3 * time.Minute
	w8m5CommandTimeout = 30 * time.Second
	w8m5SettleQuiet    = 20 * time.Second
	// w8m5AbsenceTimeout bounds a withdrawal wait. It is deliberately shorter
	// than w8m5ProbeTimeout: every withdrawal this matrix measured landed
	// inside 35 s, so a wait that reaches this bound is reporting a case the
	// branch does not carry rather than a slow host — and the row says so with
	// the daemon's own answer attached. The long bound is kept for arrivals,
	// where a cold rebuild legitimately takes longer.
	w8m5AbsenceTimeout = 90 * time.Second
)

// ------------------------------------------------------------ outcome table ---

// Outcome statuses. A case that the branch does not handle is SKIP with a
// reason that names the ledger row it belongs to — never a silent pass.
const (
	w8m5StatusPass = "PASS"
	w8m5StatusFail = "FAIL"
	w8m5StatusSkip = "SKIP"
)

// w8m5Row is one matrix case's outcome.
type w8m5Row struct {
	Case   string `json:"case"`
	Gate   string `json:"gate"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Counters is the viewmetrics delta the case produced, recorded for
	// every row whether or not the row asserts on it: "what did this cost"
	// is the measurement, and the assertion is only the part of it that is
	// a contract.
	Counters map[string]int64 `json:"viewmetrics_delta,omitempty"`
	// Bookkeeping is the catalog's heartbeat movement — the bounded real
	// bookkeeping gate 2 allows. Recorded, never asserted at zero.
	Bookkeeping map[string]int64 `json:"catalog_bookkeeping_delta,omitempty"`
	// Differences is the gate-1 oracle's verdict in full, so the artifact
	// carries what the one-line table cell had to truncate.
	Differences []w8m5Difference `json:"gate1_differences,omitempty"`
	Seconds     float64          `json:"wall_s"`
}

// w8m5Table collects the rows and renders the outcome table the ledger quotes.
type w8m5Table struct {
	t    *testing.T
	name string
	rows []w8m5Row
}

func w8m5NewTable(t *testing.T, name string) *w8m5Table {
	return &w8m5Table{t: t, name: name}
}

func (tb *w8m5Table) add(row w8m5Row) {
	tb.rows = append(tb.rows, row)
}

// w8m5TableProblems reports why a set of rows is not a usable outcome table.
// It is pure so the rule — every row carries a case, a gate and a status, and
// a skipped row carries a reason — is tested without a daemon.
func w8m5TableProblems(rows []w8m5Row) []string {
	var problems []string
	for i, row := range rows {
		switch {
		case strings.TrimSpace(row.Case) == "":
			problems = append(problems, fmt.Sprintf("row %d has no case name", i))
		case strings.TrimSpace(row.Gate) == "":
			problems = append(problems, fmt.Sprintf("row %q names no acceptance gate", row.Case))
		}
		switch row.Status {
		case w8m5StatusPass, w8m5StatusFail:
		case w8m5StatusSkip:
			if strings.TrimSpace(row.Detail) == "" {
				problems = append(problems, fmt.Sprintf("row %q is skipped with no reason", row.Case))
			}
		default:
			problems = append(problems, fmt.Sprintf("row %q has unknown status %q", row.Case, row.Status))
		}
	}
	return problems
}

// w8m5TableFailures lists every row that was marked FAILED, as a reportable
// sentence. It is pure, and render reports each entry, so a row that a scoring
// path marked FAILED fails the matrix from the table itself — a scoring path
// that marks a row and then drops its own report cannot produce a green run.
func w8m5TableFailures(rows []w8m5Row) []string {
	var failed []string
	for _, row := range rows {
		if row.Status == w8m5StatusFail {
			failed = append(failed, fmt.Sprintf("%s (%s): %s", row.Case, row.Gate, row.Detail))
		}
	}
	return failed
}

// render logs the table and, when an artifact directory is configured, writes
// it as JSON next to the sustained harness's artifacts.
func (tb *w8m5Table) render() {
	tb.t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s outcome table (%d cases)\n", tb.name, len(tb.rows))
	fmt.Fprintf(&b, "%-6s %-44s %-8s %s\n", "STATUS", "CASE", "GATE", "DETAIL")
	for _, row := range tb.rows {
		fmt.Fprintf(&b, "%-6s %-44s %-8s %s\n", row.Status, row.Case, row.Gate, row.Detail)
	}
	tb.t.Log(b.String())
	for _, problem := range w8m5TableProblems(tb.rows) {
		tb.t.Errorf("outcome table is not reportable: %s", problem)
	}
	for _, failure := range w8m5TableFailures(tb.rows) {
		tb.t.Errorf("%s: a row is FAILED: %s", tb.name, failure)
	}
	if dir := os.Getenv("GXW8_ARTIFACT_DIR"); dir != "" {
		path := filepath.Join(dir, tb.name+".json")
		if err := w8m5WriteJSON(path, map[string]any{"matrix": tb.name, "rows": tb.rows}); err != nil {
			tb.t.Logf("outcome table artifact not written: %v", err)
		} else {
			tb.t.Logf("outcome table artifact: %s", path)
		}
	}
}

func w8m5WriteJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// --------------------------------------------------------------- catalog ---

// w8m5Generation is one view_generations row, semantic columns only.
type w8m5Generation struct {
	ID                 int64  `json:"generation_id"`
	OwnerKind          string `json:"owner_kind"`
	GraphID            string `json:"graph_id"`
	CheckoutID         string `json:"checkout_id"`
	Kind               string `json:"generation_kind"`
	Base               int64  `json:"base_generation_id"`
	TreeOID            string `json:"tree_oid"`
	Provenance         string `json:"provenance_commit_oid"`
	ConfigHash         string `json:"config_hash"`
	ResolverVersion    string `json:"resolver_version"`
	DependencyRevision string `json:"dependency_revision"`
	State              string `json:"state"`
	Covered            int64  `json:"covered_files"`
	Affected           int64  `json:"affected_files"`
	StorageBytes       int64  `json:"storage_bytes"`
	Completeness       string `json:"completeness"`
	Error              string `json:"error"`
}

// w8m5Checkout is one checkouts row, semantic columns only. last_seen and
// last_accessible are deliberately absent: they are the reconcile heartbeat,
// they move on every pass by construction, and gate 2 allows bounded real
// bookkeeping. They are captured in w8m5Bookkeeping instead.
type w8m5Checkout struct {
	ID            string `json:"checkout_id"`
	State         string `json:"state"`
	DesiredMode   string `json:"desired_mode"`
	EffectiveMode string `json:"effective_mode"`
	HeadRef       string `json:"head_ref"`
	HeadCommit    string `json:"head_commit"`
	HeadTree      string `json:"head_tree"`
	Locked        int64  `json:"locked"`
	Prunable      int64  `json:"prunable"`
	Removal       string `json:"removal_evidence"`
	LastError     string `json:"last_error"`
}

// w8m5Route is one checkout_routes row. Every column is semantic.
type w8m5Route struct {
	CheckoutID string `json:"checkout_id"`
	GraphID    string `json:"graph_id"`
	Commit     int64  `json:"commit_generation_id"`
	Dirty      int64  `json:"dirty_generation_id"`
	Epoch      int64  `json:"route_epoch"`
	State      string `json:"state"`
}

// w8m5Bookkeeping is the movement gate 2 allows: heartbeats and selection
// stamps. Recorded so a reader can see it was bounded, never asserted at zero.
type w8m5Bookkeeping struct {
	CheckoutLastSeen       int64 `json:"checkouts_last_seen_sum"`
	CheckoutLastAccessible int64 `json:"checkouts_last_accessible_sum"`
	GenerationLastSelected int64 `json:"generations_last_selected_sum"`
	FamilyLastSeen         int64 `json:"families_last_seen_sum"`
}

// w8m5Catalog is one external observation of the catalog.
type w8m5Catalog struct {
	Sequence int64 `json:"view_generations_seq"`
	// SequenceRow says whether sqlite_sequence carries a row for
	// view_generations at all.
	//
	// AUTOINCREMENT writes that row on the table's FIRST insert and never
	// removes it, so the row's absence is the positive fact "no generation
	// has ever been allocated in this store" — not a failed read. It is the
	// NORMAL state of a repository family with no dependent checkout: since
	// the consumer gate (internal/indexer/dedicated_base_startup.go:776,
	// `out.Skipped = "no dependent checkout"`) such a family defers its
	// committed-base publication and performs zero catalog DML, so
	// view_generations never takes a rowid.
	SequenceRow bool              `json:"view_generations_seq_row"`
	Generations []w8m5Generation  `json:"view_generations"`
	Checkouts   []w8m5Checkout    `json:"checkouts"`
	Routes      []w8m5Route       `json:"checkout_routes"`
	Bookkeeping w8m5Bookkeeping   `json:"bookkeeping"`
	Errors      []string          `json:"errors,omitempty"`
	Extra       map[string]string `json:"extra,omitempty"`
}

func w8m5NullInt(value sql.NullInt64) int64 {
	if !value.Valid {
		return -1
	}
	return value.Int64
}

// w8m5ReadCatalog observes the catalog through a read-only connection. Every
// query is tolerated individually and its failure named: an arm whose schema
// lacks a table must still produce a row, with the missing series named rather
// than silently zeroed.
func w8m5ReadCatalog(ctx context.Context, db *sql.DB) w8m5Catalog {
	catalog := w8m5Catalog{Sequence: -1}
	note := func(err error) {
		if err != nil {
			catalog.Errors = append(catalog.Errors, err.Error())
		}
	}
	// An absent sqlite_sequence row is a reading, not a read failure: see
	// w8m5Catalog.SequenceRow. Reporting sql.ErrNoRows as a catalog read error
	// made every no-op row in this matrix fail on the INSTRUMENT rather than on
	// the daemon, once the consumer gate stopped publishing for a family with
	// no dependent checkout.
	//
	// Only the absent row is absorbed, and only as far as it goes: a missing
	// table, a closed database or any other scan failure is still an error,
	// because "this store allocated nothing" and "this store cannot be read"
	// are different facts and only one of them belongs in a census. The census
	// cross-check below refuses the incoherent third case.
	switch err := db.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name='view_generations'").Scan(&catalog.Sequence); {
	case errors.Is(err, sql.ErrNoRows):
		catalog.Sequence, catalog.SequenceRow = 0, false
	case err != nil:
		note(err)
	default:
		catalog.SequenceRow = true
	}

	rows, err := db.QueryContext(ctx, `SELECT generation_id, owner_kind, graph_id, COALESCE(checkout_id,''), generation_kind,
		base_generation_id, tree_oid, COALESCE(provenance_commit_oid,''), config_hash, resolver_version, dependency_revision,
		state, covered_files, affected_files, storage_bytes, completeness, error, last_selected
		FROM view_generations ORDER BY generation_id`)
	if err != nil {
		note(err)
	} else {
		for rows.Next() {
			var gen w8m5Generation
			var base sql.NullInt64
			var lastSelected int64
			if err := rows.Scan(&gen.ID, &gen.OwnerKind, &gen.GraphID, &gen.CheckoutID, &gen.Kind, &base, &gen.TreeOID,
				&gen.Provenance, &gen.ConfigHash, &gen.ResolverVersion, &gen.DependencyRevision, &gen.State,
				&gen.Covered, &gen.Affected, &gen.StorageBytes, &gen.Completeness, &gen.Error, &lastSelected); err != nil {
				note(err)
				break
			}
			gen.Base = w8m5NullInt(base)
			catalog.Bookkeeping.GenerationLastSelected += lastSelected
			catalog.Generations = append(catalog.Generations, gen)
		}
		note(rows.Err())
		_ = rows.Close()
	}
	// The cross-check that turns "no sqlite_sequence row" from a swallowed
	// error into a POSITIVE census reading: AUTOINCREMENT writes the sequence
	// row on the first insert and never drops it, so a store that holds
	// view_generations rows MUST carry one. A store where the two disagree
	// cannot be read coherently and is reported as a read error, exactly as a
	// missing table is.
	if !catalog.SequenceRow && len(catalog.Generations) > 0 {
		note(fmt.Errorf("sqlite_sequence carries no view_generations row while view_generations holds %d row(s): the allocation witness and the census disagree",
			len(catalog.Generations)))
	}

	rows, err = db.QueryContext(ctx, `SELECT checkout_id, state, desired_mode, effective_mode, head_ref, head_commit, head_tree,
		locked, prunable, removal_evidence, last_error, last_seen, last_accessible
		FROM checkouts ORDER BY checkout_id`)
	if err != nil {
		note(err)
	} else {
		for rows.Next() {
			var checkout w8m5Checkout
			var lastSeen, lastAccessible int64
			if err := rows.Scan(&checkout.ID, &checkout.State, &checkout.DesiredMode, &checkout.EffectiveMode,
				&checkout.HeadRef, &checkout.HeadCommit, &checkout.HeadTree, &checkout.Locked, &checkout.Prunable,
				&checkout.Removal, &checkout.LastError, &lastSeen, &lastAccessible); err != nil {
				note(err)
				break
			}
			catalog.Bookkeeping.CheckoutLastSeen += lastSeen
			catalog.Bookkeeping.CheckoutLastAccessible += lastAccessible
			catalog.Checkouts = append(catalog.Checkouts, checkout)
		}
		note(rows.Err())
		_ = rows.Close()
	}

	rows, err = db.QueryContext(ctx, `SELECT checkout_id, graph_id, commit_generation_id, dirty_generation_id, route_epoch, state
		FROM checkout_routes ORDER BY checkout_id`)
	if err != nil {
		note(err)
	} else {
		for rows.Next() {
			var route w8m5Route
			var commit, dirty sql.NullInt64
			if err := rows.Scan(&route.CheckoutID, &route.GraphID, &commit, &dirty, &route.Epoch, &route.State); err != nil {
				note(err)
				break
			}
			route.Commit, route.Dirty = w8m5NullInt(commit), w8m5NullInt(dirty)
			catalog.Routes = append(catalog.Routes, route)
		}
		note(rows.Err())
		_ = rows.Close()
	}

	var familySeen sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(SUM(last_seen),0) FROM repository_families").Scan(&familySeen); err != nil {
		note(err)
	} else {
		catalog.Bookkeeping.FamilyLastSeen = familySeen.Int64
	}
	return catalog
}

// w8m5CatalogDiff names every semantic difference between two observations, in
// the vocabulary a reader can act on. Heartbeat movement is not a difference:
// it is reported by w8m5BookkeepingDelta instead.
func w8m5CatalogDiff(before, after w8m5Catalog) []string {
	var diffs []string
	if before.Sequence != after.Sequence {
		diffs = append(diffs, fmt.Sprintf("view_generations seq %d -> %d", before.Sequence, after.Sequence))
	}
	// The first allocation a store ever performs is the one case the numeric
	// delta alone reads as an ordinary step: it is also the moment the store
	// stops being able to say "nothing was ever allocated here". Name it.
	if before.SequenceRow != after.SequenceRow {
		diffs = append(diffs, fmt.Sprintf("view_generations sqlite_sequence row present %v -> %v", before.SequenceRow, after.SequenceRow))
	}
	diffs = append(diffs, w8m5DiffRows("view_generations", w8m5KeyedGenerations(before.Generations), w8m5KeyedGenerations(after.Generations))...)
	diffs = append(diffs, w8m5DiffRows("checkouts", w8m5KeyedCheckouts(before.Checkouts), w8m5KeyedCheckouts(after.Checkouts))...)
	diffs = append(diffs, w8m5DiffRows("checkout_routes", w8m5KeyedRoutes(before.Routes), w8m5KeyedRoutes(after.Routes))...)
	return diffs
}

func w8m5KeyedGenerations(rows []w8m5Generation) map[string]string {
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[fmt.Sprintf("%d", row.ID)] = fmt.Sprintf("%+v", row)
	}
	return out
}

func w8m5KeyedCheckouts(rows []w8m5Checkout) map[string]string {
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.ID] = fmt.Sprintf("%+v", row)
	}
	return out
}

func w8m5KeyedRoutes(rows []w8m5Route) map[string]string {
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.CheckoutID] = fmt.Sprintf("%+v", row)
	}
	return out
}

// w8m5DiffRows compares two keyed row sets and names what changed. The changed
// case reports both renderings so a reader sees which column moved without
// re-running the case.
func w8m5DiffRows(table string, before, after map[string]string) []string {
	var diffs []string
	keys := map[string]bool{}
	for key := range before {
		keys[key] = true
	}
	for key := range after {
		keys[key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		old, hadOld := before[key]
		current, hasNew := after[key]
		switch {
		case !hadOld:
			diffs = append(diffs, fmt.Sprintf("%s %s inserted: %s", table, key, current))
		case !hasNew:
			diffs = append(diffs, fmt.Sprintf("%s %s deleted: %s", table, key, old))
		case old != current:
			diffs = append(diffs, fmt.Sprintf("%s %s changed: %s -> %s", table, key, old, current))
		}
	}
	return diffs
}

// w8m5BookkeepingDelta is the allowed movement, as a recorded series.
func w8m5BookkeepingDelta(before, after w8m5Catalog) map[string]int64 {
	delta := map[string]int64{}
	add := func(name string, old, current int64) {
		if current != old {
			delta[name] = current - old
		}
	}
	add("checkouts_last_seen_sum", before.Bookkeeping.CheckoutLastSeen, after.Bookkeeping.CheckoutLastSeen)
	add("checkouts_last_accessible_sum", before.Bookkeeping.CheckoutLastAccessible, after.Bookkeeping.CheckoutLastAccessible)
	add("generations_last_selected_sum", before.Bookkeeping.GenerationLastSelected, after.Bookkeeping.GenerationLastSelected)
	add("families_last_seen_sum", before.Bookkeeping.FamilyLastSeen, after.Bookkeeping.FamilyLastSeen)
	return delta
}

// --------------------------------------------------------- payload witness ---

// The base graph's payload is the witness the view catalog cannot be.
//
// Measured on this fixture: a real, same-size, view-visible working-tree
// content change moves NOTHING in view_generations / checkouts /
// checkout_routes, does not move sqlite_sequence.seq, and moves no viewmetrics
// allocation series — the served view moves and the catalog does not, because a
// dirty edit to a tracked primary lands in the base graph rather than in a new
// payload generation. A gate-2 instrument built only from those three therefore
// cannot tell a real change from a no-op, and every "allocated nothing" row
// resting on it says nothing at all.
//
// nodes(view_gen=0) is the witness that does move. Each row is one symbol's
// identity — id, kind, name, file, line span, visibility — so a content change
// that renames, moves or withdraws a declaration rewrites the rows for that
// file, while an effective no-op leaves them byte-identical. updated_at is read
// too but is recorded as bookkeeping rather than asserted: a re-extraction that
// wrote the same identity back is a cost this matrix reports and the sustained
// harness prices, not a gate-2 violation this matrix can prove from outside the
// process.
//
// Schema: internal/graph/store_sqlite/schema.go:855-884 (nodesTableBody —
// (id, view_gen) primary key, view_gen 0 is the base).

// w8m5Payload is one external observation of a repository's base payload.
type w8m5Payload struct {
	// File is the probe file's node rows, named individually so a diff can
	// say which symbol moved.
	File []string `json:"file_rows"`
	// FileStamp is the sum of updated_at over those rows: recorded, never
	// asserted (see the note above).
	FileStamp int64 `json:"file_updated_at_sum"`
	// RepoRows and RepoDigest cover the whole repository, so a no-op that
	// rewrote some OTHER file is still visible without printing the graph.
	RepoRows   int      `json:"repo_rows"`
	RepoDigest string   `json:"repo_digest"`
	Errors     []string `json:"errors,omitempty"`
}

// w8m5ReadPayload observes the base payload of one repository through a
// read-only connection. A query that fails names itself rather than reporting
// an empty graph: a payload witness that silently reads zero rows would report
// every change as a no-op, which is the exact failure this witness exists to
// prevent.
func w8m5ReadPayload(ctx context.Context, db *sql.DB, prefix, file string) w8m5Payload {
	payload := w8m5Payload{}
	rows, err := db.QueryContext(ctx, `SELECT id, kind, name, file_path, start_line, end_line,
		COALESCE(visibility,''), COALESCE(updated_at,0) FROM nodes
		WHERE view_gen=0 AND repo_prefix=? ORDER BY id`, prefix)
	if err != nil {
		payload.Errors = append(payload.Errors, err.Error())
		return payload
	}
	digest := fnv.New64a()
	want := ""
	if file != "" {
		want = path.Clean(filepath.ToSlash(file))
	}
	for rows.Next() {
		var id, kind, name, filePath, visibility string
		var start, end, updated int64
		if err := rows.Scan(&id, &kind, &name, &filePath, &start, &end, &visibility, &updated); err != nil {
			payload.Errors = append(payload.Errors, err.Error())
			break
		}
		slashed := filepath.ToSlash(filePath)
		identity := fmt.Sprintf("%s|%s|%s|%s|%d-%d|%s", id, kind, name, slashed, start, end, visibility)
		payload.RepoRows++
		_, _ = digest.Write([]byte(identity))
		_, _ = digest.Write([]byte{0})
		if want != "" && (slashed == want || strings.HasSuffix(slashed, "/"+want)) {
			payload.File = append(payload.File, identity)
			payload.FileStamp += updated
		}
	}
	if err := rows.Err(); err != nil {
		payload.Errors = append(payload.Errors, err.Error())
	}
	_ = rows.Close()
	payload.RepoDigest = fmt.Sprintf("%016x", digest.Sum64())
	sort.Strings(payload.File)
	return payload
}

// w8m5PayloadDiff names every payload movement between two observations.
func w8m5PayloadDiff(before, after w8m5Payload) []string {
	var diffs []string
	for _, added := range w8m5StringsMinus(after.File, before.File) {
		diffs = append(diffs, "payload row added: "+added)
	}
	for _, removed := range w8m5StringsMinus(before.File, after.File) {
		diffs = append(diffs, "payload row removed: "+removed)
	}
	if before.RepoDigest != after.RepoDigest {
		diffs = append(diffs, fmt.Sprintf("payload digest for the repository moved %s -> %s (%d -> %d rows)",
			before.RepoDigest, after.RepoDigest, before.RepoRows, after.RepoRows))
	}
	return diffs
}

// w8m5StringsMinus is a multiset difference: a row that appears twice on the
// left and once on the right is reported once.
func w8m5StringsMinus(left, right []string) []string {
	counts := make(map[string]int, len(right))
	for _, value := range right {
		counts[value]++
	}
	var out []string
	for _, value := range left {
		if counts[value] > 0 {
			counts[value]--
			continue
		}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// ------------------------------------------------------- counter vocabulary ---

// w8m5Series renders one viewmetrics series key exactly as the registry
// flattens it (name, then a brace-wrapped label list in declaration order).
func w8m5Series(name string, labels ...string) string {
	if len(labels) == 0 {
		return name
	}
	return name + "{" + strings.Join(labels, ",") + "}"
}

// w8m5AllocationSeries is the family that must not move across an effective
// no-op. Every entry is a published generation or a physical build: the direct
// counter-side statement of gate 2.
func w8m5AllocationSeries() []string {
	return []string{
		w8m5Series(viewmetrics.GenerationPublishedTotal, "owner="+viewmetrics.OwnerCheckout),
		w8m5Series(viewmetrics.DedicatedBasePublishTotal, "shape="+viewmetrics.DedicatedBaseRoot),
		w8m5Series(viewmetrics.DedicatedBasePublishTotal, "shape="+viewmetrics.DedicatedBaseDelta),
		w8m5Series(viewmetrics.DedicatedBaseClaimTotal, "outcome="+viewmetrics.DedicatedBaseBuilt),
		w8m5Series(viewmetrics.CoordinatorCycleTotal, "outcome="+viewmetrics.OutcomeBuiltCommit),
		w8m5Series(viewmetrics.CoordinatorCycleTotal, "outcome="+viewmetrics.OutcomeBuiltDirty),
	}
}

// w8m5ReplaySeries is the healthy claim/replay family: the observed identity
// equalled the active generation's, so adoption re-confirmed it and wrote no
// catalog row. A same-tree commit has to show one of these.
func w8m5ReplaySeries() []string {
	return []string{
		w8m5Series(viewmetrics.DedicatedBaseClaimTotal, "outcome="+viewmetrics.DedicatedBaseReused),
		w8m5Series(viewmetrics.DedicatedBasePublicationTotal, "outcome="+viewmetrics.PublicationReadopted),
		w8m5Series(viewmetrics.CoordinatorCycleTotal, "outcome="+viewmetrics.OutcomeAdoptedCommit),
	}
}

// w8m5SeriesName strips the label list, leaving the catalog name a declaration
// check can look up.
func w8m5SeriesName(key string) string {
	if index := strings.IndexByte(key, '{'); index >= 0 {
		return key[:index]
	}
	return key
}

// w8m5MovedSeries lists which of the named series moved in a counter delta.
func w8m5MovedSeries(delta map[string]int64, series []string) []string {
	var moved []string
	for _, key := range series {
		if value := delta[key]; value != 0 {
			moved = append(moved, fmt.Sprintf("%s=%+d", key, value))
		}
	}
	sort.Strings(moved)
	return moved
}

// ------------------------------------------------------------- case table ---

// The two kinds of case in matrix 1. This is ONE bit, and it is the bit the
// adversarial review's mutation flips: declaring one of the positive controls a
// no-op must FAIL the case (the payload witness moved), not pass it.
const (
	// w8m5NoChange is an effective no-op: nothing about the tree's content
	// changed, so no witness the calibration proved live may move.
	w8m5NoChange = "no-op"
	// w8m5RealChange is a correctness control: the tree's content really did
	// change, and the payload witness MUST move. A control that moved nothing
	// is a blind instrument, and the case says so instead of passing.
	w8m5RealChange = "real-change"
)

// w8m5NoopExpect is what a case promises.
type w8m5NoopExpect struct {
	// Change is w8m5NoChange or w8m5RealChange (empty means w8m5NoChange).
	Change string
	// Replay demands that the healthy claim/replay family move: the case is
	// a re-observation of an identity the daemon already holds.
	Replay bool
	// CatalogAllow lists substrings of semantic catalog diffs this case
	// legitimately produces. A same-tree amend moves HEAD, and recording the
	// commit a checkout is on is real bookkeeping, not payload mutation.
	CatalogAllow []string
	// Detail is the sentence the outcome table carries when the case passes.
	Detail string
}

// --------------------------------------------------- calibration + verdict ---

// w8m5Liveness is what a REAL change actually moved in this fixture.
//
// It exists because a "did not move" reading is only evidence when the same
// instrument is known to move for the thing it is supposed to detect. The
// matrix measures that before it asserts anything (w8m5Calibrate): one real
// working-tree content change, then the same change committed. Every clause of
// every no-op row is gated on the corresponding bit here, and a clause whose
// witness the calibration did not move is recorded as not asserted instead of
// being counted as a pass.
type w8m5Liveness struct {
	// Calibrated is false until the calibration completed. Without it no
	// no-op clause may be asserted at all.
	Calibrated bool `json:"calibrated"`
	// DirtyPayload is the bit that makes the working-tree rows meaningful: a
	// working-tree-only content change moved the base payload witness.
	DirtyPayload bool `json:"dirty_payload"`
	// Seq, Catalog and Counters are the view-catalog witnesses, set when
	// EITHER half of the calibration moved them.
	Seq      bool   `json:"sequence"`
	Catalog  bool   `json:"catalog"`
	Counters bool   `json:"counters"`
	Detail   string `json:"detail,omitempty"`
}

// w8m5NoopObservation is everything the harness measured across one case. It is
// plain data so the decision below is a pure function.
type w8m5NoopObservation struct {
	CatalogErrors []string
	PayloadErrors []string
	SeqDelta      int64
	CatalogDiffs  []string
	PayloadDiffs  []string
	CounterDelta  map[string]int64
	// CountersError is non-empty when `daemon status` could not be read.
	CountersError string
	// GenerationsAfter and SequenceRowAfter are the CLOSING census, not a
	// delta. Together they let a row state the no-allocation fact positively
	// — "view_generations is empty and AUTOINCREMENT never took a rowid for
	// it, so no generation was ever allocated in this store" — instead of
	// resting on a difference of zero between two numbers that were both
	// absent. They are also the pair the reader cross-checks: rows with no
	// sequence row is an incoherent census, and that is a failure.
	GenerationsAfter int
	SequenceRowAfter bool
}

// w8m5Verdict is one case's decision: a status, the clauses that failed and the
// clauses that were merely recorded.
type w8m5Verdict struct {
	Status   string
	Failures []string
	Recorded []string
}

// detail renders the verdict onto a row's detail sentence.
func (v w8m5Verdict) detail(base string) string {
	parts := make([]string, 0, len(v.Failures)+len(v.Recorded)+1)
	if strings.TrimSpace(base) != "" && v.Status != w8m5StatusFail {
		parts = append(parts, strings.TrimSpace(base))
	}
	parts = append(parts, v.Failures...)
	parts = append(parts, v.Recorded...)
	return strings.Join(parts, " | ")
}

// w8m5NoopVerdict is matrix 1's decision, separated from the daemon so the two
// rules that produce a row's status are pinned by an ordinary unit test:
//
//  1. an effective no-op that moved a witness a real change is known to move
//     FAILS, and
//  2. a real change that moved NO witness fails too — a control that cannot see
//     its own change is a blind instrument, and the no-op rows resting on that
//     witness would be vacuous.
//
// It also refuses the swallow the review found: a counters transport failure
// downgrades a row to SKIP only when there is nothing to fail; a case that
// allocated a generation and could not read `daemon status` is a FAIL.
func w8m5NoopVerdict(expect w8m5NoopExpect, live w8m5Liveness, obs w8m5NoopObservation) w8m5Verdict {
	verdict := w8m5Verdict{Status: w8m5StatusPass}
	fail := func(format string, args ...any) {
		verdict.Failures = append(verdict.Failures, fmt.Sprintf(format, args...))
	}
	record := func(format string, args ...any) {
		verdict.Recorded = append(verdict.Recorded, fmt.Sprintf(format, args...))
	}

	for _, err := range obs.CatalogErrors {
		fail("catalog read error: %s", err)
	}
	for _, err := range obs.PayloadErrors {
		fail("payload read error: %s", err)
	}

	// An incoherent closing census is a read failure of the same class as the
	// two above: AUTOINCREMENT writes the sequence row on the first insert and
	// never drops it, so view_generations rows with no sequence row means the
	// store cannot be read coherently — never that it allocated nothing.
	if !obs.SequenceRowAfter && obs.GenerationsAfter != 0 {
		fail("census incoherent: view_generations holds %d row(s) with no sqlite_sequence row", obs.GenerationsAfter)
	}

	// census states the closing allocation fact POSITIVELY, so a row says what
	// the store looks like rather than only that two numbers were equal.
	census := func() {
		if obs.SequenceRowAfter {
			record("the store has allocated generations before (view_generations holds %d row(s) and AUTOINCREMENT carries a sequence for it)", obs.GenerationsAfter)
			return
		}
		record("no generation was allocated: view_generations is empty and AUTOINCREMENT never took a rowid for it")
	}

	if expect.Change == w8m5RealChange {
		census()
		if len(obs.PayloadDiffs) == 0 {
			fail("gate2 instrument: a real, view-visible content change moved no payload row at all — the witness every no-op row in this matrix rests on is blind")
		} else {
			record("the real change moved %d payload row(s): %s", len(obs.PayloadDiffs),
				w8m5Truncate(strings.Join(obs.PayloadDiffs, " ;; "), 400))
		}
		for _, diff := range obs.CatalogDiffs {
			record("recorded: %s", w8m5Truncate(diff, 220))
		}
		if obs.SeqDelta != 0 {
			record("view_generations seq %+d", obs.SeqDelta)
		}
		if obs.CountersError != "" {
			record("counters unavailable (ledger row W8.3 emits them): %s", obs.CountersError)
		}
		if len(verdict.Failures) > 0 {
			verdict.Status = w8m5StatusFail
		}
		return verdict
	}

	if !live.Calibrated {
		verdict.Status = w8m5StatusSkip
		record("not measurable: the gate-2 instrument was never calibrated, so no no-op clause can be asserted")
		return verdict
	}
	census()

	asserted := 0
	if live.DirtyPayload {
		asserted++
		if len(obs.PayloadDiffs) > 0 {
			fail("gate2: the base payload moved over an effective no-op: %s",
				w8m5Truncate(strings.Join(obs.PayloadDiffs, " ;; "), 400))
		}
	} else {
		record("payload clause NOT asserted: the calibration's real working-tree change moved no payload row, so this witness cannot report a no-op")
	}

	if live.Seq {
		asserted++
		if obs.SeqDelta != 0 {
			fail("gate2: view_generations seq moved by %+d over an effective no-op", obs.SeqDelta)
		}
	} else {
		record("sequence clause NOT asserted: the calibration's real change did not move sqlite_sequence.seq")
	}

	if live.Catalog {
		asserted++
		for _, diff := range obs.CatalogDiffs {
			if w8m5DiffAllowed(diff, expect.CatalogAllow) {
				record("recorded: %s", w8m5Truncate(diff, 220))
				continue
			}
			fail("gate2: catalog changed: %s", w8m5Truncate(diff, 400))
		}
	} else {
		for _, diff := range obs.CatalogDiffs {
			record("recorded (clause not asserted): %s", w8m5Truncate(diff, 220))
		}
		record("catalog clause NOT asserted: the calibration's real change wrote no catalog row")
	}

	switch {
	case obs.CountersError != "":
		record("counters unavailable (ledger row W8.3 emits them): %s", obs.CountersError)
	case live.Counters:
		asserted++
		if moved := w8m5MovedSeries(obs.CounterDelta, w8m5AllocationSeries()); len(moved) > 0 {
			fail("gate2: allocation counters moved: %s", strings.Join(moved, " "))
		}
	default:
		record("counter clause NOT asserted: the calibration's real change moved no allocation series")
	}

	if expect.Replay {
		switch {
		case obs.CountersError != "":
			record("gate4: the claim/replay assertion could not run — the counters are unavailable")
		default:
			if moved := w8m5MovedSeries(obs.CounterDelta, w8m5ReplaySeries()); len(moved) == 0 {
				fail("gate4: no claim/replay counter moved; the daemon did not report reuse")
			} else {
				record("replay: %s", strings.Join(moved, " "))
			}
		}
	}

	switch {
	case len(verdict.Failures) > 0:
		// A transport failure never discards a clause that failed.
		verdict.Status = w8m5StatusFail
	case asserted == 0:
		verdict.Status = w8m5StatusSkip
		record("not measurable on this instrument: the calibration's real change moved none of the witnesses this row would rest on")
	case obs.CountersError != "":
		verdict.Status = w8m5StatusSkip
	}
	return verdict
}

// w8m5NoopCase is one row of matrix 1.
type w8m5NoopCase struct {
	Name string
	Gate string
	// Apply performs the case and waits for whatever evidence it promises.
	Apply  func(h *w8m5NoopHarness)
	Expect w8m5NoopExpect
}

// w8m5NoopCaseNames is the handoff's own vocabulary for this matrix, in the
// order the bullet lists it. The case table is pinned to it so a case cannot
// quietly disappear from the matrix.
func w8m5NoopCaseNames() []string {
	return []string{
		"clean_idle_polling",
		"repeated_samples",
		"touch",
		"stage_unstage",
		"same_tree_amend",
		"same_size_changed_bytes_restored_mtime",
		"atomic_replacement",
		"uncertain_filesystem_identity",
		"dirty_idle_polling",
	}
}

// w8m5NoopCases is matrix 1.
func w8m5NoopCases() []w8m5NoopCase {
	return []w8m5NoopCase{
		{
			Name: "clean_idle_polling", Gate: "gate2",
			Apply:  func(h *w8m5NoopHarness) { h.idle(h.idleWindow) },
			Expect: w8m5NoopExpect{Detail: "idle polling over a clean tree allocated nothing"},
		},
		{
			Name: "repeated_samples", Gate: "gate2",
			Apply: func(h *w8m5NoopHarness) {
				for i := 0; i < 8; i++ {
					h.f.command(w8m5CommandTimeout, h.f.primary, "daemon", "status", "--format", "json", "--no-progress")
					h.mustAnswer(h.probeName, h.probePath)
				}
			},
			Expect: w8m5NoopExpect{Detail: "repeated status and search samples allocated nothing"},
		},
		{
			Name: "touch", Gate: "gate2",
			Apply: func(h *w8m5NoopHarness) {
				h.rewriteIdentical(h.probePath)
				h.quiet()
				h.mustAnswer(h.probeName, h.probePath)
			},
			Expect: w8m5NoopExpect{Detail: "identical bytes with a new mtime allocated nothing"},
		},
		{
			Name: "stage_unstage", Gate: "gate2",
			Apply: func(h *w8m5NoopHarness) {
				h.f.git(h.f.primary, "add", "-A")
				h.quiet()
				h.f.git(h.f.primary, "reset")
				h.quiet()
				h.mustAnswer(h.probeName, h.probePath)
			},
			Expect: w8m5NoopExpect{Detail: "staging and unstaging an unchanged tree allocated nothing"},
		},
		{
			Name: "same_tree_amend", Gate: "gate2+gate4",
			Apply: func(h *w8m5NoopHarness) {
				// An empty commit followed by an amend of it: HEAD moves
				// twice over a tree that never changes. --allow-empty is
				// required on both, otherwise git refuses the amend that
				// would leave the commit empty.
				h.f.git(h.f.primary, "commit", "--allow-empty", "-m", "w8m5-pre-amend")
				h.quiet()
				treeBefore := h.gitOutput("rev-parse", "HEAD^{tree}")
				headBefore := h.gitOutput("rev-parse", "HEAD")
				h.f.git(h.f.primary, "commit", "--amend", "--allow-empty", "--no-edit", "--date=now")
				treeAfter := h.gitOutput("rev-parse", "HEAD^{tree}")
				headAfter := h.gitOutput("rev-parse", "HEAD")
				if treeBefore != treeAfter {
					h.t.Fatalf("amend was meant to keep the tree: %s -> %s", treeBefore, treeAfter)
				}
				if headBefore == headAfter {
					h.t.Fatalf("amend did not move HEAD away from %s", headBefore)
				}
				h.quiet()
				h.mustAnswer(h.probeName, h.probePath)
			},
			Expect: w8m5NoopExpect{
				Replay:       true,
				CatalogAllow: []string{"HeadCommit:"},
				Detail:       "a same-tree amend replayed the active identity instead of allocating",
			},
		},
		{
			Name: "same_size_changed_bytes_restored_mtime", Gate: "gate1+gate2",
			Apply: func(h *w8m5NoopHarness) {
				// Rev0 -> Rev1 is a same-length rename, so the file keeps
				// its size; the mtime is put back afterwards. Size and
				// mtime are therefore both unchanged and only the content
				// hash can tell the daemon the file moved.
				next := w8ProbeName(h.probeIndex, 1)
				h.replacePreservingStat(h.probePath, h.probeName, next)
				h.waitAnswer(next, h.probePath)
				h.waitAbsent(h.probeName, h.probePath)
				h.probeName = next
			},
			Expect: w8m5NoopExpect{
				Change: w8m5RealChange,
				Detail: "a same-size, same-mtime content change was observed (not a no-op: the view AND the payload witness must move)",
			},
		},
		{
			Name: "atomic_replacement", Gate: "gate1+gate4",
			Apply: func(h *w8m5NoopHarness) {
				// The editor pattern: write a sibling temp file and rename
				// it over the target. The content goes back to revision 0,
				// which is also the undo half of gate 4.
				h.renameOver(h.probePath, h.original, false)
				h.waitAnswer(w8ProbeName(h.probeIndex, 0), h.probePath)
				h.waitAbsent(h.probeName, h.probePath)
				h.probeName = w8ProbeName(h.probeIndex, 0)
			},
			Expect: w8m5NoopExpect{
				Change: w8m5RealChange,
				Detail: "an atomically replaced file was observed and the undone content answered again",
			},
		},
		{
			Name: "uncertain_filesystem_identity", Gate: "gate2",
			Apply: func(h *w8m5NoopHarness) {
				// Byte-identical content arriving on a NEW inode with the
				// original mtime restored: every cheap identity signal
				// (size, mtime, path) says nothing happened while the
				// inode says everything did.
				h.renameOver(h.probePath, h.original, true)
				h.quiet()
				h.mustAnswer(h.probeName, h.probePath)
			},
			Expect: w8m5NoopExpect{Detail: "identical bytes on a new inode allocated nothing"},
		},
		{
			Name: "dirty_idle_polling", Gate: "gate2",
			Apply: func(h *w8m5NoopHarness) {
				dirty := w8ProbeName(h.probeIndex, 2)
				h.replacePreservingStat(h.probePath, h.probeName, dirty)
				h.waitAnswer(dirty, h.probePath)
				h.probeName = dirty
				// The edit above is this case's SETUP, not what it measures:
				// the assertion is about the idle window over an already
				// dirty tree. Re-open the whole measurement window — catalog,
				// payload and counters — so the setup is not charged to it.
				h.settle()
				h.rebaseline()
				h.idle(h.idleWindow)
			},
			Expect: w8m5NoopExpect{Detail: "idle polling over a dirty tree allocated nothing after the edit settled"},
		},
	}
}

// --------------------------------------------------------------- harness ---

type w8m5NoopHarness struct {
	t          *testing.T
	f          *issue767Fixture
	db         *sql.DB
	table      *w8m5Table
	idleWindow time.Duration

	probeIndex int
	probePath  string
	probeName  string
	probeRel   string
	original   string

	// live is what the calibration proved this instrument can see. Every
	// no-op clause is gated on it.
	live w8m5Liveness

	// baseCatalog, basePayload and countersBase are the case's measurement
	// window. A case whose own setup legitimately changes the tree before the
	// behaviour it measures (dirty_idle_polling edits, waits, and only then
	// idles) re-opens the window with rebaseline, so the setup is not charged
	// to the assertion.
	baseCatalog  w8m5Catalog
	basePayload  w8m5Payload
	countersBase map[string]int64
	countersErr  error
}

// rebaseline re-opens the whole measurement window: catalog, payload and
// counters together. Resetting only the counters would leave the payload and
// catalog observations spanning the case's own setup, which is how a case that
// really did change the tree could be scored as a no-op — or, with the payload
// witness added, be failed for its own setup.
func (h *w8m5NoopHarness) rebaseline() {
	h.baseCatalog = h.catalog()
	h.basePayload = h.payload()
	h.resetCounters()
}

// swapT installs a case's own *testing.T on the harness AND on the shared
// fixture, and returns the restore. The fixture half bounds a failing fixture
// operation (a git command, a CLI call) to the case that caused it instead of
// ending the matrix; the restore is required, because the parent's cleanup
// calls the fixture back after the subtests are over.
func (h *w8m5NoopHarness) swapT(t *testing.T) func() {
	previous, previousFixture := h.t, h.f.t
	h.t, h.f.t = t, t
	return func() { h.t, h.f.t = previous, previousFixture }
}

func (h *w8m5NoopHarness) catalog() w8m5Catalog {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return w8m5ReadCatalog(ctx, h.db)
}

// payload observes the base graph's own rows for the repository, with the
// case's probe file named individually.
func (h *w8m5NoopHarness) payload() w8m5Payload { return h.payloadOf(h.probeRel) }

// payloadOf is payload with an explicit file, so the calibration can name the
// file IT edits rather than the file the cases probe.
func (h *w8m5NoopHarness) payloadOf(rel string) w8m5Payload {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return w8m5ReadPayload(ctx, h.db, issue767FixturePrefix, rel)
}

func (h *w8m5NoopHarness) counters() (map[string]int64, error) {
	output, err := h.f.tryCommand(w8m5CommandTimeout, h.f.primary, "daemon", "status", "--format", "json", "--no-progress")
	if err != nil {
		return nil, fmt.Errorf("daemon status --format json unavailable: %w", err)
	}
	return w8ParseStatusCounters(output)
}

func (h *w8m5NoopHarness) resetCounters() {
	h.countersBase, h.countersErr = h.counters()
}

// quiet waits out a window that covers several coordinator polls and janitor
// ticks, so a case that expects nothing to happen has given the daemon every
// opportunity to do something.
func (h *w8m5NoopHarness) quiet() {
	select {
	case <-h.t.Context().Done():
		h.t.Fatal(h.t.Context().Err())
	case <-time.After(w8m5SettleQuiet):
	}
}

// idle holds for a window while proving the view stays readable. A no-op that
// broke the served view is not a no-op.
func (h *w8m5NoopHarness) idle(window time.Duration) {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		h.mustAnswer(h.probeName, h.probePath)
		select {
		case <-h.t.Context().Done():
			h.t.Fatal(h.t.Context().Err())
		case <-time.After(5 * time.Second):
		}
	}
}

func (h *w8m5NoopHarness) mustAnswer(name, file string) {
	h.t.Helper()
	found, err := h.f.trySearchSymbolIn(h.f.primary, name, file)
	if err != nil || !found {
		h.t.Fatalf("the selected view stopped answering for %s in %s: found=%v err=%v", name, file, found, err)
	}
}

// waitAnswer and waitAbsent are the fixture's symbol waits with the failure
// bound to the CASE's own *testing.T (see the soft-wait note above), so an
// unmet wait fails one row instead of ending the matrix.
func (h *w8m5NoopHarness) waitAnswer(name, file string) {
	h.t.Helper()
	ok := w8m5SoftAwait(h.t.Context(), w8m5ProbeTimeout, 500*time.Millisecond, func() bool {
		found, err := h.f.trySearchSymbolIn(h.f.primary, name, file)
		return err == nil && found
	})
	if !ok {
		h.t.Fatalf("the view never answered for %s in %s within %s; fixture artifacts %s", name, file, w8m5ProbeTimeout, h.f.root)
	}
}

func (h *w8m5NoopHarness) waitAbsent(name, file string) {
	h.t.Helper()
	ok := w8m5SoftAwait(h.t.Context(), w8m5AbsenceTimeout, 500*time.Millisecond, func() bool {
		return w8m5AbsentFrom(h.f, name, file)
	})
	if !ok {
		h.t.Fatalf("%s was never withdrawn from %s within %s: %s; fixture artifacts %s",
			name, file, w8m5AbsenceTimeout, w8m5AbsenceEvidence(h.f, name, file), h.f.root)
	}
}

// settle waits for the daemon to go quiet. Non-convergence is reported to the
// caller rather than failing: a host that is busy enough to keep a generation
// counter moving for two minutes has not, by that fact alone, broken a gate —
// the row records that the closing observation was taken over a moving target.
func (h *w8m5NoopHarness) settle() bool {
	return w8m5SoftSettle(h.t.Context(), h.db, 2*time.Minute)
}

// rewriteIdentical writes a file's own bytes back, which moves its mtime and
// nothing else.
func (h *w8m5NoopHarness) rewriteIdentical(path string) {
	h.t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	h.f.write(path, string(source))
}

// replacePreservingStat performs a same-length replacement in place and then
// restores the file's previous modification time, so size and mtime are both
// unchanged afterwards.
func (h *w8m5NoopHarness) replacePreservingStat(path, from, to string) {
	h.t.Helper()
	if len(from) != len(to) {
		h.t.Fatalf("replacement is not same-length: %q -> %q", from, to)
	}
	info, err := os.Stat(path)
	if err != nil {
		h.t.Fatal(err)
	}
	source, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	if !strings.Contains(string(source), from) {
		h.t.Fatalf("%s does not carry %s", path, from)
	}
	h.f.write(path, strings.ReplaceAll(string(source), from, to))
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		h.t.Fatal(err)
	}
	if after, err := os.Stat(path); err != nil {
		h.t.Fatal(err)
	} else if after.Size() != info.Size() {
		h.t.Fatalf("replacement changed the file size: %d -> %d", info.Size(), after.Size())
	}
}

// renameOver writes content to a sibling temporary file and renames it over
// path — the atomic-replacement pattern every editor uses. When keepStat is
// set the destination's previous modification time is restored, which is the
// uncertain-identity case: new inode, same bytes, same mtime, same size.
func (h *w8m5NoopHarness) renameOver(path, content string, keepStat bool) {
	h.t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		h.t.Fatal(err)
	}
	temp := path + ".w8m5tmp"
	h.f.write(temp, content)
	if err := os.Rename(temp, path); err != nil {
		h.t.Fatal(err)
	}
	if keepStat {
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *w8m5NoopHarness) gitOutput(args ...string) string {
	h.t.Helper()
	output, err := h.f.tryGit(h.f.primary, args...)
	if err != nil {
		h.t.Fatalf("fixture git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// run executes one case inside the measurement envelope and files its row.
func (h *w8m5NoopHarness) run(c w8m5NoopCase) {
	h.t.Helper()
	started := time.Now()
	h.rebaseline()
	row := w8m5Row{Case: c.Name, Gate: c.Gate, Status: w8m5StatusPass, Detail: c.Expect.Detail}

	c.Apply(h)
	// A case that changed something waits for the change to settle before
	// its closing observation, so the measurement is of a settled state and
	// not of a build still in flight.
	settled := h.settle()
	if !settled {
		row.Detail = strings.TrimSpace(row.Detail + " | the daemon never produced three stable generation samples: the closing observation was taken over a moving target")
	}

	before, beforePayload := h.baseCatalog, h.basePayload
	after := h.catalog()
	afterPayload := h.payload()
	row.Seconds = time.Since(started).Seconds()
	row.Bookkeeping = w8m5BookkeepingDelta(before, after)
	if delta := afterPayload.FileStamp - beforePayload.FileStamp; delta != 0 {
		if row.Bookkeeping == nil {
			row.Bookkeeping = map[string]int64{}
		}
		row.Bookkeeping["payload_updated_at_sum"] = delta
	}

	obs := w8m5NoopObservation{
		CatalogErrors:    after.Errors,
		PayloadErrors:    append(append([]string{}, beforePayload.Errors...), afterPayload.Errors...),
		SeqDelta:         after.Sequence - before.Sequence,
		CatalogDiffs:     w8m5CatalogDiff(before, after),
		PayloadDiffs:     w8m5PayloadDiff(beforePayload, afterPayload),
		GenerationsAfter: len(after.Generations),
		SequenceRowAfter: after.SequenceRow,
	}
	afterCounters, err := h.counters()
	switch {
	case h.countersErr != nil:
		obs.CountersError = h.countersErr.Error()
	case err != nil:
		obs.CountersError = err.Error()
	default:
		obs.CounterDelta = w8CounterDelta(h.countersBase, afterCounters)
		row.Counters = obs.CounterDelta
	}

	verdict := w8m5NoopVerdict(c.Expect, h.live, obs)
	row.Status = verdict.Status
	row.Detail = verdict.detail(row.Detail)
	if verdict.Status == w8m5StatusFail {
		h.t.Errorf("matrix1 %s (%s): %s", c.Name, c.Gate, strings.Join(verdict.Failures, " ;; "))
	}
	h.table.add(row)
}

// ------------------------------------------------------------- soft waits ---

// The matrix never waits through the shared fixture's own await/settle.
//
// Those are fatal on the FIXTURE's *testing.T, which is the parent of every
// case subtest: one unmet wait there ends the whole matrix, and the cases after
// it are never measured or reported. An unmet wait is itself an outcome this
// table has to carry (a case the branch does not carry to a verdict is a
// reported row, never a silent absence), so the matrix polls with its own
// non-fatal equivalents and decides what the timeout means per case.

// w8m5SoftAwait polls ready at interval until it holds or the timeout expires,
// and reports whether it held. It fails nothing.
func w8m5SoftAwait(ctx context.Context, timeout, interval time.Duration, ready func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if ready() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
}

// w8m5SettleInterval is the sampling cadence of the stability rule, matched to
// the shared fixture's settle so "three stable samples" means the same span of
// quiet here as it does there.
const w8m5SettleInterval = 5 * time.Second

// w8m5SoftSettle waits for three identical generation snapshots — the shared
// fixture's own stability rule, at the same cadence — and reports whether it
// got them instead of ending the test when it does not.
func w8m5SoftSettle(ctx context.Context, db *sql.DB, timeout time.Duration) bool {
	var previous issue767GenerationSnapshot
	stable := 0
	return w8m5SoftAwait(ctx, timeout, w8m5SettleInterval, func() bool {
		read, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		current, err := issue767ReadGenerations(read, db)
		if err != nil {
			stable = 0
			return false
		}
		if current == previous {
			stable++
		} else {
			stable = 0
		}
		previous = current
		return stable >= 3
	})
}

// w8m5AbsentFrom reports whether a name no longer answers OUT OF ONE FILE.
//
// The shared fixture's own verdict cannot express this. It folds "the name
// answered, but from a different file" into an ERROR
// (issue767_fixture_shared_test.go:468-470, "symbol did not belong to selected
// source file and repository"), which is the right reading for its callers —
// they are proving a symbol came from the checkout they selected — and the
// wrong one for a rename, where the declaration is SUPPOSED to answer from the
// new path and the assertion is only that the old path no longer serves it.
// Observed directly: file_renamed sat in that wait for the full three minutes
// and failed, while the daemon had already moved the declaration correctly.
//
// This asks the same question of the same answer and reads the file-membership
// evidence instead of the verdict. A transport failure or a fallback answer is
// not evidence of absence and reports false, so a broken daemon cannot be
// mistaken for a successful withdrawal.
func w8m5AbsentFrom(f *issue767Fixture, name, file string) bool {
	return w8m5AbsenceVerdict(f.askSymbol(f.primary, name, file, f.spellingFor(f.primary)))
}

// w8m5AbsenceVerdict is the rule w8m5AbsentFrom applies, separated so it can be
// pinned without a daemon.
func w8m5AbsenceVerdict(answer issue767Answer, err error) bool {
	if err != nil || answer.Fallback {
		return false
	}
	return !answer.FromExpectedFile
}

// w8m5AbsenceEvidence renders why a withdrawal wait did not succeed, in the
// daemon's own terms. A row that only says "never withdrawn" cannot be acted
// on: "the name is still served out of that file" and "the search never
// answered" are different findings, and only one of them is about the branch.
func w8m5AbsenceEvidence(f *issue767Fixture, name, file string) string {
	answer, err := f.askSymbol(f.primary, name, file, f.spellingFor(f.primary))
	if err != nil {
		return "the search itself failed: " + err.Error()
	}
	switch {
	case answer.Fallback:
		return fmt.Sprintf("the search answered with a fallback or tool error (exact=%v, error=%q), which is not evidence of absence", answer.Exact, answer.Error)
	case answer.FromExpectedFile:
		return fmt.Sprintf("the name is STILL served out of that file (found=%v, repo_prefix=%q)", answer.Found, answer.Prefix)
	default:
		return fmt.Sprintf("the last answer read as absent (found=%v, from_expected_file=false) — the wait and the verdict disagree", answer.Found)
	}
}

// w8m5AbortedRow is the row a case files when it could not be carried to a
// verdict at all — a wait that timed out, a fixture operation that failed. The
// outcome table has to say so: a case that vanishes from the table reads as a
// case that was never in the matrix, which is the silent pass the wave
// constraints forbid.
func w8m5AbortedRow(name, gate string) w8m5Row {
	return w8m5Row{Case: name, Gate: gate, Status: w8m5StatusFail,
		Detail: "the case aborted before it could be scored (see the subtest log above): the branch did not carry it to a verdict"}
}

// w8m5RunGuarded runs one case body in its own subtest and guarantees a row.
//
// Every wait in this matrix is fatal by construction (the shared fixture's
// await calls Fatalf), so without this a single case the branch cannot carry
// would end the whole matrix and the remaining cases would never be measured.
// The subtest bounds the abort to one case; the deferred filer guarantees the
// table still carries it. swap installs the subtest's *testing.T on the
// harness for the duration, so a failure is attributed to the case.
func w8m5RunGuarded(parent *testing.T, table *w8m5Table, name, gate string, swap func(*testing.T) func(), body func()) {
	parent.Run(name, func(st *testing.T) {
		restore := swap(st)
		filed := false
		defer func() {
			restore()
			if !filed {
				table.add(w8m5AbortedRow(name, gate))
			}
		}()
		body()
		filed = true
	})
}

func w8m5DiffAllowed(diff string, allow []string) bool {
	for _, fragment := range allow {
		if strings.Contains(diff, fragment) {
			return true
		}
	}
	return false
}

func w8m5Truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

// ------------------------------------------------------------ opt-in entry ---

// w8m5Binary is the opt-in gate every isolated matrix shares.
func w8m5Binary(t *testing.T) string {
	t.Helper()
	raw := os.Getenv(w8m5BinaryEnv)
	if raw == "" {
		raw = os.Getenv(w8m5BinaryEnvAlt)
	}
	if raw == "" {
		t.Skipf("set %s (or %s) to opt into the isolated end-to-end matrix (a private daemon binary; never the user's daemon)", w8m5BinaryEnv, w8m5BinaryEnvAlt)
	}
	binary, err := filepath.Abs(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	return binary
}

// w8m5FixtureSpec is the corpus the matrices run over: small enough that a
// fresh isolated index per case is affordable, large enough to carry real
// intra- and cross-package resolution.
func w8m5FixtureSpec(t *testing.T) w8FixtureSpec {
	t.Helper()
	spec := w8FixtureSpec{Files: 24, Packages: 4, Seed: 8005}
	if raw := os.Getenv("GXW8_MATRIX_FILES"); raw != "" {
		var files int
		if _, err := fmt.Sscanf(raw, "%d", &files); err != nil || files < 4 || files > 2000 {
			t.Fatalf("GXW8_MATRIX_FILES=%q is not an integer in [4,2000]", raw)
		}
		spec.Files = files
	}
	return spec.normalize()
}

// w8m5IdleWindow is how long the idle cases hold. The default covers several
// 15 s coordinator polls and, at the fixture's accelerated 5 s reconcile
// interval, many janitor ticks.
func w8m5IdleWindow(t *testing.T) time.Duration {
	t.Helper()
	window := 45 * time.Second
	if raw := os.Getenv("GXW8_MATRIX_IDLE"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < 5*time.Second || parsed > 10*time.Minute {
			t.Fatalf("GXW8_MATRIX_IDLE=%q is not a duration in [5s,10m]", raw)
		}
		window = parsed
	}
	return window
}

// w8m5CaseFilter is the optional case selector, so an operator can re-run one
// row without paying for the whole matrix.
func w8m5CaseFilter(t *testing.T) *regexp.Regexp {
	t.Helper()
	raw := os.Getenv("GXW8_MATRIX_CASES")
	if raw == "" {
		return nil
	}
	filter, err := regexp.Compile(raw)
	if err != nil {
		t.Fatalf("GXW8_MATRIX_CASES=%q is not a regexp: %v", raw, err)
	}
	return filter
}

// w8m5NewFixture starts one isolated daemon over a generated corpus and waits
// until the primary answers exactly.
func w8m5NewFixture(t *testing.T, binary string, spec w8FixtureSpec) *issue767Fixture {
	t.Helper()
	files := w8GenerateFixture(spec)
	f := newIssue767FixtureWithCorpus(t, binary, func(f *issue767Fixture) {
		for _, file := range files {
			f.write(filepath.Join(f.primary, filepath.FromSlash(file.Path)), file.Content)
		}
	})
	f.start()
	f.awaitSymbolIn(f.primary, w8PrimaryMarker, filepath.Join(f.primary, "marker.go"), w8m5ProbeTimeout)
	f.settle()
	return f
}

// ------------------------------------------------------------ calibration ---

// w8m5CalibrationCase is the name the calibration's own outcome row carries. It
// is not one of the handoff's nine cases; it is the measurement that makes the
// nine mean something, and it is reported as a row of the same table so a
// reader can see what the instrument could and could not see.
const w8m5CalibrationCase = "instrument_calibration"

// w8m5CalibrationHalf is what one real change moved.
type w8m5CalibrationHalf struct {
	Label    string   `json:"label"`
	Seq      int64    `json:"sequence_delta"`
	Catalog  []string `json:"catalog"`
	Payload  []string `json:"payload"`
	Counters []string `json:"allocation_counters"`
	Error    string   `json:"counters_error,omitempty"`
}

func (half w8m5CalibrationHalf) moved() bool {
	return half.Seq != 0 || len(half.Catalog) > 0 || len(half.Payload) > 0 || len(half.Counters) > 0
}

func (half w8m5CalibrationHalf) String() string {
	parts := []string{fmt.Sprintf("seq %+d", half.Seq)}
	parts = append(parts, fmt.Sprintf("payload %d row(s)", len(half.Payload)))
	parts = append(parts, fmt.Sprintf("catalog %d change(s)", len(half.Catalog)))
	if half.Error != "" {
		parts = append(parts, "counters unavailable: "+half.Error)
	} else {
		parts = append(parts, fmt.Sprintf("allocation series [%s]", strings.Join(half.Counters, " ")))
	}
	return half.Label + ": " + strings.Join(parts, ", ")
}

// w8m5LivenessFrom assembles the calibration's two halves into the liveness the
// verdict is gated on. It is pure because the load-bearing asymmetry lives
// here: DirtyPayload comes from the WORKING-TREE half alone — a commit moving
// the payload says nothing about whether a dirty edit does, and reading it from
// either half would reinstate exactly the blindness the review found — while
// the view-catalog witnesses may be proven live by either half, because a no-op
// row forbids an allocation from any path.
func w8m5LivenessFrom(dirty, committed w8m5CalibrationHalf) w8m5Liveness {
	return w8m5Liveness{
		Calibrated:   true,
		DirtyPayload: len(dirty.Payload) > 0,
		Seq:          dirty.Seq != 0 || committed.Seq != 0,
		Catalog:      len(dirty.Catalog) > 0 || len(committed.Catalog) > 0,
		Counters:     len(dirty.Counters) > 0 || len(committed.Counters) > 0,
		Detail:       dirty.String() + " ;; " + committed.String(),
	}
}

// w8m5CalibrationProblems scores the calibration row itself: what must be true
// of the INSTRUMENT before any no-op row asserts with it. It is pure so both
// rules are pinned without a daemon.
//
// Rule 1 — a real content change must move SOMETHING this matrix can observe,
// in one half or the other. If it moves nothing, "nothing moved" is also what a
// genuine edit produces and no no-op row can carry evidence.
//
// Rule 2 — the COMMITTED half must allocate. It is the only half that exercises
// the committed-base lane, and it is the only half that can make the sequence
// and allocation-counter clauses live; without it the nine no-op rows fall back
// to the payload witness alone and same_tree_amend's gate-4 replay assertion
// has no counters to read. That is precisely the state the fixture's dependent
// checkout exists to prevent — a family with no dependent checkout defers its
// committed-base publication (internal/indexer/dedicated_base_startup.go:776) —
// so reporting it as a FAILURE is what keeps that dependent load-bearing:
// delete it and this row names the consequence instead of the matrix quietly
// degrading to "NOT ASSERTED" everywhere.
func w8m5CalibrationProblems(dirty, committed w8m5CalibrationHalf) []string {
	var problems []string
	if !dirty.moved() && !committed.moved() {
		problems = append(problems, "gate2 instrument: a real content change moved NOTHING this matrix can observe, in either half — no no-op row in this matrix can carry evidence")
	}
	if committed.Seq == 0 && len(committed.Counters) == 0 {
		problems = append(problems, "gate2 instrument: the committed half allocated nothing — no sequence movement and no allocation counter — so the sequence and counter clauses are dead for every no-op row and the gate-4 replay assertion has nothing to read."+
			" A family with no dependent checkout defers its committed-base publication (internal/indexer/dedicated_base_startup.go:776), so this matrix's fixture must carry one")
	}
	return problems
}

// w8m5Calibrate measures the instrument BEFORE the matrix asserts with it.
//
// The adversarial review's blocker: on this fixture a real, same-size,
// view-visible working-tree content change moved no catalog row, no sequence
// number and no allocation counter — so "nothing moved" was also what a genuine
// edit produced, and every no-op row resting on those three witnesses was
// vacuous. A positive control that is only ALLOWED to move cannot detect that;
// only a control that is REQUIRED to move can.
//
// So the matrix opens by making a real change to a file no case touches, in two
// halves — working tree first, then committed — and recording what each half
// moved. The result gates every later clause (w8m5NoopVerdict): a witness a
// real change did not move may not report a no-op, and a row left with no live
// clause is a SKIP that names the reason. The two positive controls then assert
// the converse on the payload witness, so re-declaring one of them a no-op
// fails the case.
func w8m5Calibrate(parent *testing.T, h *w8m5NoopHarness, rel string, index int) {
	w8m5RunGuarded(parent, h.table, w8m5CalibrationCase, "instrument", h.swapT, func() {
		started := time.Now()
		row := w8m5Row{Case: w8m5CalibrationCase, Gate: "instrument", Status: w8m5StatusPass}
		file := filepath.Join(h.f.primary, filepath.FromSlash(rel))
		source, err := os.ReadFile(file)
		if err != nil {
			h.t.Fatal(err)
		}
		edited, err := w8EditFileSource(string(source), index, 1)
		if err != nil {
			h.t.Fatal(err)
		}

		measure := func(label string, apply func()) w8m5CalibrationHalf {
			beforeCatalog, beforePayload := h.catalog(), h.payloadOf(rel)
			h.resetCounters()
			apply()
			h.settle()
			afterCatalog, afterPayload := h.catalog(), h.payloadOf(rel)
			half := w8m5CalibrationHalf{
				Label:   label,
				Seq:     afterCatalog.Sequence - beforeCatalog.Sequence,
				Catalog: w8m5CatalogDiff(beforeCatalog, afterCatalog),
				Payload: w8m5PayloadDiff(beforePayload, afterPayload),
			}
			afterCounters, err := h.counters()
			switch {
			case h.countersErr != nil:
				half.Error = h.countersErr.Error()
			case err != nil:
				half.Error = err.Error()
			default:
				half.Counters = w8m5MovedSeries(w8CounterDelta(h.countersBase, afterCounters), w8m5AllocationSeries())
			}
			return half
		}

		dirty := measure("a working-tree content change", func() {
			h.f.write(file, edited)
			h.waitAnswer(w8ProbeName(index, 1), file)
		})
		committed := measure("the same change committed", func() {
			h.f.git(h.f.primary, "add", "-A")
			h.f.git(h.f.primary, "commit", "-m", "w8m5: calibrate the gate-2 instrument")
			h.quiet()
			h.mustAnswer(w8ProbeName(index, 1), file)
		})

		h.live = w8m5LivenessFrom(dirty, committed)
		row.Seconds = time.Since(started).Seconds()
		row.Detail = "live witnesses after a real change — " + h.live.Detail
		if !h.live.DirtyPayload {
			row.Detail += " | the payload witness did NOT move for a working-tree-only change: every working-tree no-op row is reported as not measurable rather than as a pass"
		}
		// Both instrument rules live in w8m5CalibrationProblems, pure and
		// unit-pinned; this site only reports what it returns.
		for _, problem := range w8m5CalibrationProblems(dirty, committed) {
			row.Status = w8m5StatusFail
			row.Detail += " | " + problem
			h.t.Errorf("matrix1 %s: %s", w8m5CalibrationCase, problem)
		}
		h.table.add(row)
	})
}

// TestW8MatrixNoopFamily is matrix 1.
func TestW8MatrixNoopFamily(t *testing.T) {
	binary := w8m5Binary(t)
	spec := w8m5FixtureSpec(t)
	filter := w8m5CaseFilter(t)

	table := w8m5NewTable(t, "w8_matrix1_noop")
	defer table.render()

	f := w8m5NewFixture(t, binary, spec)
	defer f.stop()

	// A dependent checkout, added before anything is measured.
	//
	// It is not decoration and it is not a case: it is what makes the
	// view-catalog witnesses this matrix owns LIVE AT ALL. Since the consumer
	// gate (internal/indexer/dedicated_base_startup.go:774-779, the
	// `if !consumers { out.Skipped = "no dependent checkout" }` arm) a family
	// whose only checkout is the primary never publishes a committed base and
	// therefore never allocates a generation — so on a single-checkout fixture
	// sqlite_sequence, view_generations and the whole allocation counter
	// family stay still for a REAL committed change exactly as they do for a
	// no-op, and every no-op row resting on them says nothing. Measured: the
	// same calibration that moved `seq +1, catalog 3 change(s), [claim{built}
	// publish{delta} generation_published{checkout}]` before the gate moved
	// `seq +0, catalog 1 change(s), []` after it, and the gate-4 replay
	// assertion of same_tree_amend lost its counters with them.
	//
	// One dependent restores the committed-base lane: the family has a
	// consumer, the calibration's committed half publishes again, and the nine
	// no-op rows assert against witnesses a real change is proven to move. The
	// calibration below FAILS if that stops being true, so this cannot be
	// deleted silently.
	dependent := filepath.Join(f.root, "w8m5dep")
	f.git(f.primary, "worktree", "add", "-b", "w8m5-dependent", dependent)
	f.awaitSymbolAs(dependent, w8PrimaryMarker, filepath.Join(dependent, "marker.go"), w8m5ProbeTimeout, issue767AsAutomaticWorktree)
	f.settle()

	index := 0
	rel := w8FilePath(index%spec.Packages, index)
	probe := filepath.Join(f.primary, filepath.FromSlash(rel))
	original, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	h := &w8m5NoopHarness{
		t: t, f: f, db: f.openReadOnly(), table: table,
		idleWindow: w8m5IdleWindow(t),
		probeIndex: index, probePath: probe, probeName: w8ProbeName(index, 0), probeRel: rel,
		original: string(original),
	}
	h.waitAnswer(h.probeName, h.probePath)
	h.f.settle()

	// Calibrate the instrument on a file no case touches, and leave the tree
	// clean again (the commit half is part of the calibration), so the first
	// case still opens over a clean working tree.
	calibrationIndex := 1
	w8m5Calibrate(t, h, w8FilePath(calibrationIndex%spec.Packages, calibrationIndex), calibrationIndex)

	for _, c := range w8m5NoopCases() {
		if filter != nil && !filter.MatchString(c.Name) {
			table.add(w8m5Row{Case: c.Name, Gate: c.Gate, Status: w8m5StatusSkip,
				Detail: "not selected by GXW8_MATRIX_CASES"})
			continue
		}
		t.Logf("matrix1 case %s (%s)", c.Name, c.Gate)
		w8m5RunGuarded(t, table, c.Name, c.Gate, h.swapT, func() { h.run(c) })
	}
}

// ------------------------------------------------------------------ tests ---

// The instrument's own regressions. They run in an ordinary `go test`: every
// one of them fails if the corresponding rule in this file is removed.

func TestW8m5CatalogDiffIgnoresHeartbeatsAndNamesSemanticChanges(t *testing.T) {
	base := w8m5Catalog{
		Sequence:    7,
		Generations: []w8m5Generation{{ID: 7, State: "ready", TreeOID: "tree-a", StorageBytes: 100}},
		Checkouts:   []w8m5Checkout{{ID: "c1", State: "checkout_ready", HeadCommit: "commit-a"}},
		Routes:      []w8m5Route{{CheckoutID: "c1", GraphID: "g", Commit: 7, Dirty: -1, State: "ready"}},
		Bookkeeping: w8m5Bookkeeping{CheckoutLastSeen: 10, GenerationLastSelected: 3},
	}

	beating := base
	beating.Bookkeeping = w8m5Bookkeeping{CheckoutLastSeen: 999, CheckoutLastAccessible: 5, GenerationLastSelected: 42, FamilyLastSeen: 8}
	if diffs := w8m5CatalogDiff(base, beating); len(diffs) != 0 {
		t.Fatalf("heartbeat movement must not read as catalog DML, got %v", diffs)
	}
	delta := w8m5BookkeepingDelta(base, beating)
	if delta["checkouts_last_seen_sum"] != 989 || delta["generations_last_selected_sum"] != 39 {
		t.Fatalf("bookkeeping movement was not recorded: %v", delta)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*w8m5Catalog)
		expect string
	}{
		{"sequence", func(c *w8m5Catalog) { c.Sequence = 8 }, "view_generations seq 7 -> 8"},
		{"generation inserted", func(c *w8m5Catalog) {
			c.Generations = append(c.Generations, w8m5Generation{ID: 8, State: "ready"})
		}, "view_generations 8 inserted"},
		{"generation state", func(c *w8m5Catalog) { c.Generations[0].State = "superseded" }, "view_generations 7 changed"},
		{"generation storage", func(c *w8m5Catalog) { c.Generations[0].StorageBytes = 101 }, "view_generations 7 changed"},
		{"checkout state", func(c *w8m5Catalog) { c.Checkouts[0].State = "reconciling" }, "checkouts c1 changed"},
		{"checkout head", func(c *w8m5Catalog) { c.Checkouts[0].HeadCommit = "commit-b" }, "checkouts c1 changed"},
		{"route retired", func(c *w8m5Catalog) { c.Routes = nil }, "checkout_routes c1 deleted"},
		{"route epoch", func(c *w8m5Catalog) { c.Routes[0].Epoch = 1 }, "checkout_routes c1 changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := w8m5Catalog{Sequence: base.Sequence, Bookkeeping: base.Bookkeeping}
			after.Generations = append(after.Generations, base.Generations...)
			after.Checkouts = append(after.Checkouts, base.Checkouts...)
			after.Routes = append(after.Routes, base.Routes...)
			tc.mutate(&after)
			diffs := w8m5CatalogDiff(base, after)
			if len(diffs) == 0 {
				t.Fatalf("%s was not reported as a catalog change", tc.name)
			}
			joined := strings.Join(diffs, " ;; ")
			if !strings.Contains(joined, tc.expect) {
				t.Fatalf("diff did not name the change: want %q in %q", tc.expect, joined)
			}
		})
	}
}

func TestW8m5NoopCasesCoverTheDeclaredFamily(t *testing.T) {
	declared := w8m5NoopCaseNames()
	cases := w8m5NoopCases()
	if len(cases) != len(declared) {
		t.Fatalf("matrix 1 has %d cases, the declared family has %d", len(cases), len(declared))
	}
	for i, name := range declared {
		if cases[i].Name != name {
			t.Fatalf("case %d is %q, want %q (the handoff's own order)", i, cases[i].Name, name)
		}
		if cases[i].Apply == nil {
			t.Fatalf("case %q has no body", name)
		}
		if !strings.HasPrefix(cases[i].Gate, "gate") {
			t.Fatalf("case %q names no acceptance gate, got %q", name, cases[i].Gate)
		}
		if strings.TrimSpace(cases[i].Expect.Detail) == "" {
			t.Fatalf("case %q states no expectation", name)
		}
	}
	// The two correctness controls are the only real-change cases; everything
	// else is asserted as an effective no-op. Moving a case into
	// w8m5RealChange stops asserting gate 2 on it, and moving a control OUT of
	// it makes the control assert the opposite of what it exists for — both
	// are refused here, and w8m5NoopVerdict makes the second one fail the case
	// at run time as well.
	controls := map[string]bool{}
	for _, c := range cases {
		switch c.Expect.Change {
		case w8m5RealChange:
			controls[c.Name] = true
		case w8m5NoChange, "":
		default:
			t.Fatalf("case %q declares an unknown change kind %q", c.Name, c.Expect.Change)
		}
	}
	want := map[string]bool{"same_size_changed_bytes_restored_mtime": true, "atomic_replacement": true}
	if len(controls) != len(want) {
		t.Fatalf("real-change controls: %v, want %v", controls, want)
	}
	for name := range want {
		if !controls[name] {
			t.Fatalf("case %q must be declared a real change: it exists to prove the instrument can see one", name)
		}
	}
}

func TestW8m5CounterSeriesAreDeclaredInTheRegistry(t *testing.T) {
	declared := map[string]bool{}
	for _, name := range viewmetrics.SeriesNames() {
		declared[name] = true
	}
	series := append(w8m5AllocationSeries(), w8m5ReplaySeries()...)
	if len(series) < 8 {
		t.Fatalf("the counter vocabulary shrank to %d series", len(series))
	}
	for _, key := range series {
		name := w8m5SeriesName(key)
		if !declared[name] {
			t.Fatalf("series %q is not declared in the viewmetrics catalog; the assertion would silently read zero", key)
		}
		if !strings.HasSuffix(key, "}") && strings.Contains(key, "{") {
			t.Fatalf("series key %q is malformed", key)
		}
	}
	// The allocation and replay families must stay disjoint: a series in
	// both would make a replaying case fail its own no-op assertion.
	allocation := map[string]bool{}
	for _, key := range w8m5AllocationSeries() {
		allocation[key] = true
	}
	for _, key := range w8m5ReplaySeries() {
		if allocation[key] {
			t.Fatalf("series %q is in both the allocation and the replay family", key)
		}
	}
}

func TestW8m5MovedSeriesReportsOnlyTheNamedFamily(t *testing.T) {
	delta := map[string]int64{
		w8m5Series(viewmetrics.GenerationPublishedTotal, "owner="+viewmetrics.OwnerCheckout): 1,
		w8m5Series(viewmetrics.RequestServedTotal, "kind="+viewmetrics.ViewBase):             12,
	}
	moved := w8m5MovedSeries(delta, w8m5AllocationSeries())
	if len(moved) != 1 || !strings.Contains(moved[0], viewmetrics.GenerationPublishedTotal) {
		t.Fatalf("allocation family reported %v", moved)
	}
	if moved := w8m5MovedSeries(map[string]int64{}, w8m5AllocationSeries()); len(moved) != 0 {
		t.Fatalf("an empty delta moved %v", moved)
	}
	// Serving requests is not allocating: a read-only series must never be
	// able to fail the no-op assertion.
	if moved := w8m5MovedSeries(delta, w8m5ReplaySeries()); len(moved) != 0 {
		t.Fatalf("replay family reported %v for a delta that only served requests", moved)
	}
}

func TestW8m5TableProblemsRefusesAnUnreportableRow(t *testing.T) {
	good := []w8m5Row{
		{Case: "clean_idle_polling", Gate: "gate2", Status: w8m5StatusPass},
		{Case: "touch", Gate: "gate2", Status: w8m5StatusSkip, Detail: "ledger row W8.3 has not landed"},
		{Case: "stage_unstage", Gate: "gate2", Status: w8m5StatusFail, Detail: "seq moved"},
	}
	if problems := w8m5TableProblems(good); len(problems) != 0 {
		t.Fatalf("a complete table was rejected: %v", problems)
	}
	for _, tc := range []struct {
		name   string
		row    w8m5Row
		expect string
	}{
		{"silent skip", w8m5Row{Case: "touch", Gate: "gate2", Status: w8m5StatusSkip}, "skipped with no reason"},
		{"no gate", w8m5Row{Case: "touch", Status: w8m5StatusPass}, "names no acceptance gate"},
		{"no case", w8m5Row{Gate: "gate2", Status: w8m5StatusPass}, "has no case name"},
		{"unknown status", w8m5Row{Case: "touch", Gate: "gate2", Status: "ok"}, "unknown status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := w8m5TableProblems([]w8m5Row{tc.row})
			if len(problems) == 0 {
				t.Fatalf("%s was accepted as reportable", tc.name)
			}
			if !strings.Contains(strings.Join(problems, " "), tc.expect) {
				t.Fatalf("problem did not name the defect: want %q in %v", tc.expect, problems)
			}
		})
	}
}

func TestW8m5AbsenceVerdictSeparatesMovedFromBroken(t *testing.T) {
	// The rename shape: the name still answers, but from another file. That
	// is the evidence of absence this matrix needs, and it is exactly what the
	// shared fixture's own verdict reports as an error instead.
	if !w8m5AbsenceVerdict(issue767Answer{Found: true, FromExpectedFile: false}, nil) {
		t.Fatal("a declaration that moved to another file was not read as absent from the old one")
	}
	// Gone entirely.
	if !w8m5AbsenceVerdict(issue767Answer{Found: false, FromExpectedFile: false}, nil) {
		t.Fatal("a withdrawn declaration was not read as absent")
	}
	// Still there: not absent.
	if w8m5AbsenceVerdict(issue767Answer{Found: true, FromExpectedFile: true}, nil) {
		t.Fatal("a declaration still served from the file was read as absent")
	}
	// A broken or degraded answer is not evidence of anything. Without these
	// two arms a daemon that stopped answering would look like a successful
	// withdrawal, and every deletion-absence case would pass for free.
	if w8m5AbsenceVerdict(issue767Answer{}, errors.New("search command: exit 1")) {
		t.Fatal("a failed search was read as absence")
	}
	if w8m5AbsenceVerdict(issue767Answer{Fallback: true}, nil) {
		t.Fatal("a fallback answer was read as absence")
	}
}

func TestW8m5SoftAwaitReportsInsteadOfFailing(t *testing.T) {
	// It reports success without waiting a full interval when the condition
	// already holds.
	started := time.Now()
	if !w8m5SoftAwait(t.Context(), time.Second, time.Hour, func() bool { return true }) {
		t.Fatal("a condition that already holds was reported unmet")
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("the first check waited an interval first: %s", elapsed)
	}

	// It reports an unmet condition as false — and, crucially, does not fail
	// the test: a matrix case decides what its own timeout means.
	calls := 0
	if w8m5SoftAwait(t.Context(), 40*time.Millisecond, 10*time.Millisecond, func() bool { calls++; return false }) {
		t.Fatal("a condition that never held was reported met")
	}
	if calls < 2 {
		t.Fatalf("the condition was polled %d time(s); it must be retried until the timeout", calls)
	}

	// It becomes true as soon as the condition does.
	flips := 0
	if !w8m5SoftAwait(t.Context(), 2*time.Second, 10*time.Millisecond, func() bool { flips++; return flips >= 3 }) {
		t.Fatal("a condition that became true was reported unmet")
	}

	// A cancelled context stops the wait instead of burning the timeout.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	started = time.Now()
	if w8m5SoftAwait(ctx, time.Minute, time.Second, func() bool { return false }) {
		t.Fatal("a cancelled wait reported success")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("a cancelled wait kept polling for %s", elapsed)
	}
}

func TestW8m5RunGuardedAlwaysFilesARow(t *testing.T) {
	// The completed path: the body files its own row and the guard adds
	// nothing on top of it.
	complete := w8m5NewTable(t, "guard-complete")
	var caseT *testing.T
	restored := false
	w8m5RunGuarded(t, complete, "complete", "gate2",
		func(st *testing.T) func() { caseT = st; return func() { restored = true } },
		func() {
			complete.add(w8m5Row{Case: "complete", Gate: "gate2", Status: w8m5StatusPass, Detail: "scored"})
		})
	if len(complete.rows) != 1 || complete.rows[0].Status != w8m5StatusPass {
		t.Fatalf("a completed case filed %+v", complete.rows)
	}
	if caseT == nil || caseT == t {
		t.Fatal("the case did not run on its own subtest T; an abort would end the whole matrix")
	}
	if !restored {
		t.Fatal("the harness T was not restored after the case")
	}

	// The abort path: the body leaves its goroutine without filing a row —
	// what every fatal wait in this matrix does. The case must still appear in
	// the table, as a failure that names the abort, never vanish from it.
	aborted := w8m5NewTable(t, "guard-aborted")
	var sub *testing.T
	w8m5RunGuarded(t, aborted, "aborted", "gate1",
		func(st *testing.T) func() { sub = st; return func() {} },
		func() {
			// Skip is the only way to leave a subtest's goroutine without
			// also failing it, which is what makes it usable as a stand-in
			// for the fatal wait this guard exists for.
			sub.Skip("the guard's abort path: this case leaves its goroutine without filing a row")
		})
	if len(aborted.rows) != 1 {
		t.Fatalf("an aborted case left %d rows in the table; the matrix would report a case it never scored", len(aborted.rows))
	}
	row := aborted.rows[0]
	if row.Case != "aborted" || row.Gate != "gate1" || row.Status != w8m5StatusFail {
		t.Fatalf("the aborted row is %+v", row)
	}
	if !strings.Contains(row.Detail, "aborted") {
		t.Fatalf("the aborted row does not say what happened: %q", row.Detail)
	}
	if problems := w8m5TableProblems(aborted.rows); len(problems) != 0 {
		t.Fatalf("the aborted row is not reportable: %v", problems)
	}
}

func TestW8m5DiffAllowedMatchesOnlyTheNamedColumn(t *testing.T) {
	diff := "checkouts c1 changed: {ID:c1 State:checkout_ready HeadCommit:aaa} -> {ID:c1 State:checkout_ready HeadCommit:bbb}"
	if !w8m5DiffAllowed(diff, []string{"HeadCommit:"}) {
		t.Fatal("a HEAD movement the case declares was not allowed")
	}
	if w8m5DiffAllowed(diff, nil) {
		t.Fatal("a case that declares nothing allowed a catalog change")
	}
	if w8m5DiffAllowed("view_generations 8 inserted: {ID:8}", []string{"HeadCommit:"}) {
		t.Fatal("an inserted generation was excused by an unrelated allowance")
	}
}

// TestW8m5PayloadDiffNamesTheRowThatMoved pins the witness the whole no-op
// family now rests on: a renamed declaration, a moved line span and a withdrawn
// symbol are each named, and an identical observation produces nothing.
func TestW8m5PayloadDiffNamesTheRowThatMoved(t *testing.T) {
	base := w8m5Payload{
		File: []string{
			"issue767/p000/file00000.go::Fn00000S0|function|Fn00000S0|p000/file00000.go|12-12|public",
			"issue767/p000/file00000.go::W8Probe00000Rev0|function|W8Probe00000Rev0|p000/file00000.go|30-30|public",
		},
		RepoRows: 2, RepoDigest: "aaaa",
	}
	if diffs := w8m5PayloadDiff(base, base); len(diffs) != 0 {
		t.Fatalf("an identical observation produced %v", diffs)
	}

	renamed := w8m5Payload{
		File: []string{
			"issue767/p000/file00000.go::Fn00000S0|function|Fn00000S0|p000/file00000.go|12-12|public",
			"issue767/p000/file00000.go::W8Probe00000Rev1|function|W8Probe00000Rev1|p000/file00000.go|30-30|public",
		},
		RepoRows: 2, RepoDigest: "bbbb",
	}
	joined := strings.Join(w8m5PayloadDiff(base, renamed), " ;; ")
	for _, want := range []string{"payload row added", "W8Probe00000Rev1", "payload row removed", "W8Probe00000Rev0", "payload digest for the repository moved aaaa -> bbbb"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the payload diff did not name %q: %s", want, joined)
		}
	}

	moved := w8m5Payload{
		File: []string{
			"issue767/p000/file00000.go::Fn00000S0|function|Fn00000S0|p000/file00000.go|14-14|public",
			"issue767/p000/file00000.go::W8Probe00000Rev0|function|W8Probe00000Rev0|p000/file00000.go|30-30|public",
		},
		RepoRows: 2, RepoDigest: "aaaa",
	}
	if diffs := w8m5PayloadDiff(base, moved); len(diffs) != 2 {
		t.Fatalf("a moved line span produced %d diffs, want an add and a remove: %v", len(diffs), diffs)
	}

	withdrawn := w8m5Payload{File: base.File[:1], RepoRows: 1, RepoDigest: "cccc"}
	if joined := strings.Join(w8m5PayloadDiff(base, withdrawn), " ;; "); !strings.Contains(joined, "payload row removed") {
		t.Fatalf("a withdrawn declaration was not reported: %s", joined)
	}
}

func TestW8m5StringsMinusIsAMultisetDifference(t *testing.T) {
	left := []string{"a", "a", "b"}
	right := []string{"a", "c"}
	got := w8m5StringsMinus(left, right)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("multiset difference = %v, want [a b]", got)
	}
	if got := w8m5StringsMinus(right, left); len(got) != 1 || got[0] != "c" {
		t.Fatalf("reverse difference = %v, want [c]", got)
	}
}

// w8m5LiveEverything is the calibration result a healthy fixture produces: a
// real change moved every witness this matrix knows about.
func w8m5LiveEverything() w8m5Liveness {
	return w8m5Liveness{Calibrated: true, DirtyPayload: true, Seq: true, Catalog: true, Counters: true}
}

// TestW8m5NoopVerdictRefusesEveryAllocationClause is the gate-2 decision itself.
// Each sub-case moves exactly one witness over a case declared a no-op, and the
// verdict must be FAIL naming that witness. Disabling any one clause in
// w8m5NoopVerdict turns the matching subtest red.
func TestW8m5NoopVerdictRefusesEveryAllocationClause(t *testing.T) {
	noop := w8m5NoopExpect{Change: w8m5NoChange, Detail: "nothing should move"}
	for _, tc := range []struct {
		name string
		obs  w8m5NoopObservation
		want string
	}{
		{
			name: "sequence",
			obs:  w8m5NoopObservation{SeqDelta: 1},
			want: "view_generations seq moved by +1",
		},
		{
			name: "catalog",
			obs:  w8m5NoopObservation{CatalogDiffs: []string{"view_generations 9 inserted: {ID:9}"}},
			want: "catalog changed",
		},
		{
			name: "payload",
			obs:  w8m5NoopObservation{PayloadDiffs: []string{"payload row added: issue767/p000/file00000.go::X|function|X|p000/file00000.go|1-1|public"}},
			want: "the base payload moved over an effective no-op",
		},
		{
			name: "counters",
			obs: w8m5NoopObservation{CounterDelta: map[string]int64{
				w8m5Series(viewmetrics.CoordinatorCycleTotal, "outcome="+viewmetrics.OutcomeBuiltDirty): 1,
			}},
			want: "allocation counters moved",
		},
		{
			name: "catalog read error",
			obs:  w8m5NoopObservation{CatalogErrors: []string{"no such table: view_generations"}},
			want: "catalog read error",
		},
		{
			name: "payload read error",
			obs:  w8m5NoopObservation{PayloadErrors: []string{"no such table: nodes"}},
			want: "payload read error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict := w8m5NoopVerdict(noop, w8m5LiveEverything(), tc.obs)
			if verdict.Status != w8m5StatusFail {
				t.Fatalf("status = %s, want FAIL; verdict %+v", verdict.Status, verdict)
			}
			if !strings.Contains(strings.Join(verdict.Failures, " ;; "), tc.want) {
				t.Fatalf("failures %v did not name %q", verdict.Failures, tc.want)
			}
		})
	}

	if verdict := w8m5NoopVerdict(noop, w8m5LiveEverything(), w8m5NoopObservation{}); verdict.Status != w8m5StatusPass {
		t.Fatalf("a genuine no-op with a live instrument = %s, want PASS: %+v", verdict.Status, verdict)
	}
}

// TestW8m5NoopVerdictFailsAControlThatMovedNothing is the blocker's own
// mutation, as a unit test: re-declaring a real content change a no-op, or
// leaving a control that moved no payload row, must fail the case.
func TestW8m5NoopVerdictFailsAControlThatMovedNothing(t *testing.T) {
	// The observation the shipped branch actually produces for a real,
	// same-size working-tree content change: the view moves, the catalog does
	// not, no counter moves — and the payload witness does.
	realChange := w8m5NoopObservation{
		PayloadDiffs: []string{
			"payload row added: issue767/p000/file00000.go::W8Probe00000Rev1|function|W8Probe00000Rev1|p000/file00000.go|30-30|public",
			"payload row removed: issue767/p000/file00000.go::W8Probe00000Rev0|function|W8Probe00000Rev0|p000/file00000.go|30-30|public",
		},
	}
	control := w8m5NoopExpect{Change: w8m5RealChange, Detail: "a real change must be observed"}
	if verdict := w8m5NoopVerdict(control, w8m5LiveEverything(), realChange); verdict.Status != w8m5StatusPass {
		t.Fatalf("the control over a real change = %s, want PASS: %+v", verdict.Status, verdict)
	}

	// Mutation E1 of the adversarial review: the same real change re-declared
	// a no-op. It must now FAIL instead of passing.
	asNoop := w8m5NoopExpect{Change: w8m5NoChange, Detail: "declared a no-op"}
	verdict := w8m5NoopVerdict(asNoop, w8m5LiveEverything(), realChange)
	if verdict.Status != w8m5StatusFail {
		t.Fatalf("a real content change declared a no-op = %s, want FAIL: %+v", verdict.Status, verdict)
	}

	// And the converse: a control that moved NOTHING is a blind instrument,
	// not a passing case.
	blind := w8m5NoopVerdict(control, w8m5LiveEverything(), w8m5NoopObservation{})
	if blind.Status != w8m5StatusFail {
		t.Fatalf("a control that moved nothing = %s, want FAIL: %+v", blind.Status, blind)
	}
	if !strings.Contains(strings.Join(blind.Failures, " "), "blind") {
		t.Fatalf("the blind-instrument failure did not say so: %v", blind.Failures)
	}
}

// TestW8m5NoopVerdictWillNotAssertADeadWitness pins the calibration gate: a
// witness a real change did not move may not report a no-op, and a row left
// with no live clause is a SKIP that says so.
func TestW8m5NoopVerdictWillNotAssertADeadWitness(t *testing.T) {
	noop := w8m5NoopExpect{Change: w8m5NoChange, Detail: "nothing should move"}

	uncalibrated := w8m5NoopVerdict(noop, w8m5Liveness{}, w8m5NoopObservation{SeqDelta: 1})
	if uncalibrated.Status != w8m5StatusSkip {
		t.Fatalf("an uncalibrated instrument = %s, want SKIP: %+v", uncalibrated.Status, uncalibrated)
	}
	if !strings.Contains(strings.Join(uncalibrated.Recorded, " "), "never calibrated") {
		t.Fatalf("the SKIP did not name the missing calibration: %+v", uncalibrated)
	}
	if len(uncalibrated.Recorded) != 1 {
		t.Fatalf("an uncalibrated instrument evaluated clauses it has no calibration for: %+v", uncalibrated)
	}

	dead := w8m5Liveness{Calibrated: true}
	verdict := w8m5NoopVerdict(noop, dead, w8m5NoopObservation{})
	if verdict.Status != w8m5StatusSkip {
		t.Fatalf("every witness dead = %s, want SKIP: %+v", verdict.Status, verdict)
	}
	joined := strings.Join(verdict.Recorded, " ;; ")
	for _, want := range []string{"payload clause NOT asserted", "sequence clause NOT asserted", "catalog clause NOT asserted", "counter clause NOT asserted", "not measurable on this instrument"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the SKIP did not record %q: %s", want, joined)
		}
	}

	// One live witness is enough to carry a row, and the dead ones stay
	// recorded rather than counted.
	partial := w8m5Liveness{Calibrated: true, DirtyPayload: true}
	if got := w8m5NoopVerdict(noop, partial, w8m5NoopObservation{}); got.Status != w8m5StatusPass {
		t.Fatalf("one live witness = %s, want PASS: %+v", got.Status, got)
	}
	if got := w8m5NoopVerdict(noop, partial, w8m5NoopObservation{SeqDelta: 3}); got.Status != w8m5StatusPass {
		t.Fatalf("a dead sequence witness must not fail a row: %+v", got)
	}
}

// TestW8m5NoopVerdictKeepsAFailureWhenTheCountersAreUnavailable is the
// swallow the review found: a case that allocated a generation AND could not
// read `daemon status` used to be filed as SKIP.
func TestW8m5NoopVerdictKeepsAFailureWhenTheCountersAreUnavailable(t *testing.T) {
	noop := w8m5NoopExpect{Change: w8m5NoChange, Detail: "nothing should move"}
	obs := w8m5NoopObservation{SeqDelta: 1, CountersError: "dial unix: connect: connection refused"}
	verdict := w8m5NoopVerdict(noop, w8m5LiveEverything(), obs)
	if verdict.Status != w8m5StatusFail {
		t.Fatalf("an allocation with unreadable counters = %s, want FAIL: %+v", verdict.Status, verdict)
	}
	if !strings.Contains(strings.Join(verdict.Failures, " "), "seq moved by +1") {
		t.Fatalf("the gate-2 failure was discarded: %+v", verdict)
	}
	if !strings.Contains(strings.Join(verdict.Recorded, " "), "counters unavailable") {
		t.Fatalf("the transport failure was not recorded: %+v", verdict)
	}

	// With nothing else wrong, an unreadable counter set is still a named
	// skip rather than a pass.
	clean := w8m5NoopVerdict(noop, w8m5LiveEverything(), w8m5NoopObservation{CountersError: "connection refused"})
	if clean.Status != w8m5StatusSkip {
		t.Fatalf("unreadable counters alone = %s, want SKIP: %+v", clean.Status, clean)
	}

	// A replay case whose counters are unreadable records that the gate-4
	// clause could not run instead of inventing a reuse it never saw.
	replay := w8m5NoopExpect{Change: w8m5NoChange, Replay: true, Detail: "replay"}
	got := w8m5NoopVerdict(replay, w8m5LiveEverything(), w8m5NoopObservation{CountersError: "connection refused"})
	if got.Status != w8m5StatusSkip || !strings.Contains(strings.Join(got.Recorded, " "), "gate4") {
		t.Fatalf("an unreadable replay assertion = %+v", got)
	}

	// And a replay case whose counters ARE readable but show no reuse fails.
	silent := w8m5NoopVerdict(replay, w8m5LiveEverything(), w8m5NoopObservation{CounterDelta: map[string]int64{}})
	if silent.Status != w8m5StatusFail || !strings.Contains(strings.Join(silent.Failures, " "), "no claim/replay counter moved") {
		t.Fatalf("a replay case with no reuse counter = %+v", silent)
	}
}

// TestW8m5CalibrationHalfReportsWhatMoved pins the calibration's own rendering:
// a half that moved nothing says so, and the sentence names every witness.
func TestW8m5CalibrationHalfReportsWhatMoved(t *testing.T) {
	empty := w8m5CalibrationHalf{Label: "a working-tree content change"}
	if empty.moved() {
		t.Fatal("a half that moved nothing reported movement")
	}
	rendered := empty.String()
	for _, want := range []string{"a working-tree content change", "seq +0", "payload 0 row(s)", "catalog 0 change(s)", "allocation series []"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("the calibration sentence did not name %q: %s", want, rendered)
		}
	}
	for _, half := range []w8m5CalibrationHalf{
		{Seq: 1},
		{Catalog: []string{"view_generations 9 inserted"}},
		{Payload: []string{"payload row added: x"}},
		{Counters: []string{"views_coordinator_cycle_total{outcome=built_dirty}=+1"}},
	} {
		if !half.moved() {
			t.Fatalf("half %+v reported no movement", half)
		}
	}
	unreadable := w8m5CalibrationHalf{Label: "l", Error: "connection refused"}
	if !strings.Contains(unreadable.String(), "counters unavailable: connection refused") {
		t.Fatalf("an unreadable counter read was not named: %s", unreadable.String())
	}
}

// TestW8m5LivenessComesFromTheRightHalf pins the asymmetry the blocker turns
// on: the payload witness is only live for the working-tree path if the
// WORKING-TREE half of the calibration moved it. A commit that moved it proves
// nothing about a dirty edit, and accepting it would reinstate the blindness.
func TestW8m5LivenessComesFromTheRightHalf(t *testing.T) {
	dirtySilent := w8m5CalibrationHalf{Label: "dirty"}
	commitMoved := w8m5CalibrationHalf{Label: "commit", Seq: 1,
		Catalog:  []string{"view_generations 9 inserted"},
		Payload:  []string{"payload row added: x"},
		Counters: []string{w8m5Series(viewmetrics.DedicatedBaseClaimTotal, "outcome="+viewmetrics.DedicatedBaseBuilt) + "=+1"},
	}
	live := w8m5LivenessFrom(dirtySilent, commitMoved)
	if !live.Calibrated {
		t.Fatal("the calibration did not mark itself done")
	}
	if live.DirtyPayload {
		t.Fatal("a commit that moved the payload was accepted as proof that a dirty edit does")
	}
	for name, got := range map[string]bool{"Seq": live.Seq, "Catalog": live.Catalog, "Counters": live.Counters} {
		if !got {
			t.Fatalf("%s was not marked live although the commit half moved it: %+v", name, live)
		}
	}
	if !strings.Contains(live.Detail, "dirty:") || !strings.Contains(live.Detail, "commit:") {
		t.Fatalf("the liveness detail does not carry both halves: %s", live.Detail)
	}

	dirtyMoved := w8m5CalibrationHalf{Label: "dirty", Payload: []string{"payload row added: y"}}
	if got := w8m5LivenessFrom(dirtyMoved, w8m5CalibrationHalf{Label: "commit"}); !got.DirtyPayload {
		t.Fatalf("a working-tree change that moved the payload was not marked live: %+v", got)
	}
	if got := w8m5LivenessFrom(dirtyMoved, w8m5CalibrationHalf{Label: "commit"}); got.Seq || got.Catalog || got.Counters {
		t.Fatalf("a silent catalog was marked live: %+v", got)
	}
}

// TestW8m5ReadPayloadSelectsTheBaseRowsOfTheNamedFile pins the witness's own
// reader over a real SQLite database: the base generation only, the named
// repository only, the named file's rows separated from the rest, and a missing
// table reported rather than read as an empty graph.
//
// A payload reader that silently returns nothing would report every change as a
// no-op — which is the exact failure this witness was added to prevent — so the
// selection is pinned rather than trusted.
func TestW8m5ReadPayloadSelectsTheBaseRowsOfTheNamedFile(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "payload.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := t.Context()

	// Before the table exists the reader must say so, not report an empty graph.
	missing := w8m5ReadPayload(ctx, db, issue767FixturePrefix, "p000/file00000.go")
	if len(missing.Errors) == 0 {
		t.Fatal("a missing nodes table was read as an empty payload")
	}

	if _, err := db.ExecContext(ctx, `CREATE TABLE nodes (
		id TEXT NOT NULL, view_gen INTEGER NOT NULL DEFAULT 0, kind TEXT NOT NULL, name TEXT NOT NULL,
		file_path TEXT NOT NULL, start_line INTEGER NOT NULL DEFAULT 0, end_line INTEGER NOT NULL DEFAULT 0,
		visibility TEXT, updated_at INTEGER, repo_prefix TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	insert := func(id, name, file string, gen int64, prefix string, line, updated int64) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO nodes
			(id, view_gen, kind, name, file_path, start_line, end_line, visibility, updated_at, repo_prefix)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, id, gen, "function", name, file, line, line, "public", updated, prefix); err != nil {
			t.Fatal(err)
		}
	}
	insert("issue767/p000/file00000.go::Fn00000S0", "Fn00000S0", "p000/file00000.go", 0, issue767FixturePrefix, 12, 100)
	insert("issue767/p000/file00000.go::W8Probe00000Rev0", "W8Probe00000Rev0", "p000/file00000.go", 0, issue767FixturePrefix, 30, 100)
	insert("issue767/p001/file00001.go::Fn00001S0", "Fn00001S0", "p001/file00001.go", 0, issue767FixturePrefix, 12, 100)
	// Not the base generation, not this repository: neither may be counted.
	insert("issue767/p000/file00000.go::Shadow", "Shadow", "p000/file00000.go", 7, issue767FixturePrefix, 40, 100)
	insert("other/p000/file00000.go::Foreign", "Foreign", "p000/file00000.go", 0, "other", 50, 100)

	got := w8m5ReadPayload(ctx, db, issue767FixturePrefix, "p000/file00000.go")
	if len(got.Errors) != 0 {
		t.Fatalf("payload read reported %v", got.Errors)
	}
	if got.RepoRows != 3 {
		t.Fatalf("repo rows = %d, want 3 (base generation of this repo only)", got.RepoRows)
	}
	if len(got.File) != 2 {
		t.Fatalf("file rows = %v, want the two declarations of p000/file00000.go", got.File)
	}
	for _, row := range got.File {
		if !strings.Contains(row, "p000/file00000.go") {
			t.Fatalf("a row from another file was selected: %s", row)
		}
		if strings.Contains(row, "Shadow") || strings.Contains(row, "Foreign") {
			t.Fatalf("a non-base or foreign row was selected: %s", row)
		}
	}
	if got.FileStamp != 200 {
		t.Fatalf("updated_at sum = %d, want 200", got.FileStamp)
	}

	// A content change to the named file moves both the file rows and the
	// repository digest; a change to another file moves only the digest.
	if _, err := db.ExecContext(ctx, `UPDATE nodes SET name='W8Probe00000Rev1',
		id='issue767/p000/file00000.go::W8Probe00000Rev1' WHERE name='W8Probe00000Rev0'`); err != nil {
		t.Fatal(err)
	}
	moved := w8m5ReadPayload(ctx, db, issue767FixturePrefix, "p000/file00000.go")
	diffs := w8m5PayloadDiff(got, moved)
	if len(diffs) != 3 {
		t.Fatalf("a renamed declaration produced %v, want an add, a remove and a digest move", diffs)
	}

	elsewhere := w8m5ReadPayload(ctx, db, issue767FixturePrefix, "p001/file00001.go")
	if len(elsewhere.File) != 1 || !strings.Contains(elsewhere.File[0], "Fn00001S0") {
		t.Fatalf("naming another file selected %v", elsewhere.File)
	}
	if elsewhere.RepoDigest != moved.RepoDigest {
		t.Fatal("the repository digest depends on which file was named")
	}
}

// TestW8m5ReadCatalogReadsAStoreThatNeverAllocatedAGeneration is the
// instrument's regression for the consumer gate.
//
// Since internal/indexer/dedicated_base_startup.go declines a committed-base
// publication for a family with no dependent checkout, an isolated fixture can
// legitimately reach the end of a matrix with view_generations empty — and then
// sqlite_sequence carries no row for it, because AUTOINCREMENT only writes that
// row on the first insert. Reporting sql.ErrNoRows as a catalog read error made
// every case of matrix 1 fail on that one instrument line rather than on the
// daemon (nine cases, one shared message, with the calibration still passing).
//
// The absent row is absorbed as 0 and as the positive reading "nothing was ever
// allocated here", and NOTHING else is: a missing table is still an error, and
// so is the incoherent census where view_generations holds rows while the
// sequence row is gone.
func TestW8m5ReadCatalogReadsAStoreThatNeverAllocatedAGeneration(t *testing.T) {
	ctx := t.Context()
	open := func(name string) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), name)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		db.SetMaxOpenConns(1)
		return db
	}
	schema := func(db *sql.DB) {
		t.Helper()
		for _, statement := range []string{
			`CREATE TABLE view_generations (generation_id INTEGER PRIMARY KEY AUTOINCREMENT, owner_kind TEXT NOT NULL DEFAULT '',
				graph_id TEXT NOT NULL DEFAULT '', checkout_id TEXT, generation_kind TEXT NOT NULL DEFAULT '',
				base_generation_id INTEGER, tree_oid TEXT NOT NULL DEFAULT '', provenance_commit_oid TEXT,
				config_hash TEXT NOT NULL DEFAULT '', resolver_version TEXT NOT NULL DEFAULT '',
				dependency_revision TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT '',
				covered_files INTEGER NOT NULL DEFAULT 0, affected_files INTEGER NOT NULL DEFAULT 0,
				storage_bytes INTEGER NOT NULL DEFAULT 0, completeness TEXT NOT NULL DEFAULT '',
				error TEXT NOT NULL DEFAULT '', last_selected INTEGER NOT NULL DEFAULT 0)`,
			`CREATE TABLE checkouts (checkout_id TEXT PRIMARY KEY, state TEXT NOT NULL DEFAULT '',
				desired_mode TEXT NOT NULL DEFAULT '', effective_mode TEXT NOT NULL DEFAULT '',
				head_ref TEXT NOT NULL DEFAULT '', head_commit TEXT NOT NULL DEFAULT '',
				head_tree TEXT NOT NULL DEFAULT '', locked INTEGER NOT NULL DEFAULT 0,
				prunable INTEGER NOT NULL DEFAULT 0, removal_evidence TEXT NOT NULL DEFAULT '',
				last_error TEXT NOT NULL DEFAULT '', last_seen INTEGER NOT NULL DEFAULT 0,
				last_accessible INTEGER NOT NULL DEFAULT 0)`,
			`CREATE TABLE checkout_routes (checkout_id TEXT PRIMARY KEY, graph_id TEXT NOT NULL DEFAULT '',
				commit_generation_id INTEGER, dirty_generation_id INTEGER, route_epoch INTEGER NOT NULL DEFAULT 0,
				state TEXT NOT NULL DEFAULT '')`,
			`CREATE TABLE repository_families (family_id TEXT PRIMARY KEY, last_seen INTEGER NOT NULL DEFAULT 0)`,
			// An unrelated AUTOINCREMENT table, so sqlite_sequence itself
			// exists and the absent row is the row for view_generations
			// specifically, not the whole table.
			`CREATE TABLE unrelated (id INTEGER PRIMARY KEY AUTOINCREMENT)`,
		} {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO unrelated DEFAULT VALUES"); err != nil {
			t.Fatal(err)
		}
	}

	// (1) A family that never allocated: the read succeeds, reports 0, and
	// says the row is absent rather than inventing one.
	db := open("never.sqlite")
	schema(db)
	never := w8m5ReadCatalog(ctx, db)
	if len(never.Errors) != 0 {
		t.Fatalf("a store that never allocated a generation was read as an error: %v", never.Errors)
	}
	if never.Sequence != 0 || never.SequenceRow {
		t.Fatalf("sequence = %d row=%v, want 0 and absent", never.Sequence, never.SequenceRow)
	}
	if len(never.Generations) != 0 {
		t.Fatalf("the census invented generations: %+v", never.Generations)
	}
	// The whole matrix hangs off this: a no-op case over such a store must be
	// decidable, and it must state the no-allocation fact positively.
	verdict := w8m5NoopVerdict(w8m5NoopExpect{}, w8m5LiveEverything(), w8m5NoopObservation{
		GenerationsAfter: len(never.Generations), SequenceRowAfter: never.SequenceRow,
	})
	if verdict.Status != w8m5StatusPass {
		t.Fatalf("a no-op over a store that never allocated was scored %s: %+v", verdict.Status, verdict)
	}
	if !strings.Contains(strings.Join(verdict.Recorded, " ;; "), "no generation was allocated") {
		t.Fatalf("the verdict did not state the no-allocation fact positively: %v", verdict.Recorded)
	}

	// (2) An allocation is still reported as itself, and the sequence row's
	// arrival is named as a catalog change in its own right.
	if _, err := db.ExecContext(ctx, "INSERT INTO view_generations(state, tree_oid) VALUES ('ready','tree-a')"); err != nil {
		t.Fatal(err)
	}
	allocated := w8m5ReadCatalog(ctx, db)
	if len(allocated.Errors) != 0 {
		t.Fatalf("an allocated store reported %v", allocated.Errors)
	}
	if allocated.Sequence != 1 || !allocated.SequenceRow || len(allocated.Generations) != 1 {
		t.Fatalf("allocation was not observed: seq=%d row=%v rows=%d", allocated.Sequence, allocated.SequenceRow, len(allocated.Generations))
	}
	diffs := strings.Join(w8m5CatalogDiff(never, allocated), " ;; ")
	if !strings.Contains(diffs, "view_generations seq 0 -> 1") {
		t.Fatalf("the sequence movement was not named: %s", diffs)
	}
	if !strings.Contains(diffs, "sqlite_sequence row present false -> true") {
		t.Fatalf("the first allocation a store ever performs was not named: %s", diffs)
	}
	if fail := w8m5NoopVerdict(w8m5NoopExpect{}, w8m5LiveEverything(), w8m5NoopObservation{
		SeqDelta: allocated.Sequence - never.Sequence, CatalogDiffs: w8m5CatalogDiff(never, allocated),
		GenerationsAfter: len(allocated.Generations), SequenceRowAfter: allocated.SequenceRow,
	}); fail.Status != w8m5StatusFail {
		t.Fatalf("a no-op that allocated a generation was scored %s: %+v", fail.Status, fail)
	}

	// (3) An incoherent census — rows with no sequence row — is a read error,
	// not a store that "allocated nothing". This is the clause that keeps the
	// absorption in (1) from being a blanket swallow.
	if _, err := db.ExecContext(ctx, "DELETE FROM sqlite_sequence WHERE name='view_generations'"); err != nil {
		t.Fatal(err)
	}
	incoherent := w8m5ReadCatalog(ctx, db)
	if len(incoherent.Errors) == 0 {
		t.Fatalf("a census with rows and no sequence row was read as coherent: %+v", incoherent)
	}
	if !strings.Contains(strings.Join(incoherent.Errors, " ;; "), "disagree") {
		t.Fatalf("the incoherent census was not named: %v", incoherent.Errors)
	}
	if bad := w8m5NoopVerdict(w8m5NoopExpect{}, w8m5LiveEverything(), w8m5NoopObservation{
		GenerationsAfter: len(incoherent.Generations), SequenceRowAfter: incoherent.SequenceRow,
	}); bad.Status != w8m5StatusFail {
		t.Fatalf("an incoherent census was scored %s: %+v", bad.Status, bad)
	}

	// (4) A missing table is still an error: "allocated nothing" and "cannot
	// be read" must never collapse into one reading.
	if _, err := db.ExecContext(ctx, "DROP TABLE view_generations"); err != nil {
		t.Fatal(err)
	}
	if missing := w8m5ReadCatalog(ctx, db); len(missing.Errors) == 0 {
		t.Fatalf("a missing view_generations table was read as an empty census: %+v", missing)
	}
}

// TestW8m5CalibrationProblemsRequireTheCommittedHalfToAllocate pins the two
// instrument rules the calibration row is scored on, and in particular the one
// that keeps matrix 1's dependent checkout load-bearing: a committed half that
// allocated neither a sequence number nor an allocation counter is a FAILURE,
// not a quiet degradation to "NOT ASSERTED" on every no-op row.
func TestW8m5CalibrationProblemsRequireTheCommittedHalfToAllocate(t *testing.T) {
	dirtyMoved := w8m5CalibrationHalf{Label: "dirty", Payload: []string{"payload row changed: x"}}
	committedAllocated := w8m5CalibrationHalf{Label: "commit", Seq: 1,
		Catalog: []string{"seq 0 -> 1"}, Counters: []string{"views_dedicated_base_claim_total{outcome=built}"}}

	// (1) The calibrated shape: both rules hold, nothing to report.
	if problems := w8m5CalibrationProblems(dirtyMoved, committedAllocated); len(problems) != 0 {
		t.Fatalf("a calibrated instrument reported problems: %v", problems)
	}

	// (2) The dependent-checkout rule. A committed half that moved the catalog
	// but allocated nothing — exactly what a single-checkout family produces
	// since the consumer gate — must FAIL, and must name the consumer gate.
	committedSilent := w8m5CalibrationHalf{Label: "commit", Catalog: []string{"HeadCommit a -> b"}}
	problems := w8m5CalibrationProblems(dirtyMoved, committedSilent)
	if len(problems) != 1 {
		t.Fatalf("a committed half that allocated nothing produced %d problem(s): %v", len(problems), problems)
	}
	for _, want := range []string{"the committed half allocated nothing", "dependent checkout", "dedicated_base_startup.go:776"} {
		if !strings.Contains(problems[0], want) {
			t.Fatalf("the problem did not name %q: %s", want, problems[0])
		}
	}

	// (3) Either allocation witness alone satisfies the rule: the sequence...
	if problems := w8m5CalibrationProblems(dirtyMoved, w8m5CalibrationHalf{Label: "commit", Seq: 1}); len(problems) != 0 {
		t.Fatalf("a committed half that moved the sequence reported problems: %v", problems)
	}
	// ...or an allocation counter.
	if problems := w8m5CalibrationProblems(dirtyMoved, w8m5CalibrationHalf{Label: "commit",
		Counters: []string{"views_generation_published_total{owner=checkout}"}}); len(problems) != 0 {
		t.Fatalf("a committed half that moved an allocation counter reported problems: %v", problems)
	}

	// (4) A dead instrument reports BOTH rules, so the row says the whole
	// truth rather than the first thing that went wrong.
	if problems := w8m5CalibrationProblems(w8m5CalibrationHalf{Label: "dirty"}, w8m5CalibrationHalf{Label: "commit"}); len(problems) != 2 {
		t.Fatalf("a dead instrument produced %d problem(s), want 2: %v", len(problems), problems)
	}
}

// TestW8m5TableFailuresReportEveryFailedRow pins the table-level backstop: a
// row marked FAILED is reported from the table itself, so a scoring path that
// marks a row and drops its own report cannot produce a green matrix.
func TestW8m5TableFailuresReportEveryFailedRow(t *testing.T) {
	rows := []w8m5Row{
		{Case: "ok", Gate: "gate2", Status: w8m5StatusPass, Detail: "nothing moved"},
		{Case: "skipped", Gate: "gate1", Status: w8m5StatusSkip, Detail: "declared gap"},
		{Case: "broken", Gate: "gate2", Status: w8m5StatusFail, Detail: "an unchanged-content commit allocated a generation"},
	}
	failed := w8m5TableFailures(rows)
	if len(failed) != 1 {
		t.Fatalf("reported %d failed row(s), want 1: %v", len(failed), failed)
	}
	for _, want := range []string{"broken", "gate2", "allocated a generation"} {
		if !strings.Contains(failed[0], want) {
			t.Fatalf("the reported failure did not name %q: %s", want, failed[0])
		}
	}
	if none := w8m5TableFailures(rows[:2]); len(none) != 0 {
		t.Fatalf("a table with no failed row reported %v", none)
	}
}
