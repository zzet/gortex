package store_sqlite

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// withoutJSModuleIndex opens stores without edges_by_js_module for the rest of
// the benchmark, so the two variants differ in that index alone.
func withoutJSModuleIndex(b *testing.B) {
	b.Helper()
	saved := bulkDroppableIndexes
	bulkDroppableIndexes = slices.DeleteFunc(slices.Clone(saved), func(idx bulkDroppableIndex) bool {
		return idx.name == "edges_by_js_module"
	})
	b.Cleanup(func() { bulkDroppableIndexes = saved })
}

// goShapedCorpus is a non-JS graph: every edge is a Go edge, and like real Go
// edges a share of them carry JSON metadata the index expression must parse.
func goShapedCorpus(files, symbolsPerFile, edgesPerSymbol int) ([]*graph.Node, []*graph.Edge) {
	var nodes []*graph.Node
	var edges []*graph.Edge
	for f := range files {
		file := fmt.Sprintf("repo/pkg%d/file%d.go", f%40, f)
		nodes = append(nodes, &graph.Node{ID: file, Kind: graph.KindFile, Name: filepath.Base(file), FilePath: file, RepoPrefix: "repo", Language: "go"})
		for s := range symbolsPerFile {
			id := fmt.Sprintf("%s::Sym%d", file, s)
			nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("Sym%d", s), FilePath: file, RepoPrefix: "repo", Language: "go", StartLine: s*10 + 1, EndLine: s*10 + 9})
			edges = append(edges, &graph.Edge{From: file, To: id, Kind: graph.EdgeDefines, FilePath: file, Line: s*10 + 1})
			for e := range edgesPerSymbol {
				target := fmt.Sprintf("repo/pkg%d/file%d.go::Sym%d", (f+e+1)%40, (f+e+1)%files, (s+e)%symbolsPerFile)
				edge := &graph.Edge{From: id, To: target, Kind: graph.EdgeCalls, FilePath: file, Line: s*10 + 2 + e}
				switch e % 3 {
				case 0:
					edge.Meta = map[string]any{"return_usage": "assigned"}
				case 1:
					edge.Meta = map[string]any{"receiver_type": "*Store"}
				}
				edges = append(edges, edge)
			}
		}
	}
	return nodes, edges
}

func addCorpus(b *testing.B, store *Store, nodes []*graph.Node, edges []*graph.Edge) {
	b.Helper()
	const chunk = 5000
	for start := 0; start < len(edges); start += chunk {
		end := min(start+chunk, len(edges))
		var batch []*graph.Node
		if start == 0 {
			batch = nodes
		}
		store.AddBatch(batch, edges[start:end])
	}
}

// BenchmarkJSModuleIndexCost measures what edges_by_js_module adds to a
// repository with no JS/TS: a cold bulk load (the full index's write side) and
// a one-file save (evict and re-add one file's rows on a warm store).
func BenchmarkJSModuleIndexCost(b *testing.B) {
	nodes, edges := goShapedCorpus(1500, 12, 10) // 19.5k nodes, 198k edges
	for _, variant := range []string{"with_index", "without_index"} {
		b.Run(variant+"/full_load", func(b *testing.B) {
			if variant == "without_index" {
				withoutJSModuleIndex(b)
			}
			b.ReportMetric(float64(len(edges)), "edges/op")
			for b.Loop() {
				b.StopTimer()
				store, err := Open(filepath.Join(b.TempDir(), "graph.sqlite"))
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				store.BeginBulkLoad()
				addCorpus(b, store, nodes, edges)
				if err := store.FlushBulk(); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if err := store.Close(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
		b.Run(variant+"/save", func(b *testing.B) {
			if variant == "without_index" {
				withoutJSModuleIndex(b)
			}
			store, err := Open(filepath.Join(b.TempDir(), "graph.sqlite"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = store.Close() })
			store.BeginBulkLoad()
			addCorpus(b, store, nodes, edges)
			if err := store.FlushBulk(); err != nil {
				b.Fatal(err)
			}
			const file = "repo/pkg7/file7.go"
			var fileNodes []*graph.Node
			var fileEdges []*graph.Edge
			for _, n := range nodes {
				if n.FilePath == file {
					fileNodes = append(fileNodes, n)
				}
			}
			for _, e := range edges {
				if e.FilePath == file {
					fileEdges = append(fileEdges, e)
				}
			}
			b.ReportMetric(float64(len(fileEdges)), "edges/op")
			for b.Loop() {
				store.EvictFiles([]string{file})
				store.AddBatch(fileNodes, fileEdges)
			}
		})
	}
}
