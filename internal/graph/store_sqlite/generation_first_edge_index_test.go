package store_sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type privateEdgeIndexLayout int

const (
	privateBaselineEdgeIndexes privateEdgeIndexLayout = iota
	privateGenerationFirstEdgeIndexes
)

var privateEdgeIndexNames = []string{
	"edges_by_to",
	"edges_by_from",
	"edges_by_from_line",
	"edges_by_from_line_kind",
}

var privateBaselineEdgeIndexDDL = map[string]string{
	"edges_by_to":             `CREATE INDEX edges_by_to ON edges(to_id, view_gen, kind)`,
	"edges_by_from":           `CREATE INDEX edges_by_from ON edges(from_id, view_gen, kind)`,
	"edges_by_from_line":      `CREATE INDEX edges_by_from_line ON edges(from_id, line)`,
	"edges_by_from_line_kind": `CREATE INDEX edges_by_from_line_kind ON edges(from_id, line, kind)`,
}

var privateGenerationFirstEdgeIndexDDL = map[string]string{
	"edges_by_to":             `CREATE INDEX edges_by_to ON edges(view_gen, to_id, kind)`,
	"edges_by_from":           `CREATE INDEX edges_by_from ON edges(view_gen, from_id, kind)`,
	"edges_by_from_line":      `CREATE INDEX edges_by_from_line ON edges(view_gen, from_id, line)`,
	"edges_by_from_line_kind": `CREATE INDEX edges_by_from_line_kind ON edges(view_gen, from_id, line, kind)`,
}

func TestPrivateGenerationFirstEdgeIndexMigrationPreservesRowsOrderAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge-index-migration.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	open := true
	t.Cleanup(func() {
		if open {
			_ = store.Close()
		}
	})

	privateSeedEdgeIndexSemantics(t, store)
	baseline := privateEdgeRowsDigest(t, store)
	baselineLookupDigest := privateProductionEdgeLookupDigest(t, store)
	privateApplyEdgeIndexLayout(t, store, privateBaselineEdgeIndexes)
	privateAssertEdgeIndexLayout(t, store.db, privateBaselineEdgeIndexes)

	privateApplyEdgeIndexLayout(t, store, privateGenerationFirstEdgeIndexes)
	privateAssertEdgeIndexLayout(t, store.db, privateGenerationFirstEdgeIndexes)
	privateAssertEdgeRowsDigest(t, store, baseline)
	if got := privateProductionEdgeLookupDigest(t, store); got != baselineLookupDigest {
		t.Fatalf("production lookup digest after generation-first migration=%s want=%s", got, baselineLookupDigest)
	}
	privateAssertEdgeReaderPlans(t, store.db, privateGenerationFirstEdgeIndexes)

	// DDL is transactional. A failed/abandoned reverse migration must leave the
	// generation-first definitions and every edge row untouched.
	store.writeMu.Lock()
	tx, err := store.beginWrite()
	if err != nil {
		store.writeMu.Unlock()
		t.Fatalf("begin rollback probe: %v", err)
	}
	if err := privateReplaceEdgeIndexLayoutTx(tx, privateBaselineEdgeIndexes); err != nil {
		_ = tx.Rollback()
		store.writeMu.Unlock()
		t.Fatalf("stage rollback probe: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		store.writeMu.Unlock()
		t.Fatalf("rollback staged migration: %v", err)
	}
	store.writeMu.Unlock()
	privateAssertEdgeIndexLayout(t, store.db, privateGenerationFirstEdgeIndexes)
	privateAssertEdgeRowsDigest(t, store, baseline)

	if err := store.Close(); err != nil {
		t.Fatalf("close generation-first store: %v", err)
	}
	open = false
	store, err = Open(path)
	if err != nil {
		t.Fatalf("reopen generation-first store: %v", err)
	}
	open = true
	privateAssertEdgeIndexLayout(t, store.db, privateGenerationFirstEdgeIndexes)
	privateAssertEdgeRowsDigest(t, store, baseline)

	privateApplyEdgeIndexLayout(t, store, privateBaselineEdgeIndexes)
	privateAssertEdgeIndexLayout(t, store.db, privateBaselineEdgeIndexes)
	privateAssertEdgeRowsDigest(t, store, baseline)
	if got := privateProductionEdgeLookupDigest(t, store); got != baselineLookupDigest {
		t.Fatalf("production lookup digest after reverse migration=%s want=%s", got, baselineLookupDigest)
	}
	privateStampSchemaVersion(t, store, 27)
	if err := store.Close(); err != nil {
		t.Fatalf("close rolled-back store: %v", err)
	}
	open = false
	store, err = Open(path)
	if err != nil {
		t.Fatalf("reopen baseline store: %v", err)
	}
	open = true
	privateAssertEdgeIndexLayout(t, store.db, privateGenerationFirstEdgeIndexes)
	privateAssertEdgeRowsDigest(t, store, baseline)
}

func TestPrivateGenerationFirstEdgeIndexReaderPlans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge-index-plans.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open plan store: %v", err)
	}
	defer store.Close()
	privateSeedEdgeIndexSemantics(t, store)
	privateApplyEdgeIndexLayout(t, store, privateBaselineEdgeIndexes)
	privateAssertEdgeIndexLayout(t, store.db, privateBaselineEdgeIndexes)
	privateAssertEdgeReaderPlans(t, store.db, privateBaselineEdgeIndexes)
	privateApplyEdgeIndexLayout(t, store, privateGenerationFirstEdgeIndexes)
	privateAssertEdgeIndexLayout(t, store.db, privateGenerationFirstEdgeIndexes)
	privateAssertEdgeReaderPlans(t, store.db, privateGenerationFirstEdgeIndexes)
}

func TestPrivateGenerationFirstEdgeIndexV28CandidateMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge-index-v28-candidate.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	privateSeedEdgeIndexSemantics(t, store)
	beforeRows := privateEdgeRowsDigest(t, store)
	beforeLookup := privateProductionEdgeLookupDigest(t, store)
	privateAssertEdgeIndexLayout(t, store.db, privateGenerationFirstEdgeIndexes)
	privateStageV27EdgeCandidateIndexes(t, store)

	// The v28 replacement shares its caller's transaction. A failed caller
	// cannot leave either a half-rebuilt index set or changed graph rows.
	store.writeMu.Lock()
	tx, err := store.beginWrite()
	if err != nil {
		store.writeMu.Unlock()
		t.Fatalf("begin rollback probe: %v", err)
	}
	if err := scopeEdgeCandidateIndexesByViewGeneration(tx); err != nil {
		_ = tx.Rollback()
		store.writeMu.Unlock()
		t.Fatalf("stage rollback probe: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		store.writeMu.Unlock()
		t.Fatalf("rollback migration probe: %v", err)
	}
	store.writeMu.Unlock()
	privateAssertEdgeRowsDigest(t, store, beforeRows)

	// v26's registry already covers edges_by_from and edges_by_to but not the
	// two site indexes. The staged v27 layout models that chain exactly.
	if got, err := privateMismatchedGenerationFirstEdgeCandidateIndexes(store.db); err != nil {
		t.Fatalf("inspect mixed v26/v28 shape: %v", err)
	} else if want := []string{"edges_by_from_line", "edges_by_from_line_kind"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mixed v26/v28 mismatches=%v want=%v", got, want)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close staged v27 store: %v", err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatalf("Open did not apply actual v28 migration: %v", err)
	}
	privateAssertEdgeIndexLayout(t, store.db, privateGenerationFirstEdgeIndexes)
	privateAssertEdgeRowsDigest(t, store, beforeRows)
	if got := privateProductionEdgeLookupDigest(t, store); got != beforeLookup {
		t.Fatalf("production lookup after candidate migration=%s want=%s", got, beforeLookup)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatalf("reopen migrated store: %v", err)
	}
	defer store.Close()
	privateAssertEdgeIndexLayout(t, store.db, privateGenerationFirstEdgeIndexes)
	privateAssertEdgeRowsDigest(t, store, beforeRows)
	if got := privateProductionEdgeLookupDigest(t, store); got != beforeLookup {
		t.Fatalf("production lookup after reopen=%s want=%s", got, beforeLookup)
	}
}

func TestPrivateGenerationFirstEdgeIndexNewerSchemaRefusalPreservesStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "newer-schema.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	privateSeedEdgeIndexSemantics(t, store)
	store.writeMu.Lock()
	tx, err := store.beginWrite()
	if err == nil {
		_, err = tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion+1))
	}
	if err == nil {
		err = tx.Commit()
	} else if tx != nil {
		_ = tx.Rollback()
	}
	store.writeMu.Unlock()
	if err != nil {
		_ = store.Close()
		t.Fatalf("stamp newer version: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close newer store: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read newer store before refusal: %v", err)
	}
	beforeDigest := sha256.Sum256(before)
	if reopened, err := Open(path); err == nil {
		_ = reopened.Close()
		t.Fatal("opening newer schema unexpectedly succeeded")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read newer store after refusal: %v", err)
	}
	if got := sha256.Sum256(after); got != beforeDigest {
		t.Fatalf("newer schema refusal changed database bytes: got=%x want=%x", got, beforeDigest)
	}
}

func TestPrivateGenerationFirstEdgeIndexV28OpenMigratesEveryPriorLayout(t *testing.T) {
	cases := []struct {
		name        string
		definitions map[string]string
	}{
		{name: "v27_all_legacy", definitions: privateBaselineEdgeIndexDDL},
		{name: "v27_v26_endpoint_only", definitions: map[string]string{
			"edges_by_from":           privateGenerationFirstEdgeIndexDDL["edges_by_from"],
			"edges_by_to":             privateGenerationFirstEdgeIndexDDL["edges_by_to"],
			"edges_by_from_line":      privateBaselineEdgeIndexDDL["edges_by_from_line"],
			"edges_by_from_line_kind": privateBaselineEdgeIndexDDL["edges_by_from_line_kind"],
		}},
		{name: "v27_all_generation_leading", definitions: privateGenerationFirstEdgeIndexDDL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "edge-index-v27.db")
			store, err := Open(path)
			if err != nil {
				t.Fatalf("open fresh v28 store: %v", err)
			}
			privateSeedEdgeIndexSemantics(t, store)
			beforeRows := privateEdgeRowsDigest(t, store)
			beforeLookup := privateProductionEdgeLookupDigest(t, store)
			privateStageV27EdgeCandidateIndexesWith(t, store, tc.definitions)
			if err := store.Close(); err != nil {
				t.Fatalf("close staged v27 store: %v", err)
			}
			store, err = Open(path)
			if err != nil {
				t.Fatalf("actual v28 Open: %v", err)
			}
			defer store.Close()
			var version int
			if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
				t.Fatalf("read migrated schema version: %v", err)
			}
			if version != currentSchemaVersion {
				t.Fatalf("migrated schema version=%d want=%d", version, currentSchemaVersion)
			}
			privateAssertEdgeIndexLayout(t, store.db, privateGenerationFirstEdgeIndexes)
			privateAssertEdgeRowsDigest(t, store, beforeRows)
			if got := privateProductionEdgeLookupDigest(t, store); got != beforeLookup {
				t.Fatalf("production lookup after %s migration=%s want=%s", tc.name, got, beforeLookup)
			}
		})
	}
}

