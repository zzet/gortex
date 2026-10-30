package query

import (
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

var (
	benchmarkEngineOnce sync.Once
	benchmarkEngine     *Engine
	benchmarkEngineErr  error
)

const (
	benchmarkServerID  = "internal/mcp/server.go::NewServer"
	benchmarkGraphID   = "internal/graph/graph.go::Graph"
	benchmarkAddNodeID = "internal/graph/graph.go::Graph.AddNode"
	benchmarkServeID   = "cmd/gortex/serve.go::runServe"
)

// buildTestEngine indexes the Gortex repo once and shares the read-only engine
// across all benchmarks in this package.
func buildTestEngine(b testing.TB) *Engine {
	b.Helper()
	benchmarkEngineOnce.Do(func() {
		g := graph.New()
		reg := parser.NewRegistry()
		languages.RegisterAll(reg)
		idx := indexer.New(g, reg, config.IndexConfig{}, zap.NewNop())
		_, benchmarkEngineErr = idx.Index("../..")
		if benchmarkEngineErr == nil {
			benchmarkEngine = NewEngine(g)
		}
	})
	if benchmarkEngineErr != nil {
		b.Fatal(benchmarkEngineErr)
	}
	return benchmarkEngine
}

func BenchmarkSearchSymbols(b *testing.B) {
	eng := buildTestEngine(b)
	b.ResetTimer()
	for b.Loop() {
		eng.SearchSymbols("Server", 20)
	}
}

func BenchmarkGetDependencies(b *testing.B) {
	eng := buildTestEngine(b)
	b.ResetTimer()
	for b.Loop() {
		eng.GetDependencies(benchmarkServerID, QueryOptions{Depth: 2, Limit: 50})
	}
}

func BenchmarkGetDependents(b *testing.B) {
	eng := buildTestEngine(b)
	b.ResetTimer()
	for b.Loop() {
		eng.GetDependents(benchmarkGraphID, QueryOptions{Depth: 3, Limit: 50})
	}
}

func BenchmarkGetCallers(b *testing.B) {
	eng := buildTestEngine(b)
	b.ResetTimer()
	for b.Loop() {
		eng.GetCallers(benchmarkAddNodeID, QueryOptions{Depth: 2, Limit: 50})
	}
}

func BenchmarkGetCallChain(b *testing.B) {
	eng := buildTestEngine(b)
	b.ResetTimer()
	for b.Loop() {
		eng.GetCallChain(benchmarkServeID, QueryOptions{Depth: 4, Limit: 50})
	}
}

func BenchmarkFindUsages(b *testing.B) {
	eng := buildTestEngine(b)
	b.ResetTimer()
	for b.Loop() {
		eng.FindUsages(benchmarkGraphID)
	}
}

func BenchmarkGetCluster(b *testing.B) {
	eng := buildTestEngine(b)
	b.ResetTimer()
	for b.Loop() {
		eng.GetCluster(benchmarkServerID, QueryOptions{Depth: 2, Limit: 50})
	}
}

func BenchmarkStats(b *testing.B) {
	eng := buildTestEngine(b)
	b.ResetTimer()
	for b.Loop() {
		eng.Stats()
	}
}
