package indexer

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The per-save delta serves the name-only read.
var _ refFactNameReader = (*graph.DeltaWriter)(nil)

// namesOnlyStore counts full-row node reads and serves a name-only read.
type namesOnlyStore struct {
	*graph.Graph
	fullRowReads []string
	nameReads    int
}

func (s *namesOnlyStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.fullRowReads = append(s.fullRowReads, ids...)
	return s.Graph.GetNodesByIDs(ids)
}

func (s *namesOnlyStore) NodeNamesByIDs(ids []string) map[string]string {
	s.nameReads++
	out := make(map[string]string, len(ids))
	for id, n := range s.Graph.GetNodesByIDs(ids) {
		if n != nil {
			out[id] = n.Name
		}
	}
	return out
}

// A reference fact needs only its target's name: over a store with a
// name-only read, the walk reads no target row, and it derives the same facts
// (names included) as the full-row read of a store without one.
func TestRefFactsWalkReadsOnlyTheTargetNames(t *testing.T) {
	build := func() *graph.Graph {
		g := graph.New()
		g.AddBatch([]*graph.Node{
			{ID: "repo/a.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repo/a.go", Language: "go", RepoPrefix: "repo"},
			{ID: "repo/b.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repo/b.go", Language: "go", RepoPrefix: "repo"},
			{ID: "repo/b.go::Config", Kind: graph.KindType, Name: "Config", FilePath: "repo/b.go", Language: "go", RepoPrefix: "repo"},
		}, []*graph.Edge{
			{From: "repo/a.go::Run", To: "repo/b.go::Helper", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 3},
			{From: "repo/a.go::Run", To: "repo/b.go::Config", Kind: graph.EdgeReferences, FilePath: "repo/a.go", Line: 4},
			{From: "repo/a.go::Run", To: "repo/c.go::Gone", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 5},
		})
		return g
	}
	walk := func(g graph.Store) string {
		var rows []string
		source := g.GetNode("repo/a.go::Run")
		if err := walkRefFacts(g, []*graph.Node{source}, nil, func(facts []graph.RefFact) error {
			for _, f := range facts {
				rows = append(rows, fmt.Sprintf("%s->%s %s %q", f.FromID, f.ToID, f.Kind, f.RefName))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n")
	}

	store := &namesOnlyStore{Graph: build()}
	got := walk(store)
	if len(store.fullRowReads) != 0 || store.nameReads == 0 {
		t.Fatalf("the walk read target rows %v (name reads %d); want names only", store.fullRowReads, store.nameReads)
	}
	if want := walk(build()); got != want {
		t.Fatalf("name-only facts differ from full-row facts:\n names:\n%s\n rows:\n%s", got, want)
	}
	for _, needle := range []string{`Helper calls "Helper"`, `Config references "Config"`} {
		if !strings.Contains(got, needle) {
			t.Fatalf("facts lack %s:\n%s", needle, got)
		}
	}
}
