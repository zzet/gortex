package store_sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type incomingGenerationFanoutFixture struct {
	control            *Store
	path               string
	generations        []int64
	target             string
	edgesPerGeneration int
	distinct           bool
}

type incomingGenerationFanoutRange struct {
	first, last   int64
	before, after int
}

func prepareIncomingGenerationFanout(tb testing.TB, generations, edgesPerGeneration int, distinct bool) incomingGenerationFanoutFixture {
	tb.Helper()
	if generations < 1 || generations > 32 || edgesPerGeneration < 1 || edgesPerGeneration > 1024 {
		tb.Fatal("fanout fixture exceeds its explicit size bound")
	}
	path := filepath.Join(tb.TempDir(), "incoming-generation-fanout.sqlite")
	s, err := Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	f := incomingGenerationFanoutFixture{control: s, path: path, target: "fanout/target.go::Target", edgesPerGeneration: edgesPerGeneration, distinct: distinct}
	// Insert before every positive row so even an oldest selection must reject
	// a same-endpoint/kind generation0 record, not merely miss a later poison.
	s.AddBatch([]*graph.Node{{ID: "generation0-poison", Name: "Poison", RepoPrefix: "fanout", FilePath: "fanout/poison.go", Kind: graph.KindType}}, []*graph.Edge{{From: "generation0-poison", To: f.target, Kind: graph.EdgeKind("calls"), FilePath: "fanout/poison.go", Line: 1}})
	for generation := 0; generation < generations; generation++ {
		id, err := s.Catalog().CreateViewGeneration(tb.Context(), ViewGeneration{OwnerKind: "dedicated_graph", GraphID: "incoming-generation-fanout", GenerationKind: "dedicated", TreeOID: fmt.Sprintf("source-%02d", generation), ConfigHash: "policy", State: ViewGenerationBuilding, CreatedAt: 1})
		if err != nil {
			tb.Fatal(err)
		}
		f.generations = append(f.generations, id)
		handle := s.AtGeneration(id)
		nodes := []*graph.Node{{ID: f.target, Name: "Target", RepoPrefix: "fanout", FilePath: "fanout/target.go", Kind: graph.KindType}}
		edges := make([]*graph.Edge, edgesPerGeneration)
		for site := range edges {
			sourceIndex := 0
			if distinct {
				sourceIndex = site
			}
			from := incomingGenerationFanoutSource(generation, sourceIndex)
			file := fmt.Sprintf("fanout/g%02d.go", generation)
			if distinct || site == 0 {
				nodes = append(nodes, &graph.Node{ID: from, Name: "Source", RepoPrefix: "fanout", FilePath: file, Kind: graph.KindType})
			}
			edges[site] = &graph.Edge{From: from, To: f.target, Kind: graph.EdgeKind("calls"), FilePath: file, Line: site + 1}
		}
		handle.AddBatch(nodes, edges)
		if err := s.Catalog().PublishViewGeneration(tb.Context(), id, 2); err != nil {
			tb.Fatal(err)
		}
		if handle.EdgeCount() != edgesPerGeneration {
			tb.Fatal("positive fixture did not retain every edge")
		}
	}
	var physical int
	if err := s.db.QueryRowContext(tb.Context(), `SELECT COUNT(*) FROM edges WHERE to_id = ? AND kind = ?`, f.target, "calls").Scan(&physical); err != nil || physical != generations*edgesPerGeneration+1 {
		tb.Fatalf("retained physical range: %d %v", physical, err)
	}
	poison, err := s.FindIncomingSourcesBounded(tb.Context(), []string{f.target}, graph.EdgeKind("calls"), 1)
	if err != nil || poison.Truncated[f.target] || !reflect.DeepEqual(poison.Sources[f.target], []string{"generation0-poison"}) {
		tb.Fatalf("negative generation0 prerequisite: %+v %v", poison, err)
	}
	f.assertDiskBudget(tb)
	return f
}

func (f incomingGenerationFanoutFixture) assertDiskBudget(tb testing.TB) int64 {
	tb.Helper()
	var bytes int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(f.path + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			tb.Fatal(err)
		}
		bytes += info.Size()
	}
	if bytes > 128<<20 {
		tb.Fatalf("bounded fanout fixture exceeded128MiB: %d", bytes)
	}
	return bytes
}

func incomingGenerationFanoutSource(generation, source int) string {
	return fmt.Sprintf("fanout/g%02d.go::S%04d", generation, source)
}

