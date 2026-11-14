package main

import (
	"encoding/json"
	"testing"
)

// The raw producer stays failed. A B-only consumer qualification is a separate
// verdict and cannot repair incomplete functional, A, P or captured evidence.
func mixedBackgroundFixture(t *testing.T) (editLoopWire, json.RawMessage) {
	t.Helper()
	r, raw, first := coalescedEditLoopFixture(t)
	delete(first, "coalesced_functional_receipt")
	first["outcome"] = "diagnostic_failure"
	a := first["accounting"].(map[string]any)
	a["requested_generation"], a["applied_generation"] = 3, 3
	a["physical"] = map[string]any{"wall_ns": 15000000, "sum_ns": 15000000, "residual_ns": 0, "within_five_percent": true}
	reasons := []string{"requested B unavailable for actual topology/identity", "requested B observed unavailable: preexisting_path_work", "B: missing duration"}
	a["reasons"] = reasons
	first["errors"] = []string{"A/P/B accounting unavailable: requested B unavailable for actual topology/identity; requested B observed unavailable: preexisting_path_work; B: missing duration"}
	first["phases_ms"] = map[string]any{}
	for _, name := range mixedPhysicalPhases {
		first["phases_ms"].(map[string]any)[name] = map[string]any{"value": 1, "unit": "ms"}
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	r.Phases[0].EditProbe = encoded
	var wire editLoopWire
	if err = json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	phases := map[string]any{}
	states := map[string]any{}
	for _, name := range mixedPhysicalPhases {
		phases[name] = 1
		states[name] = "measured"
	}
	status := map[string]any{"receipt": "commit-a", "mutation_id": "edit-a", "reindex_generation": 3, "applied_generation": 3, "graph_status": "fresh", "ack_at": "2026-01-01T00:00:00Z", "file_graph_at": "2026-01-01T00:00:00Z", "relations_at": "2026-01-01T00:00:00Z", "derived_at": "2026-01-01T00:00:00Z", "graph_status_terminal": true, "tier_state": map[string]any{"file_graph": "fresh", "relations": "fresh", "derived": "fresh"},
		"tool": "edit_file", "disk_status": "committed", "commit_seq": 1, "content_sha256": first["ack_content_sha256"],
		"acknowledgement":                  map[string]any{"rpc_id": "rpc-2", "clock": "monotonic", "disjoint_observed_intervals": true, "caller_response_ready_ms": 0.000008, "observed_execution_ms": map[string]any{"caller_result": 0.000001, "disk_write": 0.000001, "edit_control": 0.000001, "middleware": 0.000001, "parse": 0.000001, "publish": 0.000001, "reindex_wait": 0.000001, "reply_prepare": 0.000001}, "phase_states": map[string]any{"caller_result": "measured", "disk_write": "measured", "edit_control": "measured", "middleware": "measured", "parse": "measured", "publish": "measured", "reindex_wait": "measured", "reply_prepare": "measured"}},
		"publication_phases":               map[string]any{"key": "mutation-a", "ticket": 3, "source": "mcp_edit", "clock": "monotonic", "terminal": true},
		"requested_background_unavailable": map[string]any{"publication_key": "mutation-a", "scope": "requested_background_observation", "reason": "preexisting_path_work", "requested_generation": 3, "last_observed_generation": 3, "observed_native_handoffs": 0},
		"phase_timing":                     map[string]any{"clock": "monotonic", "patch_wall_ms": 15, "patch_accounting": map[string]any{"execution_disposition": "not_observed", "scope": "accepted_winning_physical_attempt", "background_coverage": "not_established", "attempt_ticket": 3, "attempt_retry": 0, "observed_partition_complete": true, "sum_state": "partial", "phases_ms": phases, "phase_states": states}}}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	return wire, data
}

func TestMixedBackgroundQualificationOnlyWaivesMixedB(t *testing.T) {
	for _, mode := range []string{"qualified", "partial_P", "foreign_original", "foreign_refusal", "successor_P", "missing_deep", "native_noop", "unknown_phase", "invalid_sum", "unknown_reason", "wrong_ACK", "incorrect_read", "stale", "capped", "isolated", "missing_A", "invented_B", "extra_error", "record_phase_mismatch", "raw_pending_tier", "raw_missing_tiers", "raw_missing_stamp", "raw_malformed_stamp", "raw_reordered_stamp"} {
		t.Run(mode, func(t *testing.T) {
			wire, data := mixedBackgroundFixture(t)
			var s map[string]any
			if err := json.Unmarshal(data, &s); err != nil {
				t.Fatal(err)
			}
			physical := s["phase_timing"].(map[string]any)["patch_accounting"].(map[string]any)
			switch mode {
			case "raw_missing_stamp":
				delete(s, "ack_at")
			case "raw_malformed_stamp":
				s["derived_at"] = "invalid"
			case "raw_reordered_stamp":
				s["relations_at"] = "2025-01-01T00:00:00Z"
			case "raw_pending_tier":
				s["tier_state"].(map[string]any)["derived"] = "pending"
			case "raw_missing_tiers":
				delete(s, "tier_state")
			case "record_phase_mismatch":
				v := 2.0
				m := wire.Edits[0].PhasesMS["apply"]
				m.Value = &v
				wire.Edits[0].PhasesMS["apply"] = m
			case "partial_P":
				physical["observed_partition_complete"] = false
			case "foreign_original":
				physical["attempt_ticket"] = 4
			case "foreign_refusal":
				s["requested_background_unavailable"].(map[string]any)["publication_key"] = "foreign"
			case "successor_P":
				s["applied_generation"] = 4
			case "missing_deep":
				delete(physical["phases_ms"].(map[string]any), "apply")
			case "native_noop":
				physical["execution_disposition"] = "native_mtime_duplicate"
			case "unknown_phase":
				physical["phases_ms"].(map[string]any)["unknown"] = 0
			case "invalid_sum":
				s["phase_timing"].(map[string]any)["patch_wall_ms"] = 30
			case "unknown_reason":
				wire.Edits[0].Accounting.Reasons = append(wire.Edits[0].Accounting.Reasons, "unknown")
			case "wrong_ACK":
				wire.Edits[0].ACKDiskStatus = "pending"
			case "incorrect_read":
				v := false
				wire.Edits[0].Readbacks[0].Correct = &v
			case "stale":
				wire.Edits[0].MutationTiers.Tiers["derived"] = "pending"
			case "capped":
				wire.Edits[0].EvidenceBytes = wire.Edits[0].EvidenceLimit + 1
			case "isolated":
				wire.PhysicalWriteScope = nil
			case "missing_A":
				wire.Edits[0].Accounting.A = nil
			case "invented_B":
				wire.Edits[0].Accounting.B = json.RawMessage(`{"wall_ns":1}`)
			case "extra_error":
				wire.Edits[0].Errors = append(wire.Edits[0].Errors, "incorrect read")
			}
			data, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			got := mixedBackgroundRecord(wire, 0, data)
			if got != (mode == "qualified") {
				t.Fatalf("mode=%s qualified=%t", mode, got)
			}
			if wire.Edits[0].Outcome != "diagnostic_failure" || len(wire.Edits[0].Errors) == 0 {
				t.Fatal("qualification rewrote original failure")
			}
		})
	}
}
