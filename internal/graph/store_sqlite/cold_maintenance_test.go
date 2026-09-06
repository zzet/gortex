package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func newColdMaintenanceStore(t testing.TB) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// The active writer helper is essential during coordinated bulk loading: the
// only writer connection is pinned, so acquiring writerDB.Conn would deadlock.
func coldMaintenanceExec(t testing.TB, s *Store, query string, args ...any) {
	t.Helper()
	s.writeMu.Lock()
	_, err := s.execActiveWriteLocked(context.Background(), query, args...)
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("fixture SQL: %v", err)
	}
}

func coldMaintenanceCount(t testing.TB, s *Store, want int, query string, args ...any) {
	t.Helper()
	var got int
	if err := s.db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("count = %d, want %d; query %s", got, want, query)
	}
}

func coldMaintenanceNode(t testing.TB, s *Store, repo, id string, gen int) {
	t.Helper()
	coldMaintenanceExec(t, s, `INSERT INTO nodes (id, kind, name, file_path, repo_prefix, view_gen)
		VALUES (?, 'function', ?, ?, ?, ?)`, id, id, repo+"/fixture.go", repo, gen)
}

func coldMaintenanceEdge(t testing.TB, s *Store, from, to string, gen int) {
	t.Helper()
	coldMaintenanceExec(t, s, `INSERT INTO edges (from_id, to_id, kind, view_gen)
		VALUES (?, ?, 'calls', ?)`, from, to, gen)
}

func coldMaintenanceSidecars(t testing.TB, s *Store, repo string, gen int) {
	t.Helper()
	coldMaintenanceExec(t, s, `INSERT INTO semantic_binding_types
		(view_gen, repo_prefix, file_path, line, name, type_name)
		VALUES (?, ?, ?, 1, 'binding', 'Type')`, gen, repo, repo+"/fixture.go")
	coldMaintenanceExec(t, s, `INSERT INTO file_index_failures
		(view_gen, repo_prefix, file_path, error) VALUES (?, ?, ?, 'fixture failure')`,
		gen, repo, repo+"/fixture.go")
}

func coldMaintenanceEvict(t testing.TB, s *Store, predicate string, arg any, scope evictScope, wantNodes, wantEdges int) {
	t.Helper()
	nodes, edges, err := s.evictByPredicateResult(predicate, arg, scope)
	if err != nil {
		t.Fatal(err)
	}
	if nodes != wantNodes || edges != wantEdges {
		t.Fatalf("removed (%d nodes, %d edges), want (%d, %d)", nodes, edges, wantNodes, wantEdges)
	}
}

func TestColdMaintenanceEmptyGenerationCleansOnlySelectedSidecars(t *testing.T) {
	s := newColdMaintenanceStore(t)
	coldMaintenanceNode(t, s, "target", "target::a", 7)
	coldMaintenanceNode(t, s, "target", "target::b", 7)
	coldMaintenanceEdge(t, s, "target::a", "target::b", 7)
	coldMaintenanceNode(t, s, "other", "other::a", 0)
	coldMaintenanceNode(t, s, "other", "other::b", 0)
	coldMaintenanceEdge(t, s, "other::a", "other::b", 0)
	coldMaintenanceSidecars(t, s, "target", 0)
	coldMaintenanceSidecars(t, s, "target", 7)
	coldMaintenanceSidecars(t, s, "other", 0)

	coldMaintenanceEvict(t, s, evictNonEmptyRepoPredicate, "target", evictThisGeneration, 0, 0)
	coldMaintenanceCount(t, s, 4, `SELECT COUNT(*) FROM nodes`)
	coldMaintenanceCount(t, s, 2, `SELECT COUNT(*) FROM edges`)
	for _, table := range []string{"semantic_binding_types", "file_index_failures"} {
		coldMaintenanceCount(t, s, 0, `SELECT COUNT(*) FROM `+table+` WHERE repo_prefix = 'target' AND view_gen = 0`)
		coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM `+table+` WHERE repo_prefix = 'target' AND view_gen = 7`)
		coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM `+table+` WHERE repo_prefix = 'other' AND view_gen = 0`)
	}
}

