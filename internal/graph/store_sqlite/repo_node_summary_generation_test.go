package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

var legacyNodeSummaryQuery = "SELECT " + lookupNodeSummaryCols + " FROM nodes WHERE repo_prefix = ? AND language = ? AND view_gen = ?"

func summaryGenerationFixture(tb testing.TB, baseNodes, siblingNodes, targetNodes int) (*Store, *Store) {
	tb.Helper()
	store, err := Open(filepath.Join(tb.TempDir(), "node-summary.sqlite"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := store.Close(); err != nil {
			tb.Error(err)
		}
	})
	add := func(handle *Store, count int, marker string) {
		nodes := make([]*graph.Node, 0, count)
		for i := range count {
			file := fmt.Sprintf("repo/pkg/file%06d.go", i)
			nodes = append(nodes, &graph.Node{
				ID: file + "::Node", Kind: graph.KindFunction, Name: marker,
				FilePath: file, RepoPrefix: "repo", Language: "go",
				StartLine: 7, EndLine: 9, Meta: map[string]any{"opaque": "not a summary field"},
			})
		}
		handle.AddBatch(nodes, nil)
	}
	add(store, baseNodes, "Base")
	var target *Store
	for i := range 4 {
		req := payloadRequest()
		req.LayerID = fmt.Sprintf("summary-layer-%d", i)
		req.TreeOID = fmt.Sprintf("summary-tree-%d", i)
		_, handle, err := store.BeginPayloadGeneration(context.Background(), req)
		if err != nil {
			tb.Fatal(err)
		}
		if i == 3 {
			add(handle, targetNodes, "Target")
			target = handle
		} else {
			add(handle, siblingNodes, "Sibling")
		}
	}
	return store, target
}

func TestRepoNodeSummariesIsolatePositiveGeneration(t *testing.T) {
	store, target := summaryGenerationFixture(t, 20, 10, 3)
	target.AddBatch([]*graph.Node{
		{ID: "repo/other.py::Other", Kind: graph.KindFunction, Name: "WrongLanguage", FilePath: "repo/other.py", RepoPrefix: "repo", Language: "python"},
		{ID: "elsewhere/a.go::Other", Kind: graph.KindFunction, Name: "WrongRepo", FilePath: "elsewhere/a.go", RepoPrefix: "elsewhere", Language: "go"},
		{ID: "unscoped.go::Other", Kind: graph.KindFunction, Name: "Unscoped", FilePath: "unscoped.go", Language: "go"},
	}, nil)
	got := target.GetRepoNodeSummariesByLanguage("repo", "go")
	if len(got) != 3 {
		t.Fatalf("target has %d summaries, want 3", len(got))
	}
	for _, node := range got {
		if node.Name != "Target" || node.StartLine != 7 || node.Meta != nil {
			t.Fatalf("incorrect target summary: %+v", node)
		}
	}
	base := store.GetRepoNodeSummariesByLanguage("repo", "go")
	if len(base) != 20 {
		t.Fatalf("generation zero has %d summaries, want 20", len(base))
	}
	for _, node := range base {
		if node.Name != "Base" {
			t.Fatalf("generation zero leaked target summary: %+v", node)
		}
	}
	if nodes := target.GetRepoNodeSummariesByLanguage("", "go"); len(nodes) != 1 || nodes[0].Name != "Unscoped" {
		t.Fatalf("empty repository scope changed: %+v", nodes)
	}
}

func TestRepoNodeSummariesWithoutGenerationIndex(t *testing.T) {
	store, target := summaryGenerationFixture(t, 20, 10, 3)
	ids := func(nodes []*graph.Node) []string {
		var out []string
		for _, node := range nodes {
			out = append(out, node.ID)
		}
		slices.Sort(out)
		return out
	}
	want := ids(target.GetRepoNodeSummariesByLanguage("repo", "go"))
	if _, err := store.writerDB.Exec("DROP INDEX nodes_by_generation"); err != nil {
		t.Fatal(err)
	}
	got := ids(target.GetRepoNodeSummariesByLanguage("repo", "go"))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("index-absent result changed: %v != %v", got, want)
	}
}

func TestNodeSummaryPositivePredicateMakesGenerationIndexEligible(t *testing.T) {
	store, target := summaryGenerationFixture(t, 2000, 200, 3)
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			if _, err := store.writerDB.Exec("ANALYZE nodes"); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := store.db.Query("EXPLAIN QUERY PLAN "+target.repoNodeSummariesByLanguageQuery(), "repo", "go", target.viewGen)
		if err != nil {
			t.Fatal(err)
		}
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		plan := strings.Join(details, "\n")
		if !strings.Contains(plan, "nodes_by_generation") {
			t.Fatalf("analyzed=%v: positive generation is not bounded by its index:\n%s", analyzed, plan)
		}
	}
}

func BenchmarkGenerationScopedNodeSummary(b *testing.B) {
	for _, baseNodes := range []int{1000, 20000} {
		b.Run(fmt.Sprintf("base_%d", baseNodes), func(b *testing.B) {
			_, target := summaryGenerationFixture(b, baseNodes, 1000, 10)
			legacy := func() []*graph.Node {
				rows, err := target.db.Query(legacyNodeSummaryQuery, "repo", "go", target.viewGen)
				if err != nil {
					b.Fatal(err)
				}
				defer rows.Close()
				var out []*graph.Node
				for rows.Next() {
					node, err := scanNodeSummary(rows)
					if err != nil {
						b.Fatal(err)
					}
					out = append(out, node)
				}
				if err := rows.Err(); err != nil {
					b.Fatal(err)
				}
				return out
			}
			for _, method := range []string{"production", "legacy_sql"} {
				b.Run(method, func(b *testing.B) {
					read := legacy
					if method == "production" {
						read = func() []*graph.Node { return target.GetRepoNodeSummariesByLanguage("repo", "go") }
					}
					if got := read(); len(got) != 10 {
						b.Fatalf("got %d summaries, want 10", len(got))
					}
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						if got := read(); len(got) != 10 {
							b.Fatalf("got %d summaries, want 10", len(got))
						}
					}
					b.StopTimer()
					b.ReportMetric(10, "nodes/op")
				})
			}
		})
	}
}
