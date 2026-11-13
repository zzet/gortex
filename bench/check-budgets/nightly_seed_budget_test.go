package main

import (
	"os"
	"strings"
	"testing"
)

// Arithmetic/raw-contract control only: these samples are not an actual
// seeded nightly campaign. Use the shipped policy and gate evaluator, not a
// test-only budget that could accidentally give the wrong target RED credit.
func TestNightlySeedSelectedCallsRetainDeferredSearchMiss(t *testing.T) {
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
	// Independent one-second raw search observations retain the original target
	// miss. The approved #1420 deferral no longer supplies blocking RED proof.
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
			if b.HostClass != c.HostClass || b.Scenario != task.Scenario || b.Metric != "search_symbols_p95" || b.Mode != "deferred" {
				continue
			}
			matched++
			if b.Target == nil || *b.Target != .4 {
				t.Fatalf("existing absolute budget changed: %+v", b)
			}
			verdict := assess(p, b, repeated(observed), repeated(.1), nil, "nightly")
			if verdict.Pass || !verdict.Deferred || !verdict.passesGate() || verdict.After == nil || *verdict.After != 1 || verdict.Limit == nil || *verdict.Limit != .4 {
				t.Fatalf("deferred target miss was lost or made blocking: %+v", verdict)
			}
		}
	}
	if matched != c.Repeats {
		t.Fatalf("fault/budget mismatch: %d selected deferred search targets, want %d", matched, c.Repeats)
	}
}
