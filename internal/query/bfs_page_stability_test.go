package query

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// buildMixedKindCallers writes one hot symbol with callerCount callers
// whose edges mix kinds and arrive in descending insertion order, plus
// one test caller — the shape where the in-memory store (insertion
// order) and SQLite (kind-grouped) disagree on raw edge order.
func buildMixedKindCallers(g graph.Store, callerCount int) string {
	hot := &graph.Node{ID: "pkg/hot.go::Hot", Kind: graph.KindFunction, Name: "Hot", FilePath: "pkg/hot.go", StartLine: 1}
	g.AddNode(hot)
	testCaller := &graph.Node{
		ID: "pkg/hot_test.go::TestHot", Kind: graph.KindFunction, Name: "TestHot",
		FilePath: "pkg/hot_test.go", StartLine: 3, Meta: map[string]any{"is_test": true},
	}
	g.AddNode(testCaller)
	g.AddEdge(&graph.Edge{From: testCaller.ID, To: hot.ID, Kind: graph.EdgeCalls, FilePath: testCaller.FilePath, Line: 5, Origin: graph.OriginASTResolved, Confidence: 0.9})
	kinds := []graph.EdgeKind{graph.EdgeReferences, graph.EdgeCalls, graph.EdgeMatches}
	for i := callerCount - 1; i >= 0; i-- {
		file := fmt.Sprintf("pkg/use%02d.go", i)
		caller := &graph.Node{ID: fmt.Sprintf("%s::Use%02d", file, i), Kind: graph.KindFunction, Name: fmt.Sprintf("Use%02d", i), FilePath: file, StartLine: 3}
		g.AddNode(caller)
		g.AddEdge(&graph.Edge{From: caller.ID, To: hot.ID, Kind: kinds[i%len(kinds)], FilePath: file, Line: 5, Origin: graph.OriginASTResolved, Confidence: 0.9})
	}
	return hot.ID
}

// TestCappedFilteredBFSPageBackendParity pins the membership of a
// capped, filtered caller page across backends: the filtered walk takes
// the per-node path on every backend, so without one deterministic edge
// order before admission the page depends on backend iteration order
// (insertion vs kind-grouped) — the same result on the same graph must
// not change with the storage engine or index history.
func TestCappedFilteredBFSPageBackendParity(t *testing.T) {
	page := func(g graph.Store, hotID string) map[string]bool {
		eng := NewEngine(g)
		sg := eng.GetCallers(hotID, QueryOptions{Depth: 1, Limit: 4, ExcludeTests: true})
		got := make(map[string]bool, len(sg.Edges))
		for _, e := range sg.Edges {
			got[e.From] = true
		}
		return got
	}

	mem := graph.New()
	memID := buildMixedKindCallers(mem, 12)

	sqlite, err := store_sqlite.Open(filepath.Join(t.TempDir(), "bfs.sqlite"))
	require.NoError(t, err)
	defer sqlite.Close()
	sqliteID := buildMixedKindCallers(sqlite, 12)

	memPage := page(mem, memID)
	sqlitePage := page(sqlite, sqliteID)
	require.Equal(t, memPage, sqlitePage,
		"the capped filtered caller page must hold the same members on every backend")
}

// countingStore wraps a backend and counts the point reads the
// per-node BFS path issues.
type countingStore struct {
	graph.Store
	inEdgeCalls int
	nodeCalls   int
}

func (c *countingStore) GetInEdges(id string) []*graph.Edge {
	c.inEdgeCalls++
	return c.Store.GetInEdges(id)
}

func (c *countingStore) GetNode(id string) *graph.Node {
	c.nodeCalls++
	return c.Store.GetNode(id)
}

// TestFilteredBFSQueryCountStaysFrontierLinear guards the cost of the
// per-node fallback the filtered walk uses: one in-edge fetch per
// expanded frontier node and at most one node fetch per admitted edge.
// A regression to per-edge (or worse) fetch patterns multiplies disk
// round-trips on exactly the path that already gave up batching for
// correctness.
func TestFilteredBFSQueryCountStaysFrontierLinear(t *testing.T) {
	base := graph.New()
	hotID := buildMixedKindCallers(base, 12)
	cs := &countingStore{Store: base}

	eng := NewEngine(cs)
	sg := eng.GetCallers(hotID, QueryOptions{Depth: 1, Limit: 50, ExcludeTests: true})
	require.NotEmpty(t, sg.Edges)

	// Depth 1 from one seed: the walk may fetch the seed's in-edges
	// (plus bounded per-call setup reads), never one fetch per edge.
	require.LessOrEqual(t, cs.inEdgeCalls, 4,
		"the per-node walk must fetch in-edges once per expanded frontier node, got %d calls for one seed", cs.inEdgeCalls)
	// 13 raw in-edges (12 production + 1 test): node materialisation
	// stays linear in edges seen, with bounded setup overhead.
	require.LessOrEqual(t, cs.nodeCalls, 2*13+6,
		"node fetches must stay linear in edges seen, got %d", cs.nodeCalls)
}
