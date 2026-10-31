package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

type kindAdjacencySpy struct {
	graph.Reader
	backend                           graph.FilteredKindAdjacencyReader
	rows, queries, allNodes, allEdges int
}

func (r *kindAdjacencySpy) ReadKindAdjacencyFiltered(ctx context.Context, id string, dir graph.Direction, kinds []graph.EdgeKind, filter graph.OutlineEdgeFilter) (graph.KindAdjacency, error) {
	r.queries++
	out, err := r.backend.ReadKindAdjacencyFiltered(ctx, id, dir, kinds, filter)
	r.rows += len(out.Edges)
	return out, err
}
func (r *kindAdjacencySpy) AllNodes() []*graph.Node {
	r.allNodes++
	return r.Reader.AllNodes()
}
func (r *kindAdjacencySpy) AllEdges() []*graph.Edge {
	r.allEdges++
	return r.Reader.AllEdges()
}

func TestContractCoreKindAdjacencyMatchesGoWalkWithMasksCapsAndBoundaries(t *testing.T) {
	s := openContractCoreCapStore(t)
	s.AddBatch([]*graph.Node{
		{ID: "a/start", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/start.go"},
		{ID: "a/child", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/child.go"},
		{ID: "a/leaf", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/leaf.go"},
		{ID: "a/test", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/start_test.go"},
		{ID: "b/foreign", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/foreign.go"},
		{ID: "contract", Kind: graph.KindContract},
	}, []*graph.Edge{
		{From: "a/start", To: "a/child", Kind: graph.EdgeCalls, FilePath: "a/start.go", Line: 5, Meta: map[string]any{"payload": "retained"}},
		{From: "a/start", To: "a/leaf", Kind: graph.EdgeCalls, FilePath: "a/start.go", Line: 9},
		{From: "a/child", To: "a/start", Kind: graph.EdgeCalls, FilePath: "a/child.go", Line: 1},
		{From: "a/child", To: "a/leaf", Kind: graph.EdgeCalls, FilePath: "a/child.go", Line: 2},
		{From: "a/test", To: "a/start", Kind: graph.EdgeCalls},
		{From: "a/start", To: "b/foreign", Kind: graph.EdgeMatches},
		{From: "b/foreign", To: "a/start", Kind: graph.EdgeCalls},
		{From: "contract", To: "a/start", Kind: graph.EdgeCalls},
		{From: "a/start", To: "a/test", Kind: graph.EdgeCalls, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "a/start", To: "external::dynamic", Kind: graph.EdgeCalls},
	})
	// Non-call adjacency must still influence the legacy pre-cap sort test,
	// without transferring or decoding its payloads into the walker.
	for i := range 40 {
		id := fmt.Sprintf("a/value-%02d", i)
		s.AddBatch([]*graph.Node{{ID: id, Kind: graph.KindVariable, RepoPrefix: "a"}}, []*graph.Edge{{From: "a/start", To: id, Kind: graph.EdgeDefines}})
	}
	var baselineRows int
	for pass := range 2 {
		spy := &kindAdjacencySpy{Reader: s, backend: s}
		ctx := withContractCoreReadErrors(t.Context())
		fast := newContractCoreEdges(spy, ctx, nil)
		_, capable := fast.(graph.KindAdjacencyReader)
		require.True(t, capable)
		_, raw := fast.(graph.FilteredKindAdjacencyReader)
		require.False(t, raw, "a caller cannot bypass this wrapper's mask")
		slow := legacyOutlineReader{newContractCoreEdges(legacyOutlineReader{s}, ctx, nil)}
		for _, limit := range []int{3, 20} {
			for _, forward := range []bool{true, false} {
				for _, exclude := range []bool{false, true} {
					opts := query.QueryOptions{RepoAllow: map[string]bool{"a": true}, Depth: 3, Limit: limit, ExcludeTests: exclude, Context: ctx}
					eng := query.NewEngine(s)
					var got, want *query.SubGraph
					if forward {
						got = eng.WithReader(fast).GetCallChain("a/start", opts)
						want = eng.WithReader(slow).GetCallChain("a/start", opts)
					} else {
						got = eng.WithReader(fast).GetCallers("a/start", opts)
						want = eng.WithReader(slow).GetCallers("a/start", opts)
					}
					require.Equal(t, want, got, "limit=%d forward=%v exclude=%v", limit, forward, exclude)
				}
			}
		}
		require.Positive(t, spy.queries)
		require.Zero(t, spy.allNodes)
		require.Zero(t, spy.allEdges)
		require.NoError(t, contractCoreReadError(ctx))
		if pass == 0 {
			baselineRows = spy.rows
			for i := range 5 {
				prefix := fmt.Sprintf("unrelated-%d", i)
				s.AddBatch([]*graph.Node{{ID: prefix + "/from", Kind: graph.KindFunction, RepoPrefix: prefix}, {ID: prefix + "/to", Kind: graph.KindFunction, RepoPrefix: prefix}}, []*graph.Edge{{From: prefix + "/from", To: prefix + "/to", Kind: graph.EdgeCalls}})
			}
		} else {
			require.Equal(t, baselineRows, spy.rows, "unrelated repositories changed returned typed rows")
		}
	}
}

func TestContractCoreKindAdjacencyKeepsOverlayTombstones(t *testing.T) {
	s := newContractCoreEditingStore(t)
	layer := graph.NewOverlayLayer()
	layer.MarkFile("repo/b.go", true)
	selected := graph.NewOverlaidView(s, layer)
	reader := newContractCoreEdges(selected, t.Context(), nil)
	got, err := graph.ReadKindAdjacency(t.Context(), reader, "repo/a.go::F", graph.DirectionBackward, []graph.EdgeKind{graph.EdgeCalls})
	require.NoError(t, err)
	require.Empty(t, got.Edges, "tombstoned caller and masked contract/bean callers stay hidden")
	require.Zero(t, got.Total)
	// The base still has all three callers; a physical-store shortcut would
	// incorrectly resurrect at least the tombstoned one after masking.
	require.Len(t, s.GetInEdges("repo/a.go::F"), 3)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = graph.ReadKindAdjacency(ctx, reader, "repo/a.go::F", graph.DirectionBackward, []graph.EdgeKind{graph.EdgeCalls})
	require.ErrorIs(t, err, context.Canceled)
}