func TestPrivateGenerationFirstEdgeIndexV28OpenFailureRollsBackLiveStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge-index-v27-failure.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open fresh v28 store: %v", err)
	}
	privateSeedEdgeIndexSemantics(t, store)
	privateStageV27EdgeCandidateIndexes(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("close staged v27 store: %v", err)
	}
	before := privateEdgeMigrationDiskSnapshot(t, path)

	index := -1
	for i := range bulkDroppableIndexes {
		if bulkDroppableIndexes[i].name == "edges_by_from_line" {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatal("edges_by_from_line missing from production bulk registry")
	}
	original := bulkDroppableIndexes[index].ddl
	bulkDroppableIndexes[index].ddl = `CREATE INDEX edges_by_from_line ON missing_edges(view_gen, from_id, line)`
	t.Cleanup(func() { bulkDroppableIndexes[index].ddl = original })

	if reopened, err := Open(path); err == nil {
		_ = reopened.Close()
		t.Fatal("Open with invalid v28 registry DDL unexpectedly succeeded")
	}
	after := privateEdgeMigrationDiskSnapshot(t, path)
	if after != before {
		t.Fatalf("failed v28 Open changed live v27 store: after=%+v want=%+v", after, before)
	}
}

type privateEdgeMigrationSnapshot struct {
	Version     int
	IndexDDL    string
	EdgeRows    int
	DatabaseSHA string
}

