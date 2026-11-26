package main

import (
	"maps"
	"testing"
)

func TestEditProbeDeadlineReasonAndBusyAttribution(t *testing.T) {
	ready := editProbeReceiptWait{entered: true, observed: true, terminal: true, allCorrect: true, coverage: true}
	for _, tc := range []struct {
		name string
		wait editProbeReceiptWait
		want string
	}{
		{"spent_before_receipt_loop", editProbeReceiptWait{}, editProbeDeadlineReadbacks},
		{"no_status_reply", editProbeReceiptWait{entered: true}, editProbeDeadlineUnobserved},
		{"graph_pending", editProbeReceiptWait{entered: true, observed: true, allCorrect: true, coverage: true}, editProbeDeadlineGraph},
		{"oracle_missing", editProbeReceiptWait{entered: true, observed: true, terminal: true, coverage: true}, editProbeDeadlineOracles},
		{"witness_missing", editProbeReceiptWait{entered: true, observed: true, terminal: true, allCorrect: true}, editProbeDeadlineCoverage},
		{"qualified_stale_tiers", editProbeReceiptWait{entered: true, observed: true, terminal: true, allCorrect: true, coverage: true, qualified: true}, editProbeDeadlineTiers},
		{"accounting_only", ready, editProbeDeadlineAccounting},
	} {
		if got := editProbeDeadlineReason(tc.wait); got != tc.want {
			t.Errorf("%s: reason=%q, want %q", tc.name, got, tc.want)
		}
	}

	attempts := []Attempt{{Code: -32002, Outcome: "busy"}, {Code: -32002, Outcome: "busy"}, {Code: -32000, Outcome: "error"}, {Outcome: "success"}}
	if got := editProbeBusyRefusals(attempts); got != 2 {
		t.Fatalf("busy refusals=%d, want 2", got)
	}
	s := &editProbeState{}
	s.countBusy("oracle:callers", attempts)
	s.countBusy("oracle:callers", attempts[2:])
	s.countBusy("status", attempts[:1])
	if want := map[string]int{"oracle:callers": 2, "status": 1}; !maps.Equal(s.record.BusyRefusals, want) {
		t.Fatalf("per-purpose busy=%v, want %v", s.record.BusyRefusals, want)
	}

	// Only records whose final outcome is a product deadline count a reason;
	// the outcome counts themselves are unchanged.
	records := []editProbeRecord{
		{Outcome: "product_deadline", DeadlineReason: editProbeDeadlineAccounting, BusyRefusals: map[string]int{"oracle:callers": 3}},
		{Outcome: "product_deadline", DeadlineReason: editProbeDeadlineGraph},
		{Outcome: "harness_evidence_incomplete", DeadlineReason: editProbeDeadlineAccounting, BusyRefusals: map[string]int{"oracle:callers": 1, "status": 2}},
		{Outcome: "complete"},
	}
	summary := summarizeEditProbe(records, 42)
	if want := map[string]int{"product_deadline": 2, "harness_evidence_incomplete": 1, "complete": 1}; !maps.Equal(summary.Outcomes, want) {
		t.Fatalf("outcomes=%v, want %v", summary.Outcomes, want)
	}
	if want := map[string]int{editProbeDeadlineAccounting: 1, editProbeDeadlineGraph: 1}; !maps.Equal(summary.DeadlineReasons, want) {
		t.Fatalf("deadline reasons=%v, want %v", summary.DeadlineReasons, want)
	}
	if want := map[string]int{"oracle:callers": 4, "status": 2}; !maps.Equal(summary.BusyRefusals, want) {
		t.Fatalf("busy refusals=%v, want %v", summary.BusyRefusals, want)
	}
	if empty := summarizeEditProbe(records[3:], 42); empty.DeadlineReasons != nil || empty.BusyRefusals != nil {
		t.Fatalf("absent attribution became an empty measurement: %+v", empty)
	}
}
