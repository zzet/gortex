package indexer

import (
	"fmt"
	"iter"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// countingContractStore counts the contract-identity reads.
type countingContractStore struct {
	graph.Store
	reads int
}

func (s *countingContractStore) RepoNodeIdentitiesSeq(repos []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	s.reads++
	return graph.RepoNodeIdentitiesSeq(s.Store, repos, kinds...)
}

// The dependency contracts a delta's resolver reads are kept per stack and
// repository: two deltas over the stack read them once, both see the stack's
// dep:: rows, and a change set holding a module manifest installs no source
// (the delta reads its own view).
func TestEditDeltaDepContractsAreKeptPerStack(t *testing.T) {
	resetEditDeltaDeps()
	t.Cleanup(resetEditDeltaDeps)
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "dep::example.com/lib", Kind: graph.KindContract, Name: "lib", RepoPrefix: "repo"},
		{ID: "env::HOME", Kind: graph.KindContract, Name: "HOME", RepoPrefix: "repo"},
	}, nil)
	store := &countingContractStore{Store: g}
	var got []string
	for delta := 0; delta < 2; delta++ {
		idx := &Indexer{repoPrefix: "repo", graph: store, resolver: resolver.New(store)}
		installEditDeltaDeps(idx, "stack", []string{"a/a.go"}, nil)
		got = got[:0]
		for row := range editDeltaDepSourceForTest(idx)([]string{"repo"}) {
			got = append(got, row.ID)
		}
	}
	if fmt.Sprint(got) != "[dep::example.com/lib]" {
		t.Fatalf("dependency contracts = %v", got)
	}
	if store.reads != 1 {
		t.Fatalf("two deltas read the contracts %d times, want once", store.reads)
	}
	idx := &Indexer{repoPrefix: "repo", graph: store, resolver: resolver.New(store)}
	installEditDeltaDeps(idx, "stack", []string{"go.mod"}, nil)
	if editDeltaDepSourceForTest(idx) != nil {
		t.Fatal("a change set holding go.mod installed the per-stack source")
	}
}

func editDeltaDepSourceForTest(idx *Indexer) func([]string) iter.Seq[graph.RepoNodeIdentity] {
	source, ok := editDeltaDepSources.Load(idx.resolver)
	if !ok {
		return nil
	}
	return source.(func([]string) iter.Seq[graph.RepoNodeIdentity])
}
