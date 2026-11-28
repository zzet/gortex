package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// linkedReceiptCell replaces the routed cell's receipt with the trimmed
// linked receipt, bound to the edit's own receipt, mutation and checkout.
func linkedReceiptCell(t *testing.T, fixed bool) func(map[string]any) {
	return func(status map[string]any) {
		receipt, mutation, checkout := status["receipt"], status["mutation_id"], status["checkout_id"]
		for key := range status {
			delete(status, key)
		}
		for key, value := range decodeLinkedReceipt(t, fixed) {
			status[key] = value
		}
		status["receipt"], status["mutation_id"], status["checkout_id"] = receipt, mutation, checkout
	}
}

// Finish the one-edit run and fold its phase into a report, as a run does.
func finishLinkedReadbackRun(t *testing.T, got linkedReadbackResult) (editProbeRecord, map[string]any, Report) {
	t.Helper()
	run := got.run
	var kept *editProbeState
	for _, s := range run.states {
		if s.step.Ordinal == got.record.Ordinal {
			kept = s
		}
	}
	// The cell dispatched one planned edit; the run's report covers it alone.
	// Every planned state owns an open spool file, so the full set is put back
	// before the run's cleanup closes them (an open file cannot be removed on
	// Windows).
	allStates, allSteps := run.states, run.plan.Steps
	t.Cleanup(func() { run.states, run.plan.Steps = allStates, allSteps })
	run.states, run.plan.Steps = []*editProbeState{kept}, []editProbeStep{kept.step}
	finished := run.finish()
	report := Report{Phases: []Phase{*run.phase}}
	if finished != nil {
		report.Errors = append(report.Errors, finished.Error())
	}
	if err := finishReport(&report, 1); err != nil {
		t.Fatal(err)
	}
	records := run.phase.EditProbe.Records
	if len(records) != 1 {
		t.Fatalf("finished records = %d, want 1", len(records))
	}
	return records[0], encodeForTest(t, run.phase.EditProbe.Summary), report
}

