package main

import (
	"encoding/json"
	"slices"
	"testing"
)

// These runtime controls use a real owned child plus typed model witnesses.
// They do not replace the separate actual Server/transport integration proof.
func TestSharedRuntimeKeepsIndependentACKAndOwnerWindows(t *testing.T) {
	p, _, status := sharedOwnerProofModel(t)
	status["acknowledgement"] = editProbeAccountingFixture()["acknowledgement"]
	r := &editProbeRuntime{p: p, phase: &Phase{EditProbe: &editProbeReport{Role: "candidate"}}}
	got := r.accountingForStatus(t.Context(), status, "commit-a", "edit.file", false)
	if got.State != "observed_shared_owner_windows_checks_pass" || got.A == nil || got.P != nil || got.B != nil || got.Shared == nil {
		t.Fatalf("complete owned shared execution must keep independent A and no per-file P/B: %+v", got)
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing A", func(v map[string]any) { delete(v, "acknowledgement") }},
		{"missing current membership", func(v map[string]any) {
			delete(v["shared_owner_accounting_v1"].(map[string]any), "membership")
		}},
		{"incomplete original frontier", func(v map[string]any) {
			v["shared_owner_accounting_v1"].(map[string]any)["definition"].(map[string]any)["frontier_count"] = "2"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := cloneSharedOwnerModel(t, status)
			tc.change(value)
			got := r.accountingForStatus(t.Context(), value, "commit-a", "edit.file", false)
			if got.State == "observed_shared_owner_windows_checks_pass" || got.P != nil || got.B != nil || len(got.Reasons) == 0 {
				t.Fatalf("current missing proof borrowed an earlier dictionary entry: %+v", got)
			}
		})
	}
}

func TestSharedRuntimeFinalContradictionKeepsEditDenominatorAndACK(t *testing.T) {
	var report editProbeSharedOwnerReport
	proof := sharedReportProofFixture()
	ref, err := report.observe(proof, nil)
	if err != nil {
		t.Fatal(err)
	}
	ack := &editProbeWindow{WallNS: 2_000_000, SumNS: 2_000_000, WithinFivePercent: true}
	for _, outcome := range []string{"complete", "phase_censored"} {
		record := editProbeRecord{Ordinal: 9, Outcome: outcome, GraphTerminal: true, Ack: measured(2, "ms"), Accounting: editProbeAccounting{State: "observed_shared_owner_windows_checks_pass", A: ack, Shared: ref}}
		if outcome == "complete" {
			changed := sharedReportProofFixture()
			changed.definition = []byte(`{"test_definition":2}`)
			if _, err := report.observe(changed, nil); err == nil {
				t.Fatal("contradictory frozen definition accepted")
			}
		}
		invalidateSharedAccounting(&record, &report, true)
		if record.Ordinal != 9 || record.Accounting.A != ack || record.Ack.Value == nil || *record.Ack.Value != 2 || !record.GraphTerminal || record.Accounting.P != nil || record.Accounting.B != nil || len(record.Errors) == 0 || record.Accounting.State != "terminal_unavailable_shared_owner" {
			t.Fatalf("late contradiction changed edit/ACK facts or retained accounting credit: %+v", record)
		}
		if outcome == "complete" && record.Outcome != "diagnostic_failure" || outcome == "phase_censored" && record.Outcome != outcome {
			t.Fatalf("late owner failure removed/reclassified a censored edit: %+v", record)
		}
	}
}

func TestSharedRuntimeLeavesPointAccountingUnchanged(t *testing.T) {
	r := &editProbeRuntime{}
	status := editProbeAccountingFixture()
	got := r.accountingForStatus(t.Context(), status, "receipt-a", "edit.file", false)
	if got.State != "observed_single_window_checks_pass" || got.A == nil || got.P == nil || got.B == nil {
		t.Fatalf("ordinary point path acquired an owned-child dependency: %+v", got)
	}
}

