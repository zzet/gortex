package resolver

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// nameCountingStore counts batched name reads.
type nameCountingStore struct {
	graph.Store
	nameReads int
}

func (s *nameCountingStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	s.nameReads++
	return s.Store.FindNodesByNames(names)
}

// A changed file whose own rows and adjacency already fill the seed's row cap
// gains nothing from the name-dependency read — rememberNode retains nothing
// past the cap — so the seed must not pay for it, and must end with the same
// retained set it would have ended with.
func TestFrameworkSeedSkipsNameReadsOnceAtTheRowCap(t *testing.T) {
	g := graph.New()
	const file = "repo/big.go"
	target := &graph.Node{ID: file + "::Target", Kind: graph.KindFunction, Name: "Target", FilePath: file, RepoPrefix: "repo"}
	g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, Name: "big.go", FilePath: file, RepoPrefix: "repo"})
	g.AddNode(target)
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := 0; i < frameworkScopeRetainedRowCap+10; i++ {
		caller := fmt.Sprintf("repo/c%d.go::Caller%d", i, i)
		nodes = append(nodes, &graph.Node{ID: caller, Kind: graph.KindFunction, Name: fmt.Sprintf("Caller%d", i),
			FilePath: fmt.Sprintf("repo/c%d.go", i), RepoPrefix: "repo"})
		edges = append(edges, &graph.Edge{From: caller, To: target.ID, Kind: graph.EdgeCalls,
			FilePath: fmt.Sprintf("repo/c%d.go", i), Line: 1, Meta: map[string]any{"callee": fmt.Sprintf("tok%d", i)}})
	}
	g.AddBatch(nodes, edges)
	store := &nameCountingStore{Store: g}
	seed := newFrameworkScopedSeed(store, map[string]bool{"repo": true}, []string{file})
	if seed.retainedRows < frameworkScopeRetainedRowCap {
		t.Fatalf("the fixture retained %d rows, want the %d-row cap reached", seed.retainedRows, frameworkScopeRetainedRowCap)
	}
	if store.nameReads != 0 {
		t.Fatalf("the seed read name dependencies %d times after reaching the row cap", store.nameReads)
	}
}

// Below the cap the name dependencies are still read.
func TestFrameworkSeedReadsNamesBelowTheRowCap(t *testing.T) {
	g := graph.New()
	const file = "repo/small.go"
	g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, Name: "small.go", FilePath: file, RepoPrefix: "repo"})
	g.AddNode(&graph.Node{ID: file + "::F", Kind: graph.KindFunction, Name: "F", FilePath: file, RepoPrefix: "repo",
		Meta: map[string]any{"route": "Handler"}})
	store := &nameCountingStore{Store: g}
	newFrameworkScopedSeed(store, map[string]bool{"repo": true}, []string{file})
	if store.nameReads != 1 {
		t.Fatalf("the seed read name dependencies %d times below the row cap, want 1", store.nameReads)
	}
}
