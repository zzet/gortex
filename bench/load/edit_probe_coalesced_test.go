package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzet/gortex/bench/receiptproof"
)

func coalescedFunctionalStatusFixture() map[string]any {
	status := supersededStatusFixture()
	status["applied_generation"] = json.Number("4")
	winner := editProbeAccountingFixture()["coalesced_publication"].(map[string]any)
	winner["requested_generation"], winner["applied_generation"] = json.Number("3"), json.Number("4")
	winner["background_coverage"] = "not_established"
	pub := winner["publication_phases"].(map[string]any)
	pub["key"], pub["ticket"], pub["clock"], pub["terminal"] = "mutation-a/physical-4-retry-0", json.Number("4"), "monotonic", true
	physical := winner["timings"].(map[string]any)["phase_timing"].(map[string]any)
	physical["clock"] = "monotonic"
	accounting := physical["patch_accounting"].(map[string]any)
	accounting["attempt_ticket"], accounting["scope"] = json.Number("4"), "accepted_winning_physical_attempt"
	status["coalesced_publication"] = winner
	return status
}

func coalescedFunctionalRecordFixture(t *testing.T) (editProbeRecord, map[string]any) {
	t.Helper()
	record, _ := supersededRecordFixture(t)
	status := coalescedFunctionalStatusFixture()
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
	record.coalescedStatus = coalescedFunctionalStatus(decoded, raw, 34, record.Accounting)
	record.Readback = measured(50, "ms")
	if record.coalescedStatus == nil {
		t.Fatalf("invalid coalesced fixture: %+v", record.Accounting)
	}
	return record, decoded
}

func TestEditProbeQualifiedCoalescedFunctionalNeverMasksFailures(t *testing.T) {
	for _, mode := range []string{"qualified", "unknown_scope", "isolated", "wrong_ACK", "read_error", "missing_file", "missing_oracle", "false_fresh", "cap", "deadline", "accepted_original", "foreign_rpc", "missing_A", "missing_P", "invented_B"} {
		t.Run(mode, func(t *testing.T) {
			record, _ := coalescedFunctionalRecordFixture(t)
			scope := &editProbePhysicalWriteScope{Kind: "shared_worktree_file", File: "sample.go", Editors: 4}
			switch mode {
			case "unknown_scope":
				scope = nil
			case "isolated":
				record.CheckoutID = "checkout"
			case "wrong_ACK":
				record.ACKDiskStatus = "pending"
			case "read_error":
				record.Readbacks[0].Error = "failed"
			case "missing_file":
				record.Readbacks = append(record.Readbacks[:1], record.Readbacks[2:]...)
			case "missing_oracle":
				delete(record.FirstCorrect, "call_chain")
			case "false_fresh":
				record.StaleClaimedFresh = true
			case "cap":
				record.Outcome = "harness_evidence_incomplete"
			case "deadline":
				record.Outcome = "product_deadline"
			case "accepted_original":
				record.coalescedStatus.Status.PUnavailable = false
			case "foreign_rpc":
				record.coalescedStatus.Status.RPCID = "foreign"
			case "missing_A":
				record.Accounting.A = nil
			case "missing_P":
				record.Accounting.P = nil
			case "invented_B":
				record.Accounting.B = &editProbeWindow{WallNS: 1}
			}
			p := record.Accounting.P
			got := classifyCoalescedFunctionalRecord(&record, scope)
			if got != (mode == "qualified") {
				t.Fatalf("mode=%s classified=%v", mode, got)
			}
			if got {
				if record.Outcome != receiptproof.CoalescedFunctionalOutcome || record.Superseded != nil || record.Accounting.P != nil || record.Accounting.B != nil || record.CoalescedFunctional == nil || record.CoalescedFunctional.ServingPhysical != receiptproof.Window(*p) || len(record.Accounting.Reasons) == 0 {
					t.Fatal("qualified class invented or lost timing evidence")
				}
			} else if record.CoalescedFunctional != nil {
				t.Fatal("invalid evidence classified")
			}
		})
	}
}

func TestEditProbeQualifiedCoalescedLatestStatusRejectsUnknownAndAccepted(t *testing.T) {
	for _, mode := range []string{"qualified", "missing_status", "accepted_original", "wrong_winner", "wrong_background", "pending", "fresh_false", "late_background", "partial_P"} {
		t.Run(mode, func(t *testing.T) {
			record, status := coalescedFunctionalRecordFixture(t)
			switch mode {
			case "missing_status":
				status = nil
			case "accepted_original":
				status["phase_timing"].(map[string]any)["patch_wall_ms"] = json.Number("1")
			case "wrong_winner":
				status["coalesced_publication"].(map[string]any)["publication_phases"].(map[string]any)["ticket"] = json.Number("9")
			case "wrong_background":
				status["requested_background_unavailable"].(map[string]any)["publication_key"] = "foreign"
			case "pending":
				status["graph_status_terminal"] = false
			case "fresh_false":
				status["graph_status"] = "pending"
			case "late_background":
				status["requested_background"] = map[string]any{}
			case "partial_P":
				record.Accounting.Reasons = append(record.Accounting.Reasons, "missing applicable P phase apply")
			}
			candidate := coalescedFunctionalStatus(status, json.RawMessage(`{}`), 35, record.Accounting)
			if (candidate != nil) != (mode == "qualified") {
				t.Fatalf("mode=%s candidate=%+v", mode, candidate)
			}
		})
	}
}

func TestEditProbeQualifiedCoalescedFinishKeepsSeparateWinnerAndUnavailableOriginal(t *testing.T) {
	record, _ := coalescedFunctionalRecordFixture(t)
	state := &editProbeState{record: record, rawBytes: 98839}
	phase := &Phase{Metrics: map[string]Metric{}, EditProbe: &editProbeReport{PhysicalWriteScope: &editProbePhysicalWriteScope{Kind: "shared_worktree_file", File: "sample.go", Editors: 4}}}
	run := &editProbeRuntime{p: &privateRun{Root: t.TempDir(), Repos: []privateRepo{{Prefix: "repo"}}}, phase: phase, states: []*editProbeState{state}, plan: editProbePlan{Steps: []editProbeStep{{Ordinal: 1}}, Seed: 1}, requireAccounting: true}
	if err := run.finish(); err != nil {
		t.Fatal(err)
	}
	got := phase.EditProbe.Records[0]
	if got.Outcome != receiptproof.CoalescedFunctionalOutcome || got.CoalescedFunctional == nil || got.Superseded != nil || got.Accounting.P != nil || got.Accounting.B != nil || len(got.Errors) != 0 || len(got.Accounting.Reasons) == 0 {
		t.Fatalf("incorrect final class: %+v", got)
	}
	summary := phase.EditProbe.Summary
	if summary.Outcomes[receiptproof.CoalescedFunctionalOutcome] != 1 || summary.Offered != 1 {
		t.Fatal("qualified record dropped from denominator")
	}
}