func TestEditProbeLocalFileACKWaitRequiresOwnedCountedEvidence(t *testing.T) {
	for _, name := range []string{"bound", "refusal", "replayed", "full", "foreign receipt", "foreign mutation", "foreign commit", "foreign content", "missing count", "missing P/B"} {
		t.Run(name, func(t *testing.T) {
			ack := map[string]any{
				"status": "applied", "disk_status": "committed", "ack_mode": "local",
				"mutation_id": "mutation-a", "mutation_receipt": "receipt-a",
				"commit_seq": json.Number("2"), "content_sha256": "content-a",
			}
			status := editProbeAccountingFixture()
			status["found"], status["tool"] = true, "edit_file"
			for _, field := range []string{"disk_status", "ack_mode", "mutation_id", "commit_seq", "content_sha256"} {
				status[field] = ack[field]
			}
			window := status["acknowledgement"].(map[string]any)
			window["caller_response_ready_ms"] = json.Number("7")
			window["observed_execution_ms"].(map[string]any)["reindex_wait"] = nil
			window["phase_states"].(map[string]any)["reindex_wait"] = "not_applicable"
			window["phases_ms"] = map[string]any{"reindex_wait": nil}
			window["counts"] = map[string]any{"reindex_wait": map[string]any{
				"started": json.Number("1"), "completed": json.Number("1"), "not_applicable": json.Number("1"),
			}}
			switch name {
			case "refusal":
				ack["status"] = "refused"
			case "replayed":
				ack["replayed"] = true
			case "full":
				ack["ack_mode"], status["ack_mode"] = "full", "full"
			case "foreign receipt":
				status["receipt"] = "receipt-b"
			case "foreign mutation":
				status["mutation_id"] = "mutation-b"
			case "foreign commit":
				status["commit_seq"] = json.Number("3")
			case "foreign content":
				status["content_sha256"] = "content-b"
			case "missing count":
				delete(window, "counts")
			case "missing P/B":
				delete(status, "coalesced_publication")
				delete(status, "requested_background")
			}
			runtime := &editProbeRuntime{}
			owned := localFileAcknowledgementMatches(ack, status, "edit.file", "mutation-a")
			got := runtime.accountingForStatus(t.Context(), status, "receipt-a", "edit.file", owned)
			missingWait := slices.Contains(got.Reasons, "missing applicable A phase reindex_wait")
			if missingWait != (name != "bound" && name != "missing P/B") {
				t.Fatalf("unexpected wait disposition: %+v", got)
			}
			if passed := got.State == "observed_single_window_checks_pass"; passed != (name == "bound") {
				t.Fatalf("unexpected whole accounting verdict: %+v", got)
			}
			if name == "missing P/B" && (got.A == nil || got.P != nil || got.B != nil) {
				t.Fatalf("missing physical/background windows changed A or became a sample: %+v", got)
			}
			if editProbeFinished(true, false, true, got) {
				t.Fatal("wait applicability passed a missing readback")
			}
		})
	}
}

func TestSharedRuntimeCompletionRequiresDistinctOwnerSuccess(t *testing.T) {
	if !editProbeFinished(true, true, true, editProbeAccounting{State: "observed_shared_owner_windows_checks_pass"}) {
		t.Fatal("actual shared owner success cannot settle an otherwise complete edit")
	}
	for _, state := range []string{"unavailable_shared_owner", "terminal_unavailable_shared_owner"} {
		if editProbeFinished(true, false, true, editProbeAccounting{State: state}) {
			t.Fatal("missing semantic witnesses were waived by shared accounting state")
		}
	}
}

// The preparation-policy control separately proves that search-only and baseline
// runs disable required accounting. Exercise the actual final-record path here:
// optional timing evidence may lose credit without changing a semantic result.
func TestSharedRuntimeOptionalAccountingCannotPoisonCompletedSearch(t *testing.T) {
	for _, kind := range []string{"point", "move"} {
		t.Run(kind, func(t *testing.T) {
			var report editProbeSharedOwnerReport
			proof := sharedReportProofFixture()
			ref, err := report.observe(proof, nil)
			if err != nil {
				t.Fatal(err)
			}
			changed := sharedReportProofFixture()
			changed.definition = []byte(`{"test_definition":2}`)
			if _, err := report.observe(changed, nil); err == nil {
				t.Fatal("contradiction not established")
			}
			ack := &editProbeWindow{WallNS: 2_000_000, SumNS: 2_000_000, WithinFivePercent: true}
			record := editProbeRecord{Ordinal: 0, Outcome: "complete", GraphTerminal: true, Ack: measured(2, "ms")}
			if kind == "point" {
				record.Accounting = editProbeAccounting{State: "observed_shared_owner_windows_checks_pass", A: ack, Shared: ref}
			} else {
				record.MoveAccounting = &editProbeMoveAccounting{State: "observed_move_shared_owner_windows_checks_pass", A: ack, Files: []editProbeMoveFileAccounting{{Shared: ref}}}
			}
			run := &editProbeRuntime{requireAccounting: false, sharedReport: report, phase: &Phase{EditProbe: &editProbeReport{}}, plan: editProbePlan{Count: 1, Mode: "json", Steps: []editProbeStep{{Ordinal: 0}}}, states: []*editProbeState{{record: record}}}
			if err := run.finish(); err != nil {
				t.Fatalf("optional accounting poisoned completed semantics: %v", err)
			}
			got := run.phase.EditProbe.Records[0]
			if got.Outcome != "complete" || len(got.Errors) != 0 || got.Ack.Value == nil || *got.Ack.Value != 2 || !got.GraphTerminal {
				t.Fatalf("semantic/ACK result changed: %+v", got)
			}
			if kind == "point" {
				if got.Accounting.State != "terminal_unavailable_shared_owner" || len(got.Accounting.Reasons) == 0 || got.Accounting.Shared != ref || got.Accounting.A != ack || got.Accounting.P != nil || got.Accounting.B != nil {
					t.Fatalf("invalid optional timing retained credit or erased original facts: %+v", got.Accounting)
				}
			} else if got.MoveAccounting.State != "unavailable_move_shared_owner" || len(got.MoveAccounting.Reasons) == 0 || got.MoveAccounting.A != ack || got.MoveAccounting.Files[0].Shared != ref {
				t.Fatalf("invalid optional move timing retained credit or erased original facts: %+v", got.MoveAccounting)
			}
			if run.phase.EditProbe.SharedOwners.Execution.Count != 0 || run.phase.EditProbe.SharedOwners.Background.Count != 0 {
				t.Fatal("contradictory owner earned numeric credit")
			}
		})
	}
}
