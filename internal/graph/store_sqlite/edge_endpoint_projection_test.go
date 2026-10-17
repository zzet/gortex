package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// endpointFixtureGenerations are the payloads the endpoint fixture writes: the
// base corpus and one derived generation holding a different edge set at the
// same paths, so a projection that leaks across generations answers wrong.
var endpointFixtureGenerations = []int64{0, 3}

// writeEndpointFixture writes, per generation, three files of symbols with
// edges of several kinds recorded in the symbol's own file, in another file,
// and in a file neither endpoint lives in.
func writeEndpointFixture(t *testing.T, s *Store) {
	t.Helper()
	for _, generation := range endpointFixtureGenerations {
		var nodes []*graph.Node
		var edges []*graph.Edge
		for f := 0; f < 3; f++ {
			file := fmt.Sprintf("pkg/f%d.go", f)
			for n := 0; n < 4; n++ {
				id := fmt.Sprintf("%s::Sym%d", file, n)
				nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("Sym%d_g%d", n, generation), FilePath: file})
			}
		}
		// One nameless node: NodeNamesByIDs must report it, with "".
		nodes = append(nodes, &graph.Node{ID: "pkg/f2.go::anon", Kind: graph.KindFunction, FilePath: "pkg/f2.go"})
		kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences, graph.EdgeImports, graph.EdgeDefines, graph.EdgeImplements}
		line := 1
		for f := 0; f < 3; f++ {
			for n := 0; n < 4; n++ {
				from := fmt.Sprintf("pkg/f%d.go::Sym%d", f, n)
				for k, kind := range kinds {
					to := fmt.Sprintf("pkg/f%d.go::Sym%d", (f+1+k)%3, (n+k+int(generation))%4)
					if kind == graph.EdgeImports {
						to = "unresolved::import::example.com/mod"
					}
					// Record some edges in the target's file (a value flowing
					// out of a callee is recorded at the caller's site) and
					// some in a third file.
					recorded := fmt.Sprintf("pkg/f%d.go", f)
					switch (n + k) % 3 {
					case 1:
						recorded = fmt.Sprintf("pkg/f%d.go", (f+1+k)%3)
					case 2:
						recorded = "pkg/other.go"
					}
					edges = append(edges, &graph.Edge{From: from, To: to, Kind: kind, FilePath: recorded, Line: line})
					line++
				}
			}
		}
		// An edge recorded in f0 whose endpoints both live elsewhere.
		edges = append(edges, &graph.Edge{From: "pkg/f1.go::Sym0", To: "pkg/f2.go::Sym1", Kind: graph.EdgeReads, FilePath: "pkg/f0.go", Line: line})
		if generation > 0 {
			// Present only in the derived generation.
			edges = append(edges, &graph.Edge{From: "pkg/f0.go::Sym0", To: "stdlib::fmt.Println", Kind: graph.EdgeCalls, FilePath: "pkg/f0.go", Line: line + 1})
		}
		if err := s.AtGeneration(generation).AddBatchChecked(nodes, edges); err != nil {
			t.Fatalf("write generation %d: %v", generation, err)
		}
	}
}

func endpointOf(e *graph.Edge) graph.EdgeEndpointRow {
	return graph.EdgeEndpointRow{From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath}
}

func sortEndpoints(in []graph.EdgeEndpointRow) []graph.EdgeEndpointRow {
	out := append([]graph.EdgeEndpointRow(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.FilePath < b.FilePath
	})
	return out
}

