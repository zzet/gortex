package query

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// This is an executable contract for the separately pinned performance fixture.
// Ordinary tests do not index a different checkout or fetch any fixture.
func TestBenchmarkFixtureTargets(t *testing.T) {
	if os.Getenv("GORTEX_BENCHMARK_FIXTURE_CHECK") != "1" {
		t.Skip("only the owned microbenchmark fixture preflight enables this check")
	}
	eng := buildTestEngine(t)
	// The benchmarks measure the graph the daemon serves, so the fixture
	// must honour the checkout's own exclusions rather than index tests too.
	fileNodes, testSources := 0, 0
	for node := range eng.g.NodesByKind(graph.KindFile) {
		fileNodes++
		if strings.HasSuffix(node.FilePath, "_test.go") {
			testSources++
		}
	}
	if testSources > 0 {
		t.Errorf("benchmark fixture indexed %d test sources its .gortex.yaml excludes", testSources)
	}
	t.Logf("indexed fixture scope: %d files, %d file nodes, %d parse workers", benchmarkFiles, fileNodes, benchmarkWorkers)
	for _, id := range []string{
		benchmarkServerID,
		benchmarkDependencyID,
		benchmarkCallerID,
		benchmarkUsageID,
		benchmarkCallChainID,
	} {
		if eng.g.GetNode(id) == nil {
			t.Errorf("benchmark target is absent from the indexed fixture: %s", id)
		} else {
			t.Logf("indexed benchmark target: %s", id)
		}
	}
	observed := map[string][2]int{}
	observed["SearchSymbols"] = [2]int{len(eng.SearchSymbols("Server", 20)), 0}
	stats := eng.Stats()
	observed["Stats"] = [2]int{stats.TotalNodes, stats.TotalEdges}
	for name, result := range map[string]*SubGraph{
		"GetDependencies": eng.GetDependencies(benchmarkServerID, QueryOptions{Depth: 2, Limit: 50}),
		"GetDependents":   eng.GetDependents(benchmarkDependencyID, QueryOptions{Depth: 3, Limit: 50}),
		"GetCallers":      eng.GetCallers(benchmarkCallerID, QueryOptions{Depth: 2, Limit: 50}),
		"GetCallChain":    eng.GetCallChain(benchmarkCallChainID, QueryOptions{Depth: 4, Limit: 50}),
		"FindUsages":      eng.FindUsages(benchmarkUsageID),
		"GetCluster":      eng.GetCluster(benchmarkServerID, QueryOptions{Depth: 2, Limit: 50}),
	} {
		if result == nil || len(result.Nodes) < 2 || len(result.Edges) < 1 {
			t.Errorf("benchmark %s must exercise nodes and edges, not an empty lookup: %+v", name, result)
		}
		if result != nil {
			observed[name] = [2]int{len(result.Nodes), len(result.Edges)}
			for _, node := range result.Nodes {
				t.Logf("%s node: %s", name, node.ID)
			}
			for _, edge := range result.Edges {
				t.Logf("%s edge: %s --%s--> %s", name, edge.From, edge.Kind, edge.To)
			}
		}
	}
	if observed["SearchSymbols"][0] < 1 || stats.TotalNodes < 2 || stats.TotalEdges < 1 {
		t.Error("benchmark search/statistics fixture is empty")
	}
	data, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("GORTEX_BENCHMARK_FIXTURE_V1 %s", data)
}

// The benchmark scope comes from the checkout's own config and never falls
// back to the defaults, which would also index test sources.
func TestBenchmarkIndexConfigUsesRepositoryConfig(t *testing.T) {
	root := t.TempDir()
	if _, err := benchmarkIndexConfig(t.TempDir(), root); err == nil {
		t.Fatal("a checkout without .gortex.yaml must not fall back to the default scope")
	}
	path := filepath.Join(root, ".gortex.yaml")
	if err := os.WriteFile(path, []byte("index: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := benchmarkIndexConfig(t.TempDir(), root); err == nil {
		t.Fatal("a malformed .gortex.yaml must not fall back to the default scope")
	}
	for _, tc := range []struct {
		name          string
		workers, want int
	}{
		{"floor", 1, 2},
		{"pass-through", 5, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf("index:\n  exclude:\n    - \"**/*_test.go\"\n  workers: %d\n", tc.workers)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := benchmarkIndexConfig(t.TempDir(), root)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(cfg.Exclude, "**/*_test.go") || cfg.Workers != tc.want {
				t.Fatalf("benchmark scope ignores the repository config: workers=%d want %d exclude=%q", cfg.Workers, tc.want, cfg.Exclude)
			}
		})
	}
}
