package store_sqlite

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestEdgesByKindsUseKindFirstDeleteOnlyForSparseContractKinds(t *testing.T) {
	tests := []struct {
		name  string
		kinds []graph.EdgeKind
		want  bool
	}{
		{name: "matches", kinds: []graph.EdgeKind{graph.EdgeMatches}, want: true},
		{name: "produces", kinds: []graph.EdgeKind{graph.EdgeProducesTopic}, want: true},
		{name: "consumes", kinds: []graph.EdgeKind{graph.EdgeConsumesTopic}, want: true},
		{name: "all-with-duplicate", kinds: []graph.EdgeKind{graph.EdgeMatches, graph.EdgeProducesTopic, graph.EdgeConsumesTopic, graph.EdgeMatches}, want: true},
		{name: "common", kinds: []graph.EdgeKind{graph.EdgeImports}, want: false},
		{name: "mixed", kinds: []graph.EdgeKind{graph.EdgeMatches, graph.EdgeImports}, want: false},
		{name: "empty", kinds: nil, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := edgesByKindsUseKindFirstDelete(tc.kinds); got != tc.want {
				t.Fatalf("kind-first delete for %v = %v, want %v", tc.kinds, got, tc.want)
			}
		})
	}
}

func TestSparseEdgeKindDeletePlanUsesKindIndexForBaseAndDerived(t *testing.T) {
	db := openEdgeKindEvictPlanDB(t, true)
	for _, viewGen := range []int64{0, 7} {
		t.Run(fmt.Sprintf("generation-%d", viewGen), func(t *testing.T) {
			plan := edgeKindEvictPlan(t, db, edgesBySparseKindsDeleteQuery,
				`["matches","provides","consumes"]`, viewGen)
			if !strings.Contains(plan, "edges_by_kind") {
				t.Fatalf("sparse delete plan misses kind index:\n%s", plan)
			}
			if strings.Contains(plan, "edges_by_generation") {
				t.Fatalf("sparse delete plan uses broad generation index:\n%s", plan)
			}
			if strings.Contains(edgesBySparseKindsDeleteQuery, "INDEXED BY") {
				t.Fatal("sparse delete hard-codes an optional index")
			}
		})
	}
}

func TestSparseEdgeKindDeleteWorksWithoutSecondaryIndexes(t *testing.T) {
	db := openEdgeKindEvictPlanDB(t, false)
	res, err := db.Exec(edgesBySparseKindsDeleteQuery, `["matches","provides","consumes"]`, int64(7))
	if err != nil {
		t.Fatalf("delete without secondary indexes: %v", err)
	}
	removed, err := res.RowsAffected()
	if err != nil || removed != 3 {
		t.Fatalf("removed = %d, %v; want 3", removed, err)
	}
	assertEdgeKindCount(t, db, 7, "imports", 1500)
	assertEdgeKindCount(t, db, 0, "matches", 1)
	assertEdgeKindCount(t, db, 0, "provides", 1)
	assertEdgeKindCount(t, db, 0, "consumes", 1)
}

func TestEvictEdgesByKindsPreservesScopeCountsAndGenericPath(t *testing.T) {
	store, _ := openTempStore(t)
	baseNodes, baseEdges := edgeKindEvictFixture("base")
	if err := store.AddBatchChecked(baseNodes, baseEdges); err != nil {
		t.Fatalf("seed base: %v", err)
	}
	derivedNodes, derivedEdges := edgeKindEvictFixture("derived")
	derived := store.AtGeneration(7)
	if err := derived.AddBatchChecked(derivedNodes, derivedEdges); err != nil {
		t.Fatalf("seed derived: %v", err)
	}

	sparse := []graph.EdgeKind{graph.EdgeMatches, graph.EdgeProducesTopic, graph.EdgeConsumesTopic, graph.EdgeMatches}
	if got := store.EvictEdgesByKinds(sparse); got != 3 {
		t.Fatalf("base sparse removed = %d, want 3", got)
	}
	if got := store.EvictEdgesByKinds(sparse); got != 0 {
		t.Fatalf("duplicate base sparse removal = %d, want 0", got)
	}
	if got := derived.EvictEdgesByKinds(sparse); got != 3 {
		t.Fatalf("derived sparse removed = %d, want 3", got)
	}
	if got := store.EvictEdgesByKinds([]graph.EdgeKind{graph.EdgeImports}); got != 1 {
		t.Fatalf("base generic removed = %d, want 1", got)
	}
	if got := derived.EvictEdgesByKinds([]graph.EdgeKind{graph.EdgeImports}); got != 1 {
		t.Fatalf("derived generic removed = %d, want 1", got)
	}
	if got := store.EvictEdgesByKinds(nil); got != 0 {
		t.Fatalf("nil kinds removed = %d, want 0", got)
	}
}

