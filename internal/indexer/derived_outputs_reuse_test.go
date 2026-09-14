package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The read-only-context mode, and what the generation it produces still owes
// its readers.
//
// W6.1 separated a sparse generation's OUTPUT from the closure it had to READ,
// but only after the fact: the pass wrote payload for every closure file and
// the build withdrew it before publishing. The durable generation shrank; the
// writes did not. This file pins the other half — the pass holds its corpus in
// memory, the separation runs against that corpus before anything is
// persisted, and the store therefore receives rows for the change set alone.
//
// Two properties are load-bearing and both are pinned here:
//
//  1. The mode changes WHERE the separation happens and nothing else. The
//     durable generation, its masks and the composed view are identical to the
//     ones the fallback produces, table row for table row.
//  2. It changes what the store is asked to write. The physical audit below
//     measures that directly, in database pages.
//
// The third section is gate 3's second clause: each derived output a naive
// reuse would drop is either shown to survive on the gate-1 oracle, shown to
// be re-derived, or named as a limitation with the evidence for the claim.

// forceContextWithdrawalFallback makes the in-memory route unavailable for the
// rest of the test, so the same build takes the write-then-withdraw path. It
// is the honest knob for the differential: the ceiling it lowers is the
// production one the pass consults, not a flag the test invented.
func forceContextWithdrawalFallback(t *testing.T) {
	t.Helper()
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
}

