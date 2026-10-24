package graph

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReindexEdgeConvergenceSingleSlot(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "a::A", Name: "A", Kind: KindFunction, FilePath: "a", RepoPrefix: "repo"})
	g.AddNode(&Node{ID: "b::B", Name: "B", Kind: KindFunction, FilePath: "b", RepoPrefix: "repo"})

	first := &Edge{From: "a::A", To: "unresolved::first", Kind: EdgeCalls, FilePath: "a", Line: 3}
	second := &Edge{From: "a::A", To: "unresolved::second", Kind: EdgeCalls, FilePath: "a", Line: 3}
	g.AddEdge(first)
	g.AddEdge(second)
	require.Len(t, g.AllEdges(), 2) // Populate the snapshot before the slot count changes.

	for _, e := range []*Edge{first, second} {
		oldTo := e.To
		e.To = "b::B"
		g.ReindexEdge(e, oldTo)
	}

	out := g.GetOutEdges("a::A")
	in := g.GetInEdges("b::B")
	assert.Len(t, out, 1, "converging identities must share one outgoing slot")
	assert.Len(t, in, 1, "converging identities must share one incoming slot")
	assert.Equal(t, 1, g.EdgeCount(), "only one unique edge remains")
	assert.Equal(t, 1, g.RepoMemoryEstimate("repo").EdgeCount)
	assert.Equal(t, edgeBytes(second), g.RepoMemoryEstimate("repo").EdgeBytes)
	assert.Empty(t, g.GetInEdges("unresolved::first"))
	assert.Empty(t, g.GetInEdges("unresolved::second"))
	assert.Len(t, g.AllEdges(), 1, "the cached snapshot must lose the duplicate slot")

	// Re-adding and removing the surviving identity must use consistent sidecars.
	g.AddEdge(second)
	assert.Len(t, g.GetOutEdges("a::A"), 1)
	assert.Len(t, g.GetInEdges("b::B"), 1)
	assert.Equal(t, 1, g.EdgeCount())
	require.True(t, g.RemoveEdge("a::A", "b::B", EdgeCalls))
	assert.Empty(t, g.GetOutEdges("a::A"))
	assert.Empty(t, g.GetInEdges("b::B"))
	assert.Zero(t, g.EdgeCount())
	assert.Zero(t, g.RepoMemoryEstimate("repo").EdgeCount)
	assert.Zero(t, g.RepoMemoryEstimate("repo").EdgeBytes)
	assert.Empty(t, g.AllEdges())
}

func TestRefreshEdgeIdentityConvergenceSingleSlot(t *testing.T) {
	for _, oldTo := range []string{"b::B", "unresolved::B"} {
		t.Run(oldTo, func(t *testing.T) {
			g := New()
			g.AddNode(&Node{ID: "a::A", Name: "A", Kind: KindFunction, FilePath: "a", RepoPrefix: "repo"})
			g.AddNode(&Node{ID: "b::B", Name: "B", Kind: KindFunction, FilePath: "b", RepoPrefix: "repo"})

			moving := &Edge{From: "a::A", To: oldTo, Kind: EdgeCalls, FilePath: "a", Line: 9}
			existing := &Edge{From: "a::A", To: "b::B", Kind: EdgeCalls, FilePath: "a", Line: 3}
			g.AddEdge(moving)
			g.AddEdge(existing)
			require.Len(t, g.AllEdges(), 2) // Populate the snapshot before convergence.

			// Refresh a detached payload onto a stationary destination.
			updated := *moving
			updated.To = existing.To
			updated.Line = existing.Line
			g.ReindexEdges([]EdgeReindex{{
				Edge: &updated, OldTo: oldTo, OldFilePath: moving.FilePath,
				OldLine: moving.Line, RefreshIdentity: true,
			}})

			out := g.GetOutEdges("a::A")
			in := g.GetInEdges("b::B")
			assert.Len(t, out, 1, "converging identities must share one outgoing slot")
			require.Len(t, in, 1, "converging identities must share one incoming slot")
			assert.Equal(t, 1, g.EdgeCount(), "only one unique edge remains")
			assert.Equal(t, 1, g.RepoMemoryEstimate("repo").EdgeCount)
			assert.Equal(t, edgeBytes(existing), g.RepoMemoryEstimate("repo").EdgeBytes)
			assert.Equal(t, []*Edge{existing}, out)
			assert.Same(t, existing, in[0])
			assert.Equal(t, updated, *existing)
			if oldTo != updated.To {
				assert.Empty(t, g.GetInEdges(oldTo))
			}
			assert.Len(t, g.AllEdges(), 1, "the cached snapshot must lose the duplicate slot")
			require.NoError(t, g.VerifyEdgeIdentities())

			// A later replacement and removal must use consistent sidecars.
			g.AddEdge(existing)
			require.True(t, g.RemoveEdge("a::A", "b::B", EdgeCalls))
			assert.Empty(t, g.GetOutEdges("a::A"))
			assert.Empty(t, g.GetInEdges("b::B"))
			assert.Zero(t, g.EdgeCount())
			assert.Zero(t, g.RepoMemoryEstimate("repo").EdgeCount)
			assert.Zero(t, g.RepoMemoryEstimate("repo").EdgeBytes)
			assert.Empty(t, g.AllEdges())
		})
	}
}

