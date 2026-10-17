package store_sqlite

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func newCapabilityGenerationStore(tb testing.TB) *Store {
	tb.Helper()
	s, err := Open(filepath.Join(tb.TempDir(), "capability.sqlite"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	return s
}

func addCapabilityGenerationCalls(tb testing.TB, s *Store, generation int64, prefix string, start, count int) {
	tb.Helper()
	target := prefix + "::target"
	nodes := make([]*graph.Node, 0, count+1)
	nodes = append(nodes, &graph.Node{ID: target, Name: "target", Kind: graph.KindFunction, RepoPrefix: prefix})
	edges := make([]*graph.Edge, 0, count)
	for i := start; i < start+count; i++ {
		id := fmt.Sprintf("%s::caller::%d", prefix, i)
		nodes = append(nodes, &graph.Node{ID: id, Name: "caller", Kind: graph.KindFunction, RepoPrefix: prefix, FilePath: prefix + "/calls.go"})
		edges = append(edges, &graph.Edge{From: id, To: target, Kind: graph.EdgeCalls, FilePath: prefix + "/calls.go", Line: i + 1})
	}
	s.AtGeneration(generation).AddBatch(nodes, edges)
}

func capabilityGenerationArgs(generation, lastID, highWater int64, reposJSON string, allRepos bool, pageSize int) []any {
	args := make([]any, 0, 12)
	if !allRepos {
		args = append(args, reposJSON)
	}
	return append(args, lastID, highWater, generation,
		string(graph.EdgeReadsConfig), string(graph.EdgeReads), string(graph.EdgeWrites), string(graph.EdgeCalls),
		string(graph.EdgeReads), string(graph.EdgeWrites), string(graph.KindField), pageSize)
}

// Both controls execute the same projection/paging code over the same data.
// legacy selects the unchanged generation-zero SQL but still binds generation,
// reproducing the old access path for a positive-generation handle.
func scanCapabilityGenerationControl(tb testing.TB, s *Store, generation int64, prefixes []string, legacy bool) []graph.RepoCapabilityEdge {
	tb.Helper()
	if prefixes != nil && len(prefixes) == 0 {
		return nil
	}
	allRepos := prefixes == nil
	var reposJSON string
	if !allRepos {
		var ok bool
		reposJSON, ok = projectionJSON(prefixes)
		if !ok {
			return nil
		}
	}
	queryGeneration := generation
	if legacy {
		queryGeneration = 0
	}
	var highWater int64
	if err := s.db.QueryRow(capabilityProjectionHighWaterQuery(queryGeneration, s.edgeGenerationIndexPresent()), generation).Scan(&highWater); err != nil {
		tb.Fatal(err)
	}
	if highWater == 0 {
		return nil
	}
	stmt, err := s.db.Prepare(capabilityProjectionPageQuery(queryGeneration, allRepos))
	if err != nil {
		tb.Fatal(err)
	}
	defer stmt.Close()
	const pageSize = 4096
	var out []graph.RepoCapabilityEdge
	for lastID := int64(0); lastID < highWater; {
		rows, err := stmt.Query(capabilityGenerationArgs(generation, lastID, highWater, reposJSON, allRepos, pageSize)...)
		if err != nil {
			tb.Fatal(err)
		}
		count := 0
		for rows.Next() {
			var row graph.RepoCapabilityEdge
			if err := rows.Scan(&lastID, &row.RepoPrefix, &row.Identity.From, &row.Identity.To, &row.Identity.Kind, &row.Identity.FilePath, &row.Identity.Line); err != nil {
				_ = rows.Close()
				tb.Fatal(err)
			}
			out = append(out, row)
			count++
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			tb.Fatal(rowsErr)
		}
		if closeErr != nil {
			tb.Fatal(closeErr)
		}
		if count < pageSize {
			break
		}
	}
	return out
}

func TestCapabilityGenerationProjectionParity(t *testing.T) {
	s := newCapabilityGenerationStore(t)
	for _, generation := range []int64{0, 7, 9} {
		prefix := "foreign"
		fieldKind := graph.KindVariable
		if generation == 7 {
			prefix = "alpha"
			fieldKind = graph.KindField
		}
		s.AtGeneration(generation).AddBatch([]*graph.Node{
			{ID: "shared-source", Name: "source", Kind: graph.KindFunction, RepoPrefix: prefix},
			{ID: "shared-field", Name: "field", Kind: fieldKind, RepoPrefix: prefix},
			{ID: "shared-function", Name: "function", Kind: graph.KindFunction, RepoPrefix: prefix},
		}, []*graph.Edge{
			{From: "shared-source", To: "shared-function", Kind: graph.EdgeCalls, FilePath: "shared.go", Line: 1},
			{From: "shared-source", To: "shared-field", Kind: graph.EdgeReads, FilePath: "shared.go", Line: 2},
			{From: "shared-source", To: "shared-field", Kind: graph.EdgeWrites, FilePath: "shared.go", Line: 3},
			{From: "shared-source", To: "cfg::env::TOKEN", Kind: graph.EdgeReadsConfig, FilePath: "shared.go", Line: 4},
			{From: "shared-source", To: "shared-function", Kind: graph.EdgeReads, FilePath: "shared.go", Line: 5},
			{From: "shared-source", To: "missing", Kind: graph.EdgeWrites, FilePath: "shared.go", Line: 6},
			{From: "shared-source", To: "shared-field", Kind: graph.EdgeReferences, FilePath: "shared.go", Line: 7},
		})
	}
	addCapabilityGenerationCalls(t, s, 7, "beta", 0, 1)
	for _, generation := range []int64{0, 7, 9, 11} {
		for _, prefixes := range [][]string{nil, {}, {"alpha"}, {"beta"}, {"foreign"}, {"missing"}, {"alpha", "beta"}, {"alpha", "alpha"}} {
			t.Run(fmt.Sprintf("generation_%d/repos_%v", generation, prefixes), func(t *testing.T) {
				want := scanCapabilityGenerationControl(t, s, generation, prefixes, true)
				var got []graph.RepoCapabilityEdge
				s.AtGeneration(generation).ScanRepoCapabilityEdges(prefixes, 2, func(page []graph.RepoCapabilityEdge) bool {
					got = append(got, page...)
					return true
				})
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("projection mismatch:\ngot  %#v\nwant %#v", got, want)
				}
				if generation == 7 && prefixes == nil && len(got) != 5 {
					t.Fatalf("got %d eligible edges, want 5 (including own-generation field reads/writes)", len(got))
				}
			})
		}
	}
}

func TestCapabilityGenerationPaginationAndHighWater(t *testing.T) {
	s := newCapabilityGenerationStore(t)
	addCapabilityGenerationCalls(t, s, 0, "before", 0, 100)
	addCapabilityGenerationCalls(t, s, 7, "alpha", 0, 4097)
	addCapabilityGenerationCalls(t, s, 9, "after", 0, 100)
	want := scanCapabilityGenerationControl(t, s, 7, nil, true)
	var got []graph.RepoCapabilityEdge
	var sizes []int
	s.AtGeneration(7).ScanRepoCapabilityEdges(nil, 0, func(page []graph.RepoCapabilityEdge) bool {
		sizes = append(sizes, len(page))
		got = append(got, page...)
		if len(sizes) == 1 {
			// The reader is closed before yield; a same-generation write can
			// re-enter safely but must not escape the scan's frozen highwater.
			addCapabilityGenerationCalls(t, s, 7, "late", 0, 1)
		}
		return true
	})
	if !reflect.DeepEqual(sizes, []int{4096, 1}) || !reflect.DeepEqual(got, want) {
		t.Fatalf("pagination/highwater mismatch: page sizes %v, got %d, want %d", sizes, len(got), len(want))
	}
	calls := 0
	s.AtGeneration(7).ScanRepoCapabilityEdges(nil, 1, func([]graph.RepoCapabilityEdge) bool {
		calls++
		return false
	})
	if calls != 1 {
		t.Fatalf("yield cancellation called %d times, want 1", calls)
	}
	s.AtGeneration(7).ScanRepoCapabilityEdges(nil, 1, nil)
	s.AtGeneration(7).ScanRepoCapabilityEdges([]string{}, 1, func([]graph.RepoCapabilityEdge) bool {
		t.Fatal("exact empty repository scope yielded a page")
		return false
	})
}

func capabilityGenerationPlan(tb testing.TB, s *Store, query string, args ...any) string {
	tb.Helper()
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		tb.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			tb.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		tb.Fatal(err)
	}
	return strings.Join(plan, "\n")
}

