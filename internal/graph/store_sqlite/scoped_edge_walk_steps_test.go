package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/zzet/gortex/internal/graph"
)

// vmStepTotal sums SQLITE_STMTSTATUS_VM_STEP over every statement a traced
// connection finishes.
var vmStepTotal atomic.Int64

// vmStepTrace is an sqlite3_trace_v2 callback. SQLITE_TRACE_PROFILE fires
// once per statement run, when it finishes or is reset, with the statement
// as its third argument.
var vmStepTrace = func(tls *libc.TLS, mask uint32, _, stmt, _ uintptr) int32 {
	if mask == sqlite3.SQLITE_TRACE_PROFILE {
		vmStepTotal.Add(int64(sqlite3.Xsqlite3_stmt_status(tls, stmt, sqlite3.SQLITE_STMTSTATUS_VM_STEP, 1)))
	}
	return 0
}

// traceReaderVMSteps narrows the store's reader pool to one connection and
// traces it. The returned function reports the VM steps since its last call.
// Tests that use it must not run in parallel with each other.
func traceReaderVMSteps(t *testing.T, s *Store) func() int64 {
	t.Helper()
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	setTrace := func(mask uint32, fn uintptr) {
		conn, err := s.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		if err := conn.Raw(func(dc any) error {
			if g, ok := dc.(*gatedConn); ok {
				dc = g.inner
			}
			db, tls, ok := driverConnHandles(dc)
			if !ok {
				return fmt.Errorf("no sqlite handle on %T", dc)
			}
			if rc := sqlite3.Xsqlite3_trace_v2(tls, db, mask, fn, 0); rc != sqlite3.SQLITE_OK {
				return fmt.Errorf("trace_v2 rc=%d", rc)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	setTrace(sqlite3.SQLITE_TRACE_PROFILE, *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uint32, uintptr, uintptr, uintptr) int32
	}{vmStepTrace})))
	t.Cleanup(func() { setTrace(0, 0) })
	vmStepTotal.Store(0)
	return func() int64 { return vmStepTotal.Swap(0) }
}

