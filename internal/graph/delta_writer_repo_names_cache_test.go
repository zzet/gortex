package graph

import (
	"fmt"
	"strings"
	"testing"
)

// repoNameReadCounter counts the bottom store's repository name reads.
type repoNameReadCounter struct {
	*Graph
	reads int
}

func (c *repoNameReadCounter) FindNodesByNamesInRepoLanguages(names []string, repoPrefix string, languages []string) map[string][]*Node {
	c.reads++
	return FindNodesByNamesInRepoLanguages(c.Graph, names, repoPrefix, languages)
}

func (c *repoNameReadCounter) FindNodesByNamesInRepo(names []string, repoPrefix string) map[string][]*Node {
	c.reads++
	return c.Graph.FindNodesByNamesInRepo(names, repoPrefix)
}

func (c *repoNameReadCounter) FindNodesByNames(names []string) map[string][]*Node {
	c.reads++
	return c.Graph.FindNodesByNames(names)
}

func renderNamed(m map[string][]*Node) string {
	var rows []string
	for _, name := range []string{"New", "Close", "Load"} {
		for _, n := range m[name] {
			rows = append(rows, fmt.Sprintf("%s=%s/%s/%s", name, n.ID, n.RepoPrefix, n.Language))
		}
	}
	return strings.Join(rows, "\n")
}

// A delta's repository name reads (with and without a language list) are
// answered from the stack's name cache: they equal the uncached composition,
// the delta's own rows included, and a second delta over the stack reads no
// name from the store below.
func TestDeltaWriterRepoNameReadsAreKeptPerStack(t *testing.T) {
	g := New()
	for _, n := range []*Node{
		{ID: "r1/a.go::New", Name: "New", Kind: KindFunction, FilePath: "r1/a.go", RepoPrefix: "r1", Language: "go"},
		{ID: "r1/b.py::New", Name: "New", Kind: KindFunction, FilePath: "r1/b.py", RepoPrefix: "r1", Language: "python"},
		{ID: "r2/a.go::New", Name: "New", Kind: KindFunction, FilePath: "r2/a.go", RepoPrefix: "r2", Language: "go"},
		{ID: "r1/c.go::Close", Name: "Close", Kind: KindMethod, FilePath: "r1/c.go", RepoPrefix: "r1", Language: "go"},
		{ID: "r1/d.go::Load", Name: "Load", Kind: KindFunction, FilePath: "r1/d.go", RepoPrefix: "r1", Language: "go"},
	} {
		g.AddNode(n)
	}
	names := []string{"New", "Close", "Load"}
	edit := func(dw *DeltaWriter) {
		dw.EvictFiles([]string{"r1/d.go"})
		dw.AddBatch([]*Node{{ID: "r1/d.go::Close", Name: "Close", Kind: KindFunction, FilePath: "r1/d.go", RepoPrefix: "r1", Language: "go"}}, nil)
	}
	plain := NewDeltaWriter(g, nil)
	edit(plain)
	wantRepo := renderNamed(plain.FindNodesByNamesInRepo(names, "r1"))
	wantLang := renderNamed(plain.FindNodesByNamesInRepoLanguages(names, "r1", []string{"go"}))
	if !strings.Contains(wantRepo, "r1/d.go::Close") || strings.Contains(wantRepo, "r1/d.go::Load") || strings.Contains(wantLang, "python") {
		t.Fatalf("fixture precondition:\n%s\n--\n%s", wantRepo, wantLang)
	}
	below := &repoNameReadCounter{Graph: g}
	cache := NewBaseProjectionCache()
	var reads []int
	for delta := 0; delta < 2; delta++ {
		dw := NewDeltaWriter(below, nil)
		dw.SetBaseProjectionCache(cache)
		edit(dw)
		before := below.reads
		if got := renderNamed(dw.FindNodesByNamesInRepo(names, "r1")); got != wantRepo {
			t.Fatalf("delta %d: repository names\n%s\nwant\n%s", delta, got, wantRepo)
		}
		if got := renderNamed(dw.FindNodesByNamesInRepoLanguages(names, "r1", []string{"go"})); got != wantLang {
			t.Fatalf("delta %d: repository language names\n%s\nwant\n%s", delta, got, wantLang)
		}
		reads = append(reads, below.reads-before)
	}
	if reads[0] == 0 || reads[1] != 0 {
		t.Fatalf("name reads below per delta = %v; want some on the first and none on the second", reads)
	}
}
