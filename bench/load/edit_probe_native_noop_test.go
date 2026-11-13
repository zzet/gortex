package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func nativeNoOpStatusFixture(same bool) map[string]any {
	status := coalescedFunctionalStatusFixture()
	winner := status["coalesced_publication"].(map[string]any)
	physical := winner["timings"].(map[string]any)["phase_timing"].(map[string]any)
	pa := physical["patch_accounting"].(map[string]any)
	phases, states := pa["phases_ms"].(map[string]any), pa["phase_states"].(map[string]any)
	for _, name := range []string{"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "patch_analysis", "executor_control"} {
		phases[name], states[name] = nil, "not_applicable"
	}
	physical["patch_wall_ms"] = json.Number("5")
	pa["sum_state"], pa["execution_disposition"] = "partial", "native_mtime_duplicate"
	bg := status["requested_background_unavailable"].(map[string]any)
	bg["reason"] = "preexisting_path_work"
	for _, key := range []string{"binding_predicate", "background_stage", "accepted_attempt_stage"} {
		delete(bg, key)
	}
	if same {
		status["applied_generation"] = json.Number("3")
		pa["attempt_ticket"] = json.Number("3")
		status["phase_timing"] = physical
		delete(status, "coalesced_publication")
	}
	return status
}

func TestEditProbeQualifiedNativeNoOpRequiresPositiveUnexecutedEvidence(t *testing.T) {
	for _, mode := range []string{"coalesced", "same_request", "unknown_disposition", "executed_phase", "fabricated_zero", "partial_control", "unknown_scope", "incorrect_read", "false_fresh", "contradictory_full_noop", "same_request_with_background", "coalesced_with_background"} {
		t.Run(mode, func(t *testing.T) {
			record, _ := coalescedFunctionalRecordFixture(t)
			same := mode == "same_request" || mode == "same_request_with_background"
			status := nativeNoOpStatusFixture(same)
			physical := status["phase_timing"].(map[string]any)
			if !same {
				physical = status["coalesced_publication"].(map[string]any)["timings"].(map[string]any)["phase_timing"].(map[string]any)
			}
			if strings.HasSuffix(mode, "with_background") {
				phases := make(map[string]any)
				for _, name := range []string{"attempt_setup", "patch_inspection", "attempt_finalize", "cohort_admission", "lane_wait"} {
					phases[name] = json.Number("1")
				}
				for _, name := range requestedBackgroundLapNamesForProbe {
					phases[name] = json.Number("1")
				}
				applied, wall, coverage, handoffs := json.Number("4"), json.Number("12"), "one_canceled_timer_one_native_successor", json.Number("1")
				if same {
					applied, wall, coverage, handoffs = json.Number("3"), json.Number("8"), "direct_requested_attempt", json.Number("0")
					for _, name := range []string{"canceled_quiet_wait", "timer_stop", "coalescing_control", "successor_timer_arm", "successor_quiet_wait"} {
						phases[name] = json.Number("0")
					}
					phases["requested_quiet_wait"] = json.Number("1")
				}
				status["requested_background"] = map[string]any{"requested_generation": json.Number("3"), "applied_generation": applied, "observed_partition_complete": true, "scope": "accepted_scheduling_call_entry_to_serving_attempt_return", "coverage": coverage, "sum_state": "partial", "observed_native_handoffs": handoffs, "background_wall_ms": wall, "phases_ms": phases}
				delete(status, "requested_background_unavailable")
			}
			pa := physical["patch_accounting"].(map[string]any)
			switch mode {
			case "contradictory_full_noop":
				phases, states := pa["phases_ms"].(map[string]any), pa["phase_states"].(map[string]any)
				for name := range phases {
					phases[name], states[name] = json.Number("1"), "measured"
				}
				physical["patch_wall_ms"] = json.Number("14")
			case "unknown_disposition":
				pa["execution_disposition"] = "not_observed"
			case "executed_phase":
				pa["phases_ms"].(map[string]any)["apply"] = json.Number("1")
				pa["phase_states"].(map[string]any)["apply"] = "measured"
			case "fabricated_zero":
				pa["phases_ms"].(map[string]any)["apply"] = json.Number("0")
			case "partial_control":
				pa["phase_states"].(map[string]any)["attempt_setup"] = "not_observed"
			}
			raw, err := json.Marshal(stubContent(status))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeEditProbeResult(raw)
			if err != nil {
				t.Fatal(err)
			}
			record.Accounting = checkEditProbeAccounting(decoded, record.Receipt)
			record.Errors = []string{"A/P/B accounting unavailable: " + strings.Join(record.Accounting.Reasons, "; ")}
			record.coalescedStatus = coalescedFunctionalStatus(decoded, raw, 35, record.Accounting)
			scope := &editProbePhysicalWriteScope{Kind: "shared_worktree_file", File: "sample.go", Editors: 4}
			switch mode {
			case "unknown_scope":
				scope = nil
			case "incorrect_read":
				record.Readbacks[0].Correct = false
			case "false_fresh":
				record.StaleClaimedFresh = true
			}
			got := classifyCoalescedFunctionalRecord(&record, scope)
			want := mode == "coalesced" || mode == "same_request" || strings.HasSuffix(mode, "with_background")
			if got != want {
				t.Fatalf("mode=%s classified=%v accounting=%+v", mode, got, record.Accounting)
			}
			if got && (record.Accounting.P != nil || record.Accounting.B != nil || record.CoalescedFunctional == nil) {
				t.Fatal("no-op control window was attributed as indexing P/B")
			}
		})
	}
}

func TestEditProbeSameRequestNoOpWaitsForActualFreshTierObservation(t *testing.T) {
	record, _ := coalescedFunctionalRecordFixture(t)
	status := nativeNoOpStatusFixture(true)
	raw, err := json.Marshal(stubContent(status))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeEditProbeResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	accounting := checkEditProbeAccounting(decoded, record.Receipt)
	candidate := coalescedFunctionalStatus(decoded, raw, 36, accounting)
	if candidate == nil {
		t.Fatal("positive no-op candidate missing")
	}
	tiers := editProbeStatusTiers(decoded)
	tiers.Tiers["derived"] = "pending"
	if editProbeFinishedWithQualification(true, true, true, accounting, candidate, tiers) {
		t.Fatal("pending derived tier ended observation")
	}
	tiers.Tiers["derived"] = "fresh"
	if !editProbeFinishedWithQualification(true, true, true, accounting, candidate, tiers) {
		t.Fatal("later observed fresh tier did not complete")
	}
}

func TestEditProbeContradictoryNativeClaimCannotFallThroughToExecutedWinner(t *testing.T) {
	record, status := coalescedFunctionalRecordFixture(t)
	pa := status["coalesced_publication"].(map[string]any)["timings"].(map[string]any)["phase_timing"].(map[string]any)["patch_accounting"].(map[string]any)
	pa["execution_disposition"] = "native_mtime_duplicate"
	raw, err := json.Marshal(stubContent(status))
	if err != nil {
		t.Fatal(err)
	}
	if p := coalescedFunctionalStatus(status, raw, 37, record.Accounting); p != nil {
		t.Fatal("contradictory native claim fell through to complete indexing winner")
	}
}
