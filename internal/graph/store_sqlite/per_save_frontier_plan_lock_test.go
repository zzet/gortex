package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Plan locks for two per-save frontier reads that the planner turned into a
// scan of the whole generation on a real store (946k edges, 185k nodes, with
// the open-time repair's statistics rows):
//
//   - the cross-repository candidate query of a changed-file frontier drove
//     from edges_by_to on view_gen alone and probed the frontier CTE per edge
//     (13-21 s per save) until the CTE was pinned as the outer loop;
//   - the contract-eviction frontier's de-duplicating UNION was planned as a
//     sorted MERGE whose arms read every node and every edge of the generation
//     in id order (4.8 s per save) until it became UNION ALL.
func TestPerSaveFrontierPlansLockedAcrossStatisticsRegimes(t *testing.T) {
	regimes := []struct {
		name  string
		apply func(t *testing.T, ctx context.Context, conn *sql.Conn)
	}{
		{"no_stats", func(t *testing.T, ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS sqlite_stat1`); err != nil {
				if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1`); err != nil {
					t.Fatalf("clear statistics: %v", err)
				}
			}
			reloadPlannerStats(t, ctx, conn)
		}},
		{"real_store_repair_stats", func(t *testing.T, ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `ANALYZE edges_by_kind`); err != nil {
				t.Fatalf("create sqlite_stat1: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1`); err != nil {
				t.Fatalf("clear statistics: %v", err)
			}
			// The rows the measured real-repository store carried.
			for _, row := range [][3]string{
				{"edges", "edges_by_from_line", "946322 1001 14 3"},
				{"edges", "edges_by_from_line_kind", "946322 1001 14 3 3"},
				{"edges", "edges_by_generation", "946322 1001 1"},
				{"edges", "edges_by_kind", "946322 501"},
				{"nodes", "nodes_by_file", "185194 13 13"},
				{"nodes", "nodes_by_generation", "185194 1001 1"},
				{"nodes", "nodes_by_kind", "185194 501 501"},
				{"nodes", "nodes_by_name", "185194 2 2"},
				{"nodes", "nodes_by_repo", "185194 501 501"},
				{"nodes", "nodes_by_repo_kind", "185194 501 167"},
			} {
				if _, err := conn.ExecContext(ctx, `INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES (?, ?, ?)`,
					row[0], row[1], row[2]); err != nil {
					t.Fatalf("write stat row %s: %v", row[1], err)
				}
			}
			reloadPlannerStats(t, ctx, conn)
		}},
		{"fresh_analyze", func(t *testing.T, ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `ANALYZE`); err != nil {
				t.Fatalf("analyze: %v", err)
			}
			reloadPlannerStats(t, ctx, conn)
		}},
	}
	s := newGenerationScanPlanLockStore(t)
	files := []string{"pkg/file03.go"}
	crossQuery, crossArgs, ok := s.crossRepoCandidatesQuery(graph.BaseKindsForCrossRepo(), nil, files, files)
	if !ok {
		t.Fatal("no cross-repository frontier query")
	}
	filesJSON, _ := projectionJSON(files)
	scoped := evictFilesPredicate + ` AND view_gen = ?`
	contractArgs := []any{s.viewGen, string(graph.KindContract), filesJSON, s.viewGen, filesJSON, s.viewGen,
		s.viewGen, string(graph.EdgeProvides), string(graph.EdgeConsumes), string(graph.EdgeHandlesRoute)}
	for _, regime := range regimes {
		t.Run(regime.name, func(t *testing.T) {
			withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
				regime.apply(t, ctx, conn)
				cross := explainOnConn(t, ctx, conn, crossQuery, crossArgs...)
				assertNoGenerationWalk(t, "cross_repo_frontier", cross, "e")
				if joined := strings.Join(cross, "\n"); !strings.Contains(joined, "SCAN ce") {
					t.Errorf("cross_repo_frontier: the frontier CTE does not drive:\n%s", joined)
				}
				contract := explainOnConn(t, ctx, conn, contractFileEvictionFrontierSQL(scoped), contractArgs...)
				assertNoGenerationWalk(t, "contract_eviction_frontier", contract, "nodes", "edges")
			})
		})
	}
}

