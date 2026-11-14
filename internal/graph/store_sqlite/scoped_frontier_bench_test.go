package store_sqlite

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Run with -run '^$' -bench BenchmarkScopedFileFrontier -benchtime=1x.
// Both paths include edge decoding and endpoint hydration. Fixture construction
// is outside the timer. The legacy pager remains the repository-scope pager.
func BenchmarkScopedFileFrontier(b *testing.B) {
	for _, canonical := range []bool{true, false} {
		b.Run(fmt.Sprintf("canonical=%t", canonical), func(b *testing.B) {
			store, err := Open(filepath.Join(b.TempDir(), "synthetic.sqlite"))
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			const perFile = 20
			fileCount := 6000
			if value := os.Getenv("GORTEX_BENCH_FRONTIER_FILES"); value != "" {
				fileCount, err = strconv.Atoi(value)
				if err != nil || fileCount < 1 {
					b.Fatal("GORTEX_BENCH_FRONTIER_FILES must be a positive integer")
				}
			}
			files := make([]string, fileCount)
			nodes := make([]*graph.Node, 0, fileCount)
			edges := make([]*graph.Edge, 0, fileCount*perFile)
			for i := range files {
				file := fmt.Sprintf("fixture/f%05d.go", i)
				files[i] = file
				id := file + "::caller"
				nodes = append(nodes, &graph.Node{ID: id, Name: "caller", Kind: graph.KindFunction, FilePath: file, RepoPrefix: "fixture"})
				for line := 1; line <= perFile; line++ {
					provenance := file
					if !canonical {
						provenance = ""
					}
					edges = append(edges, &graph.Edge{From: id, To: "unresolved::callee", Kind: graph.EdgeCalls,
						FilePath: provenance, Line: line, Meta: map[string]any{"callee": "callee", "fixture": true}})
				}
			}
			if err := store.AddBatchChecked(nodes, edges); err != nil {
				b.Fatal(err)
			}
			maxID, err := store.edgeGenerationHighWater()
			if err != nil {
				b.Fatal(err)
			}
			query, args, _ := scopedEdgeProjectionQuery([]string{"fixture"}, files, string(graph.EdgeCalls), store.viewGen)
			if !canonical {
				query, args, _ = scopedEdgeSourceProjectionQuery([]string{"fixture"}, files, string(graph.EdgeCalls), store.viewGen, false)
			}
			for _, legacy := range []bool{true, false} {
				b.Run(fmt.Sprintf("legacy=%t", legacy), func(b *testing.B) {
					b.ReportAllocs()
					b.ReportMetric(float64(fileCount*perFile), "rows/op")
					for range b.N {
						count := 0
						yield := func(row graph.ScopedEdgeRow) bool {
							if row.Edge == nil || row.Source == nil {
								b.Fatal("missing edge or endpoint")
							}
							count++
							return true
						}
						if legacy {
							store.streamScopedEdges(query, args, maxID, yield)
						} else {
							store.EdgesInScopeSeq([]string{"fixture"}, files, graph.EdgeCalls)(yield)
						}
						if count != fileCount*perFile {
							b.Fatalf("rows = %d, want %d", count, fileCount*perFile)
						}
					}
				})
			}
			for _, offset := range []int{0, fileCount * perFile / 2, fileCount*perFile - scopedProjectionPage} {
				if offset < 0 {
					continue
				}
				b.Run(fmt.Sprintf("page-offset=%d", offset), func(b *testing.B) {
					benchmarkScopedIDPage(b, store, offset)
				})
			}
		})
	}
}

// Page measurements include decoding, excluding the shared ID census and node
// hydration, so position-dependent database work is visible independently.
func benchmarkScopedIDPage(b *testing.B, store *Store, offset int) {
	b.Helper()
	rows, err := store.db.Query(`SELECT id FROM edges WHERE view_gen = ? ORDER BY id LIMIT ? OFFSET ?`, store.viewGen, scopedProjectionPage, offset)
	if err != nil {
		b.Fatal(err)
	}
	args := make([]any, 0, scopedProjectionPage+1)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			b.Fatal(err)
		}
		args = append(args, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(args) != scopedProjectionPage {
		b.Fatalf("page inventory: count=%d err=%v", len(args), err)
	}
	args = append(args, store.viewGen)
	query := scopedEdgeIDPageQuery(scopedProjectionPage)
	b.ResetTimer()
	for range b.N {
		rows, err := store.db.Query(query, args...)
		if err != nil {
			b.Fatal(err)
		}
		count := 0
		for rows.Next() {
			if _, err := store.scanEdgeCursor(rows); err != nil {
				rows.Close()
				b.Fatal(err)
			}
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil || count != scopedProjectionPage {
			b.Fatalf("page read: count=%d err=%v", count, err)
		}
	}
}
