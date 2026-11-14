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

// Plan locks for four reads that each turned into a scan of every edge of the
// generation on a real store once sqlite_stat1 held some rows:
//
//   - baseEdgesByKindSQL (EdgesByKind on the base generation) drove from the
//     view_gen prefix of edges_by_generation / edges_by_to instead of
//     edges_by_kind;
//   - edgeGenerationHighWaterSQL (the MAX(id) that freezes every scoped edge
//     projection and the capability scan) was answered by a covering scan of
//     edges_by_to;
//   - the batched receiver rebind's two cleanup DELETEs scanned edges and
//     probed the candidate table per edge;
//   - the repository-scoped edge walk (scopedEdgeSourceProjectionQuery with
//     no file frontier) paged through edges_by_generation and so read every
//     base edge to find one kind's rows.
//
// Each plan is checked under four statistics regimes: none; the rows a real
// store carried when the misplans were measured (a partial set written by the
// open-time repair); the rows of a larger store; and a fresh full ANALYZE.
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
		{"large_store_stats", func(t *testing.T, ctx context.Context, conn *sql.Conn) {
			// Rows from a 122-repository store on which the repository-scoped
			// edge walk read every base edge through edges_by_generation.
			if _, err := conn.ExecContext(ctx, `ANALYZE edges_by_kind`); err != nil {
				t.Fatalf("create sqlite_stat1: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1`); err != nil {
				t.Fatalf("clear statistics: %v", err)
			}
			// That store had no sqlite_stat4 samples. Fixture samples would
			// tell the planner each kind's size and hide the misplan.
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat4`); err != nil {
				t.Fatalf("clear samples: %v", err)
			}
			for _, row := range [][3]string{
				{"edges", "edges_by_from_line", "6693420 1001 9 2"},
				{"edges", "edges_by_from_line_kind", "6693420 1001 9 2 2"},
				{"edges", "edges_by_generation", "6693420 1001 1"},
				{"edges", "edges_by_kind", "13793188 1001"},
				{"nodes", "nodes_by_file", "1769650 18 18"},
				{"nodes", "nodes_by_generation", "1769650 1001 1"},
				{"nodes", "nodes_by_kind", "1769650 667 667"},
				{"nodes", "nodes_by_repo", "1769650 1001 1001"},
				{"nodes", "nodes_by_repo_kind", "1769650 1001 334"},
			} {
				if _, err := conn.ExecContext(ctx, `INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES (?, ?, ?)`, row[0], row[1], row[2]); err != nil {
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
	for _, regime := range regimes {
		t.Run(regime.name, func(t *testing.T) {
			withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
				regime.apply(t, ctx, conn)

				byKind := explainOnConn(t, ctx, conn, baseEdgesByKindSQL, string(graph.EdgeProvides), baseViewGeneration)
				assertPlanShape(t, "edges_by_kind", byKind, "USING INDEX edges_by_kind (kind=?)", nil,
					[]string{"edges_by_generation", "edges_by_to", "SCAN edges"})

				// A dense kind of the fixture and a kind it has no rows of.
				for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeExtends} {
					repoQuery, repoArgs, ok := scopedEdgeSourceProjectionQuery(
						[]string{"repo"}, nil, string(kind), baseViewGeneration, edgesByKindIndexName)
					if !ok {
						t.Fatal("repository edge query was not built")
					}
					repoArgs = append(repoArgs, int64(0), int64(1<<40), scopedProjectionPage)
					repoWalk := explainOnConn(t, ctx, conn, repoQuery, repoArgs...)
					assertPlanShape(t, "repo_edge_walk_"+string(kind), repoWalk,
						"USING INDEX edges_by_kind (kind=? AND rowid>? AND rowid<?)", nil,
						[]string{"edges_by_generation", "TEMP B-TREE", "SCAN e", "INTEGER PRIMARY KEY"})

					genQuery, genArgs, _ := scopedEdgeSourceProjectionQuery(
						[]string{"repo"}, nil, string(kind), baseViewGeneration, edgesByGenerationIndexName)
					genArgs = append(genArgs, int64(0), int64(1<<40), scopedProjectionPage)
					genWalk := explainOnConn(t, ctx, conn, genQuery, genArgs...)
					assertPlanShape(t, "repo_edge_walk_by_generation_"+string(kind), genWalk,
						"USING INDEX edges_by_generation (view_gen=? AND id>? AND id<?)", nil,
						[]string{"edges_by_kind", "TEMP B-TREE", "SCAN e", "INTEGER PRIMARY KEY"})

					kindProbe := explainOnConn(t, ctx, conn, scopedEdgeKindStepSQL,
						string(kind), int64(0), int64(1<<40), scopedEdgeWalkProbeFirstStep)
					assertPlanShape(t, "repo_edge_walk_kind_probe_"+string(kind), kindProbe, "", []string{
						"USING COVERING INDEX edges_by_kind (kind=? AND rowid>? AND rowid<?)",
					}, []string{"edges_by_generation", "TEMP B-TREE", "SCAN edges"})
				}
				genProbe := explainOnConn(t, ctx, conn, scopedEdgeGenerationStepSQL,
					baseViewGeneration, int64(0), int64(1<<40), scopedEdgeWalkProbeFirstStep)
				assertPlanShape(t, "repo_edge_walk_generation_probe", genProbe, "", []string{
					"USING COVERING INDEX edges_by_generation (view_gen=? AND id>? AND id<?)",
				}, []string{"edges_by_kind", "TEMP B-TREE", "SCAN edges"})
				nextGen := explainOnConn(t, ctx, conn, scopedEdgeNextGenerationSQL, baseViewGeneration)
				assertPlanShape(t, "repo_edge_walk_next_generation", nextGen, "", []string{
					"USING COVERING INDEX edges_by_generation (view_gen>?)",
				}, []string{"edges_by_kind", "TEMP B-TREE", "SCAN edges"})

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

// edges_by_kind is bulk-droppable too: with it missing, the repository walk
// does not pin it (INDEXED BY would fail the read) and returns the same rows.
func TestScopedEdgeRepositoryWalkWithoutKindIndex(t *testing.T) {
	s := newGenerationScanPlanLockStore(t)
	read := func() []string {
		var out []string
		for row := range s.EdgesInScopeSeq([]string{"repo"}, nil, graph.EdgeCalls, graph.EdgeMemberOf) {
			out = append(out, string(row.Edge.Kind)+" "+row.Edge.From+" -> "+row.Edge.To)
		}
		return out
	}
	if !s.edgeKindIndexPresent() {
		t.Fatal("fixture lacks edges_by_kind")
	}
	want := read()
	if len(want) <= scopedProjectionPage {
		t.Fatalf("fixture projection has %d rows; want more than one page", len(want))
	}
	if _, err := s.writerDB.Exec(`DROP INDEX edges_by_kind`); err != nil {
		t.Fatal(err)
	}
	if s.edgeKindIndexPresent() {
		t.Fatal("index still reported after DROP")
	}
	if got := read(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows changed without the index:\n%v\nwant\n%v", got, want)
	}
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

// newMixedGenerationWalkStore holds few base edges and many more edges of the
// same kind in generation 1, interleaved by id: the first base batch is written
// before the overlay and the second after it, so the base high-water mark is
// above every overlay id and the keyset walk crosses the overlay's id range.
func newMixedGenerationWalkStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "mixed_generation_walk.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	nodes := []*graph.Node{
		{ID: "a", Name: "a", Kind: graph.KindFunction, FilePath: "pkg/a.go", Language: "go", RepoPrefix: "repo"},
		{ID: "b", Name: "b", Kind: graph.KindFunction, FilePath: "pkg/b.go", Language: "go", RepoPrefix: "repo"},
		{ID: "c", Name: "c", Kind: graph.KindFunction, FilePath: "other/c.go", Language: "go", RepoPrefix: "other"},
	}
	baseBatch := func(lineBase int) []*graph.Edge {
		var edges []*graph.Edge
		for i := 0; i < scopedProjectionPage/2+2; i++ {
			from, to := nodes[i%2], nodes[(i+1)%2]
			edges = append(edges, &graph.Edge{From: from.ID, To: to.ID, Kind: graph.EdgeCalls,
				FilePath: from.FilePath, Line: lineBase + i})
		}
		for i := 0; i < 10; i++ {
			edges = append(edges,
				&graph.Edge{From: "c", To: "a", Kind: graph.EdgeCalls, FilePath: "other/c.go", Line: lineBase + i},
				&graph.Edge{From: "a", To: "c", Kind: graph.EdgeReads, FilePath: "pkg/a.go", Line: lineBase + i})
		}
		return edges
	}
	if err := s.AddBatchChecked(nodes, baseBatch(0)); err != nil {
		t.Fatal(err)
	}
	overlay := s.AtGeneration(1)
	var overlayEdges []*graph.Edge
	for i := 0; i < 12*scopedProjectionPage; i++ {
		from, to := nodes[i%2], nodes[(i+1)%2]
		overlayEdges = append(overlayEdges, &graph.Edge{From: from.ID, To: to.ID, Kind: graph.EdgeCalls,
			FilePath: from.FilePath, Line: 100000 + i})
	}
	if err := overlay.AddBatchChecked(nodes, overlayEdges); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBatchChecked(nil, baseBatch(1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writerDB.Exec(`ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return s
}

// A repository walk of the base generation must stay generation-selective when
// most rows of the kind belong to other generations: edges_by_kind cannot
// narrow view_gen, so a walk pinned to it reads every overlay row of the kind.
// Whatever index drives it, the walk returns the base rows in id order.
func TestScopedEdgeRepositoryWalkMixedGenerations(t *testing.T) {
	s := newMixedGenerationWalkStore(t)
	maxID, err := s.edgeGenerationHighWater()
	if err != nil {
		t.Fatal(err)
	}
	var overlayMax int64
	if err := s.db.QueryRow(`SELECT MAX(id) FROM edges WHERE view_gen = 1`).Scan(&overlayMax); err != nil {
		t.Fatal(err)
	}
	if maxID <= overlayMax {
		t.Fatalf("base high-water %d is not above the overlay's last id %d", maxID, overlayMax)
	}

	format := func(e *graph.Edge) string { return fmt.Sprintf("%s->%s:%d", e.From, e.To, e.Line) }
	var want []string
	rows, err := s.db.Query(`SELECT e.from_id, e.to_id, e.line FROM edges AS e
JOIN nodes AS n ON n.id = e.from_id AND n.view_gen = e.view_gen
WHERE e.view_gen = 0 AND e.kind = 'calls' AND n.repo_prefix = 'repo' ORDER BY e.id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var e graph.Edge
		if err := rows.Scan(&e.From, &e.To, &e.Line); err != nil {
			t.Fatal(err)
		}
		want = append(want, format(&e))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(want) <= scopedProjectionPage {
		t.Fatalf("fixture has %d base rows; want more than one page", len(want))
	}

	if index, adaptive := s.scopedEdgeRepositoryWalkPlan(maxID); index != "" || !adaptive {
		t.Errorf("mixed-generation walk plan = %q adaptive=%v, want adaptive", index, adaptive)
	}
	// A page-sized segment from the start crosses the overlay's ids, where
	// edges_by_generation reads only the base edges.
	segIndex, segEnd, err := s.scopedEdgeWalkSegment(string(graph.EdgeCalls), 0, maxID, scopedProjectionPage)
	if err != nil {
		t.Fatal(err)
	}
	if segIndex != edgesByGenerationIndexName || segEnd <= overlayMax {
		t.Errorf("segment over the overlay = %q up to id %d, want %q past id %d",
			segIndex, segEnd, edgesByGenerationIndexName, overlayMax)
	}
	// The overlay keeps the planner's choice.
	if index, adaptive := s.AtGeneration(1).scopedEdgeRepositoryWalkPlan(maxID); index != "" || adaptive {
		t.Errorf("overlay walk plan = %q adaptive=%v, want the planner's choice", index, adaptive)
	}
	// A base-dense store keeps edges_by_kind.
	dense := newGenerationScanPlanLockStore(t)
	denseMax, err := dense.edgeGenerationHighWater()
	if err != nil {
		t.Fatal(err)
	}
	if index, adaptive := dense.scopedEdgeRepositoryWalkPlan(denseMax); index != edgesByKindIndexName || adaptive {
		t.Errorf("base-dense walk plan = %q adaptive=%v, want %q", index, adaptive, edgesByKindIndexName)
	}

	withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
		query, args, ok := scopedEdgeSourceProjectionQuery([]string{"repo"}, nil, string(graph.EdgeCalls),
			baseViewGeneration, segIndex)
		if !ok {
			t.Fatal("repository edge query was not built")
		}
		plan := explainOnConn(t, ctx, conn, query, append(args, int64(0), maxID, scopedProjectionPage)...)
		assertPlanShape(t, "mixed_generation_walk", plan,
			"USING INDEX edges_by_generation (view_gen=? AND id>? AND id<?)", nil,
			[]string{"edges_by_kind", "TEMP B-TREE", "SCAN e"})
	})

	// Every index the walk can be pinned to returns the same ordered rows.
	for _, index := range []string{"", edgesByKindIndexName, edgesByGenerationIndexName} {
		query, args, ok := scopedEdgeSourceProjectionQuery([]string{"repo"}, nil, string(graph.EdgeCalls),
			baseViewGeneration, index)
		if !ok {
			t.Fatal("repository edge query was not built")
		}
		var got []string
		s.streamScopedEdges(query, args, maxID, func(row graph.ScopedEdgeRow) bool {
			got = append(got, format(row.Edge))
			return true
		})
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("walk pinned to %q returned %d rows, want %d in id order", index, len(got), len(want))
		}
	}
	read := func() []string {
		var got []string
		for row := range s.EdgesInScopeSeq([]string{"repo"}, nil, graph.EdgeCalls) {
			got = append(got, format(row.Edge))
		}
		return got
	}
	if got := read(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("EdgesInScopeSeq returned %d rows, want %d in id order", len(got), len(want))
	}
	// Without edges_by_generation there is no generation-selective plan to
	// pin; the walk must still answer.
	if _, err := s.writerDB.Exec(`DROP INDEX edges_by_generation`); err != nil {
		t.Fatal(err)
	}
	if got := read(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("without edges_by_generation EdgesInScopeSeq returned %d rows, want %d in id order", len(got), len(want))
	}
}

// The walk probe only matters when other generations hold edges in the walk's
// id range. Without them the kind's range is a subset of the base range, so
// the choice is edges_by_kind and the probe is skipped.
func TestScopedEdgeRepositoryWalkOtherGenerationsInRange(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "a", Name: "a", Kind: graph.KindFunction, FilePath: "pkg/a.go", Language: "go", RepoPrefix: "repo"},
		{ID: "b", Name: "b", Kind: graph.KindFunction, FilePath: "pkg/b.go", Language: "go", RepoPrefix: "repo"},
	}
	edges := func(n, lineBase int) []*graph.Edge {
		var out []*graph.Edge
		for i := 0; i < n; i++ {
			from, to := nodes[i%2], nodes[(i+1)%2]
			out = append(out, &graph.Edge{From: from.ID, To: to.ID, Kind: graph.EdgeCalls,
				FilePath: from.FilePath, Line: lineBase + i})
		}
		return out
	}
	// Each step writes n edges to one generation, in id order.
	type write struct {
		gen int64
		n   int
	}
	cases := []struct {
		name    string
		writes  []write
		inRange bool
	}{
		{name: "base_only", writes: []write{{0, 40}}},
		{name: "overlay_above_base", writes: []write{{0, 40}, {1, 200}}},
		// Generation 1 sits above the base high-water; generation 2 sits below
		// it, so the census must look past the first generation.
		{name: "later_generation_below_base", writes: []write{{2, 200}, {0, 40}, {1, 200}}, inRange: true},
		{name: "overlay_below_base", writes: []write{{0, 2}, {1, 200}, {0, 2}}, inRange: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "walk_generations.sqlite"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			seeded := map[int64]bool{}
			for i, w := range tc.writes {
				var genNodes []*graph.Node
				if !seeded[w.gen] {
					genNodes, seeded[w.gen] = nodes, true
				}
				if err := s.AtGeneration(w.gen).AddBatchChecked(genNodes, edges(w.n, 1000*i)); err != nil {
					t.Fatal(err)
				}
			}
			maxID, err := s.edgeGenerationHighWater()
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.scopedEdgeOtherGenerationAtOrBelow(maxID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.inRange {
				t.Fatalf("other generation at or below %d = %v, want %v", maxID, got, tc.inRange)
			}
			index, adaptive := s.scopedEdgeRepositoryWalkPlan(maxID)
			if adaptive != tc.inRange {
				t.Fatalf("walk plan adaptive = %v, want %v", adaptive, tc.inRange)
			}
			if !adaptive {
				if index != edgesByKindIndexName {
					t.Fatalf("walk index = %q, want %q", index, edgesByKindIndexName)
				}
				return
			}
			// In range, the base range here is the smaller one.
			index, _, err = s.scopedEdgeWalkSegment(string(graph.EdgeCalls), 0, maxID, scopedEdgeWalkProbeFirstStep)
			if err != nil {
				t.Fatal(err)
			}
			if index != edgesByGenerationIndexName {
				t.Fatalf("first segment index = %q, want %q", index, edgesByGenerationIndexName)
			}
		})
	}
}