func TestColdMaintenanceEmptyScopeDoesNotPrepareGraphDeletes(t *testing.T) {
	s := newColdMaintenanceStore(t)
	coldMaintenanceSidecars(t, s, "target", 0)
	// Deliberate query-path fault injection, not a supported database layout.
	// With no candidate node, the old implementation still prepares DELETE FROM
	// edges and errors. Restore the table before Store.Close and other cleanup.
	coldMaintenanceExec(t, s, `ALTER TABLE edges RENAME TO cold_test_hidden_edges`)
	defer coldMaintenanceExec(t, s, `ALTER TABLE cold_test_hidden_edges RENAME TO edges`)
	coldMaintenanceEvict(t, s, evictNonEmptyRepoPredicate, "target", evictThisGeneration, 0, 0)
	coldMaintenanceCount(t, s, 0, `SELECT COUNT(*) FROM semantic_binding_types`)
	coldMaintenanceCount(t, s, 0, `SELECT COUNT(*) FROM file_index_failures`)
}

func TestColdMaintenanceExistingRepoIgnoresStaleLedger(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		scope                                 evictScope
		wantNodes, wantEdges, remainingTarget int
	}{
		{"current_generation", evictThisGeneration, 2, 3, 2},
		{"all_generations", evictAllGenerations, 4, 6, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newColdMaintenanceStore(t)
			for _, gen := range []int{0, 7} {
				coldMaintenanceNode(t, s, "target", "target::a", gen)
				coldMaintenanceNode(t, s, "target", "target::b", gen)
				coldMaintenanceNode(t, s, "other", "other::a", gen)
				coldMaintenanceEdge(t, s, "target::a", "target::b", gen)
				coldMaintenanceEdge(t, s, "target::a", "other::a", gen)
				coldMaintenanceEdge(t, s, "other::a", "target::a", gen)
				coldMaintenanceEdge(t, s, "other::a", "other::a", gen)
				coldMaintenanceSidecars(t, s, "target", gen)
				coldMaintenanceSidecars(t, s, "other", gen)
				// Deliberately false metadata: graph truth must determine eviction.
				coldMaintenanceExec(t, s, `INSERT INTO repo_index_state
					(view_gen, repo_prefix, node_count, edge_count) VALUES (?, 'target', 0, 0)`, gen)
			}
			coldMaintenanceEvict(t, s, evictNonEmptyRepoPredicate, "target", tc.scope, tc.wantNodes, tc.wantEdges)
			coldMaintenanceCount(t, s, tc.remainingTarget, `SELECT COUNT(*) FROM nodes WHERE repo_prefix = 'target'`)
			coldMaintenanceCount(t, s, 2, `SELECT COUNT(*) FROM nodes WHERE repo_prefix = 'other'`)
			coldMaintenanceCount(t, s, 8-tc.wantEdges, `SELECT COUNT(*) FROM edges`)
			for _, table := range []string{"semantic_binding_types", "file_index_failures"} {
				coldMaintenanceCount(t, s, 0, `SELECT COUNT(*) FROM `+table+` WHERE repo_prefix = 'target' AND view_gen = 0`)
				coldMaintenanceCount(t, s, tc.remainingTarget/2, `SELECT COUNT(*) FROM `+table+` WHERE repo_prefix = 'target' AND view_gen = 7`)
				coldMaintenanceCount(t, s, 2, `SELECT COUNT(*) FROM `+table+` WHERE repo_prefix = 'other'`)
			}
		})
	}
}

func TestColdMaintenanceEmptyPrefixIsNotAllRepositories(t *testing.T) {
	s := newColdMaintenanceStore(t)
	coldMaintenanceNode(t, s, "", "global::a", 0)
	coldMaintenanceNode(t, s, "other", "other::a", 0)
	coldMaintenanceSidecars(t, s, "", 0)
	coldMaintenanceSidecars(t, s, "other", 0)
	coldMaintenanceEvict(t, s, evictRepoPredicate, "", evictThisGeneration, 1, 0)
	coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM nodes WHERE repo_prefix = 'other'`)
	for _, table := range []string{"semantic_binding_types", "file_index_failures"} {
		coldMaintenanceCount(t, s, 0, `SELECT COUNT(*) FROM `+table+` WHERE repo_prefix = ''`)
		coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM `+table+` WHERE repo_prefix = 'other'`)
	}
}

