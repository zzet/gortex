package main

import (
	"strings"
	"testing"
)

// onlyAccountingShortfall leaves a record as the receipt loop does when its
// accounting is its only shortfall: complete with no error under a typed
// terminal state, otherwise failed with the not-terminal error alone.
func onlyAccountingShortfall(record *editProbeRecord) {
	if editProbeAccountingTerminal(record.Accounting) {
		record.Outcome, record.Errors = "complete", nil
		return
	}
	record.Outcome, record.Errors = "diagnostic_failure", []string{editProbeAccountingNotTerminalError(record.Accounting)}
}

// The receipt loop's exit and the record rule read one terminality predicate.
// Every accounting state LOAD assigns is listed with its verdict, so a state
// that drifts out of (or into) the terminal set fails here.
func TestEditProbeAccountingTerminalPredicateAgreesWithFinished(t *testing.T) {
	for state, terminal := range map[string]bool{
		"observed_single_window_checks_pass":        true,
		"observed_shared_owner_windows_checks_pass": true,
		"terminal_unavailable_background":           true,
		"terminal_partial_physical":                 true,
		"terminal_unavailable_shared_owner":         true,
		"unavailable":                               false,
		"unavailable_shared_owner":                  false,
		"not_applicable_direct_move":                false,
		"not_applicable_native_write":               false,
		"":                                          false,
	} {
		a := editProbeAccounting{State: state, Reasons: []string{"reason one", "reason two"}}
		if got := editProbeAccountingTerminal(a); got != terminal {
			t.Errorf("editProbeAccountingTerminal(%q) = %v, want %v", state, got, terminal)
		}
		if got := editProbeFinished(true, true, true, a); got != terminal {
			t.Errorf("editProbeFinished(required, %q) = %v, want %v", state, got, terminal)
		}
		if !editProbeFinished(true, true, false, a) || editProbeFinished(false, true, true, a) || editProbeFinished(true, false, true, a) {
			t.Errorf("state %q bypassed the role, graph or oracle conjunct", state)
		}
		message, failed := editProbeAccountingRecordError(a)
		if failed == terminal || failed && message != "A/P/B accounting not terminal: "+strings.Join(a.Reasons, "; ") {
			t.Errorf("record rule for %q = %q, %v; want an error only when not terminal", state, message, failed)
		}
	}
}