func TestEvictEdgesByKindsMixedKindsRetainGenericSemantics(t *testing.T) {
	store, _ := openTempStore(t)
	nodes, edges := edgeKindEvictFixture("mixed")
	if err := store.AddBatchChecked(nodes, edges); err != nil {
		t.Fatal(err)
	}
	if got := store.EvictEdgesByKinds([]graph.EdgeKind{graph.EdgeMatches, graph.EdgeImports}); got != 2 {
		t.Fatalf("mixed removal = %d, want 2", got)
	}
	if got := store.EvictEdgesByKinds([]graph.EdgeKind{graph.EdgeConsumesTopic}); got != 1 {
		t.Fatalf("remaining consumes removal = %d, want 1", got)
	}
	if got := store.EvictEdgesByKinds([]graph.EdgeKind{graph.EdgeProducesTopic}); got != 1 {
		t.Fatalf("remaining produces removal = %d, want 1", got)
	}
}

func edgeKindEvictFixture(prefix string) ([]*graph.Node, []*graph.Edge) {
	kinds := []graph.EdgeKind{graph.EdgeMatches, graph.EdgeProducesTopic, graph.EdgeConsumesTopic, graph.EdgeImports}
	nodes := make([]*graph.Node, 0, len(kinds)+1)
	edges := make([]*graph.Edge, 0, len(kinds))
	target := prefix + "/target"
	nodes = append(nodes, &graph.Node{ID: target, Kind: graph.KindContract})
	for i, kind := range kinds {
		from := fmt.Sprintf("%s/source-%d", prefix, i)
		nodes = append(nodes, &graph.Node{ID: from, Kind: graph.KindFunction})
		edges = append(edges, &graph.Edge{From: from, To: target, Kind: kind})
	}
	return nodes, edges
}

func openEdgeKindEvictPlanDB(t *testing.T, indexes bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE edges (kind TEXT NOT NULL, view_gen INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if indexes {
		for _, stmt := range []string{
			`CREATE INDEX edges_by_kind ON edges(kind)`,
			`CREATE INDEX edges_by_generation ON edges(view_gen)`,
		} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, viewGen := range []int64{0, 7} {
		if _, err := db.Exec(`
WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i < 1499)
INSERT INTO edges(kind, view_gen) SELECT 'imports', ? FROM seq`, viewGen); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO edges(kind, view_gen) VALUES
('matches', ?), ('provides', ?), ('consumes', ?)`, viewGen, viewGen, viewGen); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`
WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i < 199)
INSERT INTO edges(kind, view_gen) SELECT printf('fixture-kind-%03d', i), ? FROM seq`, viewGen); err != nil {
			t.Fatal(err)
		}
	}
	if indexes {
		if _, err := db.Exec("ANALYZE"); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func edgeKindEvictPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "\n")
}

func assertEdgeKindCount(t *testing.T, db *sql.DB, viewGen int64, kind string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT COUNT(*) FROM edges WHERE view_gen = ? AND kind = ?`, viewGen, kind).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("generation %d kind %s count = %d, want %d", viewGen, kind, got, want)
	}
}
