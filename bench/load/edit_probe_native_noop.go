package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zzet/gortex/bench/receiptproof"
)

func nativeNoOpFunctionalStatus(status map[string]any, raw json.RawMessage, wireID int64, a editProbeAccounting) *receiptproof.Coalesced {
	if status["graph_status"] != "fresh" || status["graph_status_terminal"] != true || status["read_assisted_publication"] != nil || a.A == nil || a.P == nil || a.R != nil || a.Shared != nil || a.Requested == 0 || a.Applied < a.Requested || a.State != "unavailable" && a.State != "terminal_unavailable_background" {
		return nil
	}
	text := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	number := func(m map[string]any, k string) uint64 {
		v, err := strconvEditProbeUint(m[k])
		if err != nil {
			return ^uint64(0)
		}
		return v
	}
	pub, _ := status["publication_phases"].(map[string]any)
	timing, _ := status["phase_timing"].(map[string]any)
	physical, wp := timing, pub
	scope := "requested_physical_noop"
	coverage := "not_established"
	if a.Applied > a.Requested {
		if timing["patch_wall_ms"] != nil || timing["patch_accounting"] != nil {
			return nil
		}
		winner, _ := status["coalesced_publication"].(map[string]any)
		if number(winner, "requested_generation") != a.Requested || number(winner, "applied_generation") != a.Applied {
			return nil
		}
		wp, _ = winner["publication_phases"].(map[string]any)
		wt, _ := winner["timings"].(map[string]any)
		physical, _ = wt["phase_timing"].(map[string]any)
		scope, coverage = text(winner, "scope"), text(winner, "background_coverage")
	} else if status["coalesced_publication"] != nil {
		return nil
	}
	if !editProbeObservedDuplicatePhysical(physical, wp, a.Applied) {
		return nil
	}
	pa, _ := physical["patch_accounting"].(map[string]any)
	phases, _ := pa["phases_ms"].(map[string]any)
	states, _ := pa["phase_states"].(map[string]any)
	noop := &receiptproof.NativeNoOp{ExecutionDisposition: text(pa, "execution_disposition"), SumState: text(pa, "sum_state"), PhasesNS: make(map[string]*int64), PhaseStates: make(map[string]string)}
	for name, v := range phases {
		noop.PhasesNS[name] = nil
		if v != nil {
			ns, err := editProbeNS(v)
			if err != nil {
				return nil
			}
			noop.PhasesNS[name] = &ns
		}
	}
	for name, v := range states {
		s, ok := v.(string)
		if !ok {
			return nil
		}
		noop.PhaseStates[name] = s
	}
	ack, _ := status["acknowledgement"].(map[string]any)
	bg, _ := status["requested_background_unavailable"].(map[string]any)
	if a.B == nil {
		_, owned := editProbeOwnedBackgroundRefusal(status, text(status, "receipt"), a.Requested)
		if !owned || status["requested_background"] != nil || bg["scope"] != "requested_background_observation" || bg["publication_key"] != pub["key"] || number(bg, "requested_generation") != a.Requested {
			return nil
		}
	} else {
		if status["requested_background_unavailable"] != nil {
			return nil
		}
		b, _ := status["requested_background"].(map[string]any)
		bp, _ := b["phases_ms"].(map[string]any)
		if number(b, "requested_generation") != a.Requested || number(b, "applied_generation") != a.Applied || !editProbeSupportedBackgroundCoverage(b) {
			return nil
		}
		for name, value := range bp {
			switch name {
			case "gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "patch_analysis", "executor_control":
				if value != nil {
					return nil
				}
			case "attempt_setup", "patch_inspection", "attempt_finalize", "cohort_admission", "lane_wait", "batch_admission", "scheduling_admission_setup", "initial_timer_arm", "canceled_quiet_wait", "timer_stop", "coalescing_control", "successor_timer_arm", "successor_quiet_wait", "requested_quiet_wait":
			default:
				return nil
			}
		}
		for _, reason := range a.Reasons {
			if strings.HasPrefix(reason, "B:") || strings.HasPrefix(reason, "B/P ") || strings.HasPrefix(reason, "missing B ") || strings.HasPrefix(reason, "requested B ") {
				return nil
			}
		}
		control := receiptproof.Window(*a.B)
		noop.BackgroundControl = &control
	}
	p := &receiptproof.Coalesced{NativeNoOp: noop, Status: receiptproof.Status{Receipt: text(status, "receipt"), MutationID: text(status, "mutation_id"), RPCID: text(ack, "rpc_id"), Requested: a.Requested, Applied: a.Applied, Key: text(pub, "key"), Ticket: number(pub, "ticket"), Source: text(pub, "source"), Clock: text(pub, "clock"), Terminal: pub["terminal"] == true, PUnavailable: a.Applied > a.Requested, BUnavailable: a.B == nil, CoalescedAbsent: a.Applied == a.Requested, Refusal: text(bg, "reason"), BackgroundRequest: number(bg, "requested_generation"), BackgroundLast: number(bg, "last_observed_generation"), BindingPredicate: text(bg, "binding_predicate"), BackgroundStage: text(bg, "background_stage"), AcceptedStage: text(bg, "accepted_attempt_stage"), WireID: wireID, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}, Winner: receiptproof.CoalescedWinner{Key: text(wp, "key"), Ticket: number(wp, "ticket"), Source: text(wp, "source"), Clock: text(wp, "clock"), Terminal: wp["terminal"] == true, Requested: a.Requested, Applied: a.Applied, Scope: scope, BackgroundCoverage: coverage, AttemptTicket: number(pa, "attempt_ticket"), AttemptRetry: number(pa, "attempt_retry"), PartitionComplete: pa["observed_partition_complete"] == true, PhysicalScope: text(pa, "scope")}, ServingPhysical: receiptproof.Window(*a.P), AccountingError: "A/P/B accounting unavailable: " + strings.Join(a.Reasons, "; ")}
	if a.B != nil {
		p.Status.BackgroundRequest, p.Status.BackgroundLast = 0, 0
	}
	if v, err := strconvEditProbeUint(bg["observed_native_handoffs"]); err == nil {
		p.Status.NativeHandoffs = &v
	}
	if !p.Matches(p.Status.Receipt, p.Status.MutationID, a.RPCID, a.Requested, a.Applied) {
		return nil
	}
	return p
}

func qualifiedFunctionalAccounting(a editProbeAccounting, p *receiptproof.Coalesced) bool {
	if p == nil || a.A == nil || !receiptproof.Window(*a.A).Complete() || a.P == nil || receiptproof.Window(*a.P) != p.ServingPhysical || a.R != nil || a.Shared != nil {
		return false
	}
	if p.NativeNoOp == nil {
		return a.State == "terminal_unavailable_background" && a.B == nil
	}
	if a.State != "unavailable" && a.State != "terminal_unavailable_background" {
		return false
	}
	if a.B == nil {
		return p.NativeNoOp.BackgroundControl == nil
	}
	return p.NativeNoOp.BackgroundControl != nil && receiptproof.Window(*a.B) == *p.NativeNoOp.BackgroundControl
}

// A positive functional candidate is ready only with its required tier evidence.
// In particular, a no-op's terminal control window cannot end a pending->fresh
// observation sequence before the functional predicates actually hold.
func editProbeFinishedWithQualification(terminal, all, required bool, a editProbeAccounting, p *receiptproof.Coalesced, tiers *editProbeLiveTiers) bool {
	if p != nil {
		return terminal && all && coalescedFreshTiers(tiers)
	}
	return editProbeFinished(terminal, all, required, a)
}
