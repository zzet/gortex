package store_sqlite

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestScopedFileFrontierOrderingGenerationAndDeletedPage(t *testing.T) {
	for _, canonical := range []bool{true, false} {
		t.Run(fmt.Sprintf("canonical=%t", canonical), func(t *testing.T) {
			store := openScopedProjectionTestStore(t)
			current := store.AtGeneration(41)
			stale := store.AtGeneration(40)
			const count = 3*scopedProjectionPage + 11
			files := []string{"repo/b.go", "repo/a.go"}
			nodes := []*graph.Node{
				{ID: "a", Kind: graph.KindFunction, FilePath: files[1], RepoPrefix: "repo"},
				{ID: "b", Kind: graph.KindFunction, FilePath: files[0], RepoPrefix: "repo"},
			}
			edges := make([]*graph.Edge, 0, count)
			for i := 1; i <= count; i++ {
				n := nodes[i%2]
				file := n.FilePath
				if !canonical {
					file = ""
				}
				edges = append(edges, &graph.Edge{From: n.ID, To: "unresolved::target", Kind: graph.EdgeCalls,
					FilePath: file, Line: i, Meta: map[string]any{"ordinal": i}})
			}
			if err := stale.AddBatchChecked(nodes, edges); err != nil {
				t.Fatal(err)
			}
			for _, node := range nodes {
				node.Name = "current-generation"
			}
			if err := current.AddBatchChecked(nodes, edges); err != nil {
				t.Fatal(err)
			}
			maxID, err := current.edgeGenerationHighWater()
			if err != nil {
				t.Fatal(err)
			}
			if got := current.scopedEdgeFileProvenanceCanonical([]string{"repo"}, files, []string{"calls"}, maxID); got != canonical {
				t.Fatalf("fixture selected canonical=%t, want %t", got, canonical)
			}
			query, args, _ := scopedEdgeProjectionQuery([]string{"repo"}, files, "calls", 41)
			if !canonical {
				query, args, _ = scopedEdgeSourceProjectionQuery([]string{"repo"}, files, "calls", 41, false)
			}
			var want, got []graph.ScopedEdgeRow
			current.streamScopedEdges(query, args, maxID, func(row graph.ScopedEdgeRow) bool {
				want = append(want, row)
				return true
			})
			current.EdgesInScopeSeq([]string{"repo"}, files, graph.EdgeCalls)(func(row graph.ScopedEdgeRow) bool {
				if row.Source == nil || row.Source.Name != "current-generation" {
					t.Fatalf("wrong source generation: %#v", row.Source)
				}
				got = append(got, row)
				return true
			})
			if len(got) != count || !reflect.DeepEqual(got, want) {
				t.Fatalf("full ordered rows differ: got=%d want=%d", len(got), len(want))
			}
			store.db.SetMaxOpenConns(1)
			deletedLines := make(map[int]bool, scopedProjectionPage)
			for _, row := range want[scopedProjectionPage : 2*scopedProjectionPage] {
				deletedLines[row.Edge.Line] = true
			}
			var surviving []int
			for row := range current.EdgesInScopeSeq([]string{"repo"}, files, graph.EdgeCalls) {
				if len(surviving) == 0 {
					// Remove an entire future page. A short/empty PK fetch must
					// not terminate the frozen ID inventory before later pages.
					if _, err := store.writerDB.Exec(`DELETE FROM edges WHERE id IN (SELECT id FROM edges WHERE view_gen = 41 ORDER BY id LIMIT ? OFFSET ?)`, scopedProjectionPage, scopedProjectionPage); err != nil {
						t.Fatal(err)
					}
					current.AddEdge(&graph.Edge{From: "a", To: "unresolved::new", Kind: graph.EdgeCalls, FilePath: files[1], Line: count + 1})
				}
				surviving = append(surviving, row.Edge.Line)
			}
			var wantLines []int
			for _, row := range want {
				if !deletedLines[row.Edge.Line] {
					wantLines = append(wantLines, row.Edge.Line)
				}
			}
			if !reflect.DeepEqual(surviving, wantLines) {
				t.Fatalf("surviving ordered rows differ: got=%d want=%d", len(surviving), len(wantLines))
			}
		})
	}
}

func TestScopedFileFrontierIDPagesDoNotSort(t *testing.T) {
	store := openScopedProjectionTestStore(t)
	for _, count := range []int{1, 17, scopedProjectionPage} {
		args := make([]any, count+1)
		for i := range count {
			args[i] = int64(i + 1)
		}
		args[count] = int64(41)
		plan := scopedProjectionPlan(t, store, scopedEdgeIDPageQuery(count), args...)
		if strings.Contains(plan, "TEMP B-TREE") || strings.Contains(plan, "SCAN e") {
			t.Fatalf("ID page must use ordered seeks, count=%d:\n%s", count, plan)
		}
	}
}

func TestScopedFileFrontierDropsDeletedSourcesBetweenPages(t *testing.T) {
	for _, canonical := range []bool{true, false} {
		t.Run(fmt.Sprintf("canonical=%t", canonical), func(t *testing.T) {
			store := openScopedProjectionTestStore(t)
			var nodes []*graph.Node
			var edges []*graph.Edge
			var files []string
			for _, id := range []string{"a", "b", "c"} {
				file := "repo/" + id + ".go"
				files = append(files, file)
				nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, FilePath: file, RepoPrefix: "repo"})
				for line := 1; line <= scopedProjectionPage; line++ {
					provenance := file
					if !canonical {
						provenance = ""
					}
					edges = append(edges, &graph.Edge{From: id, To: "unresolved::callee", Kind: graph.EdgeCalls, FilePath: provenance, Line: line})
				}
			}
			store.AddBatch(nodes, edges)
			seen := 0
			for row := range store.EdgesInScopeSeq([]string{"repo"}, files, graph.EdgeCalls) {
				if seen == 0 {
					if _, err := store.writerDB.Exec(`DELETE FROM nodes WHERE id = 'b' AND view_gen = 0`); err != nil {
						t.Fatal(err)
					}
				}
				if row.Source == nil || row.Edge.From == "b" {
					t.Fatal("yielded an edge whose source disappeared before its page")
				}
				seen++
			}
			if seen != 2*scopedProjectionPage {
				t.Fatalf("rows=%d, want %d", seen, 2*scopedProjectionPage)
			}
		})
	}
}