// generationTableCensus counts the rows every generation-scoped table holds
// for one generation.
//
// It discovers the tables from sqlite_master rather than listing them, so a
// table added later is audited without this test being edited — the failure
// mode of a hand-written census is that it silently stops covering the row
// class someone just introduced.
func generationTableCensus(
	t *testing.T, store *store_sqlite.Store, generationID int64,
) map[string]int {
	t.Helper()
	db, err := sql.Open("sqlite", store.Path())
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()

	tables := generationScopedTables(t, db)
	census := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM "`+table+`" WHERE view_gen = ?`, generationID,
		).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		census[table] = count
	}
	return census
}

// generationScopedTables lists every ordinary table carrying a view_gen
// column, sorted.
func generationScopedTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("table rows: %v", err)
	}
	_ = rows.Close()

	var scoped []string
	for _, name := range names {
		cols, err := db.Query(`SELECT name FROM pragma_table_info(?)`, name)
		if err != nil {
			// A virtual FTS table's shadow companions refuse table_info; they
			// carry no view_gen of their own and are reached through the
			// *_rowid sidecars this census already covers.
			continue
		}
		for cols.Next() {
			var col string
			if err := cols.Scan(&col); err != nil {
				_ = cols.Close()
				t.Fatalf("scan column name: %v", err)
			}
			if col == "view_gen" {
				scoped = append(scoped, name)
				break
			}
		}
		if err := cols.Err(); err != nil {
			_ = cols.Close()
			t.Fatalf("column rows for %s: %v", name, err)
		}
		_ = cols.Close()
	}
	sort.Strings(scoped)
	return scoped
}

// generationPathColumns maps each generation-scoped table that records a file
// path to the distinct paths it holds for one generation. It is the row-level
// half of the audit: a count census proves the two modes agree, this proves
// WHICH files the surviving rows speak for.
func generationFilePathRows(
	t *testing.T, store *store_sqlite.Store, generationID int64,
) map[string][]string {
	t.Helper()
	db, err := sql.Open("sqlite", store.Path())
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()

	out := map[string][]string{}
	for _, table := range generationScopedTables(t, db) {
		if !tableHasColumn(t, db, table, "file_path") {
			continue
		}
		rows, err := db.Query(
			`SELECT DISTINCT file_path FROM "`+table+`" WHERE view_gen = ?`, generationID)
		if err != nil {
			t.Fatalf("select file_path from %s: %v", table, err)
		}
		var paths []string
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				_ = rows.Close()
				t.Fatalf("scan %s.file_path: %v", table, err)
			}
			if path != "" {
				paths = append(paths, path)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("%s rows: %v", table, err)
		}
		_ = rows.Close()
		sort.Strings(paths)
		out[table] = paths
	}
	return out
}

func tableHasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("column rows for %s: %v", table, err)
	}
	return false
}

// databaseFootprint is the physical write audit: how many pages the database
// file has grown to, how many of them are free (a page the build allocated and
// then released — the signature of a row written and withdrawn), and the page
// size that converts both to bytes.
type databaseFootprint struct {
	pageCount int64
	freeList  int64
	pageSize  int64
	walBytes  int64
}

func (f databaseFootprint) bytes() int64     { return f.pageCount * f.pageSize }
func (f databaseFootprint) freeBytes() int64 { return f.freeList * f.pageSize }

func measureDatabaseFootprint(t *testing.T, store *store_sqlite.Store) databaseFootprint {
	t.Helper()
	db, err := sql.Open("sqlite", store.Path())
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	var out databaseFootprint
	for _, probe := range []struct {
		pragma string
		into   *int64
	}{
		{"page_count", &out.pageCount},
		{"freelist_count", &out.freeList},
		{"page_size", &out.pageSize},
	} {
		if err := db.QueryRow(`PRAGMA ` + probe.pragma).Scan(probe.into); err != nil {
			t.Fatalf("pragma %s: %v", probe.pragma, err)
		}
	}
	// The write-ahead log is where a build's bytes land before a checkpoint
	// moves them into the file, so its residual size is the closest cheap
	// proxy for "bytes this build asked SQLite to write".
	if info, err := os.Stat(store.Path() + "-wal"); err == nil {
		out.walBytes = info.Size()
	}
	return out
}

// TestReadOnlyContextIsHeldInMemoryAndNeverWritten is the first clause: the
// production entrypoint reaches the mode, and the generation it publishes
// carries the change set alone.
//
// It drives BuildCommitLayer, not the primitive: the point of the item is that
// the SPARSE BUILD stops writing what it reads, and a test that called the
// separation directly would prove only that the separation works.
func TestReadOnlyContextIsHeldInMemoryAndNeverWritten(t *testing.T) {
	store := builderOpenStore(t, "in-memory-context")
	_, generationID, report := builderContextGeneration(
		t, store, builderContextTreeA(), builderContextTreeBodyEdit())

	if !report.ContextHeldInMemory {
		t.Fatal("the pass persisted its context corpus: the read-only-context mode did not engage")
	}
	if len(report.ClosurePaths) == 0 {
		t.Fatal("the closure added no context file — the fixture proves nothing about context")
	}
	if report.ContextMasks != len(report.ClosurePaths) {
		t.Fatalf("context masks = %d for %d closure paths (%v)",
			report.ContextMasks, len(report.ClosurePaths), report.ClosurePaths)
	}

	changed := builderRepoPrefix + "/core.go"
	if got := builderContextPayloadPaths(t, store, generationID); !slices.Equal(got, []string{changed}) {
		t.Fatalf("the generation carries payload at %v, want only %q", got, changed)
	}

	// Every generation-scoped table that records a file path must record only
	// the change set's path. This is the census the sidecars escaped before:
	// files, symbol FTS ownership, constant values, mtimes and the rest all
	// answer here, and a new one is picked up without editing this test.
	for table, paths := range generationFilePathRows(t, store, generationID) {
		if table == "generation_file_masks" {
			// The one table whose whole job is to speak about paths the
			// generation carries nothing for.
			continue
		}
		for _, path := range paths {
			if path != changed {
				t.Errorf("%s carries a row for %q; the generation claims only %q", table, path, changed)
			}
		}
	}

	// The oracle: composing the generation over the corpus must equal a fresh
	// isolated index of the same tree.
	flat := builderOpenStore(t, "in-memory-context-flat")
	dirB := builderTempDir(t, "checkout-b")
	builderWriteTree(t, dirB, builderContextTreeBodyEdit())
	builderIndex(t, flat, dirB)
	composed := builderComposed(t, store, generationID)
	builderAssertReadersAgree(t, composed, flat)
	builderAssertMasksValidate(t, store, generationID)
}

// TestContextWithdrawalFallbackProducesTheSameGeneration is the safety half.
// The in-memory route is an optimisation the pass may not always be able to
// take — an oversized closure, a refused admission, a backend without the bulk
// path — and when it cannot, the build must still publish exactly the same
// generation by withdrawing the payload after writing it.
//
// The two halves are compared table by table, mask by mask, and against the
// same fresh-index oracle, so "the same generation" is a measured claim rather
// than an assertion about the code path.
func TestContextWithdrawalFallbackProducesTheSameGeneration(t *testing.T) {
	held := builderOpenStore(t, "held")
	_, heldGen, heldReport := builderContextGeneration(
		t, held, builderContextTreeA(), builderContextTreeBodyEdit())
	if !heldReport.ContextHeldInMemory {
		t.Fatal("the unconstrained build did not take the in-memory route")
	}

	forceContextWithdrawalFallback(t)
	withdrawn := builderOpenStore(t, "withdrawn")
	_, withdrawnGen, withdrawnReport := builderContextGeneration(
		t, withdrawn, builderContextTreeA(), builderContextTreeBodyEdit())
	if withdrawnReport.ContextHeldInMemory {
		t.Fatal("the constrained build still took the in-memory route; the differential is vacuous")
	}

	// What the two builds SAY.
	if !slices.Equal(heldReport.ContextPaths, withdrawnReport.ContextPaths) {
		t.Errorf("context paths differ: held %v, withdrawn %v",
			heldReport.ContextPaths, withdrawnReport.ContextPaths)
	}
	if !slices.Equal(heldReport.ContextRetainedPaths, withdrawnReport.ContextRetainedPaths) {
		t.Errorf("retained paths differ: held %v, withdrawn %v",
			heldReport.ContextRetainedPaths, withdrawnReport.ContextRetainedPaths)
	}
	if heldReport.ReplaceMasks != withdrawnReport.ReplaceMasks ||
		heldReport.DeleteMasks != withdrawnReport.DeleteMasks ||
		heldReport.NodeTombstones != withdrawnReport.NodeTombstones ||
		heldReport.EdgeSourceMarkers != withdrawnReport.EdgeSourceMarkers {
		t.Errorf("claims differ: held %+v, withdrawn %+v", heldReport, withdrawnReport)
	}

	// What the two builds CARRY, table by table.
	heldCensus := generationTableCensus(t, held, heldGen)
	withdrawnCensus := generationTableCensus(t, withdrawn, withdrawnGen)
	for table, want := range withdrawnCensus {
		if got := heldCensus[table]; got != want {
			t.Errorf("%s: in-memory generation holds %d rows, withdrawal holds %d", table, got, want)
		}
	}
	if !slices.Equal(
		builderContextPayloadPaths(t, held, heldGen),
		builderContextPayloadPaths(t, withdrawn, withdrawnGen),
	) {
		t.Errorf("payload census differs between the two modes")
	}
	if !mapsEqualModes(
		builderContextMaskModes(t, held, heldGen),
		builderContextMaskModes(t, withdrawn, withdrawnGen),
	) {
		t.Errorf("masks differ between the two modes")
	}

	// And the oracle, for both.
	flat := builderOpenStore(t, "fallback-flat")
	dirB := builderTempDir(t, "checkout-b")
	builderWriteTree(t, dirB, builderContextTreeBodyEdit())
	builderIndex(t, flat, dirB)
	builderAssertReadersAgree(t, builderComposed(t, held, heldGen), flat)
	builderAssertReadersAgree(t, builderComposed(t, withdrawn, withdrawnGen), flat)
}

func mapsEqualModes(a, b map[string]store_sqlite.OwnershipMode) bool {
	if len(a) != len(b) {
		return false
	}
	for key, want := range b {
		if a[key] != want {
			return false
		}
	}
	return true
}

// TestContextCorpusIsNeverWrittenToTheStore is the physical write audit the
// item asks for: the same six-changed-files-with-a-large-closure build, run
// once in each mode, measured in database pages.
//
// The durable generations are identical (the test above proves that
// separately), so the only thing this can be measuring is the transient write
// — the rows the fallback allocates for the closure and then releases. A
// released page stays in the file on SQLite's free list, which is why
// freelist_count is the sharpest of the three numbers.
func TestContextCorpusIsNeverWrittenToTheStore(t *testing.T) {
	const leaves = 120
	before := builderContextScaleTree(leaves, false)
	after := builderContextScaleTree(leaves, true)

	held := builderOpenStore(t, "audit-held")
	_, heldGen, heldReport := builderContextGeneration(t, held, before, after)
	if !heldReport.ContextHeldInMemory {
		t.Fatal("the unconstrained build did not take the in-memory route")
	}
	heldFootprint := measureDatabaseFootprint(t, held)

	forceContextWithdrawalFallback(t)
	fallback := builderOpenStore(t, "audit-fallback")
	_, fallbackGen, fallbackReport := builderContextGeneration(t, fallback, before, after)
	if fallbackReport.ContextHeldInMemory {
		t.Fatal("the constrained build still took the in-memory route")
	}
	fallbackFootprint := measureDatabaseFootprint(t, fallback)

	t.Logf("write audit (%d context files, %d changed): "+
		"in-memory db=%d free=%d wal=%d | withdrawal db=%d free=%d wal=%d (bytes)",
		len(heldReport.ClosurePaths), len(heldReport.IndexedPaths)-len(heldReport.ClosurePaths),
		heldFootprint.bytes(), heldFootprint.freeBytes(), heldFootprint.walBytes,
		fallbackFootprint.bytes(), fallbackFootprint.freeBytes(), fallbackFootprint.walBytes)

	if len(heldReport.ContextRetainedPaths) != 0 {
		t.Fatalf("a body-only edit retained %d closure files", len(heldReport.ContextRetainedPaths))
	}
	if heldReport.ContextMasks < leaves/2 {
		t.Fatalf("context masks = %d; the fixture's closure did not materialise", heldReport.ContextMasks)
	}

	// The two generations must still describe the same thing — otherwise the
	// smaller footprint would be bought with a smaller answer.
	heldCensus := generationTableCensus(t, held, heldGen)
	fallbackCensus := generationTableCensus(t, fallback, fallbackGen)
	for table, want := range fallbackCensus {
		if got := heldCensus[table]; got != want {
			t.Errorf("%s: in-memory generation holds %d rows, withdrawal holds %d", table, got, want)
		}
	}

	// The audit itself.
	if heldFootprint.freeBytes() >= fallbackFootprint.freeBytes() {
		t.Errorf("the in-memory build released %d bytes of pages and the withdrawal released %d: "+
			"the closure's payload was still written",
			heldFootprint.freeBytes(), fallbackFootprint.freeBytes())
	}
	if heldFootprint.bytes() >= fallbackFootprint.bytes() {
		t.Errorf("the in-memory build grew the database to %d bytes and the withdrawal to %d: "+
			"holding the context corpus in memory bought no write reduction",
			heldFootprint.bytes(), fallbackFootprint.bytes())
	}
	if heldFootprint.walBytes >= fallbackFootprint.walBytes {
		t.Errorf("the in-memory build wrote %d WAL bytes and the withdrawal %d: "+
			"the closure's payload still went through the log",
			heldFootprint.walBytes, fallbackFootprint.walBytes)
	}
}

// --- gate 3, second clause: the ten derived outputs ----------------------

// derivedOutputTree exercises the derived outputs a file-granular reuse is
// most likely to drop, in one tree: a pathless resolver stub (the stdlib call
// in leafuser.go), an unresolved reference that no file defines, incoming
// edges from a context file into the change set, and a reverse dependency
// whose own resolution does not move.
func derivedOutputTree(edited bool) map[string]string {
	core := "package fixture\n\nimport \"fmt\"\n\nconst CoreLimit = 3\n\nfunc Compute() {\n\tfmt.Println(\"x\")\n\tHelper()\n"
	if edited {
		core += "\tHelper()\n"
	}
	core += "}\n"
	return map[string]string{
		"core.go": core,
		"helper.go": `package fixture

import "os"

const HelperLimit = 7

func Helper() {
	_ = os.Getenv("GORTEX_DERIVED_OUTPUT_PROBE")
	Missing()
}
`,
		"caller.go": `package fixture

func Run() {
	Compute()
}
`,
	}
}

// TestDerivedOutputsSurviveTheInMemoryContextMode is gate 3's second clause.
//
// For each derived output a naive reuse drops, the composed view over a
// generation built in the read-only-context mode is compared against a fresh
// isolated index of the same tree. builderAssertReadersAgree is the oracle for
// the seven that live in the node/edge surface — it compares every field of
// every node and edge, both adjacency directions, the name / qualified-name /
// kind indexes and both bulk walks — so restub provenance (which rides on
// Edge.Meta), incoming edges from context files, unresolved-fact degradation,
// pathless resolver stubs, edge-source markers, capability / dataflow /
// framework-synth edges and clone symmetry all answer through it.
//
// The three that live OUTSIDE that surface are asserted separately below,
// because a reader-level oracle cannot see them.
func TestDerivedOutputsSurviveTheInMemoryContextMode(t *testing.T) {
	store := builderOpenStore(t, "derived-outputs")
	_, generationID, report := builderContextGeneration(
		t, store, derivedOutputTree(false), derivedOutputTree(true))

	if !report.ContextHeldInMemory {
		t.Fatal("the build did not take the read-only-context route")
	}
	if len(report.ClosurePaths) == 0 {
		t.Fatal("no closure: the fixture cannot say anything about context")
	}

	flat := builderOpenStore(t, "derived-outputs-flat")
	dirB := builderTempDir(t, "checkout-b")
	builderWriteTree(t, dirB, derivedOutputTree(true))
	builderIndex(t, flat, dirB)
	composed := builderComposed(t, store, generationID)

	// (1)(2)(4)(5)(6)(7)(10) — the node/edge surface, field for field.
	builderAssertReadersAgree(t, composed, flat)
	builderAssertMasksValidate(t, store, generationID)

	// (2) named explicitly: a context file's symbol must still be reachable
	// from the change set, and the change set's calls into it must land.
	helper := builderRepoPrefix + "/helper.go::Helper"
	if composed.GetNode(helper) == nil {
		t.Fatal("a context file's symbol vanished from the composed view")
	}
	if len(composed.GetInEdges(helper)) == 0 {
		t.Fatal("the changed file's calls into a context symbol were lost")
	}

	// (5) pathless resolver stubs: the generation must still speak for the
	// identities that live in no file, because a file mask cannot reach them.
	handle := store.AtGeneration(generationID)
	var pathless int
	for _, node := range handle.AllNodes() {
		if node != nil && node.FilePath == "" {
			pathless++
		}
	}
	if pathless > 0 && report.NodeTombstones == 0 {
		t.Errorf("the generation carries %d pathless identities and tombstoned none", pathless)
	}

	// (8) symbol FTS: the change set's identities are indexed, and no
	// withheld identity's rows were left behind.
	ftsRows := builderGenerationSymbolFTSRows(t, store, generationID)
	changed := builderRepoPrefix + "/core.go"
	if ftsRows[changed] == 0 {
		t.Errorf("the generation indexed no symbol FTS rows for its change set: %v", ftsRows)
	}
	for _, contextRel := range report.ClosurePaths {
		graphPath := builderGraphPath(builderRepoPrefix, contextRel)
		if slices.Contains(report.ContextRetainedPaths, graphPath) {
			continue
		}
		if got := ftsRows[graphPath]; got != 0 {
			t.Errorf("withheld path %q kept %d symbol FTS rows", graphPath, got)
		}
	}

	// Every generation-scoped table that records a file path speaks only for
	// the change set, including the ones the node/edge oracle cannot see:
	// constant values, the files inventory, mtimes, index failures.
	for table, paths := range generationFilePathRows(t, store, generationID) {
		if table == "generation_file_masks" {
			continue
		}
		for _, path := range paths {
			if path == changed || slices.Contains(report.ContextRetainedPaths, path) {
				continue
			}
			t.Errorf("%s carries a row for %q, which the generation does not claim", table, path)
		}
	}

	// (3) ref-fact sidecar rows and (9) LSP enrichment claims do not live in
	// the reader surface and are not re-derived by a sparse build in EITHER
	// mode. The assertion is that the mode changed nothing about them: the
	// generation declares what it has, and the composed ancestry serves the
	// rest. Both are measured against the fallback, so a future change that
	// starts or stops writing them is caught here rather than inferred.
	inMemoryCensus := generationTableCensus(t, store, generationID)

	forceContextWithdrawalFallback(t)
	fallbackStore := builderOpenStore(t, "derived-outputs-fallback")
	_, fallbackGen, fallbackReport := builderContextGeneration(
		t, fallbackStore, derivedOutputTree(false), derivedOutputTree(true))
	if fallbackReport.ContextHeldInMemory {
		t.Fatal("the constrained build still took the in-memory route")
	}
	fallbackCensus := generationTableCensus(t, fallbackStore, fallbackGen)

	var drifted []string
	for table, want := range fallbackCensus {
		if got := inMemoryCensus[table]; got != want {
			drifted = append(drifted, fmt.Sprintf("%s: in-memory %d, withdrawal %d", table, got, want))
		}
	}
	sort.Strings(drifted)
	if len(drifted) > 0 {
		t.Errorf("the read-only-context mode changed what the generation carries:\n  %s",
			strings.Join(drifted, "\n  "))
	}

	// The producer declarations must say the same thing in both modes: a
	// capability the generation cannot claim has to be declared incomplete,
	// not quietly narrowed by the route the pass took.
	if !slices.Equal(
		producerRowSummary(t, store, generationID),
		producerRowSummary(t, fallbackStore, fallbackGen),
	) {
		t.Errorf("producer declarations differ between the modes:\n  in-memory %v\n  withdrawal %v",
			producerRowSummary(t, store, generationID),
			producerRowSummary(t, fallbackStore, fallbackGen))
	}
}

// producerRowSummary renders one generation's producer-completeness rows as
// sorted "producer=state:reason" strings — the capability declarations gate 5
// requires to stay truthful.
func producerRowSummary(t *testing.T, store *store_sqlite.Store, generationID int64) []string {
	t.Helper()
	db, err := sql.Open("sqlite", store.Path())
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(
		`SELECT producer, state, COALESCE(reason, '') FROM generation_producer_completeness WHERE view_gen = ?`,
		generationID)
	if err != nil {
		t.Fatalf("query generation_producer_completeness: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var producer, state, reason string
		if err := rows.Scan(&producer, &state, &reason); err != nil {
			t.Fatalf("scan generation_producer_completeness: %v", err)
		}
		out = append(out, producer+"="+state+":"+reason)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("generation_producer_completeness rows: %v", err)
	}
	sort.Strings(out)
	return out
}

// TestPassCorpusFilterBoundsWhatADerivedGenerationPersists is the wiring
// proof at the seam itself: without a filter a handle pinned to a derived
// payload generation is disqualified from the in-memory route outright, and
// installing one is what admits it.
//
// It exercises the Indexer directly because that is where the decision lives;
// the builder tests above prove the production entrypoint reaches it.
func TestPassCorpusFilterBoundsWhatADerivedGenerationPersists(t *testing.T) {
	store := builderOpenStore(t, "filter-seam")
	dir := builderTempDir(t, "tree")
	builderWriteTree(t, dir, map[string]string{
		"core.go":   "package fixture\n\nfunc Compute() {\n\tHelper()\n}\n",
		"helper.go": "package fixture\n\nfunc Helper() {\n}\n",
	})

	handle := builderContextDerivedHandle(t, store)
	idx := New(handle, builderRegistry(), config.Default().Index, zap.NewNop())
	defer idx.Close()
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)

	called := false
	idx.setPassCorpusFilter(func(corpus *graph.Graph) error {
		called = true
		if corpus == nil {
			t.Error("the filter was handed no corpus")
			return nil
		}
		if corpus.GetNode(builderGraphPath(builderRepoPrefix, "helper.go")+"::Helper") == nil {
			t.Error("the pass corpus does not hold the file the filter is asked to bound")
		}
		// Withhold helper.go exactly as the builder does.
		corpus.EvictFiles([]string{builderGraphPath(builderRepoPrefix, "helper.go")})
		return nil
	})

	if _, err := idx.IndexCtx(context.Background(), dir); err != nil {
		t.Fatalf("IndexCtx: %v", err)
	}
	if !called {
		t.Fatal("the pass never handed its corpus to the filter: the derived generation was not admitted")
	}
	for _, node := range handle.AllNodes() {
		if node != nil && node.FilePath == builderGraphPath(builderRepoPrefix, "helper.go") {
			t.Fatalf("a withheld path reached the store: %s", node.ID)
		}
	}
	if handle.GetNode(builderGraphPath(builderRepoPrefix, "core.go")+"::Compute") == nil {
		t.Fatal("the surviving half of the corpus did not reach the store")
	}
}

// TestPreparedVectorsAreDroppedForWithheldIdentities pins the one projection
// the filter cannot reach on its own.
//
// Embeddings are prepared once, from the pass corpus, BEFORE the filter runs —
// the pipeline pays for them exactly once and will not re-embed. A withheld
// identity therefore still has a prepared vector in hand when the drain
// publishes, and publishing it would write a vector row for a symbol the
// generation deliberately does not carry. Both shapes are covered: a symbol's
// own vector and the AST sub-chunks hanging off it, whose ids live in no
// corpus and must be judged by their parent.
func TestPreparedVectorsAreDroppedForWithheldIdentities(t *testing.T) {
	kept := builderGraphPath(builderRepoPrefix, "core.go") + "::Compute"
	withheld := builderGraphPath(builderRepoPrefix, "helper.go") + "::Helper"

	corpus := graph.New()
	corpus.AddBatch([]*graph.Node{{
		ID: kept, Kind: graph.KindFunction, Name: "Compute",
		FilePath: builderGraphPath(builderRepoPrefix, "core.go"), RepoPrefix: builderRepoPrefix,
	}}, nil)

	plan := &preparedVectorPlan{
		repoPrefix: builderRepoPrefix,
		dims:       2,
		items: []graph.VectorCorpusItem{
			{NodeID: kept, Vec: []float32{1, 0}},
			{NodeID: kept + "#chunk0", ParentID: kept, Vec: []float32{0, 1}},
			{NodeID: withheld, Vec: []float32{1, 1}},
			{NodeID: withheld + "#chunk0", ParentID: withheld, Vec: []float32{0, 0}},
		},
		chunkMap: map[string]string{
			kept + "#chunk0":     kept,
			withheld + "#chunk0": withheld,
		},
	}

	pruneVectorPlanToCorpus(plan, corpus)

	var survivors []string
	for _, item := range plan.items {
		survivors = append(survivors, item.NodeID)
	}
	sort.Strings(survivors)
	want := []string{kept, kept + "#chunk0"}
	sort.Strings(want)
	if !slices.Equal(survivors, want) {
		t.Fatalf("prepared vectors after pruning = %v, want %v", survivors, want)
	}
	if _, present := plan.chunkMap[withheld+"#chunk0"]; present {
		t.Errorf("the chunk map still routes a withheld identity's sub-chunk")
	}
	if plan.chunkMap[kept+"#chunk0"] != kept {
		t.Errorf("the surviving identity lost its chunk mapping: %v", plan.chunkMap)
	}
}

// builtinSentinelTree references three Go builtins from the file the change
// edits, so the pass materialises repo-scoped sentinel identities that live in
// no file — the class a file mask cannot speak for and the drain's edge batch
// is most likely to overwrite.
func builtinSentinelTree(edited bool) map[string]string {
	body := "\ts := make([]int, 0)\n\ts = append(s, 1)\n\t_ = len(s)\n"
	if edited {
		body += "\ts = append(s, 2)\n"
	}
	return map[string]string{
		"core.go":   "package fixture\n\nfunc Compute() {\n" + body + "}\n",
		"caller.go": "package fixture\n\nfunc Run() {\n\tCompute()\n}\n",
	}
}

// TestBuiltinSentinelsKeepTheirBoundaryIdentityInTheInMemoryMode pins the one
// row class the drain would otherwise downgrade.
//
// The resolver materialises a builtin sentinel with the workspace and project
// of the symbol that referenced it, on purpose: a blank rewrite of those
// columns is a known regression class (go_builtins_attribution.go's own
// comment names the warm-start backfill it caused). The drain moves that row
// to disk and then hands the durable store an edge batch pointing at the same
// ids, whose lazy funnel re-materialises them unattributed. A generation that
// published those would carry strictly less than the one the write-then-
// withdraw path publishes, which the mode's contract forbids.
func TestBuiltinSentinelsKeepTheirBoundaryIdentityInTheInMemoryMode(t *testing.T) {
	store := builderOpenStore(t, "builtin-boundary")
	_, generationID, report := builderContextGeneration(
		t, store, builtinSentinelTree(false), builtinSentinelTree(true))
	if !report.ContextHeldInMemory {
		t.Fatal("the build did not take the read-only-context route")
	}

	handle := store.AtGeneration(generationID)
	var sentinels int
	for _, node := range handle.AllNodes() {
		if node == nil || !graph.IsBuiltinStub(node.ID) {
			continue
		}
		sentinels++
		if node.WorkspaceID != builderRepoPrefix || node.ProjectID != builderRepoPrefix {
			t.Errorf("builtin sentinel %q carries workspace %q / project %q, want %q for both",
				node.ID, node.WorkspaceID, node.ProjectID, builderRepoPrefix)
		}
	}
	if sentinels == 0 {
		t.Fatal("the fixture materialised no builtin sentinel — it pins nothing")
	}
	if report.NodeTombstones < sentinels {
		t.Errorf("the generation carries %d builtin sentinels and tombstoned %d; "+
			"an identity in no file needs an identity-level claim",
			sentinels, report.NodeTombstones)
	}
}

// TestAFilteredPassDoesNotQueueForAnInMemorySlot pins the latency bound.
//
// The in-memory route is an optimisation over a path that already works, so a
// pass that cannot get a slot must write and withdraw rather than wait: a
// layer build is what a checkout view blocks on, and the process-wide shadow
// budget's usual holder is a whole-repository cold drain. The filter is simply
// never called, and the caller falls back.
func TestAFilteredPassDoesNotQueueForAnInMemorySlot(t *testing.T) {
	store := builderOpenStore(t, "admission-bound")
	dir := builderTempDir(t, "tree")
	builderWriteTree(t, dir, map[string]string{
		"core.go":   "package fixture\n\nfunc Compute() {\n\tHelper()\n}\n",
		"helper.go": "package fixture\n\nfunc Helper() {\n}\n",
	})

	// One slot, already taken. Capacity is ample, so the refusal is the
	// concurrency gate rather than the "weight exceeds the budget" arm, which
	// returns immediately and would make the test vacuous.
	budget := newShadowAdmissionBudget(1<<30, 1)
	holder, err := budget.acquire(context.Background(), 1)
	if err != nil || holder == nil {
		t.Fatalf("seed the budget's only slot: lease=%v err=%v", holder, err)
	}
	defer holder.Release()

	handle := builderContextDerivedHandle(t, store)
	idx := New(handle, builderRegistry(), config.Default().Index, zap.NewNop())
	defer idx.Close()
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	idx.shadowAdmission = budget

	called := false
	idx.setPassCorpusFilter(func(*graph.Graph) error {
		called = true
		return nil
	})

	if _, err := idx.IndexCtx(context.Background(), dir); err != nil {
		t.Fatalf("a filtered pass that could not get a slot failed instead of falling back: %v", err)
	}
	if called {
		t.Fatal("the filter ran although the budget never granted a slot")
	}
	// The fallback path is the one that shipped before the mode: the pass
	// writes its whole file set, and the caller withdraws afterwards.
	if handle.GetNode(builderGraphPath(builderRepoPrefix, "helper.go")+"::Helper") == nil {
		t.Fatal("the fallback pass wrote nothing; the build has no payload to withdraw")
	}
}
