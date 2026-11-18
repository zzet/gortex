package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

func deferredSearchBudget(host string) budget {
	lane := []string{"nightly", "release"}
	if host == hostedSmokeClass {
		lane = []string{"smoke"}
	}
	return budget{Metric: "search_symbols_p95", Scenario: "loadedit-4", HostClass: host, OwningRelease: "unassigned", Mode: "deferred", Target: pointer(.4), Unit: "s", Compare: "le", Lane: lane}
}

func TestDeferredSearchPolicyIsNarrow(t *testing.T) {
	for _, host := range []string{"reference_linux", "reference_mac", "large_mac", hostedSmokeClass} {
		if err := validateBudget(deferredSearchBudget(host), nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"metric", "scenario", "target", "owner", "allowance", "comparison", "hosted lane", "reference lane"} {
		t.Run(name, func(t *testing.T) {
			b := deferredSearchBudget(hostedSmokeClass)
			switch name {
			case "metric":
				b.Metric = "read_source_p99"
			case "scenario":
				b.Scenario = "loadedit-8"
			case "target":
				b.Target = pointer(.5)
			case "owner":
				b.OwningRelease = "v0.64.12"
			case "allowance":
				b.Allowance = []string{"waiver"}
			case "comparison":
				b.Compare = "ge"
			case "hosted lane":
				b.Lane = []string{"release"}
			case "reference lane":
				b.HostClass = "reference_linux"
				b.Lane = []string{"release"}
			}
			if err := validateBudget(b, nil); err == nil {
				t.Fatal("deferral escaped the approved target")
			}
		})
	}
}

func TestDeferredSearchRetainsMissWithoutPassingTarget(t *testing.T) {
	b := deferredSearchBudget("reference_linux")
	r := assess(testPolicy(), b, repeated(.6), repeated(.5), nil, "release")
	var table bytes.Buffer
	if r.Pass || !r.Deferred || !r.passesGate() || r.After == nil || *r.After != .6 || r.Limit == nil || *r.Limit != .4 || !printRows(&table, []result{r}) || !strings.Contains(table.String(), "DEFERRED (target missed)") {
		t.Fatalf("target miss was hidden or blocked: %+v; %s", r, table.String())
	}
	for _, samples := range [][]float64{nil, {.5, .5}, {.5, math.NaN(), .5}} {
		bad := assess(testPolicy(), b, samples, repeated(.5), nil, "release")
		if bad.passesGate() || bad.Deferred {
			t.Fatal("invalid candidate evidence became nonblocking")
		}
	}
	bad := assess(testPolicy(), b, repeated(.6), nil, nil, "release")
	if bad.passesGate() || bad.Deferred {
		t.Fatal("missing main evidence became nonblocking")
	}
	regression := assess(testPolicy(), b, repeated(.7), repeated(.5), nil, "smoke")
	if regression.passesGate() || regression.Deferred {
		t.Fatal("independent smoke +20% regression rule was waived")
	}
	for _, other := range []budget{
		{Metric: "read_source_p99", Mode: "block", Target: pointer(.2), Unit: "s", Compare: "le"},
		{Metric: "readback_correct", Mode: "block", Target: pointer(.999), Unit: "fraction", Compare: "ge"},
	} {
		value := .201
		if other.Metric == "readback_correct" {
			value = .998
		}
		row := assess(testPolicy(), other, repeated(value), repeated(value), nil, "release")
		if printRows(&table, []result{r, row}) {
			t.Fatalf("search deferral waived another failing row: %+v", row)
		}
	}
}

func TestDeferredSearchStillRefusesMissingOrIncomparableReports(t *testing.T) {
	for _, mutation := range []string{"none", "missing repeat", "missing timing", "different plan"} {
		t.Run(mutation, func(t *testing.T) {
			_, d, p := syntheticSmokeJob(t)
			p.Budgets = []budget{deferredSearchBudget(hostedSmokeClass)}
			for i := range d.runs {
				d.runs[i].input.Scenario = "loadedit-4"
				tools := d.runs[i].data.Phases[0].Tools
				tool := tools["search.symbols"]
				tool.Latency = distribution{Count: 3, P95: metric{Value: pointer(500), Unit: "ms"}}
				tools["search.symbols"] = tool
			}
			switch mutation {
			case "missing repeat":
				d.runs = d.runs[:len(d.runs)-1]
			case "missing timing":
				tool := d.runs[1].data.Phases[0].Tools["search.symbols"]
				tool.Latency.P95.Value = nil
				d.runs[1].data.Phases[0].Tools["search.symbols"] = tool
			case "different plan":
				d.runs[1].data.Plan = strings.Repeat("f", 64)
			}
			rows, err := check(p, d, "smoke")
			if err != nil || len(rows) != 1 {
				t.Fatalf("check: %+v %v", rows, err)
			}
			if rows[0].passesGate() != (mutation == "none") {
				t.Fatalf("wrong evidence verdict for %s: %+v", mutation, rows[0])
			}
		})
	}
}

func TestDeferredSearchHistoryRetainsNonblockingMisses(t *testing.T) {
	r := assess(testPolicy(), deferredSearchBudget("reference_linux"), repeated(.6), repeated(.5), nil, "nightly")
	items := make([]nightlyEvidence, 0, 2)
	for _, day := range []string{"2026-11-11", "2026-11-12"} {
		items = append(items, nightlyEvidence{day: day, receipt: laneReceipt{Plan: lanePlan{HostClass: "reference_linux"}, Workflow: workflowProvenance{Event: "schedule"}}, budgets: budgetEvidence{Rows: []result{r}}})
	}
	var out bytes.Buffer
	through, _ := time.Parse(time.DateOnly, "2026-11-12")
	historyVerdict(items, through, []string{"reference_linux"}, strings.Repeat("a", 40), &out)
	if !strings.Contains(out.String(), "DEFERRED TARGET: reference_linux / loadedit-4 / search_symbols_p95") || strings.Contains(out.String(), "BLOCKER: reference_linux / loadedit-4 / search_symbols_p95") {
		t.Fatal(out.String())
	}
	items[1].budgets.Rows[0].Deferred = false
	out.Reset()
	historyVerdict(items, through, []string{"reference_linux"}, strings.Repeat("a", 40), &out)
	if strings.Contains(out.String(), "BLOCKER:") {
		t.Fatal("one evidence failure was counted as two blocking failures")
	}
	items[0].budgets.Rows[0].Deferred = false
	out.Reset()
	historyVerdict(items, through, []string{"reference_linux"}, strings.Repeat("a", 40), &out)
	if !strings.Contains(out.String(), "BLOCKER:") {
		t.Fatal("two blocking evidence failures were hidden by the target deferral")
	}
}