func encodeForTest(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A receipt whose accounting ends in a typed terminal state, the B refusal of
// the reference smoke's linked and primary receipts, is terminal: the
// candidate record completes, the state and refusal are reported, and the
// run's report is complete.
func TestEditProbeTypedTerminalAccountingIsNotARecordError(t *testing.T) {
	for _, cell := range []struct {
		name      string
		cell      linkedReadbackCell
		predicate string
	}{
		{name: "linked checkout receipt", cell: linkedReadbackCell{receipt: linkedReceiptCell(t, true)}, predicate: "checkout_refresh_cycle"},
		{name: "primary receipt", cell: linkedReadbackCell{primary: true, ackFixture: "smoke-primary-ack.json", statusFixture: "smoke-primary-receipt.json"}, predicate: "direct_physical_window_missing"},
	} {
		t.Run(cell.name, func(t *testing.T) {
			cell.cell.candidate = true
			got := runLinkedReadbackCell(t, cell.cell)
			if got.record.Outcome != "complete" || len(got.record.Errors) != 0 {
				t.Fatalf("typed terminal accounting = %s %v, want complete with no error", got.record.Outcome, got.record.Errors)
			}
			accounting, _ := got.json["accounting"].(map[string]any)
			refusal, _ := accounting["background_refusal"].(map[string]any)
			if accounting["state"] != "terminal_unavailable_background" || refusal["reason"] != "serving_attempt_binding_mismatch" || refusal["binding_predicate"] != cell.predicate || refusal["requested_generation"] != float64(1) {
				t.Fatalf("accounting = %v, want terminal_unavailable_background with the receipt's typed refusal", accounting)
			}
			record, summary, report := finishLinkedReadbackRun(t, got)
			if record.Outcome != "complete" || !report.Complete || len(report.Errors) != 0 {
				t.Fatalf("finished record %s, report complete %v errors %v", record.Outcome, report.Complete, report.Errors)
			}
			states, _ := summary["accounting_states"].(map[string]any)
			unavailable, _ := summary["accounting_unavailable"].(map[string]any)
			if states["terminal_unavailable_background"] != float64(1) || unavailable["serving_attempt_binding_mismatch|"+cell.predicate] != float64(1) {
				t.Fatalf("summary censuses = %v / %v", states, unavailable)
			}
		})
	}
}

// Accounting that never becomes terminal (the receipt without its typed B
// disposition stays unavailable until the status-cycle bound) is still a
// record error for the candidate. The baseline role requires no accounting.
func TestEditProbeNonTerminalAccountingStaysARecordError(t *testing.T) {
	got := runLinkedReadbackCell(t, linkedReadbackCell{candidate: true, receipt: linkedReceiptCell(t, false)})
	accounting := got.record.Accounting
	if accounting.State != "unavailable" || got.record.Outcome != "diagnostic_failure" || len(got.record.Errors) != 1 ||
		got.record.Errors[0] != "A/P/B accounting not terminal: "+strings.Join(accounting.Reasons, "; ") {
		t.Fatalf("non-terminal accounting = %s %v (%s), want the not-terminal record error alone", got.record.Outcome, got.record.Errors, accounting.State)
	}
	if _, present := got.json["accounting"].(map[string]any)["background_refusal"]; present {
		t.Fatalf("a receipt without its B refusal reported one: %v", got.json["accounting"])
	}
}

func TestEditProbeBaselineRoleReportsNoAccountingError(t *testing.T) {
	got := runLinkedReadbackCell(t, linkedReadbackCell{receipt: linkedReceiptCell(t, false)})
	if got.record.Outcome != "complete" || len(got.record.Errors) != 0 {
		t.Fatalf("baseline record = %s %v, want complete without accounting", got.record.Outcome, got.record.Errors)
	}
	if got.run.requireAccounting || got.run.phase.EditProbe.AccountingRequired {
		t.Fatal("baseline role required accounting")
	}
	_, _, report := finishLinkedReadbackRun(t, got)
	if !report.Complete {
		t.Fatalf("baseline report incomplete: %v", report.Errors)
	}
}

// TestSummarizeEditProbeAccountingStatesCountsEveryState: the state census
// counts every record that has a state, not only those with a background
// refusal, and the refusal census counts only the refusal.
func TestSummarizeEditProbeAccountingStatesCountsEveryState(t *testing.T) {
	var records []editProbeRecord
	for i, state := range []string{"observed_single_window_checks_pass", "terminal_partial_physical", "unavailable", "terminal_unavailable_background"} {
		row := fmt.Sprintf(`{"ordinal":%d,"outcome":"complete","readbacks":[],"accounting":{"state":%q,"reasons":[]}}`, i, state)
		var record editProbeRecord
		if err := json.Unmarshal([]byte(row), &record); err != nil {
			t.Fatal(err)
		}
		if state == "terminal_unavailable_background" {
			record.Accounting.BackgroundRefusal = &editProbeBackgroundRefusal{Reason: "serving_attempt_binding_mismatch", BindingPredicate: "checkout_refresh_cycle"}
		}
		records = append(records, record)
	}
	summary := encodeForTest(t, summarizeEditProbe(records, 42))
	states, _ := summary["accounting_states"].(map[string]any)
	unavailable, _ := summary["accounting_unavailable"].(map[string]any)
	for _, state := range []string{"observed_single_window_checks_pass", "terminal_partial_physical", "unavailable", "terminal_unavailable_background"} {
		if states[state] != float64(1) {
			t.Fatalf("accounting_states[%s] = %v, want 1 (all: %v)", state, states[state], states)
		}
	}
	if len(states) != 4 || len(unavailable) != 1 || unavailable["serving_attempt_binding_mismatch|checkout_refresh_cycle"] != float64(1) {
		t.Fatalf("accounting_states = %v accounting_unavailable = %v", states, unavailable)
	}
}
