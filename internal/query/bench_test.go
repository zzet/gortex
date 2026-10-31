package query

import (
	"fmt"
	"path/filepath"
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
	// benchmarkFiles and benchmarkWorkers record the indexed scope so the
	// fixture check can report it next to its query results.
	benchmarkFiles, benchmarkWorkers int
)

const (
	benchmarkUsageID      = "internal/graph/graph.go::hashEdgeKey"
	benchmarkServerID     = "internal/mcp/server.go::NewServer"
	benchmarkDependencyID = "internal/graph/graph.go::keyOf"
	benchmarkCallerID     = "internal/graph/graph.go::fnv1aEdge"
	benchmarkCallChainID  = "cmd/gortex/mcp.go::runMCP"
)

// buildTestEngine indexes the Gortex repo once, at the scope the daemon
// indexes it, and shares the read-only engine across all benchmarks in this
// package.
func buildTestEngine(b testing.TB) *Engine {
	b.Helper()
	benchmarkEngineOnce.Do(func() {
		root, err := filepath.Abs("../..")
		if err != nil {
			benchmarkEngineErr = err
			return
		}
		var cfg config.IndexConfig
		cfg, benchmarkEngineErr = benchmarkIndexConfig(b.TempDir(), root)
		if benchmarkEngineErr != nil {
			return
		}
		g := graph.New()
		reg := parser.NewRegistry()
		languages.RegisterAll(reg)
		idx := indexer.New(g, reg, cfg, zap.NewNop())
		var result *indexer.IndexResult
		result, benchmarkEngineErr = idx.Index(root)
		if benchmarkEngineErr == nil {
			benchmarkEngine = NewEngine(g)
			benchmarkFiles, benchmarkWorkers = result.FileCount, cfg.Workers
		}
	})
	if benchmarkEngineErr != nil {
		b.Fatal(benchmarkEngineErr)
	}
	return benchmarkEngine
}

// benchmarkIndexConfig resolves root's own .gortex.yaml the way the daemon
// resolves a tracked repository: built-in excludes, the .gitignore chain and
// the file's excludes and worker count layered over the defaults. A missing
// or malformed file is an error, not a silent fall back to defaults that would
// also index the test sources the daemon skips. The global config is an absent
// file under dir, so the indexed scope depends only on the checkout, and the
// parse pool keeps at least two workers.
func benchmarkIndexConfig(dir, root string) (config.IndexConfig, error) {
	if _, err := config.ParseWorkspaceFile(filepath.Join(root, ".gortex.yaml")); err != nil {
		return config.IndexConfig{}, fmt.Errorf("benchmark repository config: %w", err)
	}
	manager, err := config.NewConfigManager(filepath.Join(dir, "global.yaml"))
	if err != nil {
		return config.IndexConfig{}, err
	}
	manager.LoadWorkspaceConfig("", root)
	cfg := manager.GetRepoConfig("").Index
	cfg.Workers = max(cfg.Workers, 2)
	return cfg, nil
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
		eng.GetDependents(benchmarkDependencyID, QueryOptions{Depth: 3, Limit: 50})
	}
}

func BenchmarkGetCallers(b *testing.B) {
	eng := buildTestEngine(b)
	b.ResetTimer()
	for b.Loop() {
		eng.GetCallers(benchmarkCallerID, QueryOptions{Depth: 2, Limit: 50})
	}
}

func BenchmarkGetCallChain(b *testing.B) {
	eng := buildTestEngine(b)
	b.ResetTimer()
	for b.Loop() {
		eng.GetCallChain(benchmarkCallChainID, QueryOptions{Depth: 4, Limit: 50})
	}
}

func BenchmarkFindUsages(b *testing.B) {
	eng := buildTestEngine(b)
	b.ResetTimer()
	for b.Loop() {
		eng.FindUsages(benchmarkUsageID)
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
