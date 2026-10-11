package analysis

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// BenchmarkLeidenCacheRetainedHeap measures the topology heap that used to be
// pinned by LeidenPartitionCache in addition to its assignment map. It is a
// retained-heap benchmark, not a throughput benchmark; run with -benchtime=1x.
func BenchmarkLeidenCacheRetainedHeap(b *testing.B) {
	part := benchmarkLeidenPartition(10_000)
	if part == nil || len(part.comm) == 0 {
		b.Fatal("benchmark graph produced no Leiden partition")
	}

	runtime.GC()
	var withTopology runtime.MemStats
	runtime.ReadMemStats(&withTopology)

	assignments := part.comm
	adjacencyEntries := 0
	for _, neighbors := range part.neighbors {
		adjacencyEntries += len(neighbors)
	}
	part.neighbors = nil
	part.degree = nil
	part.symbolNodes = nil
	runtime.GC()
	var assignmentsOnly runtime.MemStats
	runtime.ReadMemStats(&assignmentsOnly)
	runtime.KeepAlive(assignments)

	var avoided uint64
	if withTopology.HeapAlloc > assignmentsOnly.HeapAlloc {
		avoided = withTopology.HeapAlloc - assignmentsOnly.HeapAlloc
	}
	b.ReportMetric(float64(avoided), "avoided-retained-B")
	b.ReportMetric(float64(adjacencyEntries), "avoided-adjacency-entries")
	b.ReportMetric(float64(len(assignments)), "retained-assignments")
}

func benchmarkLeidenPartition(nodeCount int) *leidenPartition {
	g := graph.New()
	nodes := make([]*graph.Node, 0, nodeCount)
	edges := make([]*graph.Edge, 0, nodeCount*2)
	for i := 0; i < nodeCount; i++ {
		id := fmt.Sprintf("pkg/mod%d/f%d.go::Fn%d", i/50, i, i)
		nodes = append(nodes, &graph.Node{
			ID:       id,
			Kind:     graph.KindFunction,
			Name:     fmt.Sprintf("Fn%d", i),
			FilePath: fmt.Sprintf("pkg/mod%d/f%d.go", i/50, i),
			Language: "go",
		})
	}
	for i := range nodes {
		base := (i / 50) * 50
		pos := i - base
		next := base + (pos+1)%50
		next2 := base + (pos+2)%50
		edges = append(edges,
			&graph.Edge{From: nodes[i].ID, To: nodes[next].ID, Kind: graph.EdgeCalls},
			&graph.Edge{From: nodes[i].ID, To: nodes[next2].ID, Kind: graph.EdgeCalls},
		)
	}
	g.AddBatch(nodes, edges)
	_, part := detectCommunitiesLeidenRaw(g, defaultLeidenOptions(), nil)
	return part
}
