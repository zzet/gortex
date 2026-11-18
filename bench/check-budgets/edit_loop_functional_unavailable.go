package main

import (
	"strings"

	"github.com/zzet/gortex/bench/receiptproof"
)

func functionalUnavailableEditLoopRecord(wire editLoopWire, index int) bool {
	r := wire.Edits[index]
	p := r.FunctionalUnavailable
	if !sharedFunctionalEditLoopRecord(wire, index) || r.CoalescedFunctional != nil || r.Accounting.State != "terminal_unavailable_background" ||
		!p.Matches(r.Receipt, r.MutationID, r.Accounting.RPCID, r.Accounting.Requested, r.Accounting.Applied) || p.AccountingError != "A/P/B accounting unavailable: "+strings.Join(r.Accounting.Reasons, "; ") {
		return false
	}
	for _, name := range receiptproof.OriginalPhysicalPhases()[:11] {
		phase, present := r.PhasesMS[name]
		if !present || phase.Value != nil || phase.Unit != "ms" || !receiptproof.UnavailablePhaseReason(name, phase.Reason) {
			return false
		}
	}
	for _, name := range receiptproof.OriginalPhysicalPhases()[11:] {
		if phase, present := r.PhasesMS[name]; present && (phase.Value != nil || phase.Unit != "ms" || !receiptproof.UnavailablePhaseReason(name, phase.Reason)) {
			return false
		}
	}
	for name := range r.PhasesMS {
		known := receiptproof.AcknowledgementPhase(name)
		for _, physical := range receiptproof.OriginalPhysicalPhases() {
			known = known || name == physical
		}
		if !known {
			return false
		}
	}
	return true
}