func TestCapabilityGenerationQueryPlan(t *testing.T) {
	s := newCapabilityGenerationStore(t)
	addCapabilityGenerationCalls(t, s, 0, "foreign", 0, 100)
	addCapabilityGenerationCalls(t, s, 7, "alpha", 0, 2)
	for _, allRepos := range []bool{true, false} {
		query := capabilityProjectionPageQuery(7, allRepos)
		plan := capabilityGenerationPlan(t, s, query, capabilityGenerationArgs(7, 0, 1<<60, `["alpha"]`, allRepos, 4096)...)
		if !strings.Contains(plan, "edges_by_generation") || strings.Contains(plan, "SEARCH e USING INTEGER PRIMARY KEY") {
			t.Fatalf("derived page must use generation index (all=%v):\n%s", allRepos, plan)
		}
		if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
			t.Fatalf("derived keyset page needs no global sort:\n%s", plan)
		}
		baseQuery := capabilityProjectionPageQuery(0, allRepos)
		if !strings.Contains(baseQuery, "edges AS e NOT INDEXED") || strings.Contains(baseQuery, "e.view_gen > 0") {
			t.Fatal("base-generation access path changed")
		}
	}
	plan := capabilityGenerationPlan(t, s, capabilityProjectionHighWaterQuery(7, true), int64(7))
	if !strings.Contains(plan, "edges_by_generation") {
		t.Fatalf("derived highwater must use generation index:\n%s", plan)
	}
	// The base highwater is pinned to the generation index when it exists
	// and falls back to the unpinned legacy form when it does not.
	if got := capabilityProjectionHighWaterQuery(0, true); got != `SELECT COALESCE(MAX(id), 0) FROM edges INDEXED BY edges_by_generation WHERE view_gen = ?` {
		t.Fatalf("base highwater changed: %s", got)
	}
	if got := capabilityProjectionHighWaterQuery(0, false); got != `SELECT COALESCE(MAX(id), 0) FROM edges WHERE view_gen = ?` {
		t.Fatalf("index-less base highwater changed: %s", got)
	}
}

