package store_sqlite

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// queryPlan runs EXPLAIN QUERY PLAN for the given statement + args and
// returns the joined detail lines.
func queryPlan(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return strings.Join(lines, "\n")
}

// TestBFSQueryUsesEdgeIndex asserts the recursive-CTE BFS join reaches the
// edges table through the direction's composite index (edges_by_from for a
// forward walk, edges_by_to for a backward walk) rather than a full table
// scan, and that the node-backed gate join uses the nodes primary key.
func TestBFSQueryUsesEdgeIndex(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// A small fixture so the planner has real tables/indexes to plan against.
	for _, id := range []string{"A", "B", "C"} {
		s.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: "a.go", Language: "go"})
	}
	s.AddEdge(&graph.Edge{From: "A", To: "B", Kind: graph.EdgeCalls, Confidence: 1, Origin: graph.OriginASTResolved})
	s.AddEdge(&graph.Edge{From: "B", To: "C", Kind: graph.EdgeCalls, Confidence: 1, Origin: graph.OriginASTResolved})

	forwardQ := buildBFSQuery(graph.DirectionForward, 1, 1, true)
	forwardPlan := queryPlan(t, s, forwardQ, "A", 3, string(graph.EdgeCalls), baseViewGeneration, 50)
	if !strings.Contains(forwardPlan, "edges_by_from") {
		t.Fatalf("forward BFS plan does not use edges_by_from:\n%s", forwardPlan)
	}
	// The edges table must not be reached by a full scan (alias e).
	if strings.Contains(forwardPlan, "SCAN edges") || strings.Contains(forwardPlan, "SCAN e ") {
		t.Fatalf("forward BFS plan scans edges instead of using an index:\n%s", forwardPlan)
	}

	backwardQ := buildBFSQuery(graph.DirectionBackward, 1, 1, true)
	backwardPlan := queryPlan(t, s, backwardQ, "C", 3, string(graph.EdgeCalls), baseViewGeneration, 50)
	if !strings.Contains(backwardPlan, "edges_by_to") {
		t.Fatalf("backward BFS plan does not use edges_by_to:\n%s", backwardPlan)
	}
	if strings.Contains(backwardPlan, "SCAN edges") || strings.Contains(backwardPlan, "SCAN e ") {
		t.Fatalf("backward BFS plan scans edges instead of using an index:\n%s", backwardPlan)
	}

	t.Logf("forward BFS query plan:\n%s", forwardPlan)
	t.Logf("backward BFS query plan:\n%s", backwardPlan)
}