func TestColdMaintenanceEmptyFilePreservesFailureLedger(t *testing.T) {
	s := newColdMaintenanceStore(t)
	coldMaintenanceSidecars(t, s, "target", 0)
	coldMaintenanceEvict(t, s, evictFilePredicate, "target/fixture.go", evictThisGeneration, 0, 0)
	coldMaintenanceCount(t, s, 0, `SELECT COUNT(*) FROM semantic_binding_types WHERE repo_prefix = 'target'`)
	// File eviction is not successful reindexing or confirmed deletion. The
	// existing failure ledger must survive even when the node scope is empty.
	coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM file_index_failures WHERE repo_prefix = 'target'`)
}

func TestColdMaintenanceProbeFailureRollsBackSidecars(t *testing.T) {
	s := newColdMaintenanceStore(t)
	coldMaintenanceSidecars(t, s, "target", 0)
	// Fault-inject the existence query after sidecar cleanup. Restoring the
	// table in a defer also protects teardown when the expected error is absent.
	coldMaintenanceExec(t, s, `ALTER TABLE nodes RENAME TO cold_test_hidden_nodes`)
	defer coldMaintenanceExec(t, s, `ALTER TABLE cold_test_hidden_nodes RENAME TO nodes`)
	_, _, err := s.evictByPredicateResult(evictNonEmptyRepoPredicate, "target", evictThisGeneration)
	if err == nil {
		t.Fatal("expected missing-node-table error")
	}
	coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM semantic_binding_types WHERE repo_prefix = 'target'`)
	coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM file_index_failures WHERE repo_prefix = 'target'`)
}

func coldMaintenanceCorpus(t testing.TB, s *Store, nodes, edges int) {
	t.Helper()
	coldMaintenanceExec(t, s, `WITH RECURSIVE seq(n) AS (
		VALUES(0) UNION ALL SELECT n + 1 FROM seq WHERE n + 1 < ?)
		INSERT INTO nodes (id, kind, name, file_path, repo_prefix, view_gen, workspace_id, project_id)
		SELECT printf('large::%d', n), 'function', printf('fn%d', n), 'large/fixture.go', 'large', 0, 'large', 'large' FROM seq`, nodes)
	coldMaintenanceExec(t, s, `WITH RECURSIVE seq(n) AS (
		VALUES(0) UNION ALL SELECT n + 1 FROM seq WHERE n + 1 < ?)
		INSERT INTO edges (from_id, to_id, kind, view_gen)
		SELECT printf('large::%d', n % ?), printf('large::%d', (n % ? + n / ? + 1) % ?), 'calls', 0 FROM seq`,
		edges, nodes, nodes, nodes, nodes)
}

func coldMaintenancePlan(t testing.TB, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(&plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan.String()
}

func TestColdMaintenanceRepoIndexAvailableDuringBulk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s != nil {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	if !s.BeginCoordinatedBulkLoad() {
		t.Fatal("fresh disk store did not enter coordinated bulk loading")
	}
	coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'nodes_by_repo_kind'`)
	coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'nodes_missing_workspace_slugs'`)
	for _, index := range bulkDroppableIndexes {
		coldMaintenanceCount(t, s, 0, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = ?`, index.name)
	}
	coldMaintenanceCorpus(t, s, 1000, 5000)
	coldMaintenanceNode(t, s, "target", "target::a", 0)
	coldMaintenanceEdge(t, s, "target::a", "large::0", 0)
	for _, query := range []string{
		`SELECT EXISTS(SELECT 1 FROM nodes WHERE repo_prefix = ? AND repo_prefix <> '' AND view_gen = ? LIMIT 1)`,
		`SELECT COUNT(*) FROM nodes WHERE repo_prefix = ? AND view_gen = ?`,
		`SELECT COUNT(*) FROM edges e JOIN nodes n ON n.id = e.from_id AND n.view_gen = e.view_gen WHERE n.repo_prefix = ? AND e.view_gen = ?`,
	} {
		plan := coldMaintenancePlan(t, s, query, "target", 0)
		repoSeek := false
		for _, line := range strings.Split(plan, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			isNode := fields[1] == "n" || fields[1] == "nodes"
			isEdge := fields[1] == "e" || fields[1] == "edges"
			if fields[0] == "SEARCH" && isNode && strings.Contains(line, "nodes_by_repo_kind") {
				repoSeek = true
			}
			if fields[0] == "SCAN" && (isNode || isEdge) {
				t.Fatalf("unexpected whole-graph-table scan for %s; plan:\n%s", query, plan)
			}
		}
		if !repoSeek {
			t.Fatalf("expected repository-indexed node seek for %s; plan:\n%s", query, plan)
		}
	}
	if err := s.EndCoordinatedBulkLoad(); err != nil {
		t.Fatal(err)
	}
	coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'nodes_by_repo_kind'`)
	coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'nodes_missing_workspace_slugs'`)
	for _, index := range bulkDroppableIndexes {
		coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = ?`, index.name)
	}
	if err := s.EndCoordinatedBulkLoad(); err != nil {
		t.Fatalf("idempotent bulk completion: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = nil
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'nodes_by_repo_kind'`)
	coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'nodes_missing_workspace_slugs'`)
	coldMaintenanceCount(t, s, 1, `SELECT COUNT(*) FROM nodes WHERE repo_prefix = 'target' AND view_gen = 0`)
}

func coldMaintenanceBenchmarkStore(b *testing.B, indexes string) *Store {
	b.Helper()
	s := newColdMaintenanceStore(b)
	if !s.BeginCoordinatedBulkLoad() {
		b.Fatal("fresh disk store did not enter bulk loading")
	}
	switch indexes {
	case "legacy_deferred":
		// Reproduce the original deferred-index set after the proposed move.
		coldMaintenanceExec(b, s, `DROP INDEX IF EXISTS nodes_by_repo_kind`)
	case "retained_repo":
		// Preserve only the proposed always-live repository index.
	case "sealed_access_paths":
		// Restore access paths while retaining the same bulk connection/cache
		// PRAGMAs. This is deliberately NOT a normal sealed connection profile.
		for _, index := range bulkDroppableIndexes {
			coldMaintenanceExec(b, s, index.ddl)
		}
	default:
		b.Fatalf("unknown fixture %q", indexes)
	}
	coldMaintenanceCorpus(b, s, 100000, 500000)
	return s
}

func BenchmarkColdBulkRepoMaintenance(b *testing.B) {
	for _, indexes := range []string{"legacy_deferred", "retained_repo", "sealed_access_paths"} {
		b.Run(indexes, func(b *testing.B) {
			b.Run("absent_eviction", func(b *testing.B) {
				s := coldMaintenanceBenchmarkStore(b, indexes)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					coldMaintenanceEvict(b, s, evictNonEmptyRepoPredicate, "absent", evictThisGeneration, 0, 0)
				}
				b.StopTimer()
			})
			b.Run("existing_tiny_eviction", func(b *testing.B) {
				s := coldMaintenanceBenchmarkStore(b, indexes)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					coldMaintenanceNode(b, s, "target", "target::a", 0)
					coldMaintenanceNode(b, s, "target", "target::b", 0)
					coldMaintenanceEdge(b, s, "target::a", "target::b", 0)
					coldMaintenanceEdge(b, s, "target::a", "large::0", 0)
					coldMaintenanceEdge(b, s, "large::0", "target::a", 0)
					b.StartTimer()
					coldMaintenanceEvict(b, s, evictNonEmptyRepoPredicate, "target", evictThisGeneration, 2, 3)
				}
				b.StopTimer()
			})
			b.Run("exact_tiny_recount", func(b *testing.B) {
				s := coldMaintenanceBenchmarkStore(b, indexes)
				coldMaintenanceNode(b, s, "target", "target::a", 0)
				coldMaintenanceEdge(b, s, "target::a", "large::0", 0)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					coldMaintenanceCount(b, s, 1, `SELECT COUNT(*) FROM nodes WHERE repo_prefix = ? AND view_gen = ?`, "target", 0)
					coldMaintenanceCount(b, s, 1, `SELECT COUNT(*) FROM edges e JOIN nodes n ON n.id = e.from_id AND n.view_gen = e.view_gen WHERE n.repo_prefix = ? AND e.view_gen = ?`, "target", 0)
				}
				b.StopTimer()
			})
		})
	}
}

func BenchmarkColdBulkRepoIndexInsertCost(b *testing.B) {
	// Each iteration gets a fresh disk store. Setup/close are untimed, and only
	// Store.AddBatch is charged. This measures the real node insertion path,
	// but not parsing or complete multi-repository ingest. Compare multiple
	// runs: there is no OS cache flush, and these synthetic nodes have no edges.
	for _, retained := range []bool{false, true} {
		name := "legacy_deferred"
		if retained {
			name = "retained_repo"
		}
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			nodes := make([]*graph.Node, 100000)
			for i := range nodes {
				id := fmt.Sprintf("large::%d", i)
				nodes[i] = &graph.Node{ID: id, Kind: graph.KindFunction, Name: id, RepoPrefix: "large"}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				s, err := Open(b.TempDir() + "/store.sqlite")
				if err != nil {
					b.Fatal(err)
				}
				if !s.BeginCoordinatedBulkLoad() {
					_ = s.Close()
					b.Fatal("fresh disk store did not enter bulk loading")
				}
				if !retained {
					coldMaintenanceExec(b, s, `DROP INDEX IF EXISTS nodes_by_repo_kind`)
				}
				b.StartTimer()
				s.AddBatch(nodes, nil)
				b.StopTimer()
				if err := s.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(nodes)), "nodes/op")
		})
	}
}
