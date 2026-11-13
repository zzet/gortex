package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func functionalUnavailableEditLoopFixture(t *testing.T, preexisting bool) (report, map[string]any, map[string]any) {
	t.Helper()
	r, raw, first := coalescedEditLoopFixture(t)
	old := first["coalesced_functional_receipt"].(map[string]any)
	status := old["status"].(map[string]any)
	status["applied_generation"], status["coalesced_absent_observed"] = 3, true
	reason := "serving_attempt_binding_mismatch"
	if preexisting {
		reason = "preexisting_path_work"
		status["background_refusal"] = reason
		status["binding_predicate"], status["background_stage"], status["accepted_attempt_stage"] = "", "", ""
	}
	accountingError := "A/P/B accounting unavailable: applicable physical P unavailable; missing applicable P phase apply; P: missing duration; requested B unavailable for actual topology/identity; requested B observed unavailable: " + reason + "; missing B phase initial_timer_arm; B: missing duration"
	proof := map[string]any{"status": status, "physical_evidence_absent_observed": true, "read_assisted_absent_observed": true, "original_accounting_error": accountingError}
	delete(first, "coalesced_functional_receipt")
	first["outcome"], first["functional_unavailable_receipt"] = "qualified_functional_unavailable_timings", proof
	first["phases_ms"] = map[string]any{}
	for _, name := range []string{"debounce", "cohort_admission", "batch_admission", "lane_wait", "gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived"} {
		first["phases_ms"].(map[string]any)[name] = map[string]any{"value": nil, "unit": "ms", "reason": "producer phase state not_observed"}
	}
	accounting := first["accounting"].(map[string]any)
	accounting["applied_generation"], accounting["reasons"] = 3, strings.Split(strings.TrimPrefix(accountingError, "A/P/B accounting unavailable: "), "; ")
	return r, raw, first
}

func TestEditLoopFunctionalUnavailableRequiresOwnedStatusAndAllFunctionalEvidence(t *testing.T) {
	for _, mode := range []string{"direct43", "preexisting86", "direct53", "missing_proof", "accepted_stamps", "physical_window", "executed_original", "partial_physical", "read_assisted", "winner", "unknown_phase", "unknown_reason", "foreign_rpc", "foreign_background", "handoff", "isolated", "wrong_ACK", "incorrect_read", "missing_oracle", "pending_tier", "missing_tiers", "cap", "partial_capture", "executed_phase", "unknown_record_phase", "missing_phase", "measured_nil", "invalid_nil"} {
		t.Run(mode, func(t *testing.T) {
			r, raw, first := functionalUnavailableEditLoopFixture(t, mode == "preexisting86")
			proof := first["functional_unavailable_receipt"].(map[string]any)

			generation := map[string]int{"direct43": 88, "preexisting86": 175, "direct53": 109}[mode]
			if generation != 0 {
				status := proof["status"].(map[string]any)
				for _, key := range []string{"requested_generation", "applied_generation", "publication_ticket", "background_requested_generation", "background_last_observed_generation"} {
					status[key] = generation
				}
				first["accounting"].(map[string]any)["requested_generation"], first["accounting"].(map[string]any)["applied_generation"] = generation, generation
			}
			status := proof["status"].(map[string]any)
			switch mode {
			case "measured_nil":
				first["phases_ms"].(map[string]any)["apply"].(map[string]any)["reason"] = "producer phase state measured"
			case "invalid_nil":
				first["phases_ms"].(map[string]any)["apply"].(map[string]any)["reason"] = "invalid phases_ms value"
			case "executed_phase":
				first["phases_ms"].(map[string]any)["apply"].(map[string]any)["value"] = 0
			case "unknown_record_phase":
				first["phases_ms"].(map[string]any)["unknown"] = map[string]any{"value": 1, "unit": "ms"}
			case "missing_phase":
				delete(first["phases_ms"].(map[string]any), "apply")
			case "missing_proof":
				delete(first, "functional_unavailable_receipt")
			case "accepted_stamps":
				status["accepted_attempt_stage"] = "accepted_stamps_retained"
			case "physical_window":
				status["accepted_attempt_stage"] = "physical_window_retained"
			case "executed_original":
				first["accounting"].(map[string]any)["physical"] = map[string]any{"wall_ns": 1}
			case "partial_physical":
				proof["physical_evidence_absent_observed"] = false
			case "read_assisted":
				proof["read_assisted_absent_observed"] = false
			case "winner":
				status["coalesced_absent_observed"] = false
			case "unknown_phase":
				proof["original_accounting_error"] = proof["original_accounting_error"].(string) + "; missing applicable P phase unknown"
			case "unknown_reason":
				status["background_refusal"] = "unknown"
			case "foreign_rpc":
				status["acknowledgement_rpc_id"] = "foreign"
			case "foreign_background":
				status["background_requested_generation"] = 99
			case "handoff":
				status["observed_native_handoffs"] = 1
			case "isolated":
				delete(raw, "physical_write_scope")
			case "wrong_ACK":
				first["ack_disk_status"] = "pending"
			case "incorrect_read":
				first["readbacks"].([]any)[0].(map[string]any)["correct"] = false
			case "missing_oracle":
				delete(first["first_correct_ms"].(map[string]any), "call_chain")
			case "pending_tier":
				first["mutation_tiers"].(map[string]any)["tier_state"].(map[string]any)["derived"] = "pending"
			case "missing_tiers":
				delete(first, "mutation_tiers")
			case "cap":
				first["evidence_bytes"] = 16777217
			case "partial_capture":
				first["outcome"] = "harness_evidence_incomplete"
			}
			encoded, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			r.Phases[0].EditProbe = encoded
			_, err = decodeEditLoopWire(r.Phases[0])
			want := mode == "direct43" || mode == "preexisting86" || mode == "direct53"
			if (err == nil) != want {
				t.Fatalf("mode=%s err=%v", mode, err)
			}
		})
	}
}
