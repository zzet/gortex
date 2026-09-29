package indexer

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The bottom store's projection cache answers a second delta over the same
// stack without reading the store, gives every delta the answers an uncached
// one gets, and never masks what the delta itself wrote: the delta's own new
// file and import compose over the cached rows exactly as over the store's.
func TestBaseProjectionCacheServesLaterDeltasAndComposesTheDelta(t *testing.T) {
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()

	var goFiles []string
	for n := range view.Reader.NodesByKind(graph.KindFile) {
		if strings.HasSuffix(n.FilePath, ".go") {
			goFiles = append(goFiles, n.FilePath)
		}
	}
	sort.Strings(goFiles)
	if len(goFiles) < 2 {
		t.Fatalf("fixture precondition: %d Go files", len(goFiles))
	}
	render := func(dw *graph.DeltaWriter) string {
		var rows []string
		for row := range dw.FileNodeIdentitiesSeq([]string{builderRepoPrefix}) {
			rows = append(rows, "file "+row.ID)
		}
		adjacency, ok := dw.ProjectImportAdjacency(goFiles)
		if !ok {
			t.Fatal("the import projection was not complete")
		}
		for _, p := range goFiles {
			targets := append([]string(nil), adjacency[p]...)
			sort.Strings(targets)
			rows = append(rows, fmt.Sprintf("imports %s %v", p, targets))
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n")
	}
	want := render(graph.NewDeltaWriter(view.Reader, nil))

	cache := graph.NewBaseProjectionCache()
	first := graph.NewDeltaWriter(view.Reader, nil)
	first.SetBaseProjectionCache(cache)
	if got := render(first); got != want {
		t.Fatalf("the first cached delta differs from an uncached one:\n cached:\n%s\n uncached:\n%s", got, want)
	}
	fileHits, fileMisses, importHits, importMisses := cache.Stats()
	if fileMisses == 0 || importMisses == 0 || fileHits != 0 || importHits != 0 {
		t.Fatalf("first delta: file %d hits / %d misses, imports %d hits / %d misses; want misses only", fileHits, fileMisses, importHits, importMisses)
	}

	second := graph.NewDeltaWriter(view.Reader, nil)
	second.SetBaseProjectionCache(cache)
	if got := render(second); got != want {
		t.Fatalf("the second cached delta differs from an uncached one:\n cached:\n%s\n uncached:\n%s", got, want)
	}
	fileHits2, fileMisses2, importHits2, importMisses2 := cache.Stats()
	if fileMisses2 != fileMisses || importMisses2 != importMisses || fileHits2 == 0 || importHits2 == 0 {
		t.Fatalf("second delta read the store again: file %d/%d, imports %d/%d (hits/misses)", fileHits2, fileMisses2, importHits2, importMisses2)
	}

	// A delta that adds a file and an import composes them over the cache.
	third := graph.NewDeltaWriter(view.Reader, nil)
	third.SetBaseProjectionCache(cache)
	added := &graph.Node{ID: builderRepoPrefix + "/a/added.go", Kind: graph.KindFile, Name: "added.go",
		FilePath: builderRepoPrefix + "/a/added.go", Language: "go", RepoPrefix: builderRepoPrefix}
	third.AddBatch([]*graph.Node{added}, []*graph.Edge{{From: added.ID, To: goFiles[0], Kind: graph.EdgeImports,
		FilePath: added.FilePath, Line: 3}})
	sawFile := false
	for row := range third.FileNodeIdentitiesSeq([]string{builderRepoPrefix}) {
		if row.ID == added.ID {
			sawFile = true
		}
	}
	adjacency, _ := third.ProjectImportAdjacency([]string{added.FilePath})
	if !sawFile || len(adjacency[added.FilePath]) != 1 || adjacency[added.FilePath][0] != goFiles[0] {
		t.Fatalf("the delta's own file (%t) or import (%v) is masked by the cache", sawFile, adjacency[added.FilePath])
	}
}