// One overlay edge below the base high-water mark puts the walk in the
// regime where it has to choose between edges_by_kind and
// edges_by_generation. Here every base edge is of the walked kind, so both
// indexes cover nearly the whole id range. Choosing must not read either
// range to its end before the first page: a consumer that stops after its
// first row pays about one pinned page, as it does when nothing needs
// choosing.
func TestScopedEdgeRepositoryWalkFirstPageCost(t *testing.T) {
	const baseEdges = 40000
	s, err := Open(filepath.Join(t.TempDir(), "walk_first_page.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	nodes := []*graph.Node{
		{ID: "a", Name: "a", Kind: graph.KindFunction, FilePath: "pkg/a.go", Language: "go", RepoPrefix: "repo"},
		{ID: "b", Name: "b", Kind: graph.KindFunction, FilePath: "pkg/b.go", Language: "go", RepoPrefix: "repo"},
	}
	calls := func(n, lineBase int) []*graph.Edge {
		edges := make([]*graph.Edge, 0, n)
		for i := 0; i < n; i++ {
			from, to := nodes[i%2], nodes[(i+1)%2]
			edges = append(edges, &graph.Edge{From: from.ID, To: to.ID, Kind: graph.EdgeCalls,
				FilePath: from.FilePath, Line: lineBase + i})
		}
		return edges
	}
	if err := s.AddBatchChecked(nodes, calls(baseEdges, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.AtGeneration(1).AddBatchChecked(nodes, calls(1, 10*baseEdges)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBatchChecked(nil, calls(1, 20*baseEdges)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writerDB.Exec(`ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	maxID, err := s.edgeGenerationHighWater()
	if err != nil {
		t.Fatal(err)
	}
	inRange, err := s.scopedEdgeOtherGenerationAtOrBelow(maxID)
	if err != nil {
		t.Fatal(err)
	}
	if !inRange {
		t.Fatal("fixture has no overlay edge below the base high-water mark")
	}

	steps := traceReaderVMSteps(t, s)
	// One page pinned to edges_by_kind, with no choice made: the cost of the
	// first page when no other generation is in range.
	query, args, ok := scopedEdgeSourceProjectionQuery([]string{"repo"}, nil, string(graph.EdgeCalls),
		baseViewGeneration, edgesByKindIndexName)
	if !ok {
		t.Fatal("repository edge query was not built")
	}
	steps()
	s.streamScopedEdges(query, args, maxID, func(graph.ScopedEdgeRow) bool { return false })
	onePage := steps()

	for range s.EdgesInScopeSeq([]string{"repo"}, nil, graph.EdgeCalls) {
		break
	}
	firstPage := steps()
	t.Logf("first row: %d VM steps; one pinned page: %d", firstPage, onePage)
	if firstPage > 2*onePage {
		t.Errorf("first row cost %d VM steps, more than twice one pinned page (%d)", firstPage, onePage)
	}

	// The whole walk still returns every base edge, once, in id order. Lines
	// rise with the id here.
	count, last := 0, -1
	for row := range s.EdgesInScopeSeq([]string{"repo"}, nil, graph.EdgeCalls) {
		if row.Edge.Line <= last {
			t.Fatalf("line %d after %d: walk is not in id order", row.Edge.Line, last)
		}
		last = row.Edge.Line
		count++
	}
	if count != baseEdges+1 || last != 20*baseEdges {
		t.Fatalf("walk returned %d edges ending at line %d, want %d ending at line %d",
			count, last, baseEdges+1, 20*baseEdges)
	}
}

// A whole adaptive walk costs about as much as a walk pinned to whichever
// index is cheaper, and less when the cheaper one changes along the id range.
// Each write adds n edges of one kind to one generation, in id order.
func TestScopedEdgeRepositoryWalkCostTracksCheaperIndex(t *testing.T) {
	type write struct {
		gen  int64
		kind graph.EdgeKind
		n    int
	}
	cases := []struct {
		name   string
		writes []write
	}{
		// Few base calls and many overlay calls between them:
		// edges_by_generation is cheaper.
		{name: "overlay_dense", writes: []write{
			{0, graph.EdgeCalls, 2}, {1, graph.EdgeCalls, 20000}, {0, graph.EdgeCalls, 1}}},
		// Few base calls among many base reads, one overlay edge below the
		// base high-water mark: edges_by_kind is cheaper.
		{name: "base_other_kinds", writes: []write{
			{0, graph.EdgeReads, 10000}, {0, graph.EdgeCalls, 300}, {1, graph.EdgeCalls, 1},
			{0, graph.EdgeReads, 10000}, {0, graph.EdgeCalls, 1}}},
		// edges_by_kind is cheaper over the first half, edges_by_generation
		// over the second.
		{name: "cheaper_index_changes", writes: []write{
			{0, graph.EdgeReads, 10000}, {0, graph.EdgeCalls, 300},
			{1, graph.EdgeCalls, 10000}, {0, graph.EdgeCalls, 300}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "walk_cost.sqlite"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			nodes := []*graph.Node{
				{ID: "a", Name: "a", Kind: graph.KindFunction, FilePath: "pkg/a.go", Language: "go", RepoPrefix: "repo"},
				{ID: "b", Name: "b", Kind: graph.KindFunction, FilePath: "pkg/b.go", Language: "go", RepoPrefix: "repo"},
			}
			seeded := map[int64]bool{}
			for i, w := range tc.writes {
				var genNodes []*graph.Node
				if !seeded[w.gen] {
					genNodes, seeded[w.gen] = nodes, true
				}
				edges := make([]*graph.Edge, 0, w.n)
				for j := 0; j < w.n; j++ {
					from, to := nodes[j%2], nodes[(j+1)%2]
					edges = append(edges, &graph.Edge{From: from.ID, To: to.ID, Kind: w.kind,
						FilePath: from.FilePath, Line: 100000*i + j})
				}
				if err := s.AtGeneration(w.gen).AddBatchChecked(genNodes, edges); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.writerDB.Exec(`ANALYZE`); err != nil {
				t.Fatalf("analyze: %v", err)
			}
			maxID, err := s.edgeGenerationHighWater()
			if err != nil {
				t.Fatal(err)
			}
			if _, adaptive := s.scopedEdgeRepositoryWalkPlan(maxID); !adaptive {
				t.Fatal("fixture does not need an adaptive walk")
			}

			steps := traceReaderVMSteps(t, s)
			pinned := map[string]int64{}
			var want []int
			for _, index := range []string{edgesByKindIndexName, edgesByGenerationIndexName} {
				query, args, ok := scopedEdgeSourceProjectionQuery([]string{"repo"}, nil, string(graph.EdgeCalls),
					baseViewGeneration, index)
				if !ok {
					t.Fatal("repository edge query was not built")
				}
				var lines []int
				steps()
				s.streamScopedEdges(query, args, maxID, func(row graph.ScopedEdgeRow) bool {
					lines = append(lines, row.Edge.Line)
					return true
				})
				pinned[index] = steps()
				if want == nil {
					want = lines
				} else if fmt.Sprint(lines) != fmt.Sprint(want) {
					t.Fatalf("walks pinned to different indexes disagree")
				}
			}
			var got []int
			steps()
			for row := range s.EdgesInScopeSeq([]string{"repo"}, nil, graph.EdgeCalls) {
				got = append(got, row.Edge.Line)
			}
			adaptive := steps()
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("adaptive walk returned %d rows, want the pinned walks' %d in id order", len(got), len(want))
			}
			cheaper := min(pinned[edgesByKindIndexName], pinned[edgesByGenerationIndexName])
			t.Logf("adaptive %d VM steps; pinned to edges_by_kind %d, to edges_by_generation %d",
				adaptive, pinned[edgesByKindIndexName], pinned[edgesByGenerationIndexName])
			if adaptive > 2*cheaper+4000 {
				t.Errorf("adaptive walk cost %d VM steps; the cheaper pinned walk costs %d", adaptive, cheaper)
			}
		})
	}
}
