package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampleCorpus() Corpus {
	return Corpus{Label: "sample", Source: "/input/sample", Commit: strings.Repeat("a", 40), File: "sample.go", Symbol: "sample.go::Example", Query: "Example", ProposedSignature: "func Example() int"}
}

func TestAgentPacingAndBusyBackoff(t *testing.T) {
	p := newPacing(agentProfile, 42)
	seen := make(map[int]bool)
	for range 1000 {
		if wait := p.think(); wait < 2*time.Second || wait > 5*time.Second {
			t.Fatalf("think = %s", wait)
		}
		burst := p.burst()
		if burst < 3 || burst > 5 {
			t.Fatalf("burst = %d", burst)
		}
		seen[burst] = true
	}
	if len(seen) != 3 {
		t.Fatalf("burst distribution omitted a size: %v", seen)
	}
	for i, want := range []time.Duration{50, 100, 200, 400, 800, 1000, 1000, 1000} {
		if got := busyBackoff(i); got != want*time.Millisecond {
			t.Errorf("backoff(%d) = %s", i, got)
		}
	}
	if p := newPacing(saturationProfile, 42); p.think() != 0 || busyBackoff(0) == 0 {
		t.Fatal("saturation must omit think time but retain busy backoff")
	}
	if _, err := parseProfile("unknown"); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestSourceTargetsAndStablePlan(t *testing.T) {
	c := sampleCorpus()
	plan, err := benchmarkPlan(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range plan {
		if r.name() == "relations.dependents" || r.name() == "read.source" {
			target, ok := r.Arguments["target"].(map[string]any)
			if !ok || len(target) != 1 {
				t.Fatalf("%s has no valid target: %v", r.name(), r.Arguments)
			}
			if symbol, ok := target["symbol"].(string); ok && strings.HasPrefix(symbol, "ext::") {
				t.Fatal("external read target")
			}
		}
		if r.name() == "change.verify" {
			source := r.Arguments["source"].(map[string]any)
			changes := source["changes"].([]map[string]any)
			if len(changes) != 1 || changes[0]["symbol_id"] != c.Symbol || changes[0]["new_signature"] != c.ProposedSignature {
				t.Fatal("verify omitted proposed source-backed signature change")
			}
		}
	}
	if len(plan) != 21 {
		t.Fatalf("benchmark operation census=%d, want21", len(plan))
	}
	seen := make(map[string]bool)
	for _, r := range plan {
		if seen[r.name()] {
			t.Fatalf("duplicate operation %s", r.name())
		}
		seen[r.name()] = true
	}
	hash, err := hashJSON(plan)
	if err != nil {
		t.Fatal(err)
	}
	c.Source = "/other/private/clone"
	other, err := benchmarkPlan(c)
	if err != nil {
		t.Fatal(err)
	}
	if next, err := hashJSON(other); err != nil || hash != next {
		t.Fatalf("plan hash depends on source location: %s %s %v", hash, next, err)
	}
	for _, symbol := range []string{"ext::dependency::Foo", ""} {
		c.Symbol = symbol
		if _, err := readingPlan(c); err == nil {
			t.Fatalf("unsafe symbol %q accepted", symbol)
		}
	}
	for _, path := range []string{"../secret.go", "/secret.go", `a\secret.go`, "", ".", "./secret.go", "a/../secret.go", "a//secret.go", "C:secret.go", "C:/secret.go", `C:\secret.go`, "//server/share/secret.go", `\\server\share\secret.go`, `\secret.go`, `\\?\C:\secret.go`, "source.go:stream", "source\x00.go"} {
		c = sampleCorpus()
		c.File = path
		if _, err := readingPlan(c); err == nil {
			t.Fatalf("unsafe file %q accepted", path)
		}
	}
	for _, file := range []string{"source.go", "internal/store/source.go", "src/δ.go"} {
		c = sampleCorpus()
		c.File = file
		if _, err := readingPlan(c); err != nil {
			t.Fatalf("canonical repo-relative path %q rejected: %v", file, err)
		}
	}
}

func TestSustainedReadPlanIsNonterminalNavigation(t *testing.T) {
	plan, err := readingPlan(sampleCorpus())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"search.symbols": true, "search.text": true, "read.source": true, "read.file": true, "read.editing_context": true, "read.summary": true, "relations.callers": true, "relations.usages": true, "explore.context": true, "change.impact": true}
	if len(plan) != len(want) {
		t.Fatalf("sustained operation census=%d", len(plan))
	}
	for _, r := range plan {
		if !want[r.name()] {
			t.Fatalf("terminal/audit operation in sustained load: %s", r.name())
		}
		delete(want, r.name())
	}
	if len(want) != 0 {
		t.Fatalf("missing sustained operations: %v", want)
	}
}

func TestSummaryRetainsErrorAndCensoringDenominators(t *testing.T) {
	attempts := []Attempt{
		{Tool: "search.symbols", Role: "workload", Outcome: "busy", Code: -32002},
		{Tool: "search.symbols", Role: "workload", Outcome: "success", LogicalMS: 60},
		{Tool: "search.symbols", Role: "probe", Outcome: "busy", Code: -32002},
		{Tool: "read.source", Role: "workload", Outcome: "error", Code: -32602, Error: "invalid_argument"},
		{Tool: "read.source", Role: "workload", Outcome: "phase_censored"},
		{Tool: "read.source", Role: "workload", Outcome: "product_deadline"},
	}
	metrics := summarize(attempts, 10, "workload")
	search := metrics["search.symbols"]
	if search.BusyRefusals != 1 || search.Successes != 1 || *search.Latency.P50.Value != 60 || *search.SuccessPerSec.Value != .1 {
		t.Fatalf("search summary: %+v", search)
	}
	read := metrics["read.source"]
	if read.Latency.P50.Value != nil || read.PhaseCensored != 1 || read.ProductExpired != 1 || read.Errors["-32602:invalid_argument"] != 1 {
		t.Fatalf("incomplete/error samples lost: %+v", read)
	}
	probe := summarize(attempts, 10, "probe")["search.symbols"]
	if probe.BusyRefusals != 1 || probe.SuccessPerSec.Value != nil {
		t.Fatalf("probe denominator mixed with continuous workload: %+v", probe)
	}
	encoded, err := json.Marshal(read.Latency)
	if err != nil || !strings.Contains(string(encoded), `"value":null`) {
		t.Fatalf("unavailable latency must be null: %s %v", encoded, err)
	}
}
