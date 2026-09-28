package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// legacyResolvedConversionUpdateJSONStatement is the resolved-conversion
// UPDATE as it shipped before its join order was pinned: UPDATE ... FROM a
// json_each CTE joined on the edge identity. It is kept verbatim as the
// reference the production statement must be observationally identical to.
// The planner ran it with edges outer (view_gen index) and json_each inner, so
// it re-parsed the whole chunk payload once per stored edge of the generation.
func legacyResolvedConversionUpdateJSONStatement(updateKind bool) string {
	if updateKind {
		return `WITH patch AS (SELECT
		value ->> 0 AS old_from_id,
		value ->> 1 AS old_to_id,
		value ->> 2 AS old_kind,
		value ->> 3 AS file_path,
		value ->> 4 AS line,
		value ->> 5 AS new_to_id,
		value ->> 6 AS new_kind,
		CAST(value ->> 7 AS REAL) AS confidence,
		value ->> 8 AS confidence_label,
		value ->> 9 AS origin,
		value ->> 10 AS tier,
		value ->> 11 AS cross_repo,
		unhex(value ->> 12) AS meta,
		value ->> 13 AS resolve_terminal,
		value ->> 14 AS resolve_terminal_reason,
		value ->> 15 AS semantic_source
	FROM json_each(?))
	UPDATE OR IGNORE edges AS e
	SET to_id = p.new_to_id,
		kind = p.new_kind,
		confidence = p.confidence,
		confidence_label = p.confidence_label,
		origin = p.origin,
		tier = p.tier,
		cross_repo = p.cross_repo,
		meta = p.meta,
		resolve_terminal = p.resolve_terminal,
		resolve_terminal_reason = p.resolve_terminal_reason,
		semantic_source = p.semantic_source
	FROM patch AS p
	WHERE e.from_id = p.old_from_id
		AND e.to_id = p.old_to_id
		AND e.kind = p.old_kind
		AND e.file_path = p.file_path
		AND e.line = p.line
		AND e.view_gen = ?`
	}
	return `WITH patch AS (SELECT
		value ->> 0 AS old_from_id,
		value ->> 1 AS old_to_id,
		value ->> 2 AS kind,
		value ->> 3 AS file_path,
		value ->> 4 AS line,
		value ->> 5 AS new_to_id,
		CAST(value ->> 6 AS REAL) AS confidence,
		value ->> 7 AS confidence_label,
		value ->> 8 AS origin,
		value ->> 9 AS tier,
		value ->> 10 AS cross_repo,
		unhex(value ->> 11) AS meta,
		value ->> 12 AS resolve_terminal,
		value ->> 13 AS resolve_terminal_reason,
		value ->> 14 AS semantic_source
	FROM json_each(?))
	UPDATE OR IGNORE edges AS e
	SET to_id = p.new_to_id,
		confidence = p.confidence,
		confidence_label = p.confidence_label,
		origin = p.origin,
		tier = p.tier,
		cross_repo = p.cross_repo,
		meta = p.meta,
		resolve_terminal = p.resolve_terminal,
		resolve_terminal_reason = p.resolve_terminal_reason,
		semantic_source = p.semantic_source
	FROM patch AS p
	WHERE e.from_id = p.old_from_id
		AND e.to_id = p.old_to_id
		AND e.kind = p.kind
		AND e.file_path = p.file_path
		AND e.line = p.line
		AND e.view_gen = ?`
}

const conversionFixtureGeneration int64 = 3