// TestEdgeEndpointProjectionsMatchTheFullRowReaders checks every projection
// against the full-row reader it replaces, on the base corpus and on a
// derived generation handle.
func TestEdgeEndpointProjectionsMatchTheFullRowReaders(t *testing.T) {
	s, _ := openTempStore(t)
	writeEndpointFixture(t, s)

	for _, generation := range endpointFixtureGenerations {
		h := s.AtGeneration(generation)
		t.Run(fmt.Sprintf("generation_%d", generation), func(t *testing.T) {
			all := h.AllEdges()
			if len(all) == 0 {
				t.Fatal("fixture wrote no edges")
			}
			paths := []string{"pkg/f0.go", "pkg/f2.go", "pkg/f0.go", ""}
			recorded := map[string]bool{"pkg/f0.go": true, "pkg/f2.go": true}

			// EdgeEndpointsRecordedAt == every row recorded at the paths.
			var want []graph.EdgeEndpointRow
			for _, e := range all {
				if recorded[e.FilePath] {
					want = append(want, endpointOf(e))
				}
			}
			got := h.EdgeEndpointsRecordedAt(paths)
			if !reflect.DeepEqual(sortEndpoints(got), sortEndpoints(want)) {
				t.Fatalf("EdgeEndpointsRecordedAt:\n got %v\nwant %v", sortEndpoints(got), sortEndpoints(want))
			}
			// ...and it covers what the two full-row readers return for the
			// files' own symbols, filtered to the recording files.
			ids := builderLikeSeedIDs(h, []string{"pkg/f0.go", "pkg/f2.go"})
			gotSet := make(map[graph.EdgeEndpointRow]bool, len(got))
			for _, e := range got {
				gotSet[e] = true
			}
			for _, read := range []map[string][]*graph.Edge{h.GetOutEdgesByNodeIDs(ids), h.GetInEdgesByNodeIDs(ids)} {
				for _, edges := range read {
					for _, e := range edges {
						if recorded[e.FilePath] && !gotSet[endpointOf(e)] {
							t.Fatalf("full-row reader row %v recorded at a requested path is missing from the projection", endpointOf(e))
						}
					}
				}
			}

			// EdgeEndpointsFrom == GetOutEdgesByNodeIDs, whole and kind-filtered.
			full := h.GetOutEdgesByNodeIDs(ids)
			for _, kinds := range [][]graph.EdgeKind{nil, {graph.EdgeCalls, graph.EdgeImports, graph.EdgeCalls}, {graph.EdgeReads}} {
				keep := map[graph.EdgeKind]bool{}
				for _, k := range kinds {
					keep[k] = true
				}
				var want []graph.EdgeEndpointRow
				for _, edges := range full {
					for _, e := range edges {
						if len(kinds) == 0 || keep[e.Kind] {
							want = append(want, endpointOf(e))
						}
					}
				}
				got := h.EdgeEndpointsFrom(append(ids, ids[0], ""), kinds)
				if !reflect.DeepEqual(sortEndpoints(got), sortEndpoints(want)) {
					t.Fatalf("EdgeEndpointsFrom(kinds=%v):\n got %v\nwant %v", kinds, sortEndpoints(got), sortEndpoints(want))
				}
			}
			if got := h.EdgeEndpointsFrom(ids, []graph.EdgeKind{""}); got != nil {
				t.Fatalf("only-empty kinds must match nothing, got %v", got)
			}

			// NodeNamesByIDs == GetNodesByIDs reduced to names.
			probe := append(append([]string(nil), ids...), "pkg/missing.go::Nope", "pkg/f2.go::anon")
			wantNames := map[string]string{}
			for id, n := range h.GetNodesByIDs(probe) {
				wantNames[id] = n.Name
			}
			if gotNames := h.NodeNamesByIDs(probe); !reflect.DeepEqual(gotNames, wantNames) {
				t.Fatalf("NodeNamesByIDs:\n got %v\nwant %v", gotNames, wantNames)
			}
			if _, ok := wantNames["pkg/f2.go::anon"]; !ok {
				t.Fatal("fixture lost its nameless node")
			}

			// OutEdgePathsFrom == distinct sorted recording files of
			// GetOutEdgesByNodeIDs.
			sources := append(append([]string(nil), ids...), "pkg/f1.go::Sym0", "pkg/missing.go::Nope")
			wantPaths := map[string][]string{}
			for from, edges := range h.GetOutEdgesByNodeIDs(sources) {
				seen := map[string]bool{}
				for _, e := range edges {
					if !seen[e.FilePath] {
						seen[e.FilePath] = true
						wantPaths[from] = append(wantPaths[from], e.FilePath)
					}
				}
				sort.Strings(wantPaths[from])
			}
			if gotPaths := h.OutEdgePathsFrom(sources); !reflect.DeepEqual(gotPaths, wantPaths) {
				t.Fatalf("OutEdgePathsFrom:\n got %v\nwant %v", gotPaths, wantPaths)
			}
		})
	}

	// Generation isolation: the derived-only edge is invisible from the base.
	derivedOnly := graph.EdgeEndpointRow{From: "pkg/f0.go::Sym0", To: "stdlib::fmt.Println", Kind: graph.EdgeCalls, FilePath: "pkg/f0.go"}
	for _, generation := range endpointFixtureGenerations {
		found := false
		for _, e := range s.AtGeneration(generation).EdgeEndpointsRecordedAt([]string{"pkg/f0.go"}) {
			if e == derivedOnly {
				found = true
			}
		}
		if found != (generation > 0) {
			t.Fatalf("generation %d: derived-only edge visible=%v", generation, found)
		}
	}

	// Empty inputs.
	if s.EdgeEndpointsRecordedAt(nil) != nil || s.EdgeEndpointsFrom(nil, nil) != nil ||
		s.NodeNamesByIDs([]string{""}) != nil || s.OutEdgePathsFrom(nil) != nil {
		t.Fatal("empty inputs must answer nil")
	}
}

