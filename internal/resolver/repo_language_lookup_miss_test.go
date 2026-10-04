package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// scopeFinderCountingStore counts scoped-finder and raw per-name reads.
type scopeFinderCountingStore struct {
	*graph.Graph
	scoped, raw int
}

func (s *scopeFinderCountingStore) FindNodesByResolverNameScopes(scopes []graph.ResolverNameScope) ([]map[string][]*graph.Node, error) {
	s.scoped++
	return graph.FindNodesByResolverNameScopes(s.Graph, scopes)
}

func (s *scopeFinderCountingStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	s.raw++
	return s.Graph.FindNodesByNames(names)
}

func (s *scopeFinderCountingStore) FindNodesByNamesInRepo(names []string, repo string) map[string][]*graph.Node {
	s.raw++
	return s.Graph.FindNodesByNamesInRepo(names, repo)
}

// A name the warm-up did not read is read through the store's scoped name
// finder (which a per-file delta keeps per stack), not the raw per-name read,
// and kept for the rest of the pass: a second miss on it reads nothing.
func TestAnUnwarmedNameIsReadThroughTheScopedFinderOnce(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repo/a.go::T.Close", Kind: graph.KindMethod, Name: "Close", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"})
	store := &scopeFinderCountingStore{Graph: g}
	r := New(store)
	r.nodesByRepoLanguageName = map[resolverNameLookupScope]map[string][]*graph.Node{}
	edge := &graph.Edge{From: "repo/b.go::F", To: "unresolved::*.Close", Kind: graph.EdgeCalls, FilePath: "repo/b.go"}
	first := r.cachedFindNodesByNameInRepoForEdge("Close", "repo", edge)
	second := r.cachedFindNodesByNameInRepoForEdge("Close", "repo", edge)
	if len(first) != 1 || len(second) != 1 || first[0].ID != "repo/a.go::T.Close" {
		t.Fatalf("hits %v / %v", first, second)
	}
	if store.scoped != 1 || store.raw != 0 {
		t.Fatalf("scoped reads %d, raw reads %d; want one scoped read", store.scoped, store.raw)
	}
}