func TestCapabilityGenerationOptionalIndexAbsent(t *testing.T) {
	s := newCapabilityGenerationStore(t)
	addCapabilityGenerationCalls(t, s, 0, "foreign", 0, 10)
	addCapabilityGenerationCalls(t, s, 7, "alpha", 0, 3)
	want := scanCapabilityGenerationControl(t, s, 7, nil, true)
	if _, err := s.writerDB.Exec(`DROP INDEX edges_by_generation`); err != nil {
		t.Fatal(err)
	}
	var got []graph.RepoCapabilityEdge
	s.AtGeneration(7).ScanRepoCapabilityEdges(nil, 2, func(page []graph.RepoCapabilityEdge) bool {
		got = append(got, page...)
		return true
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("optional-index absence changed results: got %d, want %d", len(got), len(want))
	}
}

func BenchmarkCapabilityGenerationProjection(b *testing.B) {
	for _, foreign := range []int{0, 50000} {
		b.Run(fmt.Sprintf("foreign_%d", foreign), func(b *testing.B) {
			s := newCapabilityGenerationStore(b)
			if foreign > 0 {
				addCapabilityGenerationCalls(b, s, 0, "before", 0, foreign/2)
			}
			addCapabilityGenerationCalls(b, s, 7, "alpha", 0, 128)
			if foreign > 0 {
				addCapabilityGenerationCalls(b, s, 9, "after", 0, foreign-foreign/2)
			}
			for _, legacy := range []bool{true, false} {
				name := "generation_sql"
				if legacy {
					name = "legacy_sql"
				}
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if got := scanCapabilityGenerationControl(b, s, 7, nil, legacy); len(got) != 128 {
							b.Fatalf("got %d own-generation edges, want 128", len(got))
						}
					}
					b.ReportMetric(float64(foreign), "foreign_edges")
					b.ReportMetric(128, "target_edges")
				})
			}
		})
	}
}