// builderLikeSeedIDs is the id set the affected-closure walk seeds from: every
// node the base layer places in the paths.
func builderLikeSeedIDs(s *Store, paths []string) []string {
	var ids []string
	for _, p := range paths {
		for _, n := range s.GetFileNodesByPaths([]string{p})[p] {
			ids = append(ids, n.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// TestOutEdgePathsFromAppliesTheStructuralReadFilter writes, below the
// write-side filter, one structurally invalid row per structural kind and one
// valid row of the same shape, and checks every projection drops exactly the
// rows the full-row scanner drops.
func TestOutEdgePathsFromAppliesTheStructuralReadFilter(t *testing.T) {
	s, _ := openTempStore(t)
	if err := s.AddBatchChecked([]*graph.Node{{ID: "a.go::A", Kind: graph.KindType, Name: "A", FilePath: "a.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	structural := []graph.EdgeKind{graph.EdgeImplements, graph.EdgeExtends, graph.EdgeOverrides, graph.EdgeInstantiates, graph.EdgeMemberOf, graph.EdgeCalls}
	line := 1
	for _, kind := range structural {
		for _, target := range []string{"x.go::F#param:p", "y.go::G#local:v", "z.go::Valid"} {
			// Each row in its own file so OutEdgePathsFrom's DISTINCT cannot
			// hide a leaked row behind a valid one.
			file := fmt.Sprintf("rec/%s_%d.go", kind, line)
			if _, err := s.writerDB.Exec(`INSERT INTO edges(view_gen, from_id, to_id, kind, file_path, line) VALUES (0, ?, ?, ?, ?, ?)`,
				"a.go::A", target, string(kind), file, line); err != nil {
				t.Fatalf("raw insert: %v", err)
			}
			line++
		}
	}
	full := s.GetOutEdgesByNodeIDs([]string{"a.go::A"})["a.go::A"]
	for _, e := range full {
		if graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
			t.Fatalf("full-row reader returned a structurally invalid row %v", endpointOf(e))
		}
	}
	// calls keeps all three targets; each structural kind keeps only Valid.
	if want := 3 + 5; len(full) != want {
		t.Fatalf("fixture: full reader kept %d rows, want %d", len(full), want)
	}
	var wantPaths []string
	var wantEndpoints []graph.EdgeEndpointRow
	for _, e := range full {
		wantPaths = append(wantPaths, e.FilePath)
		wantEndpoints = append(wantEndpoints, endpointOf(e))
	}
	sort.Strings(wantPaths)
	if got := s.OutEdgePathsFrom([]string{"a.go::A"})["a.go::A"]; !reflect.DeepEqual(got, wantPaths) {
		t.Fatalf("OutEdgePathsFrom:\n got %v\nwant %v", got, wantPaths)
	}
	if got := s.EdgeEndpointsFrom([]string{"a.go::A"}, nil); !reflect.DeepEqual(sortEndpoints(got), sortEndpoints(wantEndpoints)) {
		t.Fatalf("EdgeEndpointsFrom:\n got %v\nwant %v", sortEndpoints(got), sortEndpoints(wantEndpoints))
	}
	var files []string
	for i := 1; i < line; i++ {
		for _, kind := range structural {
			files = append(files, fmt.Sprintf("rec/%s_%d.go", kind, i))
		}
	}
	if got := s.EdgeEndpointsRecordedAt(files); !reflect.DeepEqual(sortEndpoints(got), sortEndpoints(wantEndpoints)) {
		t.Fatalf("EdgeEndpointsRecordedAt:\n got %v\nwant %v", sortEndpoints(got), sortEndpoints(wantEndpoints))
	}
}

// TestEdgeEndpointProjectionPlanLock locks every endpoint projection to its
// index seek on a derived generation handle and on the base, under no
// statistics (a freshly published generation), the production statistics of
// the receiver-scan store, and an honest ANALYZE.
//
// "PlanLock" is in the name because the Windows CI leg runs only
// -run 'PlanLock|PlansLocked|PlansNeverScan' in this package.
func TestEdgeEndpointProjectionPlanLock(t *testing.T) {
	s, _ := openTempStore(t)
	writeEndpointFixture(t, s)
	// A repository-shaped spread of files, so an honest ANALYZE sees what a
	// real store has: a file holds a sliver of the table and a source a
	// sliver of its generation. On the bare fixture a full scan of the
	// covering unique index is the cheapest plan, which says nothing about a
	// store with a million edges.
	for _, generation := range endpointFixtureGenerations {
		var edges []*graph.Edge
		for f := 0; f < 400; f++ {
			for e := 0; e < 6; e++ {
				edges = append(edges, &graph.Edge{
					From: fmt.Sprintf("spread/f%03d.go::S%d", f, e%3), To: fmt.Sprintf("spread/f%03d.go::T%d", (f+1)%400, e),
					Kind: graph.EdgeCalls, FilePath: fmt.Sprintf("spread/f%03d.go", f), Line: e + 1,
				})
			}
		}
		if err := s.AtGeneration(generation).AddBatchChecked(nil, edges); err != nil {
			t.Fatalf("write spread generation %d: %v", generation, err)
		}
	}

	type lockedQuery struct {
		name   string
		query  string
		args   []any
		want   string
		forbid []string
	}
	queries := func(generation int64) []lockedQuery {
		paths := []any{"pkg/f0.go", "pkg/f2.go"}
		ids := []any{"pkg/f0.go::Sym0", "pkg/f0.go::Sym1", "pkg/f1.go::Sym2"}
		kinds := []any{string(graph.EdgeCalls), string(graph.EdgeImports)}
		recordedArgs := append(append([]any(nil), paths...), generation)
		fromArgs := append(append([]any{generation}, ids...), kinds...)
		namesArgs := append(append([]any(nil), ids...), generation)
		pathsArgs := append([]any{generation}, ids...)
		return []lockedQuery{
			{
				name:   "recorded_at",
				query:  edgeEndpointsRecordedAtPrefix + inPlaceholders(len(paths)) + edgeEndpointsRecordedAtSuffix,
				args:   recordedArgs,
				want:   "USING INDEX edges_by_file (file_path=?)",
				forbid: []string{"SCAN edges", "edges_by_generation"},
			},
			{
				name:   "from_kinds",
				query:  edgeEndpointsFromPrefix + inPlaceholders(len(ids)) + `) AND kind IN (` + inPlaceholders(len(kinds)) + `)`,
				args:   fromArgs,
				want:   "from_id=?",
				forbid: []string{"SCAN edges"},
			},
			{
				name:   "from_all_kinds",
				query:  edgeEndpointsFromPrefix + inPlaceholders(len(ids)) + `)`,
				args:   append([]any{generation}, ids...),
				want:   "from_id=?",
				forbid: []string{"SCAN edges"},
			},
			{
				name:   "node_names",
				query:  nodeNamesByIDsPrefix + inPlaceholders(len(ids)) + `) AND view_gen = ?`,
				args:   namesArgs,
				want:   "USING PRIMARY KEY (id=? AND view_gen=?)",
				forbid: []string{"SCAN nodes"},
			},
			{
				name:   "out_edge_paths",
				query:  outEdgePathsFromPrefix + inPlaceholders(len(ids)) + `)`,
				args:   pathsArgs,
				want:   "from_id=?",
				forbid: []string{"SCAN edges"},
			},
		}
	}

	assertLocked := func(t *testing.T, ctx context.Context, conn *sql.Conn) {
		t.Helper()
		for _, generation := range endpointFixtureGenerations {
			for _, q := range queries(generation) {
				plan := explainOnConn(t, ctx, conn, q.query, q.args...)
				joined := strings.Join(plan, "\n")
				if !strings.Contains(joined, q.want) {
					t.Errorf("generation %d %s: plan does not seek %q:\n%s", generation, q.name, q.want, joined)
				}
				for _, bad := range q.forbid {
					if strings.Contains(joined, bad) {
						t.Errorf("generation %d %s: plan uses %q:\n%s", generation, q.name, bad, joined)
					}
				}
			}
		}
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
}
