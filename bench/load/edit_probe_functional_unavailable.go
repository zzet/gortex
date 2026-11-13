package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zzet/gortex/bench/receiptproof"
)

// A status-only witness says the original timing owner is unavailable. It does
// not say indexing never executed, nor identify a winner or a no-op attempt.
func functionalUnavailableStatus(status map[string]any, raw json.RawMessage, wireID int64, a editProbeAccounting) *receiptproof.FunctionalUnavailable {
	pub, _ := status["publication_phases"].(map[string]any)
	timing, _ := status["phase_timing"].(map[string]any)
	bg, _ := status["requested_background_unavailable"].(map[string]any)
	ack, _ := status["acknowledgement"].(map[string]any)
	if status["graph_status"] != "fresh" || status["graph_status_terminal"] != true ||
		timing["patch_wall_ms"] != nil || timing["patch_accounting"] != nil || timing["shared_set_execution"] != nil || timing["resolve_mutex_wait"] != nil ||
		status["requested_background"] != nil || status["coalesced_publication"] != nil || status["read_assisted_publication"] != nil ||
		!functionalUnavailableAccounting(a) || bg["scope"] != "requested_background_observation" || bg["publication_key"] != pub["key"] {
		return nil
	}
	for _, key := range []string{"phase_timing", "phases_ms", "phase_states"} {
		if value := status[key]; value != nil {
			if _, ok := value.(map[string]any); !ok {
				return nil
			}
		}
	}
	for _, key := range []string{"binding_predicate", "background_stage", "accepted_attempt_stage"} {
		if value := bg[key]; value != nil {
			if _, ok := value.(string); !ok {
				return nil
			}
		}
	}
	for key, value := range timing {
		switch key {
		case "clock", "coverage", "ack_wall_ms", "publication_wall_ms", "publication_unattributed_ms":
		case "patch_wall_ms", "patch_accounting", "resolve_mutex_wait", "shared_set_execution":
			if value != nil {
				return nil
			}
		default:
			return nil
		}
	}
	// Even an incomplete observed physical phase or retained acceptance cannot
	// disappear merely because its complete accounting window is missing.
	phases, _ := status["phases_ms"].(map[string]any)
	states, _ := status["phase_states"].(map[string]any)
	for _, name := range receiptproof.OriginalPhysicalPhases() {
		if phases[name] != nil || states[name] != nil && states[name] != "not_observed" {
			return nil
		}
	}
	for name := range phases {
		if !knownFunctionalPhase(name) {
			return nil
		}
	}
	for name := range states {
		if !knownFunctionalPhase(name) {
			return nil
		}
	}
	text := func(m map[string]any, key string) string { value, _ := m[key].(string); return value }
	number := func(m map[string]any, key string) uint64 {
		v, err := strconvEditProbeUint(m[key])
		if err != nil {
			return ^uint64(0)
		}
		return v
	}
	handoffs := number(bg, "observed_native_handoffs")
	p := &receiptproof.FunctionalUnavailable{Status: receiptproof.Status{Receipt: text(status, "receipt"), MutationID: text(status, "mutation_id"), RPCID: text(ack, "rpc_id"), Requested: number(status, "reindex_generation"), Applied: number(status, "applied_generation"), Key: text(pub, "key"), Ticket: number(pub, "ticket"), Source: text(pub, "source"), Clock: text(pub, "clock"), Terminal: pub["terminal"] == true, PUnavailable: true, BUnavailable: true, CoalescedAbsent: true, Refusal: text(bg, "reason"), BindingPredicate: text(bg, "binding_predicate"), BackgroundStage: text(bg, "background_stage"), AcceptedStage: text(bg, "accepted_attempt_stage"), BackgroundRequest: number(bg, "requested_generation"), BackgroundLast: number(bg, "last_observed_generation"), NativeHandoffs: &handoffs, WireID: wireID, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}, PhysicalEvidenceAbsent: true, ReadAssistedAbsent: true, AccountingError: "A/P/B accounting unavailable: " + strings.Join(a.Reasons, "; ")}
	if !p.Matches(p.Status.Receipt, p.Status.MutationID, a.RPCID, a.Requested, a.Applied) {
		return nil
	}
	return p
}

func knownFunctionalPhase(name string) bool {
	for _, candidate := range editProbePhaseNames {
		if candidate == name {
			return true
		}
	}
	for _, candidate := range receiptproof.OriginalPhysicalPhases() {
		if candidate == name {
			return true
		}
	}
	return false
}

func functionalUnavailableAccounting(a editProbeAccounting) bool {
	return a.State == "terminal_unavailable_background" && a.A != nil && receiptproof.Window(*a.A).Complete() && a.P == nil && a.B == nil && a.R == nil && a.Shared == nil
}

func classifyFunctionalUnavailableRecord(record *editProbeRecord, scope *editProbePhysicalWriteScope) bool {
	if !sharedFunctionalRecord(record, scope) || record.functionalUnavailableStatus == nil || record.Superseded != nil || record.CoalescedFunctional != nil || !functionalUnavailableAccounting(record.Accounting) || len(record.Errors) != 1 || record.Errors[0] != record.functionalUnavailableStatus.AccountingError {
		return false
	}
	for _, name := range editProbePhaseNames[8:] {
		phase, present := record.PhasesMS[name]
		if !present || phase.Value != nil || phase.Unit != "ms" || !receiptproof.UnavailablePhaseReason(name, phase.Reason) {
			return false
		}
	}
	p := record.functionalUnavailableStatus
	if !p.Matches(record.Receipt, record.MutationID, record.Accounting.RPCID, record.Accounting.Requested, record.Accounting.Applied) {
		return false
	}
	copy := *p
	record.FunctionalUnavailable, record.Outcome, record.Errors = &copy, receiptproof.FunctionalUnavailableOutcome, nil
	return true
}
