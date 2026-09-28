package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// repoNameCountingStore counts the global and the repository-scoped name
// reads.
type repoNameCountingStore struct {
	*graph.Graph
	global, scoped int
}

func (s *repoNameCountingStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	s.global++
	return s.Graph.FindNodesByNames(names)
}

func (s *repoNameCountingStore) FindNodesByNamesInRepo(names []string, repo string) map[string][]*graph.Node {
	s.scoped++
	out := make(map[string][]*graph.Node)
	for name, nodes := range s.Graph.FindNodesByNames(names) {
		for _, n := range nodes {
			if n.RepoPrefix == repo {
				out[name] = append(out[name], n)
			}
		}
	}
	return out
}

// The fields a receiver call names are read in the evaluated methods'
// repository when they share one, never across the whole store.
func TestReceiverFieldsAreReadInTheMethodsRepository(t *testing.T) {
	g := graph.New()
	for _, n := range []*graph.Node{
		{ID: "a/pkg/s.go::Store.store", Kind: graph.KindField, Name: "store", RepoPrefix: "a", Meta: map[string]any{"receiver": "Store"}},
		{ID: "b/pkg/s.go::Store.store", Kind: graph.KindField, Name: "store", RepoPrefix: "b", Meta: map[string]any{"receiver": "Store"}},
	} {
		g.AddNode(n)
	}
	store := &repoNameCountingStore{Graph: g}
	methods := map[string]*graph.Node{"a/pkg/s.go::Store.Run": {ID: "a/pkg/s.go::Store.Run", Kind: graph.KindMethod, RepoPrefix: "a"}}
	got := receiverFieldsByName(store, methods, []string{"store"})
	if store.global != 0 || store.scoped != 1 {
		t.Fatalf("name reads global=%d scoped=%d; want one repository-scoped read", store.global, store.scoped)
	}
	if len(got["store"]) != 1 || got["store"][0].RepoPrefix != "a" {
		t.Fatalf("fields = %v; want the repository's own", got["store"])
	}
	methods["b/pkg/s.go::Store.Run"] = &graph.Node{ID: "b/pkg/s.go::Store.Run", Kind: graph.KindMethod, RepoPrefix: "b"}
	if got := receiverFieldsByName(store, methods, []string{"store"}); len(got["store"]) != 2 || store.global != 1 {
		t.Fatalf("methods of two repositories: fields %v, global reads %d; want the global read", got["store"], store.global)
	}
}
