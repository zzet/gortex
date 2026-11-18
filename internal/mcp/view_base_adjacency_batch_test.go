package mcp

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type baseAdjacencyBatchCorpus struct {
	graph.Reader
	nodeBatches [][]string
	walked      map[string][]*graph.Edge
}

func (c *baseAdjacencyBatchCorpus) GetNodesByIDs(ids []string) map[string]*graph.Node {
	c.nodeBatches = append(c.nodeBatches, append([]string(nil), ids...))
	nodes := c.Reader.GetNodesByIDs(ids)
	for _, id := range ids {
		if id == "nil-row" {
			nodes[id] = nil // A present nil row has the same unresolved semantics.
		}
	}
	return nodes
}

func (c *baseAdjacencyBatchCorpus) adjacency(ids []string) map[string][]*graph.Edge {
	out := make(map[string][]*graph.Edge, len(ids))
	for _, id := range ids {
		out[id] = c.walked[id]
	}
	return out
}

func (c *baseAdjacencyBatchCorpus) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return c.adjacency(ids)
}

func (c *baseAdjacencyBatchCorpus) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return c.adjacency(ids)
}

func TestBaseGraphReaderBatchAdjacencyHydratesFarEndpointsOnce(t *testing.T) {
	for _, incoming := range []bool{false, true} {
		for _, width := range []int{1, 32} {
			t.Run(fmt.Sprintf("incoming=%t/anchors=%d", incoming, width), func(t *testing.T) {
				inner := graph.New()
				inner.AddBatch([]*graph.Node{
					{ID: "hub", Kind: graph.KindFunction, FilePath: "repo/hub.go", RepoPrefix: "repo"},
					{ID: "foreign", Kind: graph.KindFunction, FilePath: "other/hub.go", RepoPrefix: "other"},
					{ID: "unowned", Kind: graph.KindFunction, FilePath: "repo/old.go"},
					{ID: "legacy", Kind: graph.KindFunction, FilePath: "legacy.go", RepoPrefix: "repo"},
				}, nil)
				spy := &baseAdjacencyBatchCorpus{Reader: inner, walked: make(map[string][]*graph.Edge)}
				var anchors []string
				want := make(map[string][]*graph.Edge)
				far := edgeTo
				if incoming {
					far = edgeFrom
				}
				for i := range width {
					id := fmt.Sprintf("anchor-%d", i)
					inner.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"})
					anchors = append(anchors, id)
					add := func(endpoint, site string, keep bool) {
						edge := &graph.Edge{From: id, To: endpoint, Kind: graph.EdgeCalls, FilePath: site, Line: i + 1,
							Origin: graph.OriginASTResolved, Meta: map[string]any{"fixture": endpoint}}
						if incoming {
							edge.From, edge.To = endpoint, id
						}
						spy.walked[id] = append(spy.walked[id], edge)
						if keep {
							want[id] = append(want[id], edge)
						}
					}
					add("hub", "repo/a.go", true)
					add("foreign", "repo/a.go", false)
					add("missing", "repo/a.go", true)
					add("nil-row", "repo/a.go", true)
					add("", "repo/a.go", true)
					add("unowned", "repo/a.go", true)
					add("legacy", "legacy.go", true)
					add("hub", "other/hub.go", false)
					add("hub", "unattributed.go", false)
					add("missing", "", true)
					spy.walked[id] = append(spy.walked[id], nil)
				}
				// Preserve empty and nil adjacency lists and refuse out-of-scope anchors.
				inner.AddNode(&graph.Node{ID: "empty", FilePath: "repo/a.go", RepoPrefix: "repo"})
				inner.AddNode(&graph.Node{ID: "nil", FilePath: "repo/a.go", RepoPrefix: "repo"})
				spy.walked["empty"] = []*graph.Edge{}
				anchors = append(anchors, "empty", "nil", "foreign", "missing-anchor")
				want["empty"], want["nil"] = []*graph.Edge{}, nil
				scoped := &baseGraphReader{base: spy, repoPrefix: "repo"}
				var got map[string][]*graph.Edge
				if incoming {
					got = scoped.GetInEdgesByNodeIDs(anchors)
				} else {
					got = scoped.GetOutEdgesByNodeIDs(anchors)
				}
				require.Equal(t, want, got)
				require.Len(t, spy.nodeBatches, 2, "anchors plus one shared far-endpoint batch, independent of width")
				require.ElementsMatch(t, []string{"hub", "foreign", "missing", "nil-row", "unowned", "legacy"}, spy.nodeBatches[1])
				for id, edges := range got {
					// The shared hydration applies the same predicate as each existing single list.
					require.Equal(t, scoped.keepEdges(spy.walked[id], far), edges)
				}
			})
		}
	}
}

func TestBaseGraphReaderBatchAdjacencyPreservesSelectedMasks(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: "repo/a.go::anchor", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "repo/target.go::current", Kind: graph.KindFunction, FilePath: "repo/target.go", RepoPrefix: "repo"},
		{ID: "repo/deleted.go::deleted", Kind: graph.KindFunction, FilePath: "repo/deleted.go", RepoPrefix: "repo"},
	}, []*graph.Edge{
		{From: "repo/a.go::anchor", To: "repo/target.go::current", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 1},
		{From: "repo/a.go::anchor", To: "repo/deleted.go::deleted", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 2},
	})
	layer := graph.NewOverlayLayer()
	layer.MarkFile("repo/deleted.go", true)
	layer.AddNode("repo/target.go", &graph.Node{ID: "repo/target.go::current", Kind: graph.KindFunction, FilePath: "repo/target.go", RepoPrefix: "other"})
	fresh := &graph.Edge{From: "repo/a.go::anchor", To: "repo/new.go::unresolved", Kind: graph.EdgeReferences, FilePath: "repo/new.go", Line: 3, Meta: map[string]any{"source": "selected"}}
	layer.AddEdge(fresh)
	selected := graph.NewOverlaidView(base, layer)
	scoped := newBaseGraphReader(selected, "repo")
	require.Equal(t, []*graph.Edge{fresh}, scoped.GetOutEdgesByNodeIDs([]string{"repo/a.go::anchor"})["repo/a.go::anchor"])
	require.Equal(t, scoped.GetOutEdges("repo/a.go::anchor"), scoped.GetOutEdgesByNodeIDs([]string{"repo/a.go::anchor"})["repo/a.go::anchor"])
	require.Empty(t, scoped.GetInEdgesByNodeIDs([]string{"repo/target.go::current", "repo/deleted.go::deleted"}), "selected foreign or deleted anchors remain unavailable")
}
