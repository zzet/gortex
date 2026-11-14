package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func nativeNoOpEditLoopFixture(t *testing.T, same bool) (report, map[string]any, map[string]any) {
	t.Helper()
	r, raw, first := coalescedEditLoopFixture(t)
	proof := first["coalesced_functional_receipt"].(map[string]any)
	states, phases := map[string]any{}, map[string]any{}
	for _, name := range []string{"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "patch_analysis", "executor_control"} {
		states[name], phases[name] = "not_applicable", nil
	}
	for _, name := range []string{"attempt_setup", "patch_inspection", "attempt_finalize", "cohort_admission", "lane_wait"} {
		states[name], phases[name] = "measured", 1000000
	}
	proof["native_noop"] = map[string]any{"execution_disposition": "native_mtime_duplicate", "sum_state": "partial", "phase_states": states, "phases_ns": phases}
	proof["serving_physical"] = map[string]any{"wall_ns": 5000000, "sum_ns": 5000000, "residual_ns": 0, "within_five_percent": true}
	status := proof["status"].(map[string]any)
	status["background_refusal"] = "preexisting_path_work"
	proof["original_accounting_error"] = "A/P/B accounting unavailable: actual publisher deep coverage unavailable; missing applicable P phase apply; requested B observed unavailable: preexisting_path_work; B: missing duration"
	first["accounting"].(map[string]any)["reasons"] = strings.Split(strings.TrimPrefix(proof["original_accounting_error"].(string), "A/P/B accounting unavailable: "), "; ")
	if same {
		status["applied_generation"], status["physical_unavailable_observed"], status["coalesced_absent_observed"] = 3, false, true
		winner := proof["winner"].(map[string]any)
		winner["key"], winner["ticket"], winner["source"], winner["applied_generation"], winner["attempt_ticket"], winner["scope"] = "mutation-a", 3, "mcp_edit", 3, 3, "requested_physical_noop"
		first["accounting"].(map[string]any)["applied_generation"] = 3
	}
	return r, raw, first
}

func TestEditLoopQualifiedNativeNoOpIndependentlyRejectsExecutedAndUnknown(t *testing.T) {
	for _, mode := range []string{"coalesced", "same_request", "unknown_disposition", "executed_phase", "fabricated_zero", "partial_control", "unknown_scope", "bad_ACK", "bad_read", "stale", "missing_proof", "unknown_phase", "extra_executed_phase", "control_sum_gap", "same_request_background", "bad_background_control"} {
		t.Run(mode, func(t *testing.T) {
			r, raw, first := nativeNoOpEditLoopFixture(t, mode == "same_request" || mode == "same_request_background" || mode == "bad_background_control")
			proof := first["coalesced_functional_receipt"].(map[string]any)
			noop := proof["native_noop"].(map[string]any)
			switch mode {
			case "same_request_background", "bad_background_control":
				noop["background_control"] = map[string]any{"wall_ns": 8000000, "sum_ns": 8000000, "residual_ns": 0, "within_five_percent": true}
				proof["status"].(map[string]any)["background_unavailable_observed"] = false
				proof["status"].(map[string]any)["background_refusal"] = ""
				first["accounting"].(map[string]any)["state"] = "unavailable"
				if mode == "bad_background_control" {
					noop["background_control"].(map[string]any)["residual_ns"] = 1
				}
			case "unknown_phase":
				noop["phase_states"].(map[string]any)["unknown"] = "not_observed"
				noop["phases_ns"].(map[string]any)["unknown"] = nil
			case "extra_executed_phase":
				noop["phase_states"].(map[string]any)["unknown"] = "measured"
				noop["phases_ns"].(map[string]any)["unknown"] = 1000000
			case "control_sum_gap":
				noop["phases_ns"].(map[string]any)["attempt_setup"] = 999999
			case "unknown_disposition":
				noop["execution_disposition"] = "not_observed"
			case "executed_phase":
				noop["phase_states"].(map[string]any)["apply"] = "measured"
				noop["phases_ns"].(map[string]any)["apply"] = 1000000
			case "fabricated_zero":
				noop["phases_ns"].(map[string]any)["apply"] = 0
			case "partial_control":
				noop["phase_states"].(map[string]any)["attempt_setup"] = "not_observed"
			case "unknown_scope":
				delete(raw, "physical_write_scope")
			case "bad_ACK":
				first["ack_disk_status"] = "pending"
			case "bad_read":
				first["readbacks"].([]any)[0].(map[string]any)["correct"] = false
			case "stale":
				first["stale_claimed_fresh"] = true
			case "missing_proof":
				delete(proof, "native_noop")
			}
			encoded, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			r.Phases[0].EditProbe = encoded
			_, err = decodeEditLoopWire(r.Phases[0])
			want := mode == "coalesced" || mode == "same_request" || mode == "same_request_background"
			if (err == nil) != want {
				t.Fatalf("mode=%s error=%v", mode, err)
			}
		})
	}
}
