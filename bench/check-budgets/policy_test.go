package main

import (
	"strings"
	"testing"
)

const policyFixture = `schema_version: 1
baseline: {release: v0.64.10, revision: defa9f8e7a670792b5b693744d88488f741a8160, noise_fraction: 0.15}
allowances: []
budgets:
  - {metric: edit_ack_p95, scenario: loadedit-4, host_class: reference_linux, owning_release: v0.64.12, mode: block, target: 0.20, unit: s, allowance: [], lane: [smoke, nightly, release], compare: le}
`

func TestBudgetPolicyStrictFieldsAndUnits(t *testing.T) {
	if _, err := loadPolicy([]byte(policyFixture)); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"unknown field":    strings.Replace(policyFixture, "metric:", "typo: 1, metric:", 1),
		"unknown metric":   strings.ReplaceAll(policyFixture, "edit_ack_p95", "edit_ak_p95"),
		"unknown scenario": strings.ReplaceAll(policyFixture, "loadedit-4", "loadedit-four"),
		"missing target":   strings.ReplaceAll(policyFixture, "target: 0.20, ", ""),
		"wrong unit":       strings.ReplaceAll(policyFixture, "unit: s", "unit: ms"),
		"unknown lane":     strings.ReplaceAll(policyFixture, "smoke, nightly", "smoketest, nightly"),
		"repeated lane":    strings.ReplaceAll(policyFixture, "smoke, nightly", "smoke, smoke"),
		"negative":         strings.ReplaceAll(policyFixture, "target: 0.20", "target: -1"),
		"nonfinite":        strings.ReplaceAll(policyFixture, "target: 0.20", "target: .nan"),
		"wrong baseline":   strings.ReplaceAll(policyFixture, "noise_fraction: 0.15", "noise_fraction: 0.20"),
		"two documents":    policyFixture + "---\n{}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadPolicy([]byte(data)); err == nil {
				t.Fatal("malformed policy accepted")
			}
		})
	}
}

func TestBudgetPolicyGuardAllowancesHaveExplicitApplicability(t *testing.T) {
	data := strings.ReplaceAll(policyFixture, "allowances: []", `allowances:
  - name: pending_projection
    owning_release: v0.64.12
    kind: additive
    value: 64
    unit: MiB
    metrics: [peak_memory_mib]
    scenarios: [loadedit-32]`)
	data = strings.ReplaceAll(data, "metric: edit_ack_p95", "metric: peak_memory_mib")
	data = strings.ReplaceAll(data, "scenario: loadedit-4", "scenario: loadedit-32")
	data = strings.ReplaceAll(data, "mode: block, target: 0.20, unit: s, allowance: []", "mode: guard, target: null, unit: MiB, allowance: [pending_projection]")
	if _, err := loadPolicy([]byte(data)); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"missing null target":     strings.ReplaceAll(data, "target: null, ", ""),
		"nonfinite future target": strings.ReplaceAll(data, "target: null", "target: .inf"),
		"wrong scenario":          strings.Replace(data, "scenario: loadedit-32", "scenario: loadedit-4", 1),
		"wrong metric":            strings.Replace(data, "metric: peak_memory_mib", "metric: idle_memory_2s_mib", 1),
		"repeated allowance":      strings.ReplaceAll(data, "allowance: [pending_projection]", "allowance: [pending_projection, pending_projection]"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadPolicy([]byte(bad)); err == nil {
				t.Fatal("inapplicable allowance or misleading target accepted")
			}
		})
	}
}

func TestBudgetPolicyKnownUnmeasuredTargetsStayDeclared(t *testing.T) {
	data := strings.ReplaceAll(policyFixture, "edit_ack_p95", "readback_p95")
	p, err := loadPolicy([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if metricSpecs[p.Budgets[0].Metric].missing == "" {
		t.Fatal("exact file readback must not silently use indexed-symbol visibility")
	}
}

func TestGuardKnownTargetPromotesWithoutSpendingItsAllowance(t *testing.T) {
	p := testPolicy()
	p.Allowances = []allowance{{Name: "wal", Kind: "guard_limit", Value: 2048, Unit: "MiB", OwningRelease: "v0.64.12", Metrics: []string{"wal_peak_mib"}, Scenarios: []string{"loadedit-4"}}}
	p.Budgets = []budget{{Metric: "wal_peak_mib", Scenario: "loadedit-4", HostClass: "reference_linux", OwningRelease: "v0.64.17", Mode: "guard", Target: pointer(2048), Unit: "MiB", Allowance: []string{"wal"}, Lane: []string{"release"}, Compare: "le"}}
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	p.Budgets[0].Mode = "block"
	if err := p.validate(); err != nil {
		t.Fatalf("known target should promote with one mode change: %v", err)
	}
	r := assess(p, p.Budgets[0], repeated(2100), repeated(3000), nil, "release")
	if r.Pass || r.Limit == nil || *r.Limit != 2048 || r.EffectiveHeadroom == nil || *r.EffectiveHeadroom != 0 {
		t.Fatal("promoted block target must not spend guard headroom")
	}
}
