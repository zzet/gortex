package main

import (
	"math"
	"strings"
	"testing"
)

// Numbers in these tests are synthetic arithmetic fixtures, not release
// baselines or claimed benchmark evidence.
func repeated(v float64) []float64 { return []float64{v, v, v} }
func pointer(v float64) *float64   { return &v }

func testPolicy() policy {
	return policy{Version: 1, Baseline: baseline{Release: "v0.64.10", Revision: shippedRevision, NoiseFraction: .15}}
}

func TestBlockAndMedianOfThree(t *testing.T) {
	b := budget{Metric: "edit_ack_p95", Mode: "block", Target: pointer(.2), Unit: "s", Compare: "le"}
	if r := assess(testPolicy(), b, repeated(.21), repeated(.21), nil, "release"); r.Pass {
		t.Fatal("absolute block breach passed")
	}
	if r := assess(testPolicy(), b, []float64{.19, 100, .18}, repeated(.19), nil, "release"); !r.Pass || r.After == nil || *r.After != .19 {
		t.Fatalf("must use median of per-repeat measurements: %+v", r)
	}
	if r := assess(testPolicy(), b, []float64{.1, .1}, repeated(.1), nil, "release"); r.Pass || r.After != nil {
		t.Fatalf("two repeats cannot pass or become zero: %+v", r)
	}
	if r := assess(testPolicy(), b, []float64{.1, math.NaN(), .1}, repeated(.1), nil, "release"); r.Pass {
		t.Fatal("invalid repeat passed")
	}
}

func TestGuardNoiseAndNamedAdditiveAllowance(t *testing.T) {
	p := testPolicy()
	b := budget{Metric: "peak_memory_mib", Scenario: "loadedit-32", Mode: "guard", Unit: "MiB", Compare: "le"}
	if r := assess(p, b, repeated(110), repeated(100), repeated(100), "nightly"); !r.Pass {
		t.Fatalf("10%% regression should fit15%% noise: %+v", r)
	}
	if r := assess(p, b, repeated(120), repeated(100), repeated(100), "nightly"); r.Pass {
		t.Fatal("20%% regression passed without allowance")
	}
	p.Allowances = []allowance{{Name: "pending_projection", Kind: "additive", Value: 64, Unit: "MiB", OwningRelease: "v0.64.12"}}
	b.Allowance = []string{"pending_projection"}
	r := assess(p, b, repeated(120), repeated(100), repeated(100), "nightly")
	if !r.Pass || r.Limit == nil || math.Abs(*r.Limit-179) > 1e-9 || r.EffectiveHeadroom == nil || *r.EffectiveHeadroom != 64 {
		t.Fatalf("named64MiB should be added once: %+v", r)
	}
}

func TestWALAllowanceRaisesGuardLimitNotBaselineOrHardCap(t *testing.T) {
	p := testPolicy()
	p.Allowances = []allowance{{Name: "wal_reclaim_yield", Kind: "guard_limit", Value: 2048, Unit: "MiB", OwningRelease: "v0.64.12"}}
	b := budget{Metric: "wal_peak_mib", Mode: "guard", Unit: "MiB", Compare: "le", Allowance: []string{"wal_reclaim_yield"}}
	for _, tc := range []struct {
		name            string
		baseline, after float64
		limit, added    float64
		pass            bool
	}{
		{"ceiling raises headroom", 1000, 2048, 2048, 898, true},
		{"not extra2048MiB", 1000, 2100, 2048, 898, false},
		{"larger baseline retained", 3000, 3400, 3450, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := assess(p, b, repeated(tc.after), repeated(tc.baseline), repeated(tc.baseline), "release")
			if r.Pass != tc.pass || r.Limit == nil || math.Abs(*r.Limit-tc.limit) > 1e-9 || r.EffectiveHeadroom == nil || math.Abs(*r.EffectiveHeadroom-tc.added) > 1e-9 {
				t.Fatalf("wrong guard-limit arithmetic: %+v", r)
			}
		})
	}
}

func TestMainFallbackOnlyUsesDiskOrMemoryAndNeverMissingTag(t *testing.T) {
	p := testPolicy()
	b := budget{Metric: "wal_peak_mib", Mode: "guard", Unit: "MiB", Compare: "le"}
	r := assess(p, b, repeated(220), repeated(200), repeated(100), "nightly")
	if !r.Pass || !strings.HasPrefix(r.Baseline, "main (") || r.Before == nil || *r.Before != 200 {
		t.Fatalf("preexisting main disk regression should select pinned main: %+v", r)
	}
	if r := assess(p, b, repeated(220), repeated(200), nil, "nightly"); r.Pass {
		t.Fatal("missing shipped data cannot trigger main fallback")
	}
	b.Metric, b.Unit = "outline_p50", "s"
	if r := assess(p, b, repeated(220), repeated(200), repeated(100), "nightly"); r.Pass || r.Baseline != "v0.64.10" {
		t.Fatalf("timing guard must not use disk/memory fallback: %+v", r)
	}
}

func TestHardLimitsCannotHideOneBadRepeatAndSmokeP95UsesMain(t *testing.T) {
	p := testPolicy()
	b := budget{Metric: "edit_ack_max", Mode: "block", Target: pointer(1), Compare: "le"}
	r := assess(p, b, []float64{.1, .1, 1.1}, repeated(.1), nil, "release")
	if r.Pass || r.Worst == nil || *r.Worst != 1.1 {
		t.Fatalf("one hard-limit breach hidden by median: %+v", r)
	}
	b.Metric, b.Target = "edit_ack_p95", pointer(.5)
	if r := assess(p, b, repeated(.121), repeated(.1), nil, "smoke"); r.Pass {
		t.Fatal("smoke p95 >20%% regression passed despite absolute headroom")
	}
}

func TestMissingOrDifferentUnitsAndWeakerReadbackCannotPass(t *testing.T) {
	if _, err := number(metric{Unit: "s", Reason: "not observed"}, "s"); err == nil {
		t.Fatal("unavailable observation became zero")
	}
	if _, err := number(metric{Unit: "ms", Value: pointer(0)}, "s"); err == nil {
		t.Fatal("incompatible units accepted")
	}
	if _, err := extract("readback_p95", "reference_linux", report{}); err == nil {
		t.Fatal("indexed visibility must not stand in for exact file readback")
	}
}

func TestStopTwentyCannotUseThreeReports(t *testing.T) {
	d := dataset{config: inputs{Repeats: 3}}
	if _, _, err := d.samples("candidate", budget{Scenario: "idle-stop-20", MinRepeats: 20}); err == nil {
		t.Fatal("three repetitions cannot prove 20 clean stops")
	}
}
