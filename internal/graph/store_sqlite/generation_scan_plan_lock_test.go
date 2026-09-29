package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Plan locks for three reads that each turned into a scan of every edge of the
// generation on a real store once sqlite_stat1 held some rows:
//
//   - baseEdgesByKindSQL (EdgesByKind on the base generation) drove from the
//     view_gen prefix of edges_by_generation / edges_by_to instead of
//     edges_by_kind;
//   - edgeGenerationHighWaterSQL (the MAX(id) that freezes every scoped edge
//     projection and the capability scan) was answered by a covering scan of
//     edges_by_to;
//   - the batched receiver rebind's two cleanup DELETEs scanned edges and
//     probed the candidate table per edge.
//
// Each plan is checked under three statistics regimes: none; the rows a real
// store carried when the misplans were measured (a partial set written by the
// open-time repair); and a fresh full ANALYZE.
func TestGenerationScanPlansLockedAcrossStatisticsRegimes(t *testing.T) {
	regimes := []struct {
		name  string
		apply func(t *testing.T, ctx context.Context, conn *sql.Conn)
	}{
		{"no_stats", func(t *testing.T, ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS sqlite_stat1`); err != nil {
				// sqlite_stat1 cannot always be dropped; emptying it is the
				// same regime for the planner.
				if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1`); err != nil {
					t.Fatalf("clear statistics: %v", err)
				}
			}
			reloadPlannerStats(t, ctx, conn)
		}},
		{"real_store_partial_stats", func(t *testing.T, ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `ANALYZE edges_by_kind`); err != nil { // creates sqlite_stat1
				t.Fatalf("create sqlite_stat1: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1`); err != nil {
				t.Fatalf("clear statistics: %v", err)
			}
			for idx, stat := range map[string]string{
				"edges_by_from_line":      "946296 1001 13 2",
				"edges_by_from_line_kind": "946296 1001 13 2 2",
				"edges_by_generation":     "946296 1001 1",
				"edges_by_kind":           "946296 501",
			} {
				if _, err := conn.ExecContext(ctx, `INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES ('edges', ?, ?)`, idx, stat); err != nil {
					t.Fatalf("write stat row %s: %v", idx, err)
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
	for _, regime := range regimes {
		t.Run(regime.name, func(t *testing.T) {
			withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
				regime.apply(t, ctx, conn)

				byKind := explainOnConn(t, ctx, conn, baseEdgesByKindSQL, string(graph.EdgeProvides), baseViewGeneration)
				assertPlanShape(t, "edges_by_kind", byKind, "USING INDEX edges_by_kind (kind=?)", nil,
					[]string{"edges_by_generation", "edges_by_to", "SCAN edges"})

				highWater := explainOnConn(t, ctx, conn, edgeGenerationHighWaterSQL(true), baseViewGeneration)
				assertPlanShape(t, "high_water", highWater, "edges_by_generation (view_gen=?)", nil,
					[]string{"edges_by_to", "SCAN edges"})
				derived := explainOnConn(t, ctx, conn, capabilityProjectionHighWaterQuery(7, true), int64(7))
				assertPlanShape(t, "capability_high_water", derived, "edges_by_generation", nil,
					[]string{"edges_by_to", "SCAN edges"})

				if _, err := conn.ExecContext(ctx, goMethodReceiverCandidateTableSQL); err != nil {
					t.Fatalf("create candidate table: %v", err)
				}
				for label, query := range map[string]string{
					"rebind_conflict_delete":  goMethodReceiverBatchConflictDeleteSQL,
					"rebind_duplicate_delete": goMethodReceiverBatchDuplicateDeleteSQL,
				} {
					plan := explainOnConn(t, ctx, conn, query)
					joined := strings.Join(plan, "\n")
					if !strings.Contains(joined, "SCAN r") {
						t.Errorf("%s: the candidate table does not drive the delete:\n%s", label, joined)
					}
					if !strings.Contains(joined, "SEARCH old USING INTEGER PRIMARY KEY") {
						t.Errorf("%s: edges are not probed by rowid:\n%s", label, joined)
					}
					for _, line := range plan {
						if strings.HasPrefix(trimPlanLine(line), "SCAN old") {
							t.Errorf("%s: every edge is scanned:\n%s", label, joined)
							break
						}
					}
				}
			})
		})
	}
}

func reloadPlannerStats(t *testing.T, ctx context.Context, conn *sql.Conn) {
	t.Helper()
	// Hand-edited statistics reach a connection's planner only after it
	// reloads them.
	if _, err := conn.ExecContext(ctx, `ANALYZE sqlite_schema`); err != nil {
		t.Fatalf("reload statistics: %v", err)
	}
}

func newGenerationScanPlanLockStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "generation_scan_plan_lock.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var nodes []*graph.Node
	var edges []*graph.Edge
	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeCalls, graph.EdgeReads, graph.EdgeArgOf, graph.EdgeMemberOf, graph.EdgeImports}
	for f := 0; f < 30; f++ {
		file := fmt.Sprintf("pkg/file%02d.go", f)
		for n := 0; n < 20; n++ {
			id := fmt.Sprintf("%s::sym%02d", file, n)
			nodes = append(nodes, &graph.Node{ID: id, Name: fmt.Sprintf("sym%02d", n), Kind: graph.KindFunction,
				FilePath: file, Language: "go", RepoPrefix: "repo"})
			for k, kind := range kinds {
				edges = append(edges, &graph.Edge{From: id, To: fmt.Sprintf("pkg/file%02d.go::sym%02d", (f+k+1)%30, n),
					Kind: kind, FilePath: file, Line: n*10 + k})
			}
		}
	}
	edges = append(edges, &graph.Edge{From: "pkg/file00.go::sym00", To: "pkg/file01.go::sym00", Kind: graph.EdgeProvides,
		FilePath: "pkg/file00.go", Line: 1, Meta: map[string]any{"provides_for": "X", "binding": "useClass"}})
	s.AddBatch(nodes, edges)
	return s
}

// The generation index is optional: a store without it still answers the
// scoped edge projection (the high-water read falls back to the unpinned form)
// with the same rows.
func TestScopedEdgeProjectionWithoutGenerationIndex(t *testing.T) {
	s := newGenerationScanPlanLockStore(t)
	read := func() []string {
		var out []string
		for row := range s.EdgesInScopeSeq(nil, []string{"pkg/file03.go"}, graph.EdgeCalls) {
			out = append(out, row.Edge.From+" -> "+row.Edge.To)
		}
		return out
	}
	if !s.edgeGenerationIndexPresent() {
		t.Fatal("fixture lacks edges_by_generation")
	}
	want := read()
	if len(want) == 0 {
		t.Fatal("fixture projection is empty")
	}
	if _, err := s.writerDB.Exec(`DROP INDEX edges_by_generation`); err != nil {
		t.Fatal(err)
	}
	if s.edgeGenerationIndexPresent() {
		t.Fatal("index still reported after DROP")
	}
	if got := read(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows changed without the index:\n%v\nwant\n%v", got, want)
	}
}
