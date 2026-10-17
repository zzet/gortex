package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// ResolveFilesOutgoing re-binds the references recorded in the files and
// reads (and so admits) none of the references parked on the names they
// declare. The control arm shows the same fixture's parked reference IS
// reachable through the incoming leg, so the outgoing-only arm leaving it
// parked is the contract and not a fixture that binds nothing.
func TestResolveFilesOutgoingRebindsOwnReferencesOnly(t *testing.T) {
	fixture := func() (*graph.Graph, *graph.Edge, *graph.Edge) {
		own := &graph.Edge{From: "repo/b.go::Caller", To: graph.UnresolvedMarker + "F", Kind: graph.EdgeCalls, FilePath: "repo/b.go", Line: 3}
		parked := &graph.Edge{From: "repo/c.go::Other", To: graph.UnresolvedMarker + "G", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 3}
		g := graph.New()
		g.AddBatch([]*graph.Node{
			{ID: "repo/a.go::F", Kind: graph.KindFunction, Name: "F", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"},
			{ID: "repo/b.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/b.go", RepoPrefix: "repo", Language: "go"},
			{ID: "repo/b.go::G", Kind: graph.KindFunction, Name: "G", FilePath: "repo/b.go", RepoPrefix: "repo", Language: "go"},
			{ID: "repo/c.go::Other", Kind: graph.KindFunction, Name: "Other", FilePath: "repo/c.go", RepoPrefix: "repo", Language: "go"},
		}, []*graph.Edge{own, parked})
		return g, own, parked
	}

	g, own, parked := fixture()
	counted := &incomingReadCounter{Store: g}
	New(counted).ResolveFilesOutgoing([]string{"repo/b.go"})
	if counted.reads != 0 {
		t.Fatalf("the outgoing-only pass read incoming rows %d time(s); it enumerates no stub key", counted.reads)
	}
	if own.To != "repo/a.go::F" {
		t.Fatalf("the file's own reference target = %q, want repo/a.go::F", own.To)
	}
	if parked.To != graph.UnresolvedMarker+"G" {
		t.Fatalf("a reference parked on a name the file declares was re-attempted (target %q); the outgoing-only pass admits no incoming reference", parked.To)
	}

	g, _, parked = fixture()
	New(g).ResolveFilesAndIncoming([]string{"repo/b.go"})
	if parked.To != "repo/b.go::G" {
		t.Fatalf("control: the incoming leg left the parked reference at %q; the fixture must be reachable through it", parked.To)
	}
}

// incomingReadCounter counts incoming-adjacency reads.
type incomingReadCounter struct {
	graph.Store
	reads int
}

func (c *incomingReadCounter) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	c.reads++
	return c.Store.GetInEdgesByNodeIDs(ids)
}
