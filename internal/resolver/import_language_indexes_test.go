package resolver

import (
	"fmt"
	"iter"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type importLanguageCountingStore struct {
	graph.Store
	languageWalks atomic.Int64
	summaryRepos  [][]string
	summaryRows   int
}

func (s *importLanguageCountingStore) FileLanguageNodesSeq() iter.Seq[graph.FileLanguageNode] {
	s.languageWalks.Add(1)
	return graph.FileLanguageNodesSeq(s.Store)
}

func (s *importLanguageCountingStore) NodesLightByKindsInScopeSeq(repos, paths []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	s.summaryRepos = append(s.summaryRepos, append([]string(nil), repos...))
	return func(yield func(*graph.Node) bool) {
		for node := range graph.NodesLightByKindsInScopeSeq(s.Store, repos, paths, kinds...) {
			s.summaryRows++
			if !yield(node) {
				return
			}
		}
	}
}

func TestResolveAllScopedImportLanguagesDoNotWalkPerImport(t *testing.T) {
	const files, imports = 4000, 400
	g := graph.New()
	for i := 0; i < files; i++ {
		file := fmt.Sprintf("repo/pkg%d/file.go", i)
		g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, Language: "go", RepoPrefix: "repo"})
	}
	for i := 0; i < imports; i++ {
		file := fmt.Sprintf("repo/app%d/main.go", i)
		g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, Language: "go", RepoPrefix: "repo"})
		g.AddEdge(&graph.Edge{From: file, To: fmt.Sprintf("unresolved::import::example.com/repo/pkg%d", i), Kind: graph.EdgeImports, FilePath: file, Line: 3})
	}
	g.AddNode(&graph.Node{ID: "other/ignored.go", Kind: graph.KindFile, FilePath: "other/ignored.go", Language: "go", RepoPrefix: "other"})
	store := &importLanguageCountingStore{Store: g}
	r := New(store)
	r.SetScope(map[string]struct{}{"repo": {}})
	r.SetWorkspaceMembership(func(string) string { return "" })
	stats := r.ResolveAll()
	t.Logf("imports=%d languageWalks=%d scopedFileRows=%d", imports, store.languageWalks.Load(), store.summaryRows)
	if stats.Resolved != imports {
		t.Fatalf("resolved %d imports, want %d", stats.Resolved, imports)
	}
	for i := 0; i < imports; i++ {
		edges := g.GetOutEdges(fmt.Sprintf("repo/app%d/main.go", i))
		want := fmt.Sprintf("repo/pkg%d/file.go", i)
		if len(edges) != 1 || edges[0].To != want {
			t.Fatalf("import %d: edges = %#v, want target %q", i, edges, want)
		}
	}
	// Attribution passes still walk languages a constant number of times.
	// Import resolution must not add one whole-store walk per import.
	if got := store.languageWalks.Load(); got > 5 {
		t.Fatalf("whole-store language walks = %d for %d imports, want at most 5", got, imports)
	}
	if store.summaryRows != 0 {
		t.Fatalf("import resolution read %d file summaries for languages, want 0", store.summaryRows)
	}
	if r.dirIndex != nil || r.lastDirIndex != nil {
		t.Fatal("ResolveAll retained directory indexes after the pass")
	}
}
