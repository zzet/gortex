package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEditProbeSeparateWinnerPreservesPreexistingOriginalBackgroundRefusal(t *testing.T) {
	for _, mode := range []string{"separate_winner", "direct_accepted_original", "foreign_winner"} {
		t.Run(mode, func(t *testing.T) {
			record, status := coalescedFunctionalRecordFixture(t)
			bg := status["requested_background_unavailable"].(map[string]any)
			bg["reason"] = "preexisting_path_work"
			for _, key := range []string{"binding_predicate", "background_stage", "accepted_attempt_stage"} {
				delete(bg, key)
			}
			winner := status["coalesced_publication"].(map[string]any)
			switch mode {
			case "direct_accepted_original":
				status["applied_generation"] = json.Number("3")
				physical := winner["timings"].(map[string]any)["phase_timing"].(map[string]any)
				physical["patch_accounting"].(map[string]any)["attempt_ticket"] = json.Number("3")
				status["phase_timing"] = physical
				delete(status, "coalesced_publication")
			case "foreign_winner":
				winner["publication_phases"].(map[string]any)["ticket"] = json.Number("999")
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
			record.coalescedStatus = coalescedFunctionalStatus(decoded, raw, 38, record.Accounting)
			got := classifyCoalescedFunctionalRecord(&record, &editProbePhysicalWriteScope{Kind: "shared_worktree_file", File: "sample.go", Editors: 4})
			if got != (mode == "separate_winner") {
				t.Fatalf("mode=%s qualified=%v", mode, got)
			}
			if got && (record.Accounting.P != nil || record.Accounting.B != nil || record.CoalescedFunctional.Status.Refusal != "preexisting_path_work" || record.CoalescedFunctional.NativeNoOp != nil) {
				t.Fatal("original refusal/timing or separate-winner distinction lost")
			}
		})
	}
}
