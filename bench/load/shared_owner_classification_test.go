package main

import "testing"

// A new shared proof must never fall through to the old point validator when
// it is missing, contradictory, or from an unknown version. These fixtures
// deliberately retain valid old P/B numbers to expose that false credit.
// Full shared-owner/domain acceptance has its own separately reviewed path.
func TestEditProbeAccountingCannotFallBackFromSharedOwnerProofToPointNumbers(t *testing.T) {
	if got := checkEditProbeAccounting(editProbeAccountingFixture(), "receipt-a"); got.State != "observed_single_window_checks_pass" {
		t.Fatalf("the existing independent point control must stay valid: %+v", got)
	}
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{name: "null typed proof", field: "shared_owner_accounting_v1"},
		{name: "nonobject typed proof", field: "shared_owner_accounting_v1", value: "observed"},
		{name: "unresolved typed reference", field: "shared_owner_accounting_v1", value: map[string]any{
			"reference":  map[string]any{"owner": "missing-owner", "member": "receipt-a"},
			"definition": nil,
			"membership": nil,
		}},
		{name: "copied legacy numbers with empty shared membership", field: "shared_owner_accounting_v1", value: map[string]any{
			"reference":  map[string]any{},
			"definition": map[string]any{"scope": "shared_mutation_set_executor"},
			"membership": map[string]any{},
		}},
		{name: "unknown shared proof version", field: "shared_owner_accounting_v2", value: map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := editProbeAccountingFixture()
			status[tc.field] = tc.value
			got := checkEditProbeAccounting(status, "receipt-a")
			if got.State == "observed_single_window_checks_pass" {
				t.Fatalf("invalid shared proof borrowed valid independent P/B numbers: %+v", got)
			}
			if got.P != nil || got.B != nil {
				t.Fatalf("ambiguous shared/point ownership retained numeric P/B credit: %+v", got)
			}
			if got.A == nil || len(got.Reasons) == 0 {
				t.Fatalf("independent A or explicit unavailability reason was lost: %+v", got)
			}
		})
	}
}
