package indexer

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"runtime/metrics"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// recordingEmbedder returns a fresh vector per text, as every real provider
// does, and keeps the slices it returned so a test can check who owns them.
// vectorFor decides each vector; the default is a valid one-hot vector.
type recordingEmbedder struct {
	dims      int
	vectorFor func(i int) []float32
	returned  [][]float32
}

func (e *recordingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	out, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

func (e *recordingEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		n := len(e.returned)
		var vec []float32
		if e.vectorFor != nil {
			vec = e.vectorFor(n)
		} else {
			vec = make([]float32, e.dims)
			vec[n%e.dims] = float32(n + 1)
		}
		out[i] = vec
		e.returned = append(e.returned, vec)
	}
	return out, nil
}

func (e *recordingEmbedder) Dimensions() int { return e.dims }
func (e *recordingEmbedder) Close() error    { return nil }

func vectorPlanFunctionNodes(n int) []*graph.Node {
	nodes := make([]*graph.Node, n)
	for i := range nodes {
		name := fmt.Sprintf("Fn%d", i)
		nodes[i] = &graph.Node{
			ID: "repo/a.go::" + name, Kind: graph.KindFunction, Name: name,
			FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go",
			StartLine: 1, EndLine: 1,
		}
	}
	return nodes
}

func sameBacking(a, b []float32) bool {
	return len(a) > 0 && len(b) > 0 && &a[0] == &b[0]
}

// TestPrepareVectorPlanOwnsProviderVectors checks that the plan keeps the
// provider's vectors instead of copying them: each item holds the slice the
// provider returned for its text, no two items share a backing array, and
// invalid vectors are dropped and counted as before.
func TestPrepareVectorPlanOwnsProviderVectors(t *testing.T) {
	const dims = 3
	invalid := map[int]bool{1: true, 3: true, 4: true, 6: true, 7: true, 8: true}
	embedder := &recordingEmbedder{dims: dims, vectorFor: func(i int) []float32 {
		switch {
		case !invalid[i]:
			vec := make([]float32, dims)
			vec[i%dims] = float32(i + 1)
			return vec
		case i%2 == 1:
			return make([]float32, dims+1) // wrong width
		default:
			return []float32{float32(math.NaN()), 0, 0}
		}
	}}
	nodes := vectorPlanFunctionNodes(10)

	idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("repo")
	idx.SetEmbedder(embedder)
	plan, err := idx.prepareVectorPlan(context.Background(), nodes)
	require.NoError(t, err)
	require.NotNil(t, plan)
	defer plan.Release()

	require.Len(t, embedder.returned, len(nodes))
	require.Equal(t, len(invalid), plan.dropped)
	require.Equal(t, []string{
		nodes[1].ID, nodes[3].ID, nodes[4].ID, nodes[6].ID, nodes[7].ID,
	}, plan.droppedIDs, "droppedIDs keeps the first five in input order")

	var wantIDs []string
	var wantVecs [][]float32
	for i, n := range nodes {
		if !invalid[i] {
			wantIDs = append(wantIDs, n.ID)
			wantVecs = append(wantVecs, embedder.returned[i])
		}
	}
	require.Len(t, plan.items, len(wantIDs))
	for i, item := range plan.items {
		require.Equal(t, wantIDs[i], item.NodeID, "item %d keeps input order", i)
		require.True(t, sameBacking(item.Vec, wantVecs[i]),
			"item %d holds the provider's vector, not a copy", i)
		for j := range i {
			require.False(t, sameBacking(item.Vec, plan.items[j].Vec),
				"items %d and %d share a backing array", j, i)
		}
	}

	// Changing a returned slice is visible through its item and only there.
	wantVecs[0][0] = 42
	require.Equal(t, float32(42), plan.items[0].Vec[0])
	for _, item := range plan.items[1:] {
		require.NotEqual(t, float32(42), item.Vec[0])
	}
}

// BenchmarkPrepareVectorPlan_155k measures the peak heap of one vector-plan
// pass at the size of the gortex repository's own corpus (#789): 155,000
// texts embedded at 768 dimensions by an in-process fake provider that
// allocates a fresh vector per text. peak-heap-inuse-B is the highest
// HeapInuse seen during prepareVectorPlan minus HeapInuse just before it
// (text collection is its first step).
func BenchmarkPrepareVectorPlan_155k(b *testing.B) {
	const (
		texts = 155_000
		dims  = 768
	)
	nodes := vectorPlanFunctionNodes(texts)

	var peak uint64
	for b.Loop() {
		b.StopTimer()
		embedder := &benchVectorEmbedder{dims: dims}
		idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
		idx.SetRepoPrefix("repo")
		idx.SetEmbedder(embedder)
		idx.SetEmbeddingMaxSymbols(texts)
		runtime.GC()
		base := heapInuseBytes()
		stop := sampleHeapInuse()
		b.StartTimer()

		plan, err := idx.prepareVectorPlan(context.Background(), nodes)

		b.StopTimer()
		high := stop()
		if err != nil {
			b.Fatal(err)
		}
		if len(plan.items) != texts {
			b.Fatalf("plan has %d items, want %d", len(plan.items), texts)
		}
		plan.Release()
		if high > base && high-base > peak {
			peak = high - base
		}
		b.StartTimer()
	}
	b.ReportMetric(float64(peak), "peak-heap-inuse-B")
}

// benchVectorEmbedder is a cheap deterministic provider: it fills each
// fresh vector from a counter so no two vectors are identical.
type benchVectorEmbedder struct {
	dims int
	n    int
}

func (e *benchVectorEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	out, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

func (e *benchVectorEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		vec := make([]float32, e.dims)
		e.n++
		vec[e.n%e.dims] = float32(e.n)
		out[i] = vec
	}
	return out, nil
}

func (e *benchVectorEmbedder) Dimensions() int { return e.dims }
func (e *benchVectorEmbedder) Close() error    { return nil }

// HeapInuse in runtime/metrics terms: spans holding objects, live or not
// yet swept, plus their unused tail. Reading metrics does not stop the
// world, unlike runtime.ReadMemStats.
var heapInuseSamples = []metrics.Sample{
	{Name: "/memory/classes/heap/objects:bytes"},
	{Name: "/memory/classes/heap/unused:bytes"},
}

func heapInuseBytes() uint64 {
	samples := make([]metrics.Sample, len(heapInuseSamples))
	copy(samples, heapInuseSamples)
	metrics.Read(samples)
	return samples[0].Value.Uint64() + samples[1].Value.Uint64()
}

// sampleHeapInuse polls HeapInuse until the returned stop function is
// called, and stop returns the highest value seen.
func sampleHeapInuse() (stop func() uint64) {
	var high atomic.Uint64
	high.Store(heapInuseBytes())
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if v := heapInuseBytes(); v > high.Load() {
					high.Store(v)
				}
			}
		}
	}()
	return func() uint64 {
		close(done)
		<-finished
		if v := heapInuseBytes(); v > high.Load() {
			high.Store(v)
		}
		return high.Load()
	}
}