// conversionFixture seeds one store with the shapes the conversion UPDATE must
// treat identically under either statement: the same logical identities in the
// base corpus and in the target generation (only the generation may move),
// sources that are missing, destinations that already exist (OR IGNORE),
// kind changes, NULL/false/true terminal stamps, semantic sources, cross-repo
// flags, meta with JSON metacharacters and no meta at all. It returns the
// reindex batch; the fixture exceeds one json_each chunk so the statement runs
// more than once per direction.
func conversionFixture(t *testing.T, store *Store, reverse bool) []graph.EdgeReindex {
	t.Helper()
	const count = 700
	unresolved := func(i int) string { return fmt.Sprintf("%sSym%04d", graph.UnresolvedMarker, i) }
	resolved := func(i int) string { return fmt.Sprintf(`repo/target\t%04d.go::Sym%04d "q"`, i, i) }
	from := func(i int) string { return fmt.Sprintf("repo/caller%03d.go::Caller%04d", i%37, i) }
	file := func(i int) string { return fmt.Sprintf("repo/caller%03d.go", i%37) }

	oldTo, newTo := unresolved, resolved
	if reverse {
		oldTo, newTo = resolved, unresolved
	}
	var baseEdges, genEdges []*graph.Edge
	batch := make([]graph.EdgeReindex, 0, count)
	for i := 0; i < count; i++ {
		kind := graph.EdgeCalls
		stored := &graph.Edge{
			From: from(i), To: oldTo(i), Kind: kind, FilePath: file(i), Line: i%50 + 1,
			Confidence: 0.25, Origin: "syntax", Tier: "syntax",
			Meta: map[string]any{"call_text": fmt.Sprintf("obj.Sym%04d(\"a\", `b`)", i)},
		}
		// Every identity also exists in the base corpus with its own payload:
		// a conversion scoped to the generation must leave these bytes alone.
		baseCopy := *stored
		baseCopy.Origin = "base"
		baseEdges = append(baseEdges, &baseCopy)
		if i%23 != 5 { // i%23 == 5: the source row is missing in the generation.
			genEdges = append(genEdges, stored)
		}
		next := &graph.Edge{
			From: from(i), To: newTo(i), Kind: kind, FilePath: file(i), Line: i%50 + 1,
			Confidence: 0.5 + float64(i%7)/10, ConfidenceLabel: []string{"", "exact", "héuristic → ☂"}[i%3],
			Origin: "resolver", Tier: []string{"semantic", "", "lsp"}[i%3], CrossRepo: i%4 == 0,
		}
		switch i % 5 {
		case 0:
			next.Meta = map[string]any{"note": `sa"ys \ → ☂`, "n": float64(i)}
		case 1:
			next.Meta = map[string]any{"resolve_terminal": false, "semantic_source": "lsp"}
		case 2:
			next.Meta = map[string]any{"resolve_terminal": true, "resolve_terminal_reason": "bound"}
		case 3:
			// no meta at all: the column must land NULL
		case 4:
			next.Meta = map[string]any{"receiver": "obj", "arity": float64(2)}
		}
		entry := graph.EdgeReindex{OldTo: oldTo(i), Edge: next}
		if i%11 == 3 {
			next.Kind = graph.EdgeReferences
			entry.OldKind = kind
		}
		if i%29 == 7 {
			// The destination already exists in the generation: UPDATE OR
			// IGNORE must skip the row, leaving the source untouched.
			genEdges = append(genEdges, &graph.Edge{
				From: next.From, To: next.To, Kind: next.Kind, FilePath: next.FilePath, Line: next.Line,
				Confidence: 0.1, Origin: "occupant",
			})
		}
		batch = append(batch, entry)
	}
	store.AddBatch(nil, baseEdges)
	store.AtGeneration(conversionFixtureGeneration).AddBatch(nil, genEdges)
	return batch
}

