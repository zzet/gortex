package resolver

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// importClosureFixture builds a random graph of files in a handful of
// directories: file-level imports (some to placeholders and stubs the closure
// skips), symbol-level re-exports between barrel files (chains and cycles), and
// a few files with no file node (their imports leave from a symbol). Every
// edge source is a node, as in an indexed graph.
func importClosureFixture(seed int64) (*graph.Graph, []string) {
	g := graph.New()
	return g, importClosureFixtureInto(g, seed)
}

func importClosureFixtureInto(g graph.Store, seed int64) []string {
	rng := rand.New(rand.NewSource(seed))
	var files []string
	for i := 0; i < 40; i++ {
		path := fmt.Sprintf("repo/d%d/f%d.ts", rng.Intn(8), i)
		files = append(files, path)
		if i%9 != 0 { // some files have no file node
			g.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, Name: path, FilePath: path, RepoPrefix: "repo"})
		}
		g.AddNode(&graph.Node{ID: path + "::S", Kind: graph.KindFunction, Name: "S", FilePath: path, RepoPrefix: "repo"})
	}
	var edges []*graph.Edge
	for i := 0; i < 120; i++ {
		fromIndex := rng.Intn(len(files))
		from := files[fromIndex]
		src := from + "::S"
		if rng.Intn(2) == 0 && fromIndex%9 != 0 { // import from the file node when it exists
			src = from
		}
		to := files[rng.Intn(len(files))]
		switch rng.Intn(6) {
		case 0:
			to = "unresolved::import::x"
		case 1:
			to = "external::pkg"
		default:
			if rng.Intn(2) == 0 {
				to += "::S"
			}
		}
		provenance := from
		if i%7 == 0 {
			// Edge provenance naming another file than the source node's:
			// the caller is still the source node's file.
			provenance = files[rng.Intn(len(files))]
		}
		edges = append(edges, &graph.Edge{From: src, To: to, Kind: graph.EdgeImports, FilePath: provenance, Line: i})
	}
	for i := 0; i < 50; i++ {
		barrel := files[rng.Intn(len(files))]
		target := files[rng.Intn(len(files))]
		edges = append(edges, &graph.Edge{From: barrel + "::S", To: target + "::S", Kind: graph.EdgeReExports,
			FilePath: barrel, Line: 1000 + i})
	}
	g.AddBatch(nil, edges)
	return files
}

// The caller-scoped closure gives every requested file exactly the entry the
// whole-graph closure gives it, and no other file an entry.
func TestImportClosureForCallerFilesMatchesTheWholeGraphEntries(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		var g graph.Store
		var files []string
		if seed%2 == 0 {
			// The production store answers the scoped projections in SQL.
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "closure.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			files = importClosureFixtureInto(store, seed)
			g = store
		} else {
			g, files = importClosureFixture(seed)
		}
		r := New(g)
		whole := r.buildImportClosure()
		rng := rand.New(rand.NewSource(seed * 7))
		for trial := 0; trial < 6; trial++ {
			var callers []string
			for _, file := range files {
				if rng.Intn(4) == 0 {
					callers = append(callers, file)
				}
			}
			if trial == 0 {
				callers = append([]string(nil), files...)
			}
			scoped := r.buildImportClosureForCallerFiles(callers)
			want := map[string]map[string]struct{}{}
			for _, file := range callers {
				if entry, ok := whole[file]; ok {
					want[file] = entry
				}
			}
			if !reflect.DeepEqual(normalizeClosure(scoped), normalizeClosure(want)) {
				t.Fatalf("seed %d trial %d: scoped closure differs\nscoped: %v\nwhole:  %v",
					seed, trial, normalizeClosure(scoped), normalizeClosure(want))
			}
		}
	}
}

func normalizeClosure(c map[string]map[string]struct{}) map[string][]string {
	out := make(map[string][]string, len(c))
	for file, dirs := range c {
		list := make([]string, 0, len(dirs))
		for dir := range dirs {
			list = append(list, dir)
		}
		sort.Strings(list)
		out[file] = list
	}
	return out
}

// An import edge whose provenance names a requested file while its source node
// lives in another file belongs to the source node's file. The production
// store answers the file projection from provenance when it is canonical for
// the requested files, so the scoped build must attribute by source node.
func TestImportClosureForCallerFilesAttributesBySourceNode(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "provenance.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	file := func(path string) *graph.Node {
		return &graph.Node{ID: path, Kind: graph.KindFile, Name: path, FilePath: path, RepoPrefix: "repo"}
	}
	store.AddBatch([]*graph.Node{file("repo/a/a.ts"), file("repo/b/b.ts"), file("repo/x/x.ts"), file("repo/y/y.ts")},
		[]*graph.Edge{
			{From: "repo/a/a.ts", To: "repo/x/x.ts", Kind: graph.EdgeImports, FilePath: "repo/a/a.ts", Line: 1},
			// Source in b.ts, provenance a.ts.
			{From: "repo/b/b.ts", To: "repo/y/y.ts", Kind: graph.EdgeImports, FilePath: "repo/a/a.ts", Line: 2},
		})
	r := New(store)
	whole := r.buildImportClosure()
	scoped := r.buildImportClosureForCallerFiles([]string{"repo/a/a.ts"})
	want := map[string]map[string]struct{}{"repo/a/a.ts": whole["repo/a/a.ts"]}
	if !reflect.DeepEqual(normalizeClosure(scoped), normalizeClosure(want)) {
		t.Fatalf("scoped %v, want %v", normalizeClosure(scoped), normalizeClosure(want))
	}
	if _, leaked := scoped["repo/a/a.ts"]["repo/y"]; leaked {
		t.Fatal("b.ts's import was attributed to a.ts by its provenance")
	}
}
