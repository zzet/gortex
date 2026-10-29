package main

import (
	"encoding/json"
	"testing"
)

func editProbeRepairAccountingFixture() map[string]any {
	body := editProbeAccountingFixture()
	body["new_sha"] = "ce013625030ba8dba906f756967f9e9ca394464a"
	winner := body["coalesced_publication"].(map[string]any)
	pub := winner["publication_phases"].(map[string]any)
	pub["clock"], pub["terminal"] = "monotonic", true
	physical := winner["timings"].(map[string]any)["phase_timing"].(map[string]any)
	physical["clock"], physical["patch_wall_ms"] = "monotonic", json.Number("5")
	a := physical["patch_accounting"].(map[string]any)
	a["scope"], a["sum_state"], a["execution_disposition"] = "accepted_winning_physical_attempt", "partial", "native_mtime_duplicate"
	phases, states := a["phases_ms"].(map[string]any), a["phase_states"].(map[string]any)
	b := body["requested_background"].(map[string]any)
	b["background_wall_ms"] = json.Number("12")
	bPhases := b["phases_ms"].(map[string]any)
	for _, name := range []string{"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "patch_analysis", "executor_control"} {
		phases[name], states[name] = nil, "not_applicable"
		delete(bPhases, name)
	}
	rPhases, rStates := map[string]any{}, map[string]any{}
	for _, name := range append(editProbeRepairDeepPhases[:], "repair_control") {
		rPhases[name], rStates[name] = json.Number("1"), "measured"
	}
	body["read_assisted_publication"] = map[string]any{
		"scope": "read_assisted_reconcile", "clock": "monotonic", "owner_generation": json.Number("3"),
		"requested_receipt": "receipt-a", "requested_generation": json.Number("1"), "accepted_blob_sha": body["new_sha"],
		"window_ns": json.Number("8000000"), "additive_with_requested_background": false, "background_relationship": "unavailable",
		"reader_causes": []any{
			map[string]any{"session": "reader", "request_id_key": "n:7", "admission_generation": json.Number("2")},
			map[string]any{"session": "reader", "request_id_key": "n:7", "admission_generation": json.Number("3")},
		},
		"phase_accounting": map[string]any{
			"scope": "read_assisted_reconcile", "clock": "monotonic", "owner_generation": json.Number("3"),
			"window_ns": json.Number("8000000"), "observed_subtotal_ns": json.Number("8000000"), "unattributed_ns": json.Number("0"),
			"sum_state": "partial", "observed_partition_complete": true, "additive_with_requested_background": false,
			"phases_ms": rPhases, "phase_states": rStates,
		},
	}
	return body
}

func TestEditProbeValidatesIndependentPublisherWithoutSummingOverlappingWindows(t *testing.T) {
	body := editProbeRepairAccountingFixture()
	got := checkEditProbeAccounting(body, "receipt-a")
	if got.State != "observed_single_window_checks_pass" || got.R == nil || got.R.WallNS != 8000000 || got.B == nil || got.B.WallNS != 12000000 || got.P == nil || got.P.WallNS != 5000000 {
		t.Fatalf("independent R3 confused with requested1/watcher2 or summed into B/P: %+v", got)
	}
	// Reused wire IDs are still only causes; their admission generations differ.
	physical := body["coalesced_publication"].(map[string]any)["timings"].(map[string]any)["phase_timing"].(map[string]any)
	if physical["patch_accounting"].(map[string]any)["phases_ms"].(map[string]any)["apply"] != nil {
		t.Fatal("R retroactively filled the no-op P")
	}
}

func TestEditProbeMissingOrContradictoryPublisherRetainsTerminalPartialRefusal(t *testing.T) {
	for _, bad := range []string{"missing", "bytes", "receipt", "requested", "owner", "bounds", "missing_phase", "phase_state", "extra_phase", "sum", "additive", "duplicate_admission"} {
		t.Run(bad, func(t *testing.T) {
			body := editProbeRepairAccountingFixture()
			r := body["read_assisted_publication"].(map[string]any)
			a := r["phase_accounting"].(map[string]any)
			switch bad {
			case "missing":
				delete(body, "read_assisted_publication")
			case "bytes":
				r["accepted_blob_sha"] = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"
			case "receipt":
				r["requested_receipt"] = "foreign"
			case "requested":
				r["requested_generation"] = json.Number("2")
			case "owner":
				a["owner_generation"] = json.Number("4")
			case "bounds":
				a["window_ns"] = json.Number("9000000")
			case "missing_phase":
				a["phases_ms"].(map[string]any)["apply"] = nil
			case "phase_state":
				a["phase_states"].(map[string]any)["apply"] = "not_observed"
			case "extra_phase":
				a["phases_ms"].(map[string]any)["lane_wait"] = json.Number("1")
			case "sum":
				a["observed_subtotal_ns"] = json.Number("7")
			case "additive":
				r["additive_with_requested_background"] = true
			case "duplicate_admission":
				r["reader_causes"].([]any)[1].(map[string]any)["admission_generation"] = json.Number("2")
			}
			got := checkEditProbeAccounting(body, "receipt-a")
			if got.State != "terminal_partial_physical" || got.R != nil || len(got.Reasons) == 0 {
				t.Fatalf("unproven R was accepted: %+v", got)
			}
		})
	}
}