// dumpEdgesExact renders every stored edge column through quote(), which
// spells the storage class (X'..' blobs, integer vs real, NULL), ordered by
// row id so identity and payload are compared byte for byte.
func dumpEdgesExact(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.db.Query(`SELECT name FROM pragma_table_info('edges') ORDER BY cid`)
	require.NoError(t, err)
	var columns []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		columns = append(columns, "quote("+name+")")
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.NotEmpty(t, columns)

	rows, err = store.db.Query(`SELECT ` + strings.Join(columns, ` || '|' || `) + ` FROM edges ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		out = append(out, line)
	}
	require.NoError(t, rows.Err())
	return out
}

// runConversionStatement drives one conversion arm exactly as the production
// transaction does — same candidate filter, same payload chunks, same bound
// generation — but through the given statement text.
func runConversionStatement(t *testing.T, store *Store, batch []graph.EdgeReindex, statement func(bool) string) []int64 {
	t.Helper()
	mutations, err := sqliteReindexMutations(batch)
	require.NoError(t, err)
	plan, ok := sqliteResolvedConversionUpdatePlan(mutations)
	require.True(t, ok, "fixture must take the resolved-conversion fast path")

	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	tx, err := store.beginWriteContext(context.Background())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var affected []int64
	for _, updateKind := range []bool{false, true} {
		var candidates []sqliteReindexMutation
		for _, mutation := range mutations {
			if plan.updateCandidate(mutation, updateKind) && jsonSafeResolvedConversion(mutation) {
				candidates = append(candidates, mutation)
			}
		}
		for start := 0; start < len(candidates); {
			payload, rowCount, err := encodeResolvedConversionRows(candidates[start:], updateKind)
			require.NoError(t, err)
			result, err := tx.Exec(statement(updateKind), payload, store.viewGen)
			require.NoError(t, err)
			n, err := result.RowsAffected()
			require.NoError(t, err)
			affected = append(affected, n)
			start += rowCount
		}
	}
	require.NoError(t, tx.Commit())
	return affected
}

// TestResolvedConversionUpdateMatchesLegacyStatement is the identity proof for
// the pinned join order: over a fixture with resolved and unresolved
// conversions, missing sources, occupied destinations, kind changes and base
// corpus twins, the production statement and the legacy statement leave the
// edges table byte-identical (every column, storage class and row id) and
// report the same affected-row counts per chunk — the count drives the repair
// path, so it is part of the contract.
func TestResolvedConversionUpdateMatchesLegacyStatement(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			open := func(name string) (*Store, []graph.EdgeReindex) {
				store, err := Open(filepath.Join(t.TempDir(), name))
				require.NoError(t, err)
				t.Cleanup(func() { _ = store.Close() })
				batch := conversionFixture(t, store, reverse)
				return store.AtGeneration(conversionFixtureGeneration), batch
			}
			legacy, legacyBatch := open("legacy.sqlite")
			pinned, pinnedBatch := open("pinned.sqlite")
			require.Equal(t, dumpEdgesExact(t, legacy), dumpEdgesExact(t, pinned), "fixtures must seed identically")

			legacyAffected := runConversionStatement(t, legacy, legacyBatch, legacyResolvedConversionUpdateJSONStatement)
			pinnedAffected := runConversionStatement(t, pinned, pinnedBatch, sqliteResolvedConversionUpdateJSONStatement)
			require.Equal(t, legacyAffected, pinnedAffected, "affected-row counts drive the repair path")
			require.Greater(t, len(pinnedAffected), 2, "fixture must span more than one chunk")
			total := int64(0)
			for _, n := range pinnedAffected {
				total += n
			}
			require.Greater(t, total, int64(0))

			legacyRows, pinnedRows := dumpEdgesExact(t, legacy), dumpEdgesExact(t, pinned)
			require.Equal(t, len(legacyRows), len(pinnedRows))
			for i := range legacyRows {
				require.Equal(t, legacyRows[i], pinnedRows[i], "row %d diverged", i)
			}
		})
	}
}

// TestReindexEdgesResolvedConversionPipelineAppliesEveryRebind runs the same
// fixture through the whole production reindex transaction: the in-place arm,
// the short-count repair of chunks with missing sources and occupied
// destinations, and generation scoping. Every rebind must be visible
// afterwards and the base corpus twins must stay untouched.
func TestReindexEdgesResolvedConversionPipelineAppliesEveryRebind(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "pipeline.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	batch := conversionFixture(t, store, false)
	derived := store.AtGeneration(conversionFixtureGeneration)
	stats, err := derived.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	require.Positive(t, stats.updatedRows)
	require.Positive(t, stats.insertedRows+stats.deletedRows, "occupied destinations and missing sources must reach the repair path")

	for _, edge := range batch {
		want := edge.Edge
		got := derived.GetOutEdges(want.From)
		found := false
		for _, e := range got {
			if e.To == want.To && e.Kind == want.Kind && e.Line == want.Line {
				found = true
			}
		}
		require.True(t, found, "rebind %s -> %s missing after reindex", want.From, want.To)
	}
	// The base corpus twins are untouched.
	var moved int
	require.NoError(t, store.db.QueryRow(
		`SELECT count(*) FROM edges WHERE view_gen = 0 AND (origin <> 'base' OR to_id NOT LIKE ?)`,
		graph.UnresolvedMarker+"%").Scan(&moved))
	require.Zero(t, moved, "a generation-scoped conversion must not touch base rows")
}

// TestResolvedConversionUpdatePlanDrivesFromPayload locks the join order: the
// payload is scanned once, each row probes edges on its full identity, and the
// update reaches the table by rowid. The failure mode it guards is the
// planner putting edges outer (view_gen index) and re-running json_each per
// stored edge — O(generation edges x payload bytes) per statement, which held
// the writer gate for an hour on a sparse build. Checked with and without
// planner statistics over a store whose generations are uneven.
func TestResolvedConversionUpdatePlanDrivesFromPayload(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "plan.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	conversionFixture(t, store, false)
	for g := int64(10); g < 30; g++ {
		store.AtGeneration(g).AddBatch(nil, []*graph.Edge{{
			From: fmt.Sprintf("small%d::F", g), To: "x::Y", Kind: graph.EdgeCalls, FilePath: "s.go", Line: 1,
		}})
	}
	derived := store.AtGeneration(conversionFixtureGeneration)

	check := func(label string) {
		for _, updateKind := range []bool{false, true} {
			plan := explainQueryPlanArgs(t, derived, sqliteResolvedConversionUpdateJSONStatement(updateKind), "[]", conversionFixtureGeneration)
			joined := strings.Join(plan, "\n")
			jsonAt, probeAt := -1, -1
			for i, line := range plan {
				if strings.Contains(line, "json_each") && jsonAt < 0 {
					jsonAt = i
				}
				if strings.HasPrefix(line, "SEARCH e USING") && strings.Contains(line, "from_id=?") &&
					strings.Contains(line, "view_gen=?") && probeAt < 0 {
					probeAt = i
				}
				require.False(t, strings.HasPrefix(line, "SCAN e") || strings.HasPrefix(line, "SCAN edges"),
					"%s updateKind=%t: edges must never be scanned:\n%s", label, updateKind, joined)
			}
			require.True(t, jsonAt >= 0 && probeAt > jsonAt,
				"%s updateKind=%t: json_each must drive an identity probe of edges:\n%s", label, updateKind, joined)
			require.Contains(t, joined, "SEARCH edges USING INTEGER PRIMARY KEY (rowid=?)",
				"%s updateKind=%t: the update must reach its target by rowid", label, updateKind)
		}
	}
	check("no stats")
	store.writeMu.Lock()
	err = store.refreshPlannerStatsLocked(context.Background())
	store.writeMu.Unlock()
	require.NoError(t, err)
	check("with stats")
}

// TestReindexEdgesReleasesWriterGateBetweenChunks pins the liveness contract a
// long rebind owes every other writer: the gate is held per transaction chunk,
// not for the whole batch, so a writer queued behind the reindex gets its turn
// between chunks and observes a partially applied batch.
func TestReindexEdgesReleasesWriterGateBetweenChunks(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gate.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	derived := store.AtGeneration(conversionFixtureGeneration)

	count := 2*reindexChunkSize + 1
	seed := make([]*graph.Edge, 0, count)
	batch := make([]graph.EdgeReindex, 0, count)
	for i := 0; i < count; i++ {
		from := fmt.Sprintf("repo/c%05d.go::Caller", i)
		oldTo := fmt.Sprintf("%sT%05d", graph.UnresolvedMarker, i)
		seed = append(seed, &graph.Edge{From: from, To: oldTo, Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: i + 1})
		batch = append(batch, graph.EdgeReindex{OldTo: oldTo, Edge: &graph.Edge{
			From: from, To: fmt.Sprintf("repo/t%05d.go::T", i), Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: i + 1,
		}})
	}
	derived.AddBatch(nil, seed)
	converted := func() int {
		var n int
		require.NoError(t, store.db.QueryRow(
			`SELECT count(*) FROM edges WHERE view_gen = ? AND to_id NOT LIKE ?`,
			conversionFixtureGeneration, graph.UnresolvedMarker+"%").Scan(&n))
		return n
	}

	release, err := store.HoldWriteGate(context.Background())
	require.NoError(t, err)
	var (
		wg       sync.WaitGroup
		stats    sqliteReindexSetStats
		stopped  = make(chan struct{})
		mu       sync.Mutex
		observed []int
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stopped)
		stats, err = derived.reindexEdgesSetOriented(batch)
	}()
	// A competing writer that queues on the gate for as long as the reindex runs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopped:
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			lockErr := store.writeMu.LockContext(ctx)
			cancel()
			if lockErr != nil {
				continue
			}
			n := converted()
			store.writeMu.Unlock()
			mu.Lock()
			observed = append(observed, n)
			mu.Unlock()
		}
	}()
	time.Sleep(20 * time.Millisecond)
	release()
	wg.Wait()
	require.NoError(t, err)
	require.Equal(t, count, stats.updatedRows)
	require.Equal(t, count, converted())

	partial := false
	for _, n := range observed {
		if n > 0 && n < count {
			require.Zero(t, n%reindexChunkSize, "writers interleave only at chunk boundaries, saw %d", n)
			partial = true
		}
	}
	require.True(t, partial, "a queued writer never got the gate mid-batch; observed %v", observed)
}