func privateEdgeMigrationDiskSnapshot(tb testing.TB, path string) privateEdgeMigrationSnapshot {
	tb.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		tb.Fatalf("open snapshot database: %v", err)
	}
	defer db.Close()
	var snapshot privateEdgeMigrationSnapshot
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&snapshot.Version); err != nil {
		tb.Fatalf("read snapshot schema version: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM edges`).Scan(&snapshot.EdgeRows); err != nil {
		tb.Fatalf("read snapshot edge rows: %v", err)
	}
	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'index' AND name IN ('edges_by_from', 'edges_by_to', 'edges_by_from_line', 'edges_by_from_line_kind') ORDER BY name`)
	if err != nil {
		tb.Fatalf("read snapshot index definitions: %v", err)
	}
	defer rows.Close()
	var definitions strings.Builder
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			tb.Fatalf("scan snapshot index definition: %v", err)
		}
		definitions.WriteString(name)
		definitions.WriteByte('\x00')
		definitions.WriteString(ddl)
		definitions.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		tb.Fatalf("iterate snapshot index definitions: %v", err)
	}
	snapshot.IndexDDL = definitions.String()
	bytes, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read snapshot database bytes: %v", err)
	}
	snapshot.DatabaseSHA = fmt.Sprintf("%x", sha256.Sum256(bytes))
	return snapshot
}

func privateStageV27EdgeCandidateIndexes(tb testing.TB, store *Store) {
	privateStageV27EdgeCandidateIndexesWith(tb, store, map[string]string{
		"edges_by_from":           privateGenerationFirstEdgeIndexDDL["edges_by_from"],
		"edges_by_to":             privateGenerationFirstEdgeIndexDDL["edges_by_to"],
		"edges_by_from_line":      privateBaselineEdgeIndexDDL["edges_by_from_line"],
		"edges_by_from_line_kind": privateBaselineEdgeIndexDDL["edges_by_from_line_kind"],
	})
}

func privateStageV27EdgeCandidateIndexesWith(tb testing.TB, store *Store, definitions map[string]string) {
	tb.Helper()
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	tx, err := store.beginWrite()
	if err != nil {
		tb.Fatalf("begin v27 staging: %v", err)
	}
	for _, name := range privateEdgeIndexNames {
		if _, err := tx.Exec(`DROP INDEX ` + name); err != nil {
			_ = tx.Rollback()
			tb.Fatalf("drop staged %s: %v", name, err)
		}
		if _, err := tx.Exec(definitions[name]); err != nil {
			_ = tx.Rollback()
			tb.Fatalf("create staged %s: %v", name, err)
		}
	}
	if _, err := tx.Exec(`PRAGMA user_version = 27`); err != nil {
		_ = tx.Rollback()
		tb.Fatalf("stamp staged v27 store: %v", err)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit v27 staging: %v", err)
	}
}

func privateStampSchemaVersion(tb testing.TB, store *Store, version int) {
	tb.Helper()
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	tx, err := store.beginWrite()
	if err != nil {
		tb.Fatalf("begin schema version stamp: %v", err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		_ = tx.Rollback()
		tb.Fatalf("stamp schema version %d: %v", version, err)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit schema version stamp: %v", err)
	}
}

func privateMismatchedGenerationFirstEdgeCandidateIndexes(db *sql.DB) ([]string, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	mismatched, err := privateMismatchedGenerationFirstEdgeCandidateIndexesTx(tx)
	if rollbackErr := tx.Rollback(); rollbackErr != nil && err == nil {
		err = rollbackErr
	}
	return mismatched, err
}

func privateMismatchedGenerationFirstEdgeCandidateIndexesTx(tx *sql.Tx) ([]string, error) {
	want := map[string][]string{
		"edges_by_from":           {"view_gen", "from_id", "kind"},
		"edges_by_to":             {"view_gen", "to_id", "kind"},
		"edges_by_from_line":      {"view_gen", "from_id", "line"},
		"edges_by_from_line_kind": {"view_gen", "from_id", "line", "kind"},
	}
	var mismatched []string
	for _, name := range privateEdgeIndexNames {
		rows, err := tx.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, name)
		if err != nil {
			return nil, fmt.Errorf("read %s shape: %w", name, err)
		}
		var got []string
		for rows.Next() {
			var column string
			if err := rows.Scan(&column); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan %s shape: %w", name, err)
			}
			got = append(got, column)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read %s shape: %w", name, err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close %s shape: %w", name, err)
		}
		if len(got) != len(want[name]) {
			mismatched = append(mismatched, name)
			continue
		}
		for i := range got {
			if got[i] != want[name][i] {
				mismatched = append(mismatched, name)
				break
			}
		}
	}
	return mismatched, nil
}

func TestPrivateGenerationFirstEdgeIndexSiteCrossJoinCandidateParity(t *testing.T) {
	baseline, err := Open(filepath.Join(t.TempDir(), "baseline.db"))
	if err != nil {
		t.Fatalf("open baseline: %v", err)
	}
	defer baseline.Close()
	candidate, err := Open(filepath.Join(t.TempDir(), "candidate.db"))
	if err != nil {
		t.Fatalf("open candidate: %v", err)
	}
	defer candidate.Close()

	privateSeedEdgeIndexSemantics(t, baseline)
	privateSeedEdgeIndexSemantics(t, candidate)
	for _, store := range []*Store{baseline, candidate} {
		if err := store.AtGeneration(7).AddBatchChecked(nil, []*graph.Edge{
			privateIndexEdge("source-a", "target-site-second", graph.EdgeCalls, "site-second.go", 90, 7, "site-second"),
		}); err != nil {
			t.Fatalf("seed duplicate-site edge: %v", err)
		}
	}
	privateApplyEdgeIndexLayout(t, candidate, privateGenerationFirstEdgeIndexes)

	endpoints := []graph.EdgeEndpoint{
		{},
		{From: "source-a", To: "target-hot"},
		{From: "source-a", To: "target-hot"},
	}
	sites := []graph.EdgeSite{
		{},
		{From: "source-a", Line: 90, Kind: graph.EdgeCalls},
		{From: "source-a", Line: 90, Kind: graph.EdgeCalls},
		{From: "source-a", Line: 10},
		{From: "source-a", Line: 10},
	}

	if got := privateSiteCrossJoinCandidateDigest(t, baseline.AtGeneration(7).GetEdgeCandidates(nil, nil)); got != "" {
		t.Fatalf("baseline zero-key lookup=%q, want empty", got)
	}
	if got := privateSiteCrossJoinCandidateDigest(t, candidate.AtGeneration(7).GetEdgeCandidates(nil, nil)); got != "" {
		t.Fatalf("candidate zero-key lookup=%q, want empty", got)
	}
	baselineDigest := privateSiteCrossJoinCandidateDigest(t, baseline.AtGeneration(7).GetEdgeCandidates(endpoints, sites))
	candidateDigest := privateSiteCrossJoinCandidateDigest(t, candidate.AtGeneration(7).GetEdgeCandidates(endpoints, sites))
	if candidateDigest != baselineDigest {
		t.Fatalf("candidate duplicate-key/first-match digest=%s want baseline=%s", candidateDigest, baselineDigest)
	}
}

// TestPrivateGenerationFirstEndpointCrossJoinBatchPlanAndSemantics models the
// semantic provider's real 337-key endpoint batch. Generation zero is much
// larger than the active generation so ANALYZE has a reason to prefer a broad
// generation path if the VALUES relation is not the outer loop.
func TestPrivateGenerationFirstEndpointCrossJoinBatchPlanAndSemantics(t *testing.T) {
	const (
		endpointCount = 337
		baseNoise     = 4096
	)
	store, err := Open(filepath.Join(t.TempDir(), "endpoint-cross-join.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	endpoints := make([]graph.EdgeEndpoint, 0, endpointCount+1)
	for _, generation := range []int64{baseViewGeneration, 7, 19, 64} {
		edges := make([]*graph.Edge, 0, endpointCount+1)
		for index := 0; index < endpointCount; index++ {
			from := fmt.Sprintf("endpoint-%03d", index)
			to := fmt.Sprintf("target-%03d", index)
			edges = append(edges, privateIndexEdge(
				from, to, graph.EdgeCalls, fmt.Sprintf("g%d-%03d.go", generation, index), index+1, generation, "endpoint-call",
			))
			if generation == 7 {
				endpoints = append(endpoints, graph.EdgeEndpoint{From: from, To: to})
			}
		}
		// The first endpoint has two kinds. ORDER BY kind,id must retain Calls
		// as Endpoint's first match, while EndpointKind and Site reuse its pointer.
		edges = append(edges, privateIndexEdge("endpoint-000", "target-000", graph.EdgeReferences,
			fmt.Sprintf("g%d-reference.go", generation), 1, generation, "endpoint-reference"))
		if err := store.AtGeneration(generation).AddBatchChecked(nil, edges); err != nil {
			t.Fatalf("seed generation %d endpoints: %v", generation, err)
		}
	}
	lookupEndpoints := append([]graph.EdgeEndpoint(nil), endpoints...)
	lookupEndpoints = append(lookupEndpoints,
		endpoints[0], // caller duplicates must not change the SQL batch shape.
		graph.EdgeEndpoint{From: "missing", To: "target"},
	)
	noise := make([]*graph.Edge, 0, baseNoise)
	for index := 0; index < baseNoise; index++ {
		noise = append(noise, privateIndexEdge("base-noise", fmt.Sprintf("noise-%04d", index), graph.EdgeCalls,
			"base-noise.go", index+1, baseViewGeneration, "base-noise"))
	}
	if err := store.AddBatchChecked(nil, noise); err != nil {
		t.Fatalf("seed generation-zero skew: %v", err)
	}
	privateApplyEdgeIndexLayout(t, store, privateGenerationFirstEdgeIndexes)
	if _, err := store.writerDB.Exec(`ANALYZE`); err != nil {
		t.Fatalf("analyze skewed endpoint fixture: %v", err)
	}

	endpointArgs := func(count int) []any {
		args := make([]any, 0, count*2+1)
		for index := 0; index < count; index++ {
			endpoint := endpoints[index%len(endpoints)]
			args = append(args, endpoint.From, endpoint.To)
		}
		return append(args, int64(7))
	}
	args := endpointArgs(endpointCount)
	actualQuery := edgeCandidatesEndpointQuery(endpointCount)
	beforeQuery := strings.Replace(actualQuery, "CROSS JOIN edges AS e INDEXED BY sqlite_autoindex_edges_1 ON", "JOIN edges AS e ON", 1)
	plainGenerationQuery := strings.Replace(actualQuery, "WHERE +e.view_gen = ?", "WHERE e.view_gen = ?", 1)
	beforePlan := privateEdgeIndexPlan(t, store.db, beforeQuery, args...)
	plainGenerationPlan := privateEdgeIndexPlan(t, store.db, plainGenerationQuery, args...)
	actualPlan := privateEdgeIndexPlan(t, store.db, actualQuery, args...)
	t.Logf("endpoint batch=%d before=%s", endpointCount, beforePlan)
	t.Logf("endpoint batch=%d plain_generation=%s", endpointCount, plainGenerationPlan)
	t.Logf("endpoint batch=%d unary_cross_join=%s", endpointCount, actualPlan)
	assertEndpointPlan := func(label string, count int, plan string) {
		t.Helper()
		if !strings.Contains(plan, "SCAN w") || !strings.Contains(plan, "SEARCH e USING INDEX sqlite_autoindex_edges_1") ||
			!strings.Contains(plan, "from_id=? AND to_id=?") || privatePlanScansEdges(plan) ||
			strings.Contains(plan, "AUTOMATIC") || strings.Contains(plan, "edges_by_generation") || strings.Contains(plan, "edges_by_from") || strings.Contains(plan, "edges_by_to") {
			t.Fatalf("%s endpoint batch did not probe its full endpoint key:\n%s", label, plan)
		}
	}
	assertEndpointPlan("stack-shape", endpointCount, actualPlan)
	// GetEdgeCandidates chunks at lookupChunkSize, so a 5,001-key caller
	// executes these exact two SQL arities. They use repeated plan-only values
	// here; result semantics below exercise the real deduplicated 337-key call.
	for _, count := range []int{lookupChunkSize, 1} {
		plan := privateEdgeIndexPlan(t, store.db, edgeCandidatesEndpointQuery(count), endpointArgs(count)...)
		assertEndpointPlan(fmt.Sprintf("chunk-%d", count), count, plan)
	}

	queries := map[string]string{
		"before_unary_join": beforeQuery,
		"plain_generation":  plainGenerationQuery,
		"unary_cross_join":  actualQuery,
	}
	var wantDigest string
	for name, query := range queries {
		edges, err := store.queryEdgeCandidatesSQL(query, args...)
		if err != nil {
			t.Fatalf("%s endpoint query: %v", name, err)
		}
		digest := privateEdgeSliceDigest(t, edges)
		if wantDigest == "" {
			wantDigest = digest
		} else if digest != wantDigest {
			t.Fatalf("%s endpoint rows changed: %s want %s", name, digest, wantDigest)
		}
	}

	sites := []graph.EdgeSite{{From: "endpoint-000", Line: 1, Kind: graph.EdgeCalls}}
	for _, generation := range []int64{baseViewGeneration, 7, 19, 64} {
		set := store.AtGeneration(generation).GetEdgeCandidates(lookupEndpoints, sites)
		endpoint := set.Endpoint("endpoint-000", "target-000")
		if endpoint == nil || endpoint.Kind != graph.EdgeCalls || endpoint.FilePath != fmt.Sprintf("g%d-000.go", generation) {
			t.Fatalf("generation %d endpoint first match = %#v", generation, endpoint)
		}
		byKind := set.EndpointKind("endpoint-000", "target-000", graph.EdgeCalls)
		atSite := set.Site("endpoint-000", 1, graph.EdgeCalls)
		if byKind != endpoint || len(atSite) != 1 || atSite[0] != endpoint {
			t.Fatalf("generation %d lost canonical endpoint/site pointer: endpoint=%p kind=%p site=%#v", generation, endpoint, byKind, atSite)
		}
		if missing := set.Endpoint("missing", "target"); missing != nil {
			t.Fatalf("generation %d returned missing endpoint %#v", generation, missing)
		}
	}

	// 5,001 unique endpoint keys cross GetEdgeCandidates' 5,000-key boundary.
	// The first and final stored keys must survive the two physical queries;
	// blank and duplicate input still must not become query keys.
	last := graph.EdgeEndpoint{From: "endpoint-chunk-last", To: "target-chunk-last"}
	lastEdge := privateIndexEdge(last.From, last.To, graph.EdgeCalls, "chunk-last.go", 1, 7, "chunk-last")
	if err := store.AtGeneration(7).AddBatchChecked(nil, []*graph.Edge{lastEdge}); err != nil {
		t.Fatalf("seed chunk-boundary edge: %v", err)
	}
	chunked := []graph.EdgeEndpoint{{}, endpoints[0], endpoints[0]}
	for index := 0; index < 4_999; index++ {
		chunked = append(chunked, graph.EdgeEndpoint{From: fmt.Sprintf("missing-%04d", index), To: "target"})
	}
	chunked = append(chunked, last)
	chunkedSet := store.AtGeneration(7).GetEdgeCandidates(chunked, nil)
	firstOnly := store.AtGeneration(7).GetEdgeCandidates([]graph.EdgeEndpoint{endpoints[0]}, nil)
	lastOnly := store.AtGeneration(7).GetEdgeCandidates([]graph.EdgeEndpoint{last}, nil)
	gotFirst, gotLast := chunkedSet.Endpoint(endpoints[0].From, endpoints[0].To), chunkedSet.Endpoint(last.From, last.To)
	if gotFirst == nil || gotLast == nil || chunkedSet.Endpoint("missing-0000", "target") != nil {
		t.Fatalf("chunked endpoint result lost a boundary row or admitted missing input: first=%#v last=%#v", gotFirst, gotLast)
	}
	gotDigest := privateEdgeSliceDigest(t, []*graph.Edge{gotFirst, gotLast})
	expectedChunkDigest := privateEdgeSliceDigest(t, []*graph.Edge{
		firstOnly.Endpoint(endpoints[0].From, endpoints[0].To), lastOnly.Endpoint(last.From, last.To),
	})
	if gotDigest != expectedChunkDigest {
		t.Fatalf("chunked endpoint digest=%s want=%s", gotDigest, expectedChunkDigest)
	}
}

func TestPrivateGenerationFirstEdgeIndexOrdinaryReadsRemainGenerationScoped(t *testing.T) {
	baseline, err := Open(filepath.Join(t.TempDir(), "ordinary-baseline.db"))
	if err != nil {
		t.Fatalf("open baseline: %v", err)
	}
	defer baseline.Close()
	candidate, err := Open(filepath.Join(t.TempDir(), "ordinary-candidate.db"))
	if err != nil {
		t.Fatalf("open candidate: %v", err)
	}
	defer candidate.Close()
	privateSeedEdgeIndexSemantics(t, baseline)
	privateSeedEdgeIndexSemantics(t, candidate)
	privateApplyEdgeIndexLayout(t, candidate, privateGenerationFirstEdgeIndexes)

	for _, generation := range []int64{0, 7} {
		wantOut := privateEdgeSliceDigest(t, baseline.AtGeneration(generation).GetOutEdges("source-a"))
		gotOut := privateEdgeSliceDigest(t, candidate.AtGeneration(generation).GetOutEdges("source-a"))
		if gotOut != wantOut {
			t.Fatalf("generation %d GetOutEdges digest=%s want=%s", generation, gotOut, wantOut)
		}
		wantIn := privateEdgeSliceDigest(t, baseline.AtGeneration(generation).GetInEdges("target-hot"))
		gotIn := privateEdgeSliceDigest(t, candidate.AtGeneration(generation).GetInEdges("target-hot"))
		if gotIn != wantIn {
			t.Fatalf("generation %d GetInEdges digest=%s want=%s", generation, gotIn, wantIn)
		}
	}
}

func privateSiteCrossJoinCandidateDigest(tb testing.TB, set graph.EdgeCandidateSet) string {
	tb.Helper()
	endpoint := set.Endpoint("source-a", "target-hot")
	endpointKind := set.EndpointKind("source-a", "target-hot", graph.EdgeCalls)
	exact := set.Site("source-a", 90, graph.EdgeCalls)
	anyKind := set.Site("source-a", 10, "")
	if endpoint == nil && endpointKind == nil && len(exact) == 0 && len(anyKind) == 0 {
		return ""
	}
	if endpoint == nil || endpointKind == nil || len(exact) != 2 || len(anyKind) != 1 {
		tb.Fatalf("candidate result missing expected rows: endpoint=%v endpoint_kind=%v exact=%d any_kind=%d", endpoint != nil, endpointKind != nil, len(exact), len(anyKind))
	}
	if endpoint != endpointKind || endpoint != exact[0] {
		tb.Fatalf("candidate result did not preserve first-match canonical identity")
	}
	if marker, _ := endpoint.Meta["marker"].(string); marker != "first" {
		tb.Fatalf("candidate first endpoint=%q, want first", marker)
	}
	return privateEdgeSliceDigest(tb, []*graph.Edge{endpoint, endpointKind, exact[0], exact[1], anyKind[0]})
}

func TestPrivateGenerationFirstEdgeIndexBulkFixtureUsesProductionWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge-index-bulk-window.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	timing := privateInsertGeneratedEdgesBulk(t, store, 101, 0, 8, 4)
	if timing.Settings.ActiveAutoCheckpoint != 0 {
		t.Fatalf("active wal_autocheckpoint=%d, want 0", timing.Settings.ActiveAutoCheckpoint)
	}
	if timing.Settings.ActiveCacheBytes != 256<<20 {
		t.Fatalf("active cache bytes=%d, want %d", timing.Settings.ActiveCacheBytes, 256<<20)
	}
	var rows int
	if err := store.db.QueryRow(`SELECT count(*) FROM edges WHERE view_gen = ?`, 101).Scan(&rows); err != nil {
		t.Fatalf("count bulk rows: %v", err)
	}
	if rows != 8 {
		t.Fatalf("bulk row count=%d, want 8", rows)
	}
}

func TestPrivateGenerationFirstEdgeIndexQuiescesScheduledDrain(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "1")
	path := filepath.Join(t.TempDir(), "edge-index-quiescence.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store.passiveCheckpointTimeout = time.Nanosecond
	// Keep the window end on its inline PASSIVE (which the nanosecond budget
	// times out) instead of the large-backlog deferral, which measures the
	// residue from the wal-index and schedules the drain.
	previousMaxFrames := generationInlineCheckpointMaxFrames
	generationInlineCheckpointMaxFrames = math.MaxInt64
	t.Cleanup(func() { generationInlineCheckpointMaxFrames = previousMaxFrames })

	beforeRequests := store.walDrainRequests.Load()
	privateInsertGeneratedEdgesBulk(t, store, 201, 0, 25_000, 8_192)
	if requests := store.walDrainRequests.Load(); requests != beforeRequests {
		_ = store.Close()
		t.Fatalf("a timed-out End unexpectedly scheduled a result-based drain: before=%d after=%d", beforeRequests, requests)
	}
	walInfo, err := os.Stat(path + "-wal")
	if err != nil {
		_ = store.Close()
		t.Fatalf("stat timed-out End residue: %v", err)
	}
	if walInfo.Size() <= 32 {
		_ = store.Close()
		t.Fatalf("timed-out End left no meaningful WAL residue: bytes=%d", walInfo.Size())
	}
	var pageSize int64
	if err := store.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		_ = store.Close()
		t.Fatalf("read residue page size: %v", err)
	}
	residueFrames := (walInfo.Size() - 32) / (pageSize + 24)
	t.Logf("forced residue before Close: wal_bytes=%d wal_frames=%d pressure_pages=%d drain_requests=%d",
		walInfo.Size(), residueFrames, sqliteWALAutoCheckpointPages(), store.walDrainRequests.Load())
	observed := privateCloseAndVerifyWAL(t, "forced-residue", path, store)
	t.Logf("forced residue after Close: wal_bytes=%d mx_frame=%d n_backfill=%d drain_requests=%d drains=%d",
		observed.WALBytes, observed.MXFrame, observed.NBackfill, observed.DrainRequests, observed.Drains)
	if observed.MXFrame != observed.NBackfill || observed.Busy != 0 || observed.BackgroundActive ||
		observed.BackgroundLeasePresent || !observed.WriterIdle || !observed.CheckpointLoopStopped {
		t.Fatalf("store did not quiesce: %+v", observed)
	}
}

// BenchmarkPrivateGenerationFirstEdgeIndexWriteLocality is intentionally
// opt-in. It uses the production generation bulk window for prefill and target
// writes. The fixture grows until the four affected indexes together exceed
// the active writer cache; total database size alone is not used as evidence
// of cache pressure. Target rounds run in opposite order to expose run-order
// bias, and bulk-window finalization is timed separately from row insertion.
func BenchmarkPrivateGenerationFirstEdgeIndexWriteLocality(b *testing.B) {
	if os.Getenv("GORTEX_RUN_EDGE_INDEX_LOCALITY") != "1" {
		b.Skip("set GORTEX_RUN_EDGE_INDEX_LOCALITY=1 during an approved quiet window")
	}
	b.StopTimer()
	const (
		minimumRelevantIndexBytes = int64(320 << 20)
		maximumTempBytes          = int64(6 << 30)
		minimumFreeBytes          = int64(8 << 30)
		maximumPrefillRows        = 2_400_000
		minimumProjectionRows     = 300_000
		rowsPerGeneration         = 100_000
		targetRows                = 150_000
		targetPermutationSeed     = uint64(0x6a09e667f3bcc909)
		chunkRows                 = 8_192
	)

	dir := b.TempDir()
	privateRequireFreeBytes(b, dir, minimumFreeBytes)
	baselinePath := filepath.Join(dir, "baseline.db")
	candidatePath := filepath.Join(dir, "generation-first.db")
	baseline, err := Open(baselinePath)
	if err != nil {
		b.Fatalf("open baseline: %v", err)
	}
	defer func() {
		if baseline != nil {
			_ = baseline.Close()
		}
	}()
	candidate, err := Open(candidatePath)
	if err != nil {
		b.Fatalf("open candidate: %v", err)
	}
	defer func() {
		if candidate != nil {
			_ = candidate.Close()
		}
	}()
	privateApplyEdgeIndexLayoutTB(b, candidate, privateGenerationFirstEdgeIndexes)
	privateBenchmarkProgress(b, "setup", map[string]any{"baseline_path": baselinePath, "candidate_path": candidatePath})

	type sizingSample struct {
		Rows                        int              `json:"rows"`
		BaselineRelevantBytes       int64            `json:"baseline_relevant_bytes"`
		CandidateRelevantBytes      int64            `json:"candidate_relevant_bytes"`
		BaselineProjectedRows       int              `json:"baseline_projected_rows"`
		CandidateProjectedRows      int              `json:"candidate_projected_rows"`
		BaselineDatabaseBytes       int64            `json:"baseline_database_bytes"`
		CandidateDatabaseBytes      int64            `json:"candidate_database_bytes"`
		BaselineAffectedIndexBytes  map[string]int64 `json:"baseline_affected_index_bytes"`
		CandidateAffectedIndexBytes map[string]int64 `json:"candidate_affected_index_bytes"`
	}
	prefillRows := 0
	var baselineSizes, candidateSizes map[string]int64
	var sizingSamples []sizingSample
	for generation := int64(1); prefillRows < maximumPrefillRows; generation++ {
		privateInsertGeneratedEdgesBulk(b, baseline, generation, prefillRows, rowsPerGeneration, chunkRows)
		privateInsertGeneratedEdgesBulk(b, candidate, generation, prefillRows, rowsPerGeneration, chunkRows)
		prefillRows += rowsPerGeneration
		privateRequireTempCap(b, maximumTempBytes, baselinePath, candidatePath)
		baselineSizes = privateEdgeDBStatSizes(b, baseline)
		candidateSizes = privateEdgeDBStatSizes(b, candidate)
		baselineRelevant := privateRelevantEdgeIndexBytes(baselineSizes)
		candidateRelevant := privateRelevantEdgeIndexBytes(candidateSizes)
		sample := sizingSample{
			Rows:                        prefillRows,
			BaselineRelevantBytes:       baselineRelevant,
			CandidateRelevantBytes:      candidateRelevant,
			BaselineProjectedRows:       privateProjectedRows(minimumRelevantIndexBytes, baselineRelevant, prefillRows),
			CandidateProjectedRows:      privateProjectedRows(minimumRelevantIndexBytes, candidateRelevant, prefillRows),
			BaselineDatabaseBytes:       privateSQLiteWorkingSetBytes(b, baseline),
			CandidateDatabaseBytes:      privateSQLiteWorkingSetBytes(b, candidate),
			BaselineAffectedIndexBytes:  privateAffectedEdgeIndexSizes(baselineSizes),
			CandidateAffectedIndexBytes: privateAffectedEdgeIndexSizes(candidateSizes),
		}
		sizingSamples = append(sizingSamples, sample)
		privateBenchmarkProgress(b, "sizing_sample", sample)
		b.Logf("edge-index sizing sample=%s", privateJSON(b, sample))
		if baselineRelevant >= minimumRelevantIndexBytes && candidateRelevant >= minimumRelevantIndexBytes {
			break
		}
		if prefillRows >= minimumProjectionRows &&
			(sample.BaselineProjectedRows > maximumPrefillRows || sample.CandidateProjectedRows > maximumPrefillRows) {
			b.Fatalf("measured affected-index growth projects beyond %d-row bound: samples=%s", maximumPrefillRows, privateJSON(b, sizingSamples))
		}
	}
	if privateRelevantEdgeIndexBytes(baselineSizes) < minimumRelevantIndexBytes ||
		privateRelevantEdgeIndexBytes(candidateSizes) < minimumRelevantIndexBytes {
		b.Fatalf("fixture did not establish %d relevant-index bytes per store by %d rows: baseline=%s candidate=%s",
			minimumRelevantIndexBytes, prefillRows, privateJSON(b, baselineSizes), privateJSON(b, candidateSizes))
	}
	if got, want := privateEdgeRowsDigestTB(b, candidate), privateEdgeRowsDigestTB(b, baseline); got != want {
		b.Fatalf("prefill digest differs: candidate=%s baseline=%s", got, want)
	}

	type measurement struct {
		Round      int                        `json:"round"`
		Variant    string                     `json:"variant"`
		Generation int64                      `json:"generation"`
		Timing     privateBulkTiming          `json:"timing"`
		Quiescence privateWALCloseObservation `json:"quiescence"`
	}
	prefillQuiescence := []privateWALCloseObservation{
		privateCloseAndVerifyWAL(b, "prefill/baseline", baselinePath, baseline),
		privateCloseAndVerifyWAL(b, "prefill/candidate", candidatePath, candidate),
	}
	baseline = nil
	candidate = nil
	privateBenchmarkProgress(b, "prefill_quiescent", prefillQuiescence)

	var results []measurement
	run := func(round int, variant, path string, generation int64, start int) {
		privateBenchmarkProgress(b, "target_start", map[string]any{"round": round, "variant": variant, "generation": generation, "permutation_seed": targetPermutationSeed})
		store, err := Open(path)
		if err != nil {
			b.Fatalf("round %d open %s: %v", round, variant, err)
		}
		timing := privateInsertGeneratedEdgesBulkPermuted(b, store, generation, start, targetRows, chunkRows, targetPermutationSeed)
		result := measurement{Round: round, Variant: variant, Generation: generation, Timing: timing}
		privateBenchmarkProgress(b, "target_write_complete", result)
		result.Quiescence = privateCloseAndVerifyWAL(b, fmt.Sprintf("round-%d-%s", round, variant), path, store)
		results = append(results, result)
		privateRequireTempCap(b, maximumTempBytes, baselinePath, candidatePath)
		privateBenchmarkProgress(b, "target_complete", result)
	}

	// Counterbalanced order. Each target store is closed, its checkpoint loop is
	// joined, and its WAL is proven backfilled before the next store is opened.
	run(1, "baseline", baselinePath, 10_000, prefillRows)
	run(1, "generation-first", candidatePath, 10_000, prefillRows)
	if got, want := privateClosedStoreDigest(b, candidatePath), privateClosedStoreDigest(b, baselinePath); got != want {
		b.Fatalf("round-one digest differs: candidate=%s baseline=%s", got, want)
	} else {
		privateBenchmarkProgress(b, "digest", map[string]any{"round": 1, "sha256": got})
	}
	run(2, "generation-first", candidatePath, 10_001, prefillRows+targetRows)
	run(2, "baseline", baselinePath, 10_001, prefillRows+targetRows)
	if got, want := privateClosedStoreDigest(b, candidatePath), privateClosedStoreDigest(b, baselinePath); got != want {
		b.Fatalf("round-two digest differs: candidate=%s baseline=%s", got, want)
	} else {
		privateBenchmarkProgress(b, "digest", map[string]any{"round": 2, "sha256": got})
	}

	const readProbeRows = 5_000
	readProbeStart := prefillRows + targetRows
	readResults := []privateEdgeReadObservation{
		privateMeasureEdgeCandidateReads(b, "read-round-1-baseline", baselinePath, 10_001, readProbeStart, readProbeRows),
		privateMeasureEdgeCandidateReads(b, "read-round-1-generation-first", candidatePath, 10_001, readProbeStart, readProbeRows),
		privateMeasureEdgeCandidateReads(b, "read-round-2-generation-first", candidatePath, 10_001, readProbeStart, readProbeRows),
		privateMeasureEdgeCandidateReads(b, "read-round-2-baseline", baselinePath, 10_001, readProbeStart, readProbeRows),
	}
	for i := 1; i < len(readResults); i++ {
		if readResults[i].FreshDigest != readResults[0].FreshDigest || readResults[i].WarmDigest != readResults[0].WarmDigest {
			b.Fatalf("read-arm result digest differs: first=%+v current=%+v", readResults[0], readResults[i])
		}
	}
	privateBenchmarkProgress(b, "read_arm_complete", readResults)

	baselineSizes, baselineDBBytes := privateClosedStoreSizes(b, baselinePath)
	candidateSizes, candidateDBBytes := privateClosedStoreSizes(b, candidatePath)
	b.Logf("generation-first edge-index diagnostic prefill_rows=%d target_rows=%d baseline_db_bytes=%d candidate_db_bytes=%d baseline_dbstat=%s candidate_dbstat=%s write_results=%s read_results=%s",
		prefillRows, targetRows, baselineDBBytes, candidateDBBytes,
		privateJSON(b, baselineSizes), privateJSON(b, candidateSizes), privateJSON(b, results), privateJSON(b, readResults))
	b.Logf("generation-first edge-index sizing_samples=%s", privateJSON(b, sizingSamples))
	privateBenchmarkProgress(b, "complete", map[string]any{
		"prefill_rows": prefillRows, "target_rows": targetRows, "baseline_dbstat": baselineSizes,
		"candidate_dbstat": candidateSizes, "write_results": results, "read_results": readResults,
		"target_permutation_seed": targetPermutationSeed,
	})
}

type privateEdgeReadObservation struct {
	Label             string                     `json:"label"`
	Generation        int64                      `json:"generation"`
	ProbeRows         int                        `json:"probe_rows"`
	ProductionPlans   map[string]string          `json:"production_plans"`
	DiagnosticPlans   map[string]string          `json:"diagnostic_plans"`
	FreshElapsedNanos int64                      `json:"fresh_elapsed_ns"`
	WarmElapsedNanos  int64                      `json:"warm_elapsed_ns"`
	FreshDigest       string                     `json:"fresh_digest"`
	WarmDigest        string                     `json:"warm_digest"`
	Quiescence        privateWALCloseObservation `json:"quiescence"`
}

func privateMeasureEdgeCandidateReads(tb testing.TB, label, path string, generation int64, start, count int) privateEdgeReadObservation {
	tb.Helper()
	store, err := Open(path)
	if err != nil {
		tb.Fatalf("%s open read-arm store: %v", label, err)
	}
	endpoints, sites, expected := privateGeneratedEdgeReadProbe(start, count, generation)
	endpointArgs := make([]any, 0, len(endpoints)*2+1)
	for _, key := range endpoints {
		endpointArgs = append(endpointArgs, key.From, key.To)
	}
	endpointArgs = append(endpointArgs, generation)
	exactArgs := make([]any, 0, count*3+1)
	anyArgs := make([]any, 0, count*2+1)
	for i := 0; i < len(sites); i += 2 {
		exactArgs = append(exactArgs, sites[i].From, sites[i].Line, string(sites[i].Kind))
		anyArgs = append(anyArgs, sites[i+1].From, sites[i+1].Line)
	}
	exactArgs = append(exactArgs, generation)
	anyArgs = append(anyArgs, generation)
	observation := privateEdgeReadObservation{
		Label: label, Generation: generation, ProbeRows: count,
		ProductionPlans: map[string]string{
			"endpoint_unary_generation": privateEdgeIndexPlanTB(tb, store.db, edgeCandidatesEndpointQuery(len(endpoints)), endpointArgs...),
			"exact_site":                privateEdgeIndexPlanTB(tb, store.db, edgeCandidatesExactSiteQuery(count), exactArgs...),
			"any_site":                  privateEdgeIndexPlanTB(tb, store.db, edgeCandidatesAnySiteQuery(count), anyArgs...),
		},
		DiagnosticPlans: map[string]string{
			"endpoint_no_unary":              privateEdgeIndexPlanTB(tb, store.db, privateEndpointQueryWithoutUnaryGeneration(len(endpoints)), endpointArgs...),
			"endpoint_generation_index_hint": privateEdgeIndexPlanTB(tb, store.db, privateEndpointQueryWithGenerationIndexHint(len(endpoints)), endpointArgs...),
		},
	}
	privateAssertReadArmPlanShape(tb, label, observation.ProductionPlans)
	for name, plan := range observation.ProductionPlans {
		if privatePlanScansEdges(plan) {
			_ = store.Close()
			tb.Fatalf("%s production %s plan scans edges: %s", label, name, plan)
		}
	}
	view := store.AtGeneration(generation)
	started := time.Now()
	fresh := view.GetEdgeCandidates(endpoints, sites)
	observation.FreshElapsedNanos = time.Since(started).Nanoseconds()
	observation.FreshDigest = privateEdgeCandidateSetDigest(tb, fresh, expected)
	started = time.Now()
	warm := view.GetEdgeCandidates(endpoints, sites)
	observation.WarmElapsedNanos = time.Since(started).Nanoseconds()
	observation.WarmDigest = privateEdgeCandidateSetDigest(tb, warm, expected)
	if observation.FreshDigest != observation.WarmDigest {
		_ = store.Close()
		tb.Fatalf("%s fresh/warm candidate digest differs: fresh=%s warm=%s", label, observation.FreshDigest, observation.WarmDigest)
	}
	observation.Quiescence = privateCloseAndVerifyWAL(tb, label, path, store)
	privateBenchmarkProgress(tb, "read_arm_variant", observation)
	return observation
}

func privateAssertReadArmPlanShape(tb testing.TB, label string, plans map[string]string) {
	tb.Helper()
	for _, name := range []string{"exact_site", "any_site"} {
		plan := plans[name]
		if !strings.Contains(plan, "SCAN w") || !strings.Contains(plan, "SEARCH e USING INDEX") {
			tb.Fatalf("%s production %s must probe edges once per wanted key: %s", label, name, plan)
		}
	}
	if !strings.Contains(plans["exact_site"], "edges_by_from_line_kind") {
		tb.Fatalf("%s production exact-site did not use kind index: %s", label, plans["exact_site"])
	}
	if !strings.Contains(plans["any_site"], "edges_by_from_line") {
		tb.Fatalf("%s production any-site did not use line index: %s", label, plans["any_site"])
	}
}

func privateGeneratedEdgeReadProbe(start, count int, generation int64) ([]graph.EdgeEndpoint, []graph.EdgeSite, []*graph.Edge) {
	endpoints := make([]graph.EdgeEndpoint, 0, count)
	sites := make([]graph.EdgeSite, 0, count*2)
	expected := make([]*graph.Edge, 0, count)
	payload := strings.Repeat("x", 192)
	for i := 0; i < count; i++ {
		edge := privateGeneratedEdge(start+i, generation, payload)
		expected = append(expected, edge)
		endpoints = append(endpoints, graph.EdgeEndpoint{From: edge.From, To: edge.To})
		sites = append(sites,
			graph.EdgeSite{From: edge.From, Line: edge.Line, Kind: edge.Kind},
			graph.EdgeSite{From: edge.From, Line: edge.Line},
		)
	}
	return endpoints, sites, expected
}

func privateEdgeCandidateSetDigest(tb testing.TB, set graph.EdgeCandidateSet, expected []*graph.Edge) string {
	tb.Helper()
	h := sha256.New()
	for _, want := range expected {
		endpoint := set.Endpoint(want.From, want.To)
		endpointKind := set.EndpointKind(want.From, want.To, want.Kind)
		site := set.Site(want.From, want.Line, want.Kind)
		if endpoint == nil || endpointKind == nil || len(site) != 1 {
			tb.Fatalf("candidate set missing expected edge %s->%s kind=%s line=%d: endpoint=%v endpointKind=%v site=%d",
				want.From, want.To, want.Kind, want.Line, endpoint != nil, endpointKind != nil, len(site))
		}
		if endpoint != endpointKind || endpoint != site[0] {
			tb.Fatalf("candidate set did not canonicalize pointers for %s->%s kind=%s line=%d", want.From, want.To, want.Kind, want.Line)
		}
		_, _ = fmt.Fprintln(h, privateEdgeSliceDigest(tb, []*graph.Edge{endpoint}))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func privateEdgeIndexPlanTB(tb testing.TB, db *sql.DB, query string, args ...any) string {
	tb.Helper()
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		tb.Fatalf("explain read-arm query: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			tb.Fatalf("scan read-arm plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		tb.Fatalf("read read-arm plan: %v", err)
	}
	return strings.Join(details, " | ")
}

func privateBenchmarkProgress(tb testing.TB, event string, payload any) {
	tb.Helper()
	path := os.Getenv("GORTEX_EDGE_INDEX_PROGRESS_PATH")
	if path == "" {
		return
	}
	record := map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "event": event, "payload": payload}
	encoded, err := json.Marshal(record)
	if err != nil {
		tb.Fatalf("encode benchmark progress: %v", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		tb.Fatalf("open benchmark progress: %v", err)
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		tb.Fatalf("write benchmark progress: %v", err)
	}
	if err := file.Close(); err != nil {
		tb.Fatalf("close benchmark progress: %v", err)
	}
}

func privateSeedEdgeIndexSemantics(t *testing.T, store *Store) {
	t.Helper()
	for _, generation := range []int64{0, 7, 64} {
		edges := []*graph.Edge{
			privateIndexEdge("source-a", "target-hot", graph.EdgeCalls, "zzz.go", 90, generation, "first"),
			privateIndexEdge("source-a", "target-hot", graph.EdgeCalls, "aaa.go", 10, generation, "second"),
			privateIndexEdge("source-a", "target-hot", graph.EdgeReferences, "ref.go", 50, generation, "other-kind"),
			privateIndexEdge("source-b", "target-hot", graph.EdgeImports, "b.go", 7, generation, "fan-in"),
			privateIndexEdge("rare-source", "rare-target", graph.EdgeProvides, "rare.go", 1, generation, "rare"),
		}
		if err := store.AtGeneration(generation).AddBatchChecked(nil, edges); err != nil {
			t.Fatalf("seed generation %d: %v", generation, err)
		}
	}
}

func privateIndexEdge(from, to string, kind graph.EdgeKind, file string, line int, generation int64, marker string) *graph.Edge {
	return &graph.Edge{
		From:            from,
		To:              to,
		Kind:            kind,
		FilePath:        file,
		Line:            line,
		Confidence:      0.625,
		ConfidenceLabel: "medium",
		Origin:          "generation-first-private",
		Tier:            "exact",
		CrossRepo:       marker == "fan-in",
		Meta: map[string]any{
			"marker":     marker,
			"generation": fmt.Sprintf("%d", generation),
		},
	}
}

func privateApplyEdgeIndexLayout(t *testing.T, store *Store, layout privateEdgeIndexLayout) {
	t.Helper()
	privateApplyEdgeIndexLayoutTB(t, store, layout)
}

func privateApplyEdgeIndexLayoutTB(tb testing.TB, store *Store, layout privateEdgeIndexLayout) {
	tb.Helper()
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	tx, err := store.beginWrite()
	if err != nil {
		tb.Fatalf("begin index migration: %v", err)
	}
	if err := privateReplaceEdgeIndexLayoutTx(tx, layout); err != nil {
		_ = tx.Rollback()
		tb.Fatalf("replace edge indexes: %v", err)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit edge index migration: %v", err)
	}
}

func privateReplaceEdgeIndexLayoutTx(tx *sql.Tx, layout privateEdgeIndexLayout) error {
	definitions := privateBaselineEdgeIndexDDL
	if layout == privateGenerationFirstEdgeIndexes {
		definitions = privateGenerationFirstEdgeIndexDDL
	}
	for _, name := range privateEdgeIndexNames {
		if _, err := tx.Exec(`DROP INDEX ` + name); err != nil {
			return fmt.Errorf("drop %s: %w", name, err)
		}
	}
	for _, name := range privateEdgeIndexNames {
		if _, err := tx.Exec(definitions[name]); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
	}
	return nil
}

func privateAssertEdgeIndexLayout(t *testing.T, db *sql.DB, layout privateEdgeIndexLayout) {
	t.Helper()
	want := privateBaselineEdgeIndexDDL
	if layout == privateGenerationFirstEdgeIndexes {
		want = privateGenerationFirstEdgeIndexDDL
	}
	for _, name := range privateEdgeIndexNames {
		var sqlText string
		if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&sqlText); err != nil {
			t.Fatalf("read %s definition: %v", name, err)
		}
		if privateNormalizeSQL(sqlText) != privateNormalizeSQL(want[name]) {
			t.Fatalf("%s definition=%q want=%q", name, sqlText, want[name])
		}
	}
}

func privateNormalizeSQL(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

func privateAssertEdgeReaderPlans(t *testing.T, db *sql.DB, layout privateEdgeIndexLayout) {
	t.Helper()
	forced := []struct {
		name  string
		index string
		query string
		args  []any
	}{
		{name: "incoming", index: "edges_by_to", query: `SELECT id FROM edges INDEXED BY edges_by_to WHERE view_gen=? AND to_id=?`, args: []any{int64(7), "target-hot"}},
		{name: "outgoing", index: "edges_by_from", query: `SELECT id FROM edges INDEXED BY edges_by_from WHERE view_gen=? AND from_id=?`, args: []any{int64(7), "source-a"}},
		{name: "any-site", index: "edges_by_from_line", query: `SELECT id FROM edges INDEXED BY edges_by_from_line WHERE view_gen=? AND from_id=? AND line=?`, args: []any{int64(7), "source-a", 90}},
		{name: "exact-site", index: "edges_by_from_line_kind", query: `SELECT id FROM edges INDEXED BY edges_by_from_line_kind WHERE view_gen=? AND from_id=? AND line=? AND kind=?`, args: []any{int64(7), "source-a", 90, string(graph.EdgeCalls)}},
	}
	for _, tc := range forced {
		plan := privateEdgeIndexPlan(t, db, tc.query, tc.args...)
		if !strings.Contains(plan, tc.index) {
			t.Fatalf("%s plan does not use %s:\n%s", tc.name, tc.index, plan)
		}
		if layout == privateGenerationFirstEdgeIndexes && !strings.Contains(plan, "view_gen=?") {
			t.Fatalf("%s generation-first plan does not constrain leading view_gen:\n%s", tc.name, plan)
		}
		t.Logf("layout=%d forced-reader=%s plan=%s", layout, tc.name, plan)
	}

	production := []struct {
		name  string
		query string
		args  []any
	}{
		{name: "endpoint-unary-generation", query: edgeCandidatesEndpointQuery(1), args: []any{"source-a", "target-hot", int64(7)}},
		{name: "exact-site", query: edgeCandidatesExactSiteQuery(1), args: []any{"source-a", 90, string(graph.EdgeCalls), int64(7)}},
		{name: "any-site", query: edgeCandidatesAnySiteQuery(1), args: []any{"source-a", 90, int64(7)}},
	}
	plans := make(map[string]string, len(production)+2)
	for _, tc := range production {
		plan := privateEdgeIndexPlan(t, db, tc.query, tc.args...)
		if privatePlanScansEdges(plan) {
			t.Fatalf("layout=%d exact production %s performs a full edge scan:\n%s", layout, tc.name, plan)
		}
		plans[tc.name] = plan
	}

	// These endpoint variants are diagnostic only. The timed read arm always
	// executes edgeCandidatesEndpointQuery unchanged, including unary +view_gen.
	plans["endpoint-no-unary-diagnostic"] = privateEdgeIndexPlan(t, db,
		privateEndpointQueryWithoutUnaryGeneration(1), "source-a", "target-hot", int64(7))
	plans["endpoint-generation-index-hint-diagnostic"] = privateEdgeIndexPlan(t, db,
		privateEndpointQueryWithGenerationIndexHint(1), "source-a", "target-hot", int64(7))

	if layout == privateGenerationFirstEdgeIndexes {
		endpointPlan := plans["endpoint-unary-generation"]
		if strings.Contains(endpointPlan, "edges_by_from (view_gen=?)") {
			t.Fatalf("unary generation predicate unexpectedly constrained the leading generation-first column:\n%s", endpointPlan)
		}
		if !strings.Contains(endpointPlan, "sqlite_autoindex_edges_1") {
			t.Fatalf("exact endpoint query did not retain the logical endpoint-key plan with unary generation:\n%s", endpointPlan)
		}
		if !strings.Contains(plans["exact-site"], "edges_by_from_line_kind") ||
			!strings.Contains(plans["any-site"], "edges_by_from_line") {
			t.Fatalf("generation-first site plans did not use the intended indexes: %#v", plans)
		}
	}
	for name, plan := range plans {
		t.Logf("layout=%d production-or-diagnostic-reader=%s plan=%s", layout, name, plan)
	}
}

func privatePlanScansEdges(plan string) bool {
	return strings.Contains(plan, "SCAN edges") || strings.Contains(plan, "SCAN e ") || strings.HasSuffix(plan, "SCAN e")
}

func privateEndpointQueryWithoutUnaryGeneration(pairs int) string {
	return strings.Replace(edgeCandidatesEndpointQuery(pairs), "WHERE +e.view_gen = ?", "WHERE e.view_gen = ?", 1)
}

func privateEndpointQueryWithGenerationIndexHint(pairs int) string {
	query := privateEndpointQueryWithoutUnaryGeneration(pairs)
	return strings.Replace(query, "JOIN edges AS e ON", "JOIN edges AS e INDEXED BY edges_by_from ON", 1)
}

func privateProductionEdgeLookupDigest(t *testing.T, store *Store) string {
	t.Helper()
	type lookup struct {
		name  string
		query string
		args  []any
	}
	lookups := []lookup{
		{name: "endpoint", query: edgeCandidatesEndpointQuery(1), args: []any{"source-a", "target-hot", int64(7)}},
		{name: "exact-site", query: edgeCandidatesExactSiteQuery(1), args: []any{"source-a", 90, string(graph.EdgeCalls), int64(7)}},
		{name: "any-site", query: edgeCandidatesAnySiteQuery(1), args: []any{"source-a", 90, int64(7)}},
	}
	h := sha256.New()
	for _, lookup := range lookups {
		edges, err := store.queryEdgeCandidatesSQL(lookup.query, lookup.args...)
		if err != nil {
			t.Fatalf("query production %s lookup: %v", lookup.name, err)
		}
		_, _ = fmt.Fprintf(h, "%s\x00%s\n", lookup.name, privateEdgeSliceDigest(t, edges))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func privateEdgeSliceDigest(tb testing.TB, edges []*graph.Edge) string {
	tb.Helper()
	h := sha256.New()
	for _, edge := range edges {
		if edge == nil {
			_, _ = fmt.Fprintln(h, "<nil>")
			continue
		}
		meta, err := json.Marshal(edge.Meta)
		if err != nil {
			tb.Fatalf("encode edge metadata: %v", err)
		}
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%d\x00%g\x00%s\x00%s\x00%s\x00%t\x00%s\n",
			edge.From, edge.To, edge.Kind, edge.FilePath, edge.Line, edge.Confidence,
			edge.ConfidenceLabel, edge.Origin, edge.Tier, edge.CrossRepo, meta)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func privateEdgeIndexPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	return strings.Join(details, " | ")
}

func privateEdgeRowsDigest(t *testing.T, store *Store) string {
	t.Helper()
	return privateEdgeRowsDigestTB(t, store)
}

func privateAssertEdgeRowsDigest(t *testing.T, store *Store, want string) {
	t.Helper()
	if got := privateEdgeRowsDigest(t, store); got != want {
		t.Fatalf("ordered edge digest=%s want=%s", got, want)
	}
}

func privateEdgeRowsDigestTB(tb testing.TB, store *Store) string {
	tb.Helper()
	rows, err := store.db.Query(`SELECT id,from_id,to_id,kind,file_path,line,confidence,confidence_label,origin,tier,cross_repo,view_gen,hex(meta),coalesce(resolve_terminal,-1),coalesce(resolve_terminal_reason,''),coalesce(semantic_source,'') FROM edges ORDER BY id`)
	if err != nil {
		tb.Fatalf("query ordered edge rows: %v", err)
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var values [16]any
		var id, line, crossRepo, generation, resolveTerminal int64
		var confidence float64
		var from, to, kind, file, confidenceLabel, origin, tier, metaHex, reason, semantic string
		values = [16]any{&id, &from, &to, &kind, &file, &line, &confidence, &confidenceLabel, &origin, &tier, &crossRepo, &generation, &metaHex, &resolveTerminal, &reason, &semantic}
		if err := rows.Scan(values[:]...); err != nil {
			tb.Fatalf("scan ordered edge row: %v", err)
		}
		_, _ = fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s\x00%s\x00%d\x00%g\x00%s\x00%s\x00%s\x00%d\x00%d\x00%s\x00%d\x00%s\x00%s\n",
			id, from, to, kind, file, line, confidence, confidenceLabel, origin, tier, crossRepo, generation, metaHex, resolveTerminal, reason, semantic)
	}
	if err := rows.Err(); err != nil {
		tb.Fatalf("read ordered edge rows: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func privateInsertGeneratedEdges(tb testing.TB, store *Store, generation int64, start, count, chunk int) {
	tb.Helper()
	for offset := 0; offset < count; offset += chunk {
		n := chunk
		if remaining := count - offset; remaining < n {
			n = remaining
		}
		edges := privateGeneratedEdges(start+offset, n, generation)
		if err := store.AtGeneration(generation).AddBatchChecked(nil, edges); err != nil {
			tb.Fatalf("insert generation %d rows [%d,%d): %v", generation, start+offset, start+offset+n, err)
		}
	}
}

func privateInsertGeneratedEdgesPermuted(tb testing.TB, store *Store, generation int64, start, count, chunk int, seed uint64) {
	tb.Helper()
	order := privateDeterministicPermutation(count, seed)
	payload := strings.Repeat("x", 192)
	for offset := 0; offset < count; offset += chunk {
		end := min(offset+chunk, count)
		edges := make([]*graph.Edge, end-offset)
		for i, position := range order[offset:end] {
			edges[i] = privateGeneratedEdge(start+position, generation, payload)
		}
		if err := store.AtGeneration(generation).AddBatchChecked(nil, edges); err != nil {
			tb.Fatalf("insert permuted generation %d rows [%d,%d): %v", generation, offset, end, err)
		}
	}
}

func privateDeterministicPermutation(count int, seed uint64) []int {
	order := make([]int, count)
	for i := range order {
		order[i] = i
	}
	state := seed
	next := func() uint64 {
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		return z ^ (z >> 31)
	}
	for i := len(order) - 1; i > 0; i-- {
		j := int(next() % uint64(i+1))
		order[i], order[j] = order[j], order[i]
	}
	return order
}

type privateWALCloseObservation struct {
	Label                  string `json:"label"`
	CloseElapsedNanos      int64  `json:"close_elapsed_ns"`
	Busy                   int64  `json:"busy"`
	MXFrame                int64  `json:"mx_frame"`
	NBackfill              int64  `json:"n_backfill"`
	WALBytes               int64  `json:"wal_bytes"`
	DrainRequests          int64  `json:"drain_requests"`
	Drains                 int64  `json:"drains"`
	BackgroundActive       bool   `json:"background_active"`
	BackgroundLeasePresent bool   `json:"background_lease_present"`
	WriterIdle             bool   `json:"writer_idle"`
	CheckpointLoopStopped  bool   `json:"checkpoint_loop_stopped"`
}

func privateCloseAndVerifyWAL(tb testing.TB, label, path string, store *Store) privateWALCloseObservation {
	tb.Helper()
	observed := privateWALCloseObservation{Label: label}
	started := time.Now()
	if err := store.Close(); err != nil {
		tb.Fatalf("%s close store: %v", label, err)
	}
	observed.CloseElapsedNanos = time.Since(started).Nanoseconds()
	observed.DrainRequests = store.walDrainRequests.Load()
	observed.Drains = store.walDrains.Load()
	coordination := &store.backgroundCheckpoint
	coordination.mu.Lock()
	observed.BackgroundActive = coordination.active != nil
	observed.BackgroundLeasePresent = coordination.generationLease != 0
	coordination.mu.Unlock()
	store.writeMu.Lock()
	observed.WriterIdle = store.generationBulkLoad == 0 && store.bulkConn == nil
	store.writeMu.Unlock()
	select {
	case <-store.checkpointDone:
		observed.CheckpointLoopStopped = true
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	checkpointDB, err := sql.Open("sqlite", sqliteCheckpointDSN(path))
	if err != nil {
		tb.Fatalf("%s open post-close checkpoint connection: %v", label, err)
	}
	configureWriterPool(checkpointDB)
	if err := checkpointDB.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&observed.Busy, &observed.MXFrame, &observed.NBackfill); err != nil {
		_ = checkpointDB.Close()
		tb.Fatalf("%s inspect post-close WAL: %v", label, err)
	}
	if err := checkpointDB.Close(); err != nil {
		tb.Fatalf("%s close post-close checkpoint connection: %v", label, err)
	}
	if info, err := os.Stat(path + "-wal"); err == nil {
		observed.WALBytes = info.Size()
	} else if !os.IsNotExist(err) {
		tb.Fatalf("%s stat post-close WAL: %v", label, err)
	}
	if observed.Busy != 0 || observed.MXFrame != observed.NBackfill || observed.BackgroundActive ||
		observed.BackgroundLeasePresent || !observed.WriterIdle || !observed.CheckpointLoopStopped {
		tb.Fatalf("%s did not reach quiescence: %+v", label, observed)
	}
	privateBenchmarkProgress(tb, "wal_quiescent", observed)
	return observed
}

func privateClosedStoreDigest(tb testing.TB, path string) string {
	tb.Helper()
	store, err := Open(path)
	if err != nil {
		tb.Fatalf("open closed store for digest: %v", err)
	}
	digest := privateEdgeRowsDigestTB(tb, store)
	privateCloseAndVerifyWAL(tb, "digest", path, store)
	return digest
}

func privateClosedStoreSizes(tb testing.TB, path string) (map[string]int64, int64) {
	tb.Helper()
	store, err := Open(path)
	if err != nil {
		tb.Fatalf("open closed store for dbstat: %v", err)
	}
	sizes := privateEdgeDBStatSizes(tb, store)
	databaseBytes := privateSQLiteWorkingSetBytes(tb, store)
	privateCloseAndVerifyWAL(tb, "dbstat", path, store)
	return sizes, databaseBytes
}

type privateBulkSettings struct {
	ActiveCacheSetting     int64 `json:"active_cache_setting"`
	ActiveCacheBytes       int64 `json:"active_cache_bytes"`
	ActiveAutoCheckpoint   int64 `json:"active_wal_autocheckpoint"`
	PreviousCacheSetting   int64 `json:"previous_cache_setting"`
	PreviousAutoCheckpoint int64 `json:"previous_wal_autocheckpoint"`
	RestoredCacheSetting   int64 `json:"restored_cache_setting"`
	RestoredAutoCheckpoint int64 `json:"restored_wal_autocheckpoint"`
}

type privateBulkTiming struct {
	BeginNanos  int64               `json:"begin_ns"`
	InsertNanos int64               `json:"insert_ns"`
	EndNanos    int64               `json:"end_ns"`
	TotalNanos  int64               `json:"total_ns"`
	Settings    privateBulkSettings `json:"settings"`
}

func privateInsertGeneratedEdgesBulk(tb testing.TB, store *Store, generation int64, start, count, chunk int) privateBulkTiming {
	tb.Helper()
	totalStarted := time.Now()
	beginStarted := time.Now()
	opened, err := store.BeginGenerationBulkLoad(generation)
	beginElapsed := time.Since(beginStarted)
	if err != nil {
		tb.Fatalf("begin generation %d bulk load: %v", generation, err)
	}
	if !opened {
		tb.Fatalf("generation %d did not acquire a physical bulk window", generation)
	}
	ended := false
	defer func() {
		if !ended {
			_ = store.EndGenerationBulkLoadFor(generation)
		}
	}()

	settings := privateActiveBulkSettings(tb, store, generation)
	if settings.ActiveCacheSetting != int64(bulkCacheSizeKiB) {
		tb.Fatalf("generation %d active cache_size=%d, want bulk setting %d", generation, settings.ActiveCacheSetting, bulkCacheSizeKiB)
	}
	if settings.ActiveAutoCheckpoint != 0 {
		tb.Fatalf("generation %d active wal_autocheckpoint=%d, want 0", generation, settings.ActiveAutoCheckpoint)
	}

	insertStarted := time.Now()
	privateInsertGeneratedEdges(tb, store, generation, start, count, chunk)
	insertElapsed := time.Since(insertStarted)
	endStarted := time.Now()
	if err := store.EndGenerationBulkLoadFor(generation); err != nil {
		tb.Fatalf("end generation %d bulk load: %v", generation, err)
	}
	endElapsed := time.Since(endStarted)
	ended = true

	settings.RestoredCacheSetting, settings.RestoredAutoCheckpoint = privateWriterPragmas(tb, store)
	if settings.RestoredCacheSetting != settings.PreviousCacheSetting {
		tb.Fatalf("generation %d restored cache_size=%d, want %d", generation, settings.RestoredCacheSetting, settings.PreviousCacheSetting)
	}
	if settings.RestoredAutoCheckpoint != settings.PreviousAutoCheckpoint {
		tb.Fatalf("generation %d restored wal_autocheckpoint=%d, want %d", generation, settings.RestoredAutoCheckpoint, settings.PreviousAutoCheckpoint)
	}
	return privateBulkTiming{
		BeginNanos: beginElapsed.Nanoseconds(), InsertNanos: insertElapsed.Nanoseconds(), EndNanos: endElapsed.Nanoseconds(),
		TotalNanos: time.Since(totalStarted).Nanoseconds(), Settings: settings,
	}
}

func privateInsertGeneratedEdgesBulkPermuted(tb testing.TB, store *Store, generation int64, start, count, chunk int, seed uint64) privateBulkTiming {
	tb.Helper()
	totalStarted := time.Now()
	beginStarted := time.Now()
	opened, err := store.BeginGenerationBulkLoad(generation)
	beginElapsed := time.Since(beginStarted)
	if err != nil {
		tb.Fatalf("begin generation %d permuted bulk load: %v", generation, err)
	}
	if !opened {
		tb.Fatalf("generation %d did not acquire a physical bulk window", generation)
	}
	ended := false
	defer func() {
		if !ended {
			_ = store.EndGenerationBulkLoadFor(generation)
		}
	}()
	settings := privateActiveBulkSettings(tb, store, generation)
	if settings.ActiveCacheSetting != int64(bulkCacheSizeKiB) || settings.ActiveAutoCheckpoint != 0 {
		tb.Fatalf("generation %d unexpected active bulk settings: %+v", generation, settings)
	}

	insertStarted := time.Now()
	privateInsertGeneratedEdgesPermuted(tb, store, generation, start, count, chunk, seed)
	insertElapsed := time.Since(insertStarted)
	endStarted := time.Now()
	if err := store.EndGenerationBulkLoadFor(generation); err != nil {
		tb.Fatalf("end generation %d permuted bulk load: %v", generation, err)
	}
	endElapsed := time.Since(endStarted)
	ended = true
	settings.RestoredCacheSetting, settings.RestoredAutoCheckpoint = privateWriterPragmas(tb, store)
	if settings.RestoredCacheSetting != settings.PreviousCacheSetting || settings.RestoredAutoCheckpoint != settings.PreviousAutoCheckpoint {
		tb.Fatalf("generation %d did not restore bulk settings: %+v", generation, settings)
	}
	return privateBulkTiming{
		BeginNanos: beginElapsed.Nanoseconds(), InsertNanos: insertElapsed.Nanoseconds(), EndNanos: endElapsed.Nanoseconds(),
		TotalNanos: time.Since(totalStarted).Nanoseconds(), Settings: settings,
	}
}

func privateActiveBulkSettings(tb testing.TB, store *Store, generation int64) privateBulkSettings {
	tb.Helper()
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	if store.generationBulkLoad != generation || store.bulkConn == nil {
		tb.Fatalf("generation %d bulk window is not active", generation)
	}
	ctx := context.Background()
	cache, err := pragmaInt(ctx, store.bulkConn, "cache_size")
	if err != nil {
		tb.Fatalf("read active bulk cache_size: %v", err)
	}
	auto, err := pragmaInt(ctx, store.bulkConn, "wal_autocheckpoint")
	if err != nil {
		tb.Fatalf("read active bulk wal_autocheckpoint: %v", err)
	}
	pageSize, err := pragmaInt(ctx, store.bulkConn, "page_size")
	if err != nil {
		tb.Fatalf("read active bulk page_size: %v", err)
	}
	return privateBulkSettings{
		ActiveCacheSetting:     cache,
		ActiveCacheBytes:       privateSQLiteCacheBytes(cache, pageSize),
		ActiveAutoCheckpoint:   auto,
		PreviousCacheSetting:   store.bulkPrevCacheSize,
		PreviousAutoCheckpoint: store.bulkPrevAutoCheckpoint,
	}
}

func privateWriterPragmas(tb testing.TB, store *Store) (int64, int64) {
	tb.Helper()
	ctx := context.Background()
	conn, err := store.writerDB.Conn(ctx)
	if err != nil {
		tb.Fatalf("borrow writer connection: %v", err)
	}
	defer conn.Close()
	cache, err := pragmaInt(ctx, conn, "cache_size")
	if err != nil {
		tb.Fatalf("read restored cache_size: %v", err)
	}
	auto, err := pragmaInt(ctx, conn, "wal_autocheckpoint")
	if err != nil {
		tb.Fatalf("read restored wal_autocheckpoint: %v", err)
	}
	return cache, auto
}

func privateSQLiteCacheBytes(setting, pageSize int64) int64 {
	if setting < 0 {
		return -setting * 1024
	}
	return setting * pageSize
}

func privateGeneratedEdges(start, count int, generation int64) []*graph.Edge {
	edges := make([]*graph.Edge, count)
	payload := strings.Repeat("x", 192)
	for i := range edges {
		edges[i] = privateGeneratedEdge(start+i, generation, payload)
	}
	return edges
}

func privateGeneratedEdge(n int, generation int64, payload string) *graph.Edge {
	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences, graph.EdgeImports, graph.EdgeImplements, graph.EdgeProvides, graph.EdgeConsumes, graph.EdgeReads, graph.EdgeWrites}
	return &graph.Edge{
		From:            fmt.Sprintf("repo/github.com/fixture/very-long-x/source-%07d", n%180_000),
		To:              fmt.Sprintf("repo/github.com/fixture/target-symbol%06d", (n*7919+17)%90_000),
		Kind:            kinds[n%len(kinds)],
		FilePath:        fmt.Sprintf("repo/pkg-%04d/fixture-%07d.go", n%4096, n),
		Line:            n%20_000 + 1,
		Confidence:      0.5,
		ConfidenceLabel: "medium",
		Origin:          "generation-first-locality",
		Tier:            "exact",
		CrossRepo:       n%17 == 0,
		Meta: map[string]any{
			"generation": fmt.Sprintf("%d", generation),
			"payload":    payload,
		},
	}
}

func privateEdgeDBStatSizes(tb testing.TB, store *Store) map[string]int64 {
	tb.Helper()
	rows, err := store.db.Query(`SELECT name, COALESCE(sum(pgsize), 0) FROM dbstat WHERE name = 'edges' OR name = 'sqlite_autoindex_edges_1' OR name LIKE 'edges_%' GROUP BY name ORDER BY name`)
	if err != nil {
		tb.Fatalf("read edge dbstat sizes: %v", err)
	}
	defer rows.Close()
	sizes := make(map[string]int64)
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			tb.Fatalf("scan edge dbstat size: %v", err)
		}
		sizes[name] = size
	}
	if err := rows.Err(); err != nil {
		tb.Fatalf("iterate edge dbstat sizes: %v", err)
	}
	for _, name := range privateEdgeIndexNames {
		if sizes[name] == 0 {
			tb.Fatalf("dbstat did not report affected index %q: %s", name, privateJSON(tb, sizes))
		}
	}
	return sizes
}

func privateAffectedEdgeIndexSizes(sizes map[string]int64) map[string]int64 {
	affected := make(map[string]int64, len(privateEdgeIndexNames))
	for _, name := range privateEdgeIndexNames {
		affected[name] = sizes[name]
	}
	return affected
}

func privateProjectedRows(targetBytes, observedBytes int64, observedRows int) int {
	if observedBytes <= 0 || observedRows <= 0 {
		return int(^uint(0) >> 1)
	}
	return int((targetBytes*int64(observedRows) + observedBytes - 1) / observedBytes)
}

func privateRelevantEdgeIndexBytes(sizes map[string]int64) int64 {
	var total int64
	for _, name := range privateEdgeIndexNames {
		total += sizes[name]
	}
	return total
}

func privateJSON(tb testing.TB, value any) string {
	tb.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		tb.Fatalf("encode private fixture evidence: %v", err)
	}
	return string(encoded)
}

func privateSQLiteWorkingSetBytes(tb testing.TB, store *Store) int64 {
	tb.Helper()
	var pageCount, pageSize int64
	if err := store.db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		tb.Fatalf("read page_count: %v", err)
	}
	if err := store.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		tb.Fatalf("read page_size: %v", err)
	}
	return pageCount * pageSize
}

func privateRequireTempCap(tb testing.TB, limit int64, paths ...string) {
	tb.Helper()
	var total int64
	for _, path := range paths {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if info, err := os.Stat(path + suffix); err == nil {
				total += info.Size()
			} else if !os.IsNotExist(err) {
				tb.Fatalf("stat %s: %v", path+suffix, err)
			}
		}
	}
	if total > limit {
		tb.Fatalf("private fixture uses %d bytes, cap %d", total, limit)
	}
}

func privateRequireFreeBytes(tb testing.TB, path string, want int64) {
	tb.Helper()
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		tb.Fatalf("statfs %s: %v", path, err)
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	if free < want {
		tb.Skipf("need %d free bytes for bounded fixture, have %d", want, free)
	}
}