func (f incomingGenerationFanoutFixture) selectedRange(tb testing.TB, selected int) incomingGenerationFanoutRange {
	tb.Helper()
	var r incomingGenerationFanoutRange
	if err := f.control.db.QueryRowContext(tb.Context(), `SELECT MIN(id), MAX(id) FROM edges WHERE to_id = ? AND kind = ? AND view_gen = ?`, f.target, "calls", f.generations[selected]).Scan(&r.first, &r.last); err != nil {
		tb.Fatal(err)
	}
	if err := f.control.db.QueryRowContext(tb.Context(), `SELECT COUNT(*) FROM edges WHERE to_id = ? AND kind = ? AND id < ?`, f.target, "calls", r.first).Scan(&r.before); err != nil {
		tb.Fatal(err)
	}
	if err := f.control.db.QueryRowContext(tb.Context(), `SELECT COUNT(*) FROM edges WHERE to_id = ? AND kind = ? AND id > ?`, f.target, "calls", r.last).Scan(&r.after); err != nil {
		tb.Fatal(err)
	}
	if r.before != selected*f.edgesPerGeneration+1 || r.after != (len(f.generations)-selected-1)*f.edgesPerGeneration {
		tb.Fatalf("fixture does not expose expected incoming-index range: %+v", r)
	}
	return r
}

func checkIncomingGenerationFanout(tb testing.TB, f incomingGenerationFanoutFixture, selected int, page graph.BoundedIncomingSourceProjection, err error) {
	tb.Helper()
	if err != nil {
		tb.Fatalf("selected generation query refused: %v", err)
	}
	if page.Truncated[f.target] != f.distinct {
		tb.Fatalf("limit1 truncation: %+v", page)
	}
	if f.distinct {
		if len(page.Sources[f.target]) != 0 {
			tb.Fatal("distinct sentinel leaked a partial source set")
		}
	} else if !reflect.DeepEqual(page.Sources[f.target], []string{incomingGenerationFanoutSource(selected, 0)}) {
		tb.Fatalf("wrong generation source set: %+v", page)
	}
	for target := range page.Sources {
		if target != f.target {
			tb.Fatal("unrequested source target")
		}
	}
	for target := range page.Truncated {
		if target != f.target {
			tb.Fatal("unrequested truncation target")
		}
	}
}

func TestIncomingSourceGenerationFanoutSeparatesSelectedRowsFromIndexRanges(t *testing.T) {
	for _, generations := range []int{1, 10, 32} {
		for _, distinct := range []bool{false, true} {
			t.Run(fmt.Sprintf("generations_%d_distinct_%t", generations, distinct), func(t *testing.T) {
				f := prepareIncomingGenerationFanout(t, generations, 256, distinct)
				for _, selected := range []int{0, generations - 1} {
					budget := &graph.IncomingSourceBudget{}
					page, err := f.control.AtGeneration(f.generations[selected]).FindIncomingSourcesScoped(t.Context(), []string{f.target}, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, budget)
					checkIncomingGenerationFanout(t, f, selected, page, err)
					wantSelectedRows := f.edgesPerGeneration
					if distinct {
						wantSelectedRows = 2
					}
					charged := graph.MaxIncomingSourceCandidateRows - budget.Remaining()
					if charged != wantSelectedRows {
						t.Fatalf("selected physical row budget=%d, want%d", charged, wantSelectedRows)
					}
					r := f.selectedRange(t, selected)
					t.Logf("selected=%d generation=%d selected_rows=%d endpoint_prefix_rows=%d endpoint_tail_rows=%d (range counts, NOT measured VM visits)", selected, f.generations[selected], charged, r.before, r.after)
				}
			})
		}
	}
}

func incomingGenerationFanoutPlans(t *testing.T, db *sql.DB, query string, target string, generation int64) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, target, "calls", int64(0), generation, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var details []string
	for rows.Next() {
		var node, parent, unused int
		var detail string
		if err := rows.Scan(&node, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "\n")
}