// assertNoGenerationWalk fails when a plan scans one of tables or reads it
// through an index keyed on view_gen alone.
func assertNoGenerationWalk(t *testing.T, label string, plan []string, tables ...string) {
	t.Helper()
	joined := strings.Join(plan, "\n")
	for _, line := range plan {
		trimmed := trimPlanLine(line)
		for _, table := range tables {
			scan := trimmed == "SCAN "+table || strings.HasPrefix(trimmed, "SCAN "+table+" ")
			generationOnly := strings.HasPrefix(trimmed, "SEARCH "+table+" USING ") &&
				strings.HasSuffix(trimmed, "(view_gen=?)")
			if scan || generationOnly {
				t.Errorf("%s: walks the generation (%s):\n%s", label, trimmed, joined)
			}
		}
	}
}

// The incident-edge DELETEs of a derived contract replacement remove exactly
// the edges touching the removed bridges and orphaned topics, in either
// direction, and nothing else.
func TestReplaceDerivedContractsRemovesIncidentEdgesBothWays(t *testing.T) {
	s := newGenerationScanPlanLockStore(t)
	nodes := []*graph.Node{
		{ID: "bridge::1", Kind: graph.KindContractBridge, Name: "b1"},
		{ID: "bridge::2", Kind: graph.KindContractBridge, Name: "b2"},
		{ID: "topic::orphan", Kind: graph.KindTopic, Name: "orphan"},
		{ID: "topic::owned", Kind: graph.KindTopic, Name: "owned"},
	}
	edges := []*graph.Edge{
		{From: "bridge::1", To: "pkg/file00.go::sym00", Kind: graph.EdgeBridges, FilePath: "x", Line: 1},
		{From: "pkg/file01.go::sym00", To: "bridge::1", Kind: graph.EdgeReferences, FilePath: "x", Line: 2},
		{From: "bridge::2", To: "pkg/file02.go::sym00", Kind: graph.EdgeBridges, FilePath: "x", Line: 3},
		{From: "topic::orphan", To: "pkg/file03.go::sym00", Kind: graph.EdgeReferences, FilePath: "x", Line: 4},
		{From: "pkg/file04.go::sym00", To: "topic::orphan", Kind: graph.EdgeReferences, FilePath: "x", Line: 5},
		{From: "pkg/file05.go::sym00", To: "topic::owned", Kind: graph.EdgeProducesTopic, FilePath: "x", Line: 6},
		{From: "topic::owned", To: "pkg/file06.go::sym00", Kind: graph.EdgeReferences, FilePath: "x", Line: 7},
	}
	s.AddBatch(nodes, edges)
	before := allEdgeRows(t, s)
	result, err := s.ReplaceDerivedContracts(graph.DerivedContractReplacement{
		RemoveBridgeNodeIDs: []string{"bridge::1"},
		TouchedTopicNodeIDs: []string{"topic::orphan", "topic::owned"},
	})
	if err != nil {
		t.Fatal(err)
	}
	after := allEdgeRows(t, s)
	var removed []string
	kept := make(map[string]struct{}, len(after))
	for _, row := range after {
		kept[row] = struct{}{}
	}
	for _, row := range before {
		if _, ok := kept[row]; !ok {
			removed = append(removed, row)
		}
	}
	sort.Strings(removed)
	want := []string{
		"bridge::1->pkg/file00.go::sym00",
		"pkg/file01.go::sym00->bridge::1",
		"pkg/file04.go::sym00->topic::orphan",
		"topic::orphan->pkg/file03.go::sym00",
	}
	if fmt.Sprint(removed) != fmt.Sprint(want) || len(after) != len(before)-len(want) {
		t.Fatalf("removed edges = %v, want %v (before %d, after %d)", removed, want, len(before), len(after))
	}
	if result.EdgesRemoved != len(want) || result.NodesRemoved != 2 {
		t.Fatalf("result = %+v, want %d edges and 2 nodes removed", result, len(want))
	}
	if s.GetNode("topic::owned") == nil || s.GetNode("bridge::2") == nil {
		t.Fatal("an owned topic or an untouched bridge was removed")
	}
}

func allEdgeRows(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT from_id, to_id FROM edges WHERE view_gen = ?`, s.viewGen)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			t.Fatal(err)
		}
		out = append(out, from+"->"+to)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}
