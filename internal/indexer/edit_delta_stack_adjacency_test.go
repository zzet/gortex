package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The stack below a delta is immutable, so the import adjacency its layers
// compose for a caller file is kept per stack: a later delta over the same
// stack answers a layer-covered file without reading the layers again, every
// answer equals the file's nodes' import rows, and the delta's own writes (a
// new import out of a covered file, an evicted file) are served as written.
func TestEditDeltaStackImportAdjacencyIsKeptPerStack(t *testing.T) {
	tree := sharedRowsTree()
	tree["d/d.go"] = "package d\n\nimport \"" + sharedRowsModule + "/c\"\n\nfunc D() int { return c.C() }\n"
	f := newCoordinatorFixtureWithTree(t, tree)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	for i, body := range []string{
		"package a\n\nimport \"errors\"\n\nfunc A() error {\n\treturn errors.New(\"bang\")\n}\n\nfunc Size(xs []int) int {\n\treturn len(xs) + 1\n}\n",
		"package a\n\nimport (\n\t\"errors\"\n\t\"strings\"\n)\n\nfunc A() error {\n\treturn errors.New(strings.ToUpper(\"bang\"))\n}\n\nfunc Size(xs []int) int {\n\treturn len(xs) + 2\n}\n",
	} {
		if err := os.WriteFile(filepath.Join(f.worktree, "a", "a.go"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if out := coordinatorReconcile(t, c); !out.DirtyBuilt {
			t.Fatalf("edit %d built nothing", i)
		}
	}
	view := chainMaterialize(t, f)
	defer view.Close()
	var paths []string
	for n := range view.Reader.NodesByKind(graph.KindFile) {
		if strings.HasSuffix(n.FilePath, ".go") {
			paths = append(paths, n.FilePath)
		}
	}
	sort.Strings(paths)
	covered := builderRepoPrefix + "/a/a.go"
	reference := func(dw *graph.DeltaWriter) string {
		var rows []string
		for _, p := range paths {
			var ids []string
			for _, n := range dw.GetFileNodes(p) {
				ids = append(ids, n.ID)
			}
			var targets []string
			for _, edges := range dw.GetOutEdgesByNodeIDs(ids) {
				for _, e := range edges {
					if e.Kind == graph.EdgeImports {
						targets = append(targets, e.To)
					}
				}
			}
			sort.Strings(targets)
			rows = append(rows, fmt.Sprintf("%s %v", p, targets))
		}
		return strings.Join(rows, "\n")
	}
	project := func(dw *graph.DeltaWriter) string {
		got, complete := dw.ProjectImportAdjacency(paths)
		if !complete {
			t.Fatal("the projection refused a canonical request")
		}
		var rows []string
		for _, p := range paths {
			targets := append([]string(nil), got[p]...)
			sort.Strings(targets)
			rows = append(rows, fmt.Sprintf("%s %v", p, targets))
		}
		return strings.Join(rows, "\n")
	}
	cache := graph.NewBaseProjectionCache()
	delta := func() *graph.DeltaWriter {
		dw := graph.NewDeltaWriter(view.Reader, nil)
		dw.SetBaseProjectionCache(cache)
		return dw
	}
	first := delta()
	if got, want := project(first), reference(first); got != want {
		t.Fatalf("first delta:\n projected:\n%s\n rows:\n%s", got, want)
	}
	if !strings.Contains(project(first), "strings") {
		t.Fatalf("fixture precondition: the layer's import of strings is not served:\n%s", project(first))
	}
	hits, misses := cache.StackStats()
	if misses == 0 {
		t.Fatalf("the first delta did not fill the stack cache (hits %d)", hits)
	}
	second := delta()
	if got, want := project(second), reference(second); got != want {
		t.Fatalf("second delta:\n projected:\n%s\n rows:\n%s", got, want)
	}
	hits2, misses2 := cache.StackStats()
	if misses2 != misses || hits2 == hits {
		t.Fatalf("the second delta read the stack again: %d/%d hits/misses after %d/%d", hits2, misses2, hits, misses)
	}

	// A delta's own import out of the layer-covered file is its own answer.
	third := delta()
	fileID := covered
	third.AddBatch(nil, []*graph.Edge{{From: fileID, To: builderRepoPrefix + "/c/c.go", Kind: graph.EdgeImports, FilePath: covered, Line: 4}})
	if got, want := project(third), reference(third); got != want || !strings.Contains(got, builderRepoPrefix+"/c/c.go]") && !strings.Contains(got, builderRepoPrefix+"/c/c.go ") {
		t.Fatalf("the delta's own import is not served:\n projected:\n%s\n rows:\n%s", got, want)
	}
	// An evicted file is answered as the delta holds it, and a file that
	// imports it keeps no import of what the delta removed.
	fourth := delta()
	fourth.EvictFile(builderRepoPrefix + "/b/b.go")
	if got, want := project(fourth), reference(fourth); got != want {
		t.Fatalf("after an eviction:\n projected:\n%s\n rows:\n%s", got, want)
	}
	fifth := delta()
	fifth.EvictFile(builderRepoPrefix + "/c/c.go")
	got, want := project(fifth), reference(fifth)
	if got != want {
		t.Fatalf("after evicting an imported file:\n projected:\n%s\n rows:\n%s", got, want)
	}
	if !strings.Contains(project(second), builderRepoPrefix+"/c/c.go") {
		t.Fatalf("fixture precondition: nothing imports c/c.go:\n%s", project(second))
	}
}

// The directory index's file identities are kept per stack the same way: the
// layers' composition is read once, and each delta's own added and evicted
// files are applied over it exactly as the uncached composition applies them.
func TestEditDeltaStackFileIdentitiesAreKeptPerStack(t *testing.T) {
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	if err := os.WriteFile(filepath.Join(f.worktree, "a", "added.go"), []byte("package a\n\nfunc Added() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := coordinatorReconcile(t, c); !out.DirtyBuilt {
		t.Fatal("the edit built nothing")
	}
	view := chainMaterialize(t, f)
	defer view.Close()
	render := func(dw *graph.DeltaWriter) string {
		var rows []string
		for row := range dw.FileNodeIdentitiesSeq([]string{builderRepoPrefix}) {
			rows = append(rows, row.ID+" "+row.FilePath)
		}
		return strings.Join(rows, "\n")
	}
	cache := graph.NewBaseProjectionCache()
	delta := func(cached bool) *graph.DeltaWriter {
		dw := graph.NewDeltaWriter(view.Reader, nil)
		if cached {
			dw.SetBaseProjectionCache(cache)
		}
		return dw
	}
	edit := func(dw *graph.DeltaWriter) {
		p := builderRepoPrefix + "/c/new.go"
		dw.AddBatch([]*graph.Node{{ID: p, Kind: graph.KindFile, Name: "new.go", FilePath: p, Language: "go", RepoPrefix: builderRepoPrefix}}, nil)
		dw.EvictFile(builderRepoPrefix + "/b/b.go")
	}
	want := render(delta(false))
	if !strings.Contains(want, "/a/added.go") {
		t.Fatalf("fixture precondition: the layer's file is not served:\n%s", want)
	}
	if got := render(delta(true)); got != want {
		t.Fatalf("first cached delta:\n%s\nwant:\n%s", got, want)
	}
	_, misses := cache.StackFileStats()
	plain, cached := delta(false), delta(true)
	edit(plain)
	edit(cached)
	if got, want := render(cached), render(plain); got != want || !strings.Contains(got, "/c/new.go") || strings.Contains(got, "/b/b.go") {
		t.Fatalf("a delta's own files over the cache:\n%s\nwant:\n%s", got, want)
	}
	hits, misses2 := cache.StackFileStats()
	if hits == 0 || misses2 != misses {
		t.Fatalf("the second delta read the layers again: %d hits, %d misses after %d", hits, misses2, misses)
	}
}

// A package's type index read (NodesInFilesByKind) is kept per stack for the
// files the delta does not speak for: every delta's answer equals the
// uncached composition's, a second delta over the stack reads nothing again,
// and a delta's own type node, in a file it covers or not, is served.
func TestEditDeltaStackTypeIndexReadsAreKeptPerStack(t *testing.T) {
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	if err := os.WriteFile(filepath.Join(f.worktree, "a", "types.go"), []byte("package a\n\ntype Box struct{ n int }\n\ntype Shape interface{ Area() int }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := coordinatorReconcile(t, c); !out.DirtyBuilt {
		t.Fatal("the edit built nothing")
	}
	view := chainMaterialize(t, f)
	defer view.Close()
	files := []string{builderRepoPrefix + "/a/a.go", builderRepoPrefix + "/a/types.go", builderRepoPrefix + "/b/b.go"}
	kinds := []graph.NodeKind{graph.KindType, graph.KindInterface}
	render := func(dw *graph.DeltaWriter) string {
		var rows []string
		for _, n := range dw.NodesInFilesByKind(files, kinds) {
			rows = append(rows, fmt.Sprintf("%s %s %d", n.ID, n.Kind, n.StartLine))
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n")
	}
	cache := graph.NewBaseProjectionCache()
	delta := func(cached bool) *graph.DeltaWriter {
		dw := graph.NewDeltaWriter(view.Reader, nil)
		if cached {
			dw.SetBaseProjectionCache(cache)
		}
		return dw
	}
	want := render(delta(false))
	if !strings.Contains(want, "::Box") || !strings.Contains(want, "::Shape") {
		t.Fatalf("fixture precondition: the layer's types are not served:\n%s", want)
	}
	if got := render(delta(true)); got != want {
		t.Fatalf("first cached delta:\n%s\nwant:\n%s", got, want)
	}
	_, misses := cache.StackNodeStats()
	edit := func(dw *graph.DeltaWriter) {
		p := builderRepoPrefix + "/b/b.go"
		dw.AddBatch([]*graph.Node{{ID: p + "::Added", Kind: graph.KindType, Name: "Added", FilePath: p, Language: "go", RepoPrefix: builderRepoPrefix, StartLine: 9}}, nil)
	}
	plain, cached := delta(false), delta(true)
	edit(plain)
	edit(cached)
	if got, want := render(cached), render(plain); got != want || !strings.Contains(got, "::Added") {
		t.Fatalf("a delta's own type over the cache:\n%s\nwant:\n%s", got, want)
	}
	hits, misses2 := cache.StackNodeStats()
	if hits == 0 || misses2 != misses {
		t.Fatalf("the second delta read the files again: %d hits, %d misses after %d", hits, misses2, misses)
	}
	// A file the delta evicts answers as the delta holds it, not as kept.
	plain, cached = delta(false), delta(true)
	plain.EvictFile(builderRepoPrefix + "/a/types.go")
	cached.EvictFile(builderRepoPrefix + "/a/types.go")
	if got, want := render(cached), render(plain); got != want || strings.Contains(got, "::Box") {
		t.Fatalf("an evicted file over the cache:\n%s\nwant:\n%s", got, want)
	}
}

// countingFactsBase is a view below a delta that serves fixed reference
// facts and counts the target reads.
type countingFactsBase struct {
	graph.Reader
	facts []graph.RefFact
	reads int
}

func (b *countingFactsBase) LoadRefFactsByTargets(repo string, targets []string) (map[string][]graph.RefFact, error) {
	b.reads++
	want := make(map[string]struct{}, len(targets))
	for _, id := range targets {
		want[id] = struct{}{}
	}
	out := make(map[string][]graph.RefFact)
	for _, fact := range b.facts {
		if _, ok := want[fact.ToID]; ok && fact.RepoPrefix == repo {
			out[fact.FilePath] = append(out[fact.FilePath], fact)
		}
	}
	return out, nil
}

func (b *countingFactsBase) LoadRefFactsByFiles(string, []string) ([]graph.RefFact, error) {
	return nil, nil
}

// The reference facts the affected-by planner reads by target are kept per
// stack: every delta's answer equals the uncached read, a covered file's
// facts are the delta's, and a second delta reads no target again.
func TestEditDeltaStackRefFactsByTargetsAreKeptPerStack(t *testing.T) {
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()
	p := builderRepoPrefix
	facts := []graph.RefFact{
		{RepoPrefix: p, FromID: p + "/a/a.go::A", ToID: p + "/c/c.go::C", Kind: "calls", Line: 5, FilePath: p + "/a/a.go"},
		{RepoPrefix: p, FromID: p + "/b/b.go::B", ToID: p + "/c/c.go::C", Kind: "calls", Line: 6, FilePath: p + "/b/b.go"},
		{RepoPrefix: p, FromID: p + "/b/b.go::B", ToID: p + "/a/a.go::Size", Kind: "calls", Line: 7, FilePath: p + "/b/b.go"},
	}
	targets := []string{p + "/c/c.go::C", p + "/a/a.go::Size"}
	render := func(dw *graph.DeltaWriter) string {
		byFile, err := dw.LoadRefFactsByTargets(p, targets)
		if err != nil {
			t.Fatal(err)
		}
		var rows []string
		for file, list := range byFile {
			for _, fact := range list {
				rows = append(rows, fmt.Sprintf("%s %s->%s %d", file, fact.FromID, fact.ToID, fact.Line))
			}
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n")
	}
	base := &countingFactsBase{Reader: view.Reader, facts: facts}
	cache := graph.NewBaseProjectionCache()
	delta := func(cached bool) *graph.DeltaWriter {
		dw := graph.NewDeltaWriter(base, nil)
		if cached {
			dw.SetBaseProjectionCache(cache)
		}
		return dw
	}
	want := render(delta(false))
	if strings.Count(want, "\n") != 2 {
		t.Fatalf("fixture precondition: three facts expected:\n%s", want)
	}
	if got := render(delta(true)); got != want {
		t.Fatalf("first cached delta:\n%s\nwant:\n%s", got, want)
	}
	reads := base.reads
	plain, cached := delta(false), delta(true)
	plain.EvictFile(p + "/b/b.go")
	cached.EvictFile(p + "/b/b.go")
	got := render(cached)
	if want := render(plain); got != want || strings.Contains(got, "/b/b.go") {
		t.Fatalf("a covered file's facts over the cache:\n%s\nwant:\n%s", got, want)
	}
	if base.reads != reads+1 { // only the uncached delta read the view below
		t.Fatalf("the cached delta read the view below again: %d reads after %d", base.reads, reads)
	}
	if hits, _ := cache.StackRefFactStats(); hits == 0 {
		t.Fatal("no stack hit")
	}
}

// Node placements and the whole directory index are kept per stack too: every
// delta's answer equals the uncached one (the delta's own and evicted nodes
// included), and a second delta reads no stack placement again.
func TestEditDeltaStackPlacementsAndWholeDirectoryIndex(t *testing.T) {
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	if err := os.WriteFile(filepath.Join(f.worktree, "a", "added.go"), []byte("package a\n\nfunc Added() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := coordinatorReconcile(t, c); !out.DirtyBuilt {
		t.Fatal("the edit built nothing")
	}
	view := chainMaterialize(t, f)
	defer view.Close()
	p := builderRepoPrefix
	ids := []string{p + "/a/a.go", p + "/a/added.go", p + "/a/added.go::Added", p + "/b/b.go::B", p + "/c/c.go::C", p + "/c/new.go::New", "missing::x"}
	placements := func(dw *graph.DeltaWriter) string {
		got := dw.NodePlacementsByIDs(ids)
		var rows []string
		for _, id := range ids {
			if pl, ok := got[id]; ok {
				rows = append(rows, fmt.Sprintf("%s %s %s %s", id, pl.Kind, pl.FilePath, pl.RepoPrefix))
			}
		}
		return strings.Join(rows, "\n")
	}
	uncached := func(dw *graph.DeltaWriter) string {
		got := graph.NodePlacementsByIDs(struct{ graph.Store }{dw}, ids)
		var rows []string
		for _, id := range ids {
			if pl, ok := got[id]; ok {
				rows = append(rows, fmt.Sprintf("%s %s %s %s", id, pl.Kind, pl.FilePath, pl.RepoPrefix))
			}
		}
		return strings.Join(rows, "\n")
	}
	whole := func(dw *graph.DeltaWriter) string {
		var rows []string
		for row := range dw.FileNodeIdentitiesSeq(nil) {
			rows = append(rows, row.ID)
		}
		sort.Strings(rows) // the resolver sorts each directory bucket
		return strings.Join(rows, "\n")
	}
	cache := graph.NewBaseProjectionCache()
	edit := func(dw *graph.DeltaWriter) {
		np := p + "/c/new.go"
		dw.AddBatch([]*graph.Node{{ID: np, Kind: graph.KindFile, Name: "new.go", FilePath: np, Language: "go", RepoPrefix: p},
			{ID: np + "::New", Kind: graph.KindFunction, Name: "New", FilePath: np, Language: "go", RepoPrefix: p}}, nil)
		dw.EvictFile(p + "/b/b.go")
	}
	for delta := 0; delta < 2; delta++ {
		plain := graph.NewDeltaWriter(view.Reader, nil)
		cached := graph.NewDeltaWriter(view.Reader, nil)
		cached.SetBaseProjectionCache(cache)
		if delta == 1 {
			edit(plain)
			edit(cached)
		}
		if got, want := placements(cached), uncached(plain); got != want {
			t.Fatalf("delta %d placements:\n%s\nwant:\n%s", delta, got, want)
		}
		if got, want := whole(cached), whole(plain); got != want {
			t.Fatalf("delta %d whole directory index:\n%s\nwant:\n%s", delta, got, want)
		}
	}
	if !strings.Contains(placements(func() *graph.DeltaWriter {
		dw := graph.NewDeltaWriter(view.Reader, nil)
		dw.SetBaseProjectionCache(cache)
		return dw
	}()), "/a/added.go::Added") {
		t.Fatal("fixture precondition: the layer's node is not placed")
	}
	if hits, _ := cache.StackPlacementStats(); hits == 0 {
		t.Fatal("no stack placement hit")
	}
}
