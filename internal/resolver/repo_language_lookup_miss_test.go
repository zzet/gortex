package resolver

import (
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
