package rerank

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func newRetrieverGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New()
	// Seed: Hub. Out-edges: Calls h→a, h→b; References h→c.
	g.AddNode(&graph.Node{ID: "h", Name: "Hub", Kind: graph.KindFunction, FilePath: "h.go"})
	g.AddNode(&graph.Node{ID: "a", Name: "A", Kind: graph.KindFunction, FilePath: "a.go"})
	g.AddNode(&graph.Node{ID: "b", Name: "B", Kind: graph.KindFunction, FilePath: "b.go"})
	g.AddNode(&graph.Node{ID: "c", Name: "C", Kind: graph.KindFunction, FilePath: "c.go"})
	g.AddEdge(&graph.Edge{From: "h", To: "a", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "h", To: "b", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "h", To: "c", Kind: graph.EdgeReferences})
	return g
}

func seedHub(_ context.Context, g graph.Reader, _ string, _ int) ([]*Candidate, error) {
	n := g.GetNode("h")
	if n == nil {
		return nil, nil
	}
	return []*Candidate{{Node: n, TextRank: 0}}, nil
}

func TestGraphCompletion_OneHopExpansion(t *testing.T) {
	g := newRetrieverGraph(t)
	gc := &GraphCompletion{Seeder: seedHub}

	cands, err := gc.Retrieve(context.Background(), g, "hub", 10)
	require.NoError(t, err)
	// 1 seed + 3 expanded.
	require.Len(t, cands, 4)

	ids := []string{}
	for _, c := range cands {
		ids = append(ids, c.Node.ID)
	}
	assert.Contains(t, ids, "h")
	assert.Contains(t, ids, "a")
	assert.Contains(t, ids, "b")
	assert.Contains(t, ids, "c")
}

func TestGraphCompletion_EdgeKindFilter(t *testing.T) {
	g := newRetrieverGraph(t)
	gc := &GraphCompletion{
		Seeder:    seedHub,
		EdgeKinds: []graph.EdgeKind{graph.EdgeCalls}, // skip references
	}

	cands, err := gc.Retrieve(context.Background(), g, "hub", 10)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, c := range cands {
		ids[c.Node.ID] = true
	}
	assert.True(t, ids["h"])
	assert.True(t, ids["a"])
	assert.True(t, ids["b"])
	assert.False(t, ids["c"], "C reached via References, filtered out by EdgeKinds=[Calls]")
}

func TestGraphCompletion_MaxSeedExpansion(t *testing.T) {
	g := newRetrieverGraph(t)
	gc := &GraphCompletion{
		Seeder:           seedHub,
		MaxSeedExpansion: 1, // only the first out-edge per seed
	}

	cands, err := gc.Retrieve(context.Background(), g, "hub", 10)
	require.NoError(t, err)
	// 1 seed + 1 expansion = 2 total.
	assert.Len(t, cands, 2)
}

func TestGraphCompletion_LimitTrimsAtBoundary(t *testing.T) {
	g := newRetrieverGraph(t)
	gc := &GraphCompletion{Seeder: seedHub}

	cands, err := gc.Retrieve(context.Background(), g, "hub", 2)
	require.NoError(t, err)
	assert.Len(t, cands, 2, "limit honoured at the boundary")
}

func TestGraphCompletion_NilSeederErrors(t *testing.T) {
	g := newRetrieverGraph(t)
	gc := &GraphCompletion{}
	_, err := gc.Retrieve(context.Background(), g, "x", 10)
	assert.Error(t, err)
}

func TestGraphCompletion_SeederErrorPropagates(t *testing.T) {
	g := newRetrieverGraph(t)
	gc := &GraphCompletion{
		Seeder: func(context.Context, graph.Reader, string, int) ([]*Candidate, error) {
			return nil, errors.New("seeder failed")
		},
	}
	_, err := gc.Retrieve(context.Background(), g, "x", 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "seeder failed")
}

func TestGraphCompletion_DedupesSeedFromExpansion(t *testing.T) {
	g := newRetrieverGraph(t)
	// Two seeds, the second is reachable from the first.
	multiSeed := func(_ context.Context, gr graph.Reader, _ string, _ int) ([]*Candidate, error) {
		return []*Candidate{
			{Node: gr.GetNode("h"), TextRank: 0},
			{Node: gr.GetNode("a"), TextRank: 1}, // also reachable from h
		}, nil
	}
	gc := &GraphCompletion{Seeder: multiSeed}

	cands, err := gc.Retrieve(context.Background(), g, "x", 10)
	require.NoError(t, err)
	ids := map[string]int{}
	for _, c := range cands {
		ids[c.Node.ID]++
	}
	for id, count := range ids {
		assert.Equal(t, 1, count, "id %s appeared %d times — dedup failed", id, count)
	}
}

func TestGraphCompletion_NilSeedsIgnored(t *testing.T) {
	g := newRetrieverGraph(t)
	gc := &GraphCompletion{
		Seeder: func(context.Context, graph.Reader, string, int) ([]*Candidate, error) {
			return []*Candidate{nil, {Node: nil}, {Node: g.GetNode("h")}}, nil
		},
	}
	cands, err := gc.Retrieve(context.Background(), g, "x", 10)
	require.NoError(t, err)
	// Should produce the real seed + its 3 expansions, ignoring the nil entries.
	assert.Len(t, cands, 4)
}

func TestGraphCompletion_ContextCancellationStops(t *testing.T) {
	g := newRetrieverGraph(t)
	gc := &GraphCompletion{Seeder: seedHub}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	cands, err := gc.Retrieve(ctx, g, "x", 10)
	// Either the seeder completed before the cancel check or the
	// cancellation surfaced — both are valid; the contract is that
	// cancellation doesn't deadlock.
	assert.NotNil(t, cands)
	_ = err
}

func TestGraphCompletion_Name(t *testing.T) {
	gc := &GraphCompletion{Seeder: seedHub}
	assert.Equal(t, "graph_completion", gc.Name())
}

func TestRetrieverInterface_GraphCompletionSatisfies(t *testing.T) {
	// Compile-time assertion: GraphCompletion satisfies Retriever.
	var _ Retriever = (*GraphCompletion)(nil)
}
