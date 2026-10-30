package analysis

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type cancelAtAnalysisCheck struct {
	context.Context
	cancel       context.CancelFunc
	after, calls int
}

func (c *cancelAtAnalysisCheck) Err() error {
	c.calls++
	if c.calls == c.after {
		c.cancel()
	}
	return c.Context.Err()
}

func TestContextualHotspotsAndBetweennessPreserveCompletedRanking(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for _, g := range []*graph.Graph{pathGraph(20), relayStar(24), pathGraph(betweennessExactThreshold + 1)} {
		before := ComputeBetweenness(g)
		after, err := ComputeBetweennessContext(ctx, g)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("context changed complete centrality: %v", err)
		}
	}
	g := relayStar(24)
	before := FindHotspots(g, nil, 0)
	after, err := FindHotspotsContext(ctx, g, nil, 0)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("context changed complete hotspot ranking: %v", err)
	}
	cancel()
	if result, err := FindHotspotsContext(ctx, g, nil, 0); !errors.Is(err, context.Canceled) || result != nil {
		t.Fatal("cancelled ranking became a valid empty result")
	}
}

func TestBetweennessCancellationDiscardsAccumulatedPivots(t *testing.T) {
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &cancelAtAnalysisCheck{Context: base, cancel: cancel, after: 50}
	result, err := ComputeBetweennessContext(ctx, pathGraph(128))
	if !errors.Is(err, context.Canceled) || result != nil || ctx.calls < 50 {
		t.Fatalf("partial pivot scores escaped: %v %+v", err, result)
	}
}

func TestBrandesCancellationInsideAdjacencyAndDependencyWork(t *testing.T) {
	t.Run("high_degree_adjacency", func(t *testing.T) {
		adj := make([][]int, 4097)
		for i := 1; i < len(adj); i++ {
			adj[0] = append(adj[0], i)
		}
		scratch := newBrandesScratch(len(adj))
		base, cancel := context.WithCancel(t.Context())
		defer cancel()
		ctx := &cancelAtAnalysisCheck{Context: base, cancel: cancel, after: 9}
		err := brandesAccumulateContext(ctx, 0, adj, make([]float64, len(adj)), &scratch)
		if !errors.Is(err, context.Canceled) || len(scratch.queue) <= 1024 || len(scratch.queue) >= len(adj) {
			t.Fatalf("adjacency did not cancel during actual work: err%v queued%d", err, len(scratch.queue))
		}
	})
	t.Run("dependency_accumulation", func(t *testing.T) {
		adj := make([][]int, 1025)
		for i := 0; i < len(adj)-1; i++ {
			adj[i] = []int{i + 1}
		}
		scratch := newBrandesScratch(len(adj))
		score := make([]float64, len(adj))
		base, cancel := context.WithCancel(t.Context())
		defer cancel()
		ctx := &cancelAtAnalysisCheck{Context: base, cancel: cancel, after: 10}
		err := brandesAccumulateContext(ctx, 0, adj, score, &scratch)
		if !errors.Is(err, context.Canceled) || score[1023] == 0 || score[1] != 0 {
			t.Fatalf("dependency did not cancel during partial accumulation: err%v tail%f head%f", err, score[1023], score[1])
		}
	})
}
