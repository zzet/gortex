package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The delta answers the resolver's repository/language name scopes with the
// scoped read composed through its stack, and every scope's answer equals the
// one the unscoped name read filtered in Go gives: the layers' own nodes are
// in it, a node the delta rewrote hides the stored copy, another repository's
// same-named node stays out of a repository scope, and the order is the same.
func TestEditDeltaScopedNameReadsMatchTheFilteredNameRead(t *testing.T) {
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	for i, body := range []string{
		"package a\n\nimport \"errors\"\n\nfunc A() error {\n\treturn errors.New(\"bang\")\n}\n\nfunc Size(xs []int) int {\n\treturn len(xs) + 1\n}\n",
		"package a\n\nimport \"errors\"\n\nfunc A() error {\n\treturn errors.New(\"bang\")\n}\n\nfunc Size(xs []int) int {\n\treturn len(xs) + 2\n}\n\nfunc More() int { return Size(nil) }\n",
	} {
		if err := os.WriteFile(filepath.Join(f.worktree, "a", "a.go"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := os.WriteFile(filepath.Join(f.worktree, "a", "added.go"), []byte("package a\n\nfunc Added() int { return 1 }\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if out := coordinatorReconcile(t, c); !out.DirtyBuilt {
			t.Fatalf("edit %d built nothing", i)
		}
	}
	view := chainMaterialize(t, f)
	defer view.Close()
	dw := graph.NewDeltaWriter(view.Reader, nil)

	var stored *graph.Node
	for _, n := range dw.FindNodesByNames([]string{"B"})["B"] {
		if n.Kind == graph.KindFunction {
			stored = n
		}
	}
	if stored == nil {
		t.Fatal("fixture precondition: no stored B")
	}
	rewritten := *stored
	rewritten.StartLine += 40
	dw.AddBatch([]*graph.Node{
		&rewritten,
		{ID: builderRepoPrefix + "/c/fresh.go::Fresh", Kind: graph.KindFunction, Name: "Fresh", FilePath: builderRepoPrefix + "/c/fresh.go",
			Language: "go", RepoPrefix: builderRepoPrefix, StartLine: 3},
		{ID: "other/x.go::Size", Kind: graph.KindFunction, Name: "Size", FilePath: "other/x.go", Language: "go", RepoPrefix: "other", StartLine: 1},
		{ID: builderRepoPrefix + "/c/c.ts::Size", Kind: graph.KindFunction, Name: "Size", FilePath: builderRepoPrefix + "/c/c.ts",
			Language: "typescript", RepoPrefix: builderRepoPrefix, StartLine: 1},
	}, nil)

	names := []string{"A", "Added", "B", "C", "Fresh", "Missing", "More", "Size"}
	scopes := []graph.ResolverNameScope{
		{RepoPrefix: builderRepoPrefix, Languages: []string{"go"}, Names: names},
		{RepoPrefix: builderRepoPrefix, Names: names},
		{RepoPrefix: "other", Languages: []string{"go"}, Names: names},
		{AllRepos: true, Languages: []string{"go"}, Names: names},
	}
	render := func(results []map[string][]*graph.Node) string {
		var b strings.Builder
		for i, hits := range results {
			for _, name := range names {
				for _, n := range hits[name] {
					fmt.Fprintf(&b, "%d %s %s %s:%d\n", i, name, n.ID, n.Language, n.StartLine)
				}
			}
		}
		return b.String()
	}
	got, err := dw.FindNodesByResolverNameScopes(scopes)
	if err != nil {
		t.Fatal(err)
	}
	want, err := graph.FindNodesByResolverNameScopes(struct{ graph.Store }{dw}, scopes)
	if err != nil {
		t.Fatal(err)
	}
	if render(got) != render(want) {
		t.Fatalf("scoped name read differs from the filtered name read:\n scoped:\n%s\n filtered:\n%s", render(got), render(want))
	}
	for _, needle := range []string{
		fmt.Sprintf("0 B %s go:%d", rewritten.ID, rewritten.StartLine),
		"0 Fresh " + builderRepoPrefix + "/c/fresh.go::Fresh",
		"0 Added ", "0 More ", "0 C " + builderRepoPrefix + "/c/c.go::C",
		"1 Size " + builderRepoPrefix + "/c/c.ts::Size",
		"2 Size other/x.go::Size",
	} {
		if !strings.Contains(render(got), needle) {
			t.Fatalf("scoped name read lacks %q:\n%s", needle, render(got))
		}
	}
	if strings.Contains(render(got), fmt.Sprintf("B %s go:%d\n", stored.ID, stored.StartLine)) {
		t.Fatalf("the stored copy of a node the delta rewrote was served:\n%s", render(got))
	}
	repoHits := dw.FindNodesByNamesInRepo([]string{"Size"}, "other")
	if len(repoHits["Size"]) != 1 || repoHits["Size"][0].ID != "other/x.go::Size" {
		t.Fatalf("FindNodesByNamesInRepo(other) = %v", repoHits["Size"])
	}
}

// The scoped name read is kept per stack for the layers below the delta: with
// a projection cache every delta answers exactly what the uncached scoped read
// answers (the delta's own rewrites, additions and hidden rows included), and
// a second delta over the stack reads no name again.
func TestEditDeltaScopedNameReadsAreKeptPerStack(t *testing.T) {
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
	names := []string{"A", "Added", "B", "C", "Fresh", "Missing", "Size"}
	scopes := []graph.ResolverNameScope{
		{RepoPrefix: builderRepoPrefix, Languages: []string{"go"}, Names: names},
		{RepoPrefix: builderRepoPrefix, Names: names},
		{AllRepos: true, Languages: []string{"go"}, Names: names},
	}
	render := func(dw *graph.DeltaWriter) string {
		results, err := dw.FindNodesByResolverNameScopes(scopes)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for i, hits := range results {
			for _, name := range names {
				for _, n := range hits[name] {
					fmt.Fprintf(&b, "%d %s %s %d\n", i, name, n.ID, n.StartLine)
				}
			}
		}
		return b.String()
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
		var stored *graph.Node
		for _, n := range dw.FindNodesByNames([]string{"B"})["B"] {
			if n.Kind == graph.KindFunction {
				stored = n
			}
		}
		if stored == nil {
			t.Fatal("fixture precondition: no stored B")
		}
		rewritten := *stored
		rewritten.StartLine += 40
		dw.AddBatch([]*graph.Node{&rewritten, {ID: builderRepoPrefix + "/c/fresh.go::Fresh", Kind: graph.KindFunction, Name: "Fresh",
			FilePath: builderRepoPrefix + "/c/fresh.go", Language: "go", RepoPrefix: builderRepoPrefix, StartLine: 3}}, nil)
		dw.EvictFile(builderRepoPrefix + "/c/c.go")
	}
	if got, want := render(delta(true)), render(delta(false)); got != want || !strings.Contains(got, "Added") {
		t.Fatalf("first cached delta:\n%s\nwant:\n%s", got, want)
	}
	_, misses := cache.StackNameStats()
	if misses != len(scopes)*len(names) {
		t.Fatalf("the first delta kept %d scope/name entries, want every scope's (%d)", misses, len(scopes)*len(names))
	}
	plain, cached := delta(false), delta(true)
	edit(plain)
	edit(cached)
	got, want := render(cached), render(plain)
	if got != want || !strings.Contains(got, "2 Fresh") || strings.Contains(got, "::C ") {
		t.Fatalf("a delta's own names over the cache:\n%s\nwant:\n%s", got, want)
	}
	hits, misses2 := cache.StackNameStats()
	if hits == 0 || misses2 != misses {
		t.Fatalf("the second delta read the names again: %d hits, %d misses after %d", hits, misses2, misses)
	}
}
