package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// evictionEdgeLossFixture builds the shape the eviction receipt gate was
// written for: a surviving file holds a RESOLVED incoming edge into a node
// that a neighbouring file's reindex is about to destroy. accesses_field is
// used because restubIncomingRefs parks only IsResolvableRefEdge kinds, so a
// capability edge is deleted outright rather than being moved to a stub.
func evictionEdgeLossFixture() (*graph.Graph, func() []*graph.Edge) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/b.go::T.F", Kind: graph.KindField, Name: "F", QualName: "T.F", FilePath: "b.go", RepoPrefix: "repo", Language: "go"},
	}, []*graph.Edge{{
		From: "repo/a.go::Caller", To: "repo/b.go::T.F", Kind: graph.EdgeAccessesField,
		FilePath: "a.go", Line: 3, Origin: graph.OriginASTInferred, Confidence: 1,
	}})

	surviving := func() []*graph.Edge {
		var out []*graph.Edge
		for _, e := range g.GetOutEdges("repo/a.go::Caller") {
			if e != nil && e.Kind == graph.EdgeAccessesField {
				out = append(out, e)
			}
		}
		return out
	}
	return g, surviving
}

// The load-bearing fact behind dropping the surviving-source eviction gate.
// The gate detected this edge's destruction and answered by failing the
// receipt closed, which forces the whole-graph fallback resolve. That fallback
// cannot put the edge back: it retargets edges that still exist and are parked
// under a stub, and this one was deleted, not parked. So the two paths reach
// the same graph and the gate buys nothing but the cost of the larger pass.
//
// The destruction itself is real and pre-existing; it is tracked separately.
// What this pins is only that the RESOLUTION delta stays exactly describable,
// which is the single question a receipt answers.
func TestResolveAllDoesNotRestoreAnEvictionDestroyedCapabilityEdge(t *testing.T) {
	g, surviving := evictionEdgeLossFixture()
	if len(surviving()) != 1 {
		t.Fatalf("fixture did not seed the resolved capability edge: %v", surviving())
	}

	// Reindex b.go the way the incremental path does: evict the file, then
	// re-add the identical definition.
	g.EvictFile("b.go")
	g.AddBatch([]*graph.Node{
		{ID: "repo/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/b.go::T.F", Kind: graph.KindField, Name: "F", QualName: "T.F", FilePath: "b.go", RepoPrefix: "repo", Language: "go"},
	}, nil)

	if got := surviving(); len(got) != 0 {
		t.Fatalf("eviction left the capability edge in place, fixture no longer reproduces the loss: %v", got)
	}

	New(g).ResolveAll()

	if got := surviving(); len(got) != 0 {
		t.Fatalf("whole-graph fallback restored the destroyed edge (%v); the gate's premise would then be wrong", got)
	}
}