func TestReindexEdgesPreservesMovingSiblings(t *testing.T) {
	for _, refresh := range []bool{true, false} {
		name := "targets"
		if refresh {
			name = "spans"
		}
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", name, reverse), func(t *testing.T) {
				g := New()
				g.AddNode(&Node{ID: "a::A", RepoPrefix: "repo"})
				first := &Edge{From: "a::A", To: "b::B", Kind: EdgeCalls, FilePath: "a", Line: 10}
				second := &Edge{From: "a::A", To: "b::B", Kind: EdgeCalls, FilePath: "a", Line: 11}
				if !refresh {
					second.To = "c::C"
					second.Line = first.Line
				}
				g.AddEdge(first)
				g.AddEdge(second)
				require.Len(t, g.AllEdges(), 2)
				wantFirst, wantSecond := *first, *second
				batch := []EdgeReindex{
					{Edge: first, OldTo: first.To},
					{Edge: second, OldTo: second.To},
				}
				if refresh {
					wantFirst.Line++
					wantSecond.Line++
					for i, want := range []*Edge{&wantFirst, &wantSecond} {
						batch[i].RefreshIdentity = true
						batch[i].OldFilePath = batch[i].Edge.FilePath
						batch[i].OldLine = batch[i].Edge.Line
						batch[i].Edge = want
					}
				} else {
					wantFirst.To = "c::C"
					wantSecond.To = "longer::D"
					*first, *second = wantFirst, wantSecond
				}
				if reverse {
					batch[0], batch[1] = batch[1], batch[0]
				}
				g.ReindexEdges(batch)

				out := g.GetOutEdges("a::A")
				require.Len(t, out, 2, "an old identity occupied by a moving sibling is not convergence")
				assert.Equal(t, []*Edge{first, second}, out, "moving edges keep their outgoing slots")
				assert.Equal(t, wantFirst, *first)
				assert.Equal(t, wantSecond, *second)
				assert.Equal(t, 2, g.EdgeCount())
				assert.Equal(t, 2, g.RepoMemoryEstimate("repo").EdgeCount)
				assert.Equal(t, edgeBytes(&wantFirst)+edgeBytes(&wantSecond), g.RepoMemoryEstimate("repo").EdgeBytes)
				assert.Len(t, g.AllEdges(), 2)
				require.NoError(t, g.VerifyEdgeIdentities())
			})
		}
	}
}

func TestReindexEdgesConvergenceSingleSlot(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh=%t", refresh), func(t *testing.T) {
			g := New()
			g.AddNode(&Node{ID: "a::A", RepoPrefix: "repo"})
			existing := &Edge{From: "a::A", To: "b::B", Kind: EdgeCalls, FilePath: "a", Line: 3}
			g.AddEdge(existing)
			batch := make([]EdgeReindex, 0, 2)
			for _, target := range []string{"unresolved::first", "unresolved::second"} {
				e := *existing
				e.To = target
				if refresh {
					e.FilePath = "a/longer/path.go"
					e.Line = 9
				}
				g.AddEdge(&e)
				r := EdgeReindex{OldTo: e.To, OldFilePath: e.FilePath, OldLine: e.Line, RefreshIdentity: refresh}
				updated := *existing
				r.Edge = &updated
				batch = append(batch, r)
			}
			g.ReindexEdges(batch)
			assert.Len(t, g.GetOutEdges("a::A"), 1)
			assert.Len(t, g.GetInEdges("b::B"), 1)
			assert.Equal(t, 1, g.EdgeCount())
			assert.Equal(t, 1, g.RepoMemoryEstimate("repo").EdgeCount)
			assert.Equal(t, edgeBytes(existing), g.RepoMemoryEstimate("repo").EdgeBytes)
			require.NoError(t, g.VerifyEdgeIdentities())
		})
	}
}

func TestReindexEdgesKeepsOutgoingOrderAroundConvergence(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "a::A", RepoPrefix: "repo"})
	var edges []*Edge
	for _, to := range []string{"unresolved::p", "unresolved::q", "r::R", "s::S"} {
		e := &Edge{From: "a::A", To: to, Kind: EdgeCalls, FilePath: "a", Line: 3}
		edges = append(edges, e)
		g.AddEdge(e)
	}
	p, q, r, s := edges[0], edges[1], edges[2], edges[3]
	require.Equal(t, []*Edge{p, q, r, s}, g.GetOutEdges("a::A"))

	// q converges onto the stationary r; p moves without converging.
	p.To, q.To = "p::P", r.To
	g.ReindexEdges([]EdgeReindex{{Edge: q, OldTo: "unresolved::q"}, {Edge: p, OldTo: "unresolved::p"}})

	assert.Equal(t, []*Edge{p, r, s}, g.GetOutEdges("a::A"), "survivors keep their order; stationary r keeps its slot")
	assert.Equal(t, 3, g.EdgeCount())
	assert.Equal(t, 3, g.RepoMemoryEstimate("repo").EdgeCount)
	assert.Equal(t, edgeBytes(p)+edgeBytes(q)+edgeBytes(s), g.RepoMemoryEstimate("repo").EdgeBytes)
	require.NoError(t, g.VerifyEdgeIdentities())
}

func TestGraphReindexEdgesNonconvergingRefresh(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "a::A", RepoPrefix: "repo"})
	edge := &Edge{From: "a::A", To: "b::B", Kind: EdgeCalls, FilePath: "a.go", Line: 3}
	g.AddEdge(edge)
	updated := cloneDeltaEdge(edge)
	updated.Line = 4
	g.ReindexEdges([]EdgeReindex{{}, {Edge: updated, OldTo: edge.To,
		OldFilePath: edge.FilePath, OldLine: edge.Line, RefreshIdentity: true}})
	assert.Equal(t, []*Edge{updated}, g.GetOutEdges("a::A"))
	assert.Equal(t, []*Edge{updated}, g.GetInEdges("b::B"))
	assert.Equal(t, 1, g.EdgeCount())
	assert.Equal(t, 1, g.RepoMemoryEstimate("repo").EdgeCount)
	require.NoError(t, g.VerifyEdgeIdentities())
}