func incomingGenerationFanoutRawProbe(t *testing.T, db *sql.DB, query string, target string, generation int64) []incomingSourcePageRow {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query, target, "calls", int64(0), generation, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []incomingSourcePageRow
	for rows.Next() {
		var row incomingSourcePageRow
		if err := rows.Scan(&row.id, &row.candidate.From, &row.candidate.FilePath); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestIncomingSourceGenerationFanoutRecordsExistingIndexAlternatives(t *testing.T) {
	f := prepareIncomingGenerationFanout(t, 32, 256, true)
	// Only this untimed diagnostic needs a statistics-writing connection. Do
	// not assume the configured Store read pool permits ANALYZE. The database
	// is the exact disposable Store fixture, never a copied or live store.
	diagnostic, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = diagnostic.Close() })
	if strings.Count(scopedIncomingSourcePageSQL, "FROM edges INDEXED BY edges_by_to") != 1 || strings.Count(scopedIncomingSourcePageSQL, "AND view_gen = ?") != 1 {
		t.Fatal("frozen query shape changed; update this characterization explicitly")
	}
	// These are PRIVATE diagnostic statements, never a production dispatch.
	// The partial generation index exists already; no new DDL is introduced.
	positiveUnforced := strings.Replace(scopedIncomingSourcePageSQL, "FROM edges INDEXED BY edges_by_to", "FROM edges", 1)
	positiveUnforced = strings.Replace(positiveUnforced, "AND view_gen = ?", "AND view_gen = ? AND view_gen > 0", 1)
	positiveForced := strings.Replace(positiveUnforced, "FROM edges", "FROM edges INDEXED BY edges_by_generation", 1)
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			// Statistics affect only this disposable fixture. Record both states;
			// do not extrapolate the unforced planner choice to a live database.
			if _, err := diagnostic.ExecContext(t.Context(), "ANALYZE edges"); err != nil {
				t.Fatal(err)
			}
		}
		for _, selected := range []int{0, len(f.generations) - 1} {
			id := f.generations[selected]
			want := incomingGenerationFanoutRawProbe(t, diagnostic, scopedIncomingSourcePageSQL, f.target, id)
			if len(want) != 2 || want[0].candidate.From != incomingGenerationFanoutSource(selected, 0) || want[1].candidate.From != incomingGenerationFanoutSource(selected, 1) {
				t.Fatalf("current raw semantic prerequisite: %+v", want)
			}
			for _, shape := range []struct{ name, query string }{{"current_forced_incoming", scopedIncomingSourcePageSQL}, {"positive_unforced", positiveUnforced}, {"positive_forced_existing_generation", positiveForced}} {
				got := incomingGenerationFanoutRawProbe(t, diagnostic, shape.query, f.target, id)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("diagnostic%s changed selected rows: %+v want%+v", shape.name, got, want)
				}
				plan := incomingGenerationFanoutPlans(t, diagnostic, shape.query, f.target, id)
				if shape.name == "current_forced_incoming" && !strings.Contains(plan, "edges_by_to") {
					t.Fatalf("current plan no longer forced incoming: %s", plan)
				}
				if shape.name == "positive_forced_existing_generation" && !strings.Contains(plan, "edges_by_generation") {
					t.Fatalf("positive existing index unavailable: %s", plan)
				}
				t.Logf("analyzed=%t selected=%d shape=%s plan=%s", analyzed, selected, shape.name, plan)
			}
		}
	}
	f.assertDiskBudget(t)
}

var incomingGenerationFanoutBenchmarkPage graph.BoundedIncomingSourceProjection
var incomingGenerationFanoutBenchmarkError error

// A separate24-case measurement of the actual Store scoped projection. Setup,
// publication, first query, semantic controls, and range counts are untimed.
// No optional alternative SQL above is used by this benchmark.
func BenchmarkIncomingSourceGenerationFanout(b *testing.B) {
	for _, generations := range []int{1, 10, 32} {
		for _, edgesPerGeneration := range []int{256, 1024} {
			for _, distinct := range []bool{false, true} {
				b.Run(fmt.Sprintf("generations_%d_edges_%d_distinct_%t", generations, edgesPerGeneration, distinct), func(b *testing.B) {
					f := prepareIncomingGenerationFanout(b, generations, edgesPerGeneration, distinct)
					for _, selection := range []struct {
						name  string
						index int
					}{{"oldest", 0}, {"newest", generations - 1}} {
						b.Run(selection.name, func(b *testing.B) {
							handle := f.control.AtGeneration(f.generations[selection.index])
							ids := []string{f.target}
							ctx := b.Context()
							page, err := handle.FindIncomingSourcesScoped(ctx, ids, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, nil)
							checkIncomingGenerationFanout(b, f, selection.index, page, err)
							r := f.selectedRange(b, selection.index)
							failures := 0
							b.ReportAllocs()
							b.ResetTimer()
							for i := 0; i < b.N; i++ {
								page, err = handle.FindIncomingSourcesScoped(ctx, ids, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, nil)
								if err != nil {
									failures++
								}
							}
							b.StopTimer()
							incomingGenerationFanoutBenchmarkPage, incomingGenerationFanoutBenchmarkError = page, err
							checkIncomingGenerationFanout(b, f, selection.index, page, err)
							if failures != 0 {
								b.Fatalf("%d measured selected-generation queries failed", failures)
							}
							if handle.EdgeCount() != edgesPerGeneration {
								b.Fatal("query changed selected payload")
							}
							b.ReportMetric(float64(generations), "retained-generations")
							b.ReportMetric(float64(edgesPerGeneration), "selected-edges")
							b.ReportMetric(float64(r.before), "endpoint-prefix-rows")
							b.ReportMetric(float64(r.after), "endpoint-tail-rows")
							b.ReportMetric(float64(f.assertDiskBudget(b)), "fixture-disk-bytes")
						})
					}
				})
			}
		}
	}
}
