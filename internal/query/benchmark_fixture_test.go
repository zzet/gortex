package query

import (
	"encoding/json"
	"os"
	"testing"
)

// This is an executable contract for the separately pinned performance fixture.
// Ordinary tests do not index a different checkout or fetch any fixture.
func TestBenchmarkFixtureTargets(t *testing.T) {
	if os.Getenv("GORTEX_BENCHMARK_FIXTURE_CHECK") != "1" {
		t.Skip("only the owned microbenchmark fixture preflight enables this check")
	}
	eng := buildTestEngine(t)
	for _, id := range []string{
		benchmarkServerID,
		benchmarkGraphID,
		benchmarkAddNodeID,
		benchmarkServeID,
	} {
		if eng.g.GetNode(id) == nil {
			t.Fatalf("benchmark target is absent from the indexed fixture: %s", id)
		}
	}
	observed := map[string][2]int{}
	observed["SearchSymbols"] = [2]int{len(eng.SearchSymbols("Server", 20)), 0}
	stats := eng.Stats()
	observed["Stats"] = [2]int{stats.TotalNodes, stats.TotalEdges}
	for name, result := range map[string]*SubGraph{
		"GetDependencies": eng.GetDependencies(benchmarkServerID, QueryOptions{Depth: 2, Limit: 50}),
		"GetDependents":   eng.GetDependents(benchmarkGraphID, QueryOptions{Depth: 3, Limit: 50}),
		"GetCallers":      eng.GetCallers(benchmarkAddNodeID, QueryOptions{Depth: 2, Limit: 50}),
		"GetCallChain":    eng.GetCallChain(benchmarkServeID, QueryOptions{Depth: 4, Limit: 50}),
		"FindUsages":      eng.FindUsages(benchmarkGraphID),
		"GetCluster":      eng.GetCluster(benchmarkServerID, QueryOptions{Depth: 2, Limit: 50}),
	} {
		if result == nil || len(result.Nodes) < 2 || len(result.Edges) < 1 {
			t.Fatalf("benchmark %s must exercise nodes and edges, not an empty lookup: %+v", name, result)
		}
		observed[name] = [2]int{len(result.Nodes), len(result.Edges)}
	}
	if observed["SearchSymbols"][0] < 1 || stats.TotalNodes < 2 || stats.TotalEdges < 1 {
		t.Fatal("benchmark search/statistics fixture is empty")
	}
	data, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("GORTEX_BENCHMARK_FIXTURE_V1 %s", data)
}
