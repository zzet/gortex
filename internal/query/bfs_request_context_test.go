package query_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// cancellingBFSGraph is an in-memory graph whose request-aware BFS ends the
// request while it runs, the way a tool deadline fires mid-walk, and which
// counts every adjacency read the layer-walk fallback would make.
type cancellingBFSGraph struct {
	*graph.Graph
	cancel        context.CancelFunc
	bfsContext    atomic.Int32
	adjacencyRead atomic.Int32
}

func (g *cancellingBFSGraph) BFSContext(ctx context.Context, seeds []string, dir graph.Direction, kinds []graph.EdgeKind, maxDepth, limit int) ([]graph.BFSHop, error) {
	g.bfsContext.Add(1)
	if g.cancel != nil {
		g.cancel()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return g.BFS(seeds, dir, kinds, maxDepth, limit)
}

func (g *cancellingBFSGraph) GetInEdges(id string) []*graph.Edge {
	g.adjacencyRead.Add(1)
	return g.Graph.GetInEdges(id)
}

func (g *cancellingBFSGraph) GetOutEdges(id string) []*graph.Edge {
	g.adjacencyRead.Add(1)
	return g.Graph.GetOutEdges(id)
}

func (g *cancellingBFSGraph) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	g.adjacencyRead.Add(1)
	return g.Graph.GetInEdgesByNodeIDs(ids)
}

func (g *cancellingBFSGraph) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	g.adjacencyRead.Add(1)
	return g.Graph.GetOutEdgesByNodeIDs(ids)
}

// TestRequestBoundWalkUsesTheRequestAwareBFS pins that a request engine walks
// through the request-aware capability and answers what the plain walk does.
func TestRequestBoundWalkUsesTheRequestAwareBFS(t *testing.T) {
	g := &cancellingBFSGraph{Graph: graph.New()}
	bfsFixture(g.Graph)
	plain := query.NewEngine(g.Graph).GetCallers("d", query.QueryOptions{Depth: 4, Limit: 50})
	bound := query.NewEngine(g).WithRequestContext(context.Background()).GetCallers("d", query.QueryOptions{Depth: 4, Limit: 50})
	if g.bfsContext.Load() != 1 {
		t.Fatalf("request-aware BFS ran %d times, want 1", g.bfsContext.Load())
	}
	if got, want := nodeIDSet(bound), nodeIDSet(plain); !eqStrings(got, want) || len(want) < 2 {
		t.Fatalf("request-bound callers = %v, want %v", got, want)
	}
}

// TestAbandonedWalkDoesNotFallBackToTheLayerWalk pins the abandonment
// contract: once the request ends inside the backend BFS, the engine answers
// nothing and never re-runs the walk layer by layer.
func TestAbandonedWalkDoesNotFallBackToTheLayerWalk(t *testing.T) {
	g := &cancellingBFSGraph{Graph: graph.New()}
	bfsFixture(g.Graph)
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	sg := query.NewEngine(g).WithRequestContext(ctx).GetCallers("d", query.QueryOptions{Depth: 4, Limit: 50})
	if len(sg.Nodes) != 0 || len(sg.Edges) != 0 {
		t.Fatalf("abandoned walk answered %d nodes / %d edges, want none", len(sg.Nodes), len(sg.Edges))
	}
	if reads := g.adjacencyRead.Load(); reads != 0 {
		t.Fatalf("abandoned walk fell back to the layer walk (%d adjacency reads)", reads)
	}

	// A request that already ended does not even reach the backend.
	before := g.bfsContext.Load()
	query.NewEngine(g).WithRequestContext(ctx).GetCallers("d", query.QueryOptions{Depth: 4, Limit: 50})
	if g.bfsContext.Load() != before || g.adjacencyRead.Load() != 0 {
		t.Fatal("a walk for an ended request still read the graph")
	}
}

// TestWalkHonoursTheCallersRequestContext covers the shared base engine,
// which carries no request of its own: the caller's QueryOptions.Context
// binds the walk instead.
func TestWalkHonoursTheCallersRequestContext(t *testing.T) {
	g := &cancellingBFSGraph{Graph: graph.New()}
	bfsFixture(g.Graph)
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	sg := query.NewEngine(g).GetCallers("d", query.QueryOptions{Depth: 4, Limit: 50, Context: ctx})
	if g.bfsContext.Load() != 1 {
		t.Fatalf("request-aware BFS ran %d times, want 1", g.bfsContext.Load())
	}
	if len(sg.Nodes) != 0 || g.adjacencyRead.Load() != 0 {
		t.Fatalf("abandoned walk answered %d nodes after %d adjacency reads, want none", len(sg.Nodes), g.adjacencyRead.Load())
	}
}
