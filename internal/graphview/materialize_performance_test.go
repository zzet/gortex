//go:build performance

package graphview

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestPerformanceComposedReadCostByAncestryDepth is the measurement behind the policy
// number: what one more persisted ancestor actually costs a reader.
//
// It is a recorded measurement, not a threshold assertion — the harness runs
// with GOMAXPROCS=2 on a shared machine, so a timing bound here would be a
// flake generator. The point is that the numbers exist and are attributable:
// the chain is otherwise identical at every depth, each generation claims one
// file, and the probed symbol is the one the BOTTOM generation owns, which is
// the lookup that pays for every layer above it.
func TestPerformanceComposedReadCostByAncestryDepth(t *testing.T) {
	const lookups = 2000
	ctx := context.Background()
	for _, depth := range []int{1, 8, 16, 32} {
		t.Run(fmt.Sprintf("depth_%d", depth), func(t *testing.T) {
			store := openStackStore(t, fmt.Sprintf("cost-depth-%d", depth))
			chain := writeDedicatedChain(t, store, depth)
			seedStackControlPlane(t, store, chain[0])
			materializer := newTestMaterializer(store)

			start := time.Now()
			view, err := materializer.assemble(ctx, testGraphID, stackRepo, []int64{chain[len(chain)-1]}, nil)
			if err != nil {
				t.Fatalf("assemble depth %d: %v", depth, err)
			}
			defer view.Close()
			assembled := time.Since(start)

			deepest := chainSymbolID(0)
			if view.Reader.GetNode(deepest) == nil {
				t.Fatalf("depth %d lost the root symbol %q", depth, deepest)
			}
			start = time.Now()
			for range lookups {
				_ = view.Reader.GetNode(deepest)
			}
			perNode := time.Since(start) / lookups
			start = time.Now()
			for range lookups {
				_ = view.Reader.GetOutEdges(deepest)
			}
			perEdge := time.Since(start) / lookups

			t.Logf("ancestry depth %2d: assemble %8v | GetNode(root symbol) %8v/op | GetOutEdges(root symbol) %8v/op",
				depth, assembled, perNode, perEdge)
		})
	}
}
