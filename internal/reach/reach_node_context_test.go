package reach

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type contextNodeStoreStub struct {
	graph.Store
	err    error
	called bool
}

func (s *contextNodeStoreStub) GetNodeContext(context.Context, string) (*graph.Node, error) {
	s.called = true
	return nil, s.err
}

func TestGetNodeContextUsesOptionalStoreMethod(t *testing.T) {
	s := &contextNodeStoreStub{err: context.Canceled}
	_, err := getNodeContext(context.Background(), s, "seed")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("getNodeContext error = %v, want context.Canceled", err)
	}
	if !s.called {
		t.Fatal("optional GetNodeContext was not called")
	}
}

type legacyNodeStoreStub struct {
	graph.Store
	called bool
}

func (s *legacyNodeStoreStub) GetNode(string) *graph.Node {
	s.called = true
	return nil
}

func TestGetNodeContextChecksCancellationBeforeLegacyFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &legacyNodeStoreStub{}

	_, err := getNodeContext(ctx, s, "seed")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("getNodeContext error = %v, want context.Canceled", err)
	}
	if s.called {
		t.Fatal("legacy GetNode called after cancellation")
	}
}

// kindProjectionStore answers node kinds from its own projection and fails
// any read of node bodies.
type kindProjectionStore struct {
	graph.Store
	t         *testing.T
	kinds     map[string]graph.NodeKind
	projected int
}

func (s *kindProjectionStore) GetNodeKindsByIDsContext(_ context.Context, ids []string) (map[string]graph.NodeKindRow, error) {
	s.projected++
	out := make(map[string]graph.NodeKindRow)
	for _, id := range ids {
		if kind, ok := s.kinds[id]; ok {
			out[id] = graph.NodeKindRow{Kind: kind}
		}
	}
	return out, nil
}

func (s *kindProjectionStore) GetNodesByIDs([]string) map[string]*graph.Node {
	s.t.Error("compute read node bodies; reached nodes are classified from the kind projection")
	return nil
}

func TestComputeClassifiesReachedNodesFromTheKindProjection(t *testing.T) {
	g, ids := newCallChain(t, 3)
	g.AddNode(&graph.Node{ID: "ghost", Kind: graph.KindFunction, Name: "ghost", FilePath: "main.go"})
	g.AddEdge(&graph.Edge{From: "ghost", To: ids[2], Kind: graph.EdgeCalls, Confidence: 1})
	// The projection decides: a file is walked through but left out of the
	// tiers, and an identity it does not return is left out entirely.
	store := &kindProjectionStore{Store: g, t: t, kinds: map[string]graph.NodeKind{
		ids[0]: graph.KindFunction,
		ids[1]: graph.KindFile,
	}}

	tiers, truncated := compute(context.Background(), store, ids[2], nil)
	if truncated {
		t.Fatal("compute truncated an unbounded three-node chain")
	}
	if store.projected == 0 {
		t.Fatal("compute never read the kind projection")
	}
	if len(tiers[0].IDs) != 0 {
		t.Fatalf("depth 1 = %v, want the file and the unprojected caller left out", tiers[0].IDs)
	}
	if len(tiers[1].IDs) != 1 || tiers[1].IDs[0] != ids[0] {
		t.Fatalf("depth 2 = %v, want [%s] reached through the file", tiers[1].IDs, ids[0])
	}
}

type blockingContextNodesStore struct {
	graph.Store
}

func (s *blockingContextNodesStore) GetNodesByIDsContext(ctx context.Context, _ []string) (map[string]*graph.Node, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestComputeCancellationBoundsNodeHydration(t *testing.T) {
	g, ids := newCallChain(t, 2)
	store := &blockingContextNodesStore{Store: g}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, truncated := compute(ctx, store, ids[1], nil)
	elapsed := time.Since(started)
	if !truncated {
		t.Fatal("compute must mark a cancelled node hydration as truncated")
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("compute returned after %s, want cancellation-bounded traversal", elapsed)
	}
}
