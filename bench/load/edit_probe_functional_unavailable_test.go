package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzet/gortex/bench/receiptproof"
)

func functionalUnavailableRecordFixture(t *testing.T, preexisting bool) (editProbeRecord, map[string]any) {
	t.Helper()
	record, _ := coalescedFunctionalRecordFixture(t)
	record.coalescedStatus = nil
	status := supersededStatusFixture()
	// These retained status shapes omit the entire original physical window.
	delete(status["phase_timing"].(map[string]any), "patch_wall_ms")
	status["phases_ms"], status["phase_states"] = map[string]any{}, map[string]any{}
	for _, name := range receiptproof.OriginalPhysicalPhases() {
		status["phases_ms"].(map[string]any)[name] = nil
		status["phase_states"].(map[string]any)[name] = "not_observed"
	}
	for _, key := range []string{"ack_at", "file_graph_at", "relations_at", "derived_at"} {
		status[key] = "2026-01-01T00:00:00Z"
	}
	status["tier_state"] = map[string]any{"file_graph": "fresh", "relations": "fresh", "derived": "fresh"}
	if preexisting {
		bg := status["requested_background_unavailable"].(map[string]any)
		bg["reason"] = "preexisting_path_work"
		for _, key := range []string{"binding_predicate", "background_stage", "accepted_attempt_stage"} {
			delete(bg, key)
		}
	}
	return record, status
}

