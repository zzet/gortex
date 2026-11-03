package main

import (
	"os"
	"strings"
	"testing"
)

// Arithmetic/raw-contract control only: these samples are not an actual
// seeded nightly campaign. Use the shipped policy and gate evaluator, not a
// test-only budget that could accidentally give the wrong target RED credit.
func TestNightlySeedSelectedCallsReachExistingAbsoluteGate(t *testing.T) {
	data, err := os.ReadFile("../budgets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPolicy(data)
	if err != nil {
		t.Fatal(err)
	}
	c := fixtureLaneConfig()
	c.Lane, c.HostClass = "nightly", "reference_linux"
	c.NightlySeed = &nightlyWorkloadSeed{Version: 1, Kind: "search-delay-v1", CandidateRevision: c.Builds["candidate"].Revision}
	plan, err := makeLanePlan(p, c)
	if err != nil {
		t.Fatal(err)
	}
	// Independent one-second raw search observations must fail the unchanged
	// actual absolute budget in the scenario that receives the real fault.
	raw := report{Phases: []phase{{Tools: map[string]toolResult{
		"search.symbols": {Latency: distribution{Count: 3, P95: metric{Value: pointer(1000), Unit: "ms"}}},
	}}}}
	observed, err := extract("search_symbols_p95", c.HostClass, raw)
	if err != nil {
		t.Fatal(err)
	}
	matched := 0
	for _, task := range plan.Tasks {
		args := strings.Join(taskArgs(c, task, "manifest", "state", "report"), " ")
		if !strings.Contains(args, "--nightly-workload-seed ") {
			continue
		}
		for _, b := range p.Budgets {
			if b.HostClass != c.HostClass || b.Scenario != task.Scenario || b.Metric != "search_symbols_p95" || b.Mode != "block" {
				continue
			}
			matched++
			if b.Target == nil || *b.Target != .4 {
				t.Fatalf("existing absolute budget changed: %+v", b)
			}
			verdict := assess(p, b, repeated(observed), repeated(.1), nil, "nightly")
			if verdict.Pass || verdict.After == nil || *verdict.After != 1 || verdict.Limit == nil || *verdict.Limit != .4 {
				t.Fatalf("real gate did not propagate delayed raw search to RED: %+v", verdict)
			}
		}
	}
	if matched != c.Repeats {
		t.Fatalf("fault/budget mismatch: %d selected absolute-search gates, want %d; existing selector does not establish a seeded budget RED", matched, c.Repeats)
	}
}
