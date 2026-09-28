package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// receiverMutationCallStatRows are the sqlite_stat1 rows of a production
// store (ten repositories, 7.96M edges, 1.47M nodes) on which the whole-graph
// receiver-call scan ran for most of an hour: with them the planner drove the
// page's exact join from edges_by_generation (view_gen=?) and rescanned the
// json_each id list once per edge of the generation. edges_by_from and
// edges_by_to have no row there, as on that store. (On this small fixture the
// plain JOIN misplans the same way without statistics; with these rows it
// happens to plan well. The pinned query must hold under all three.)
var receiverMutationCallStatRows = [][3]string{
	{"edges", "edges_by_from_line", "7963653 1001 19 2"},
	{"edges", "edges_by_from_line_kind", "7963653 1001 19 2 2"},
	{"edges", "edges_by_generation", "7963653 1001 1"},
	{"edges", "edges_by_kind", "7963653 667"},
	{"nodes", "nodes_by_file", "1466778 69 69"},
	{"nodes", "nodes_by_generation", "1466778 1001 1"},
	{"nodes", "nodes_by_kind", "1466778 1001 201"},
	{"nodes", "nodes_by_name", "1466778 3 2"},
	{"nodes", "nodes_by_repo", "1466778 1001 1001"},
	{"nodes", "nodes_by_repo_kind", "1466778 1001 334"},
}

// TestReceiverMutationCallScanPlanLock locks the loop order of the
// receiver-call scan's per-page exact join under three statistics regimes:
// none, the production rows above, and an honest ANALYZE of the fixture. The
// page's id list must drive the join and every edge must be a point probe
// on its id; an edges access that does not bind the id, driven or driving, is
// the O(edges x candidates) plan.
//
// "PlanLock" is in the name because the Windows CI leg runs only
// -run 'PlanLock|PlansLocked|PlansNeverScan' in this package.
func TestReceiverMutationCallScanPlanLock(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const methods = 64
	nodes := make([]*graph.Node, 0, methods*2)
	edges := make([]*graph.Edge, 0, methods*8)
	for i := 0; i < methods; i++ {
		caller := fmt.Sprintf("pkg/f%03d.go::T.Caller%03d", i, i)
		target := fmt.Sprintf("pkg/f%03d.go::T.Target%03d", i, i)
		nodes = append(nodes,
			&graph.Node{ID: caller, Kind: graph.KindMethod, Name: fmt.Sprintf("Caller%03d", i), Meta: map[string]any{"receiver": "T"}},
			&graph.Node{ID: target, Kind: graph.KindMethod, Name: fmt.Sprintf("Target%03d", i), Meta: map[string]any{"receiver": "T"}},
		)
		edges = append(edges, &graph.Edge{
			From: caller, To: target, Kind: graph.EdgeCalls, FilePath: fmt.Sprintf("pkg/f%03d.go", i), Line: 1,
			Meta: map[string]any{"recv_self": true},
		})
		for j := 0; j < 7; j++ {
			edges = append(edges, &graph.Edge{
				From: caller, To: fmt.Sprintf("external::noise%03d_%d", i, j), Kind: graph.EdgeCalls,
				FilePath: fmt.Sprintf("pkg/f%03d.go", i), Line: 2 + j,
			})
		}
	}
	s.AddBatch(nodes, edges)

	var want []graph.ReceiverMutationCall
	s.ScanReceiverMutationCalls(16, func(page []graph.ReceiverMutationCall) bool {
		want = append(want, page...)
		return true
	})
	if len(want) != methods {
		t.Fatalf("receiver calls = %d, want %d", len(want), methods)
	}

	var ids []int64
	if err := func() error {
		rows, err := s.db.Query(`SELECT id FROM edges WHERE kind = ? ORDER BY id LIMIT 32`, string(graph.EdgeCalls))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}(); err != nil {
		t.Fatalf("collect edge ids: %v", err)
	}
	encodedIDs, err := json.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	args := []any{string(encodedIDs), s.viewGen}

	assertLocked := func(t *testing.T, ctx context.Context, conn *sql.Conn) {
		t.Helper()
		plan := explainOnConn(t, ctx, conn, receiverMutationCallRowsSQL, args...)
		joined := strings.Join(plan, "\n")
		driver := ""
		for _, line := range plan {
			trimmed := trimPlanLine(line)
			if strings.HasPrefix(trimmed, "SCAN") || strings.HasPrefix(trimmed, "SEARCH") {
				driver = trimmed
				break
			}
		}
		if !strings.Contains(driver, "json_each") {
			t.Errorf("the page's id list does not drive the join (first loop %q):\n%s", driver, joined)
		}
		// Every edge must be a point probe on the id (the rowid, or the
		// (view_gen, id) key of edges_by_generation, which sqlite_stat1 can
		// make look cheaper); an edges access that does not bind the id is a
		// range or scan over the generation.
		probed := false
		for _, line := range plan {
			trimmed := trimPlanLine(line)
			if !strings.HasPrefix(trimmed, "SEARCH e ") && !strings.HasPrefix(trimmed, "SCAN e") {
				continue
			}
			if strings.HasPrefix(trimmed, "SEARCH e ") && strings.Contains(trimmed, "rowid=?") {
				probed = true
				continue
			}
			t.Errorf("edges are reached through %q, which does not bind the page's ids:\n%s", trimmed, joined)
		}
		if !probed {
			t.Errorf("edges are not probed by id:\n%s", joined)
		}
		// The keyword is what pins the order; show what the planner does
		// without it under the same statistics.
		unpinned := strings.Replace(receiverMutationCallRowsSQL, "CROSS JOIN edges AS e", "JOIN edges AS e", 1)
		t.Logf("pinned:\n%s\nunpinned:\n%s", joined, strings.Join(explainOnConn(t, ctx, conn, unpinned, args...), "\n"))
	}

	t.Run("no_stats", func(t *testing.T) {
		if statsTableExists(t, s) {
			t.Fatal("fixture created sqlite_stat1 without an explicit refresh")
		}
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			assertLocked(t, ctx, conn)
		})
	})
	t.Run("production_rows", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `ANALYZE`); err != nil {
				t.Fatalf("create sqlite_stat1: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1 WHERE tbl IN ('edges', 'nodes')`); err != nil {
				t.Fatalf("clear stat rows: %v", err)
			}
			for _, row := range receiverMutationCallStatRows {
				if _, err := conn.ExecContext(ctx, `INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES (?, ?, ?)`, row[0], row[1], row[2]); err != nil {
					t.Fatalf("insert stat row %v: %v", row, err)
				}
			}
			if _, err := conn.ExecContext(ctx, `ANALYZE sqlite_schema`); err != nil {
				t.Fatalf("reload statistics: %v", err)
			}
			assertLocked(t, ctx, conn)
		})
	})
	t.Run("refreshed", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1`); err != nil {
				t.Fatalf("clear stat rows: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `ANALYZE`); err != nil {
				t.Fatalf("refresh statistics: %v", err)
			}
			assertLocked(t, ctx, conn)
		})
	})

	// The pinned order returns exactly what the scan returned before.
	var got []graph.ReceiverMutationCall
	s.ScanReceiverMutationCalls(16, func(page []graph.ReceiverMutationCall) bool {
		got = append(got, page...)
		return true
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("receiver calls changed across statistics regimes:\n got %#v\nwant %#v", got, want)
	}
}