func TestEditProbeFunctionalUnavailablePreservesUnknownTimingAndRejectsExecutedOriginal(t *testing.T) {
	for _, mode := range []string{"direct43", "preexisting86", "direct53", "accepted_stamps", "physical_window", "physical_accounting", "physical_phase", "measured_state", "unknown_phase", "read_assisted", "winner", "foreign_owner", "foreign_rpc", "handoff", "unknown_refusal", "isolated", "missing_A", "bad_ACK", "read_failure", "missing_oracle", "pending_tier", "cap", "extra_error", "malformed_stage", "malformed_timing", "malformed_phases"} {
		t.Run(mode, func(t *testing.T) {
			record, status := functionalUnavailableRecordFixture(t, mode == "preexisting86")

			generation := map[string]json.Number{"direct43": "88", "preexisting86": "175", "direct53": "109"}[mode]
			if generation != "" {
				status["reindex_generation"], status["applied_generation"] = generation, generation
				status["publication_phases"].(map[string]any)["ticket"] = generation
				status["requested_background_unavailable"].(map[string]any)["requested_generation"], status["requested_background_unavailable"].(map[string]any)["last_observed_generation"] = generation, generation
			}
			bg := status["requested_background_unavailable"].(map[string]any)
			scope := &editProbePhysicalWriteScope{Kind: "shared_worktree_file", File: "sample.go", Editors: 4}
			switch mode {
			case "malformed_stage":
				bg["reason"] = "preexisting_path_work"
				bg["binding_predicate"], bg["background_stage"], bg["accepted_attempt_stage"] = nil, nil, json.Number("1")
			case "malformed_timing":
				status["phase_timing"] = "bad"
			case "malformed_phases":
				status["phases_ms"] = "bad"
			case "accepted_stamps":
				bg["accepted_attempt_stage"] = "accepted_stamps_retained"
			case "physical_window":
				status["phase_timing"].(map[string]any)["patch_wall_ms"] = json.Number("0")
			case "physical_accounting":
				status["phase_timing"].(map[string]any)["patch_accounting"] = map[string]any{}
			case "physical_phase":
				status["phases_ms"].(map[string]any)["apply"] = json.Number("0")
			case "measured_state":
				status["phase_states"].(map[string]any)["apply"] = "measured"
			case "unknown_phase":
				status["phases_ms"].(map[string]any)["unknown"] = json.Number("1")
			case "read_assisted":
				status["read_assisted_publication"] = map[string]any{}
			case "winner":
				status["coalesced_publication"] = map[string]any{}
			case "foreign_owner":
				bg["publication_key"] = "foreign"
			case "foreign_rpc":
				// Preserve the independently bound A RPC; mutate only the proof below.
			case "handoff":
				bg["observed_native_handoffs"] = json.Number("1")
			case "unknown_refusal":
				bg["reason"] = "unknown"
			case "isolated":
				scope = nil
			case "bad_ACK":
				record.ACKDiskStatus = "pending"
			case "read_failure":
				record.Readbacks[0].Correct = false
			case "missing_oracle":
				delete(record.FirstCorrect, "call_chain")
			case "cap":
				record.Outcome = "harness_evidence_incomplete"
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
			if mode == "missing_A" {
				record.Accounting.A = nil
			}
			if mode == "extra_error" {
				record.Errors = append(record.Errors, "read failed")
			}
			record.PhasesMS, record.MutationTiers = editProbePhases(decoded, "status exposes no phases_ms"), editProbeStatusTiers(decoded)
			if mode == "pending_tier" {
				record.MutationTiers.Tiers["derived"] = "pending"
			}
			record.functionalUnavailableStatus = functionalUnavailableStatus(decoded, raw, 41, record.Accounting)
			if mode == "foreign_rpc" && record.functionalUnavailableStatus != nil {
				record.functionalUnavailableStatus.Status.RPCID = "foreign"
			}
			got := classifyFunctionalUnavailableRecord(&record, scope)
			want := mode == "direct43" || mode == "preexisting86" || mode == "direct53"
			if got != want {
				t.Fatalf("mode=%s qualified=%v candidate=%+v reasons=%v", mode, got, record.functionalUnavailableStatus, record.Accounting.Reasons)
			}
			if got {
				if record.Accounting.P != nil || record.Accounting.B != nil || record.CoalescedFunctional != nil || record.Superseded != nil || record.Outcome != receiptproof.FunctionalUnavailableOutcome || len(record.Errors) != 0 {
					t.Fatal("invented window/owner or lost original diagnostic")
				}
				encoded, err := json.Marshal(record.FunctionalUnavailable)
				if err != nil {
					t.Fatal(err)
				}
				var proof map[string]any
				if err := json.Unmarshal(encoded, &proof); err != nil {
					t.Fatal(err)
				}
				for _, invented := range []string{"winner", "serving_physical", "native_noop", "guard"} {
					if _, present := proof[invented]; present {
						t.Fatal("invented indexing evidence: " + invented)
					}
				}
				if len(record.Accounting.Reasons) == 0 {
					t.Fatal("original timing refusal erased")
				}
			}
		})
	}
}

func TestEditProbeFunctionalUnavailableFinishKeepsDenominatorAndUnavailableDeepPhases(t *testing.T) {
	record, status := functionalUnavailableRecordFixture(t, false)
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
	record.PhasesMS, record.MutationTiers = editProbePhases(decoded, "status exposes no phases_ms"), editProbeStatusTiers(decoded)
	record.functionalUnavailableStatus = functionalUnavailableStatus(decoded, raw, 42, record.Accounting)
	if record.functionalUnavailableStatus == nil {
		t.Fatal("owned unavailable status missing")
	}
	record.MutationTiers.Tiers["derived"] = "pending"
	if editProbeFinishedWithQualification(true, true, true, record.Accounting, true, record.MutationTiers) {
		t.Fatal("pending tier ended observation")
	}
	record.MutationTiers.Tiers["derived"] = "fresh"
	if !editProbeFinishedWithQualification(true, true, true, record.Accounting, true, record.MutationTiers) {
		t.Fatal("actual final fresh tiers did not settle")
	}
	state := &editProbeState{record: record, rawBytes: 98839}
	phase := &Phase{Metrics: map[string]Metric{}, EditProbe: &editProbeReport{PhysicalWriteScope: &editProbePhysicalWriteScope{Kind: "shared_worktree_file", File: "sample.go", Editors: 4}}}
	run := &editProbeRuntime{p: &privateRun{Root: t.TempDir(), Repos: []privateRepo{{Prefix: "repo"}}}, phase: phase, states: []*editProbeState{state}, plan: editProbePlan{Steps: []editProbeStep{{Ordinal: 1}}, Seed: 1}, requireAccounting: true}
	if err := run.finish(); err != nil {
		t.Fatal(err)
	}
	got := phase.EditProbe.Records[0]
	if got.Outcome != receiptproof.FunctionalUnavailableOutcome || got.FunctionalUnavailable == nil || got.Accounting.P != nil || got.Accounting.B != nil || got.CoalescedFunctional != nil || got.Superseded != nil {
		t.Fatal("unavailable timing converted into execution proof")
	}
	for _, name := range editProbePhaseNames[8:] {
		if got.PhasesMS[name].Value != nil {
			t.Fatal("fabricated deep phase " + name)
		}
	}
	if phase.EditProbe.Summary.Outcomes[receiptproof.FunctionalUnavailableOutcome] != 1 || phase.EditProbe.Summary.Offered != 1 {
		t.Fatal("functional success lost from denominator")
	}
}
