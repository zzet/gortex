package receiptproof

import (
	"strconv"
	"strings"
)

// CoalescedFunctionalOutcome proves functional success, never supersession or
// the requested edit's indexing window. The accepted successor stays separate.
const CoalescedFunctionalOutcome = "qualified_coalesced_functional"

type Coalesced struct {
	Status          Status          `json:"status"`
	Winner          CoalescedWinner `json:"winner"`
	ServingPhysical Window          `json:"serving_physical"`
	AccountingError string          `json:"original_accounting_error"`
}

type CoalescedWinner struct {
	Key                string `json:"key"`
	Ticket             uint64 `json:"ticket"`
	Source             string `json:"source"`
	Clock              string `json:"clock"`
	Terminal           bool   `json:"terminal"`
	Requested          uint64 `json:"requested_generation"`
	Applied            uint64 `json:"applied_generation"`
	Scope              string `json:"scope"`
	BackgroundCoverage string `json:"background_coverage"`
	AttemptTicket      uint64 `json:"attempt_ticket"`
	AttemptRetry       uint64 `json:"attempt_retry"`
	PartitionComplete  bool   `json:"observed_partition_complete"`
	PhysicalScope      string `json:"physical_scope"`
}

type Window struct {
	WallNS            int64 `json:"wall_ns"`
	SumNS             int64 `json:"sum_ns"`
	ResidualNS        int64 `json:"residual_ns"`
	WithinFivePercent bool  `json:"within_five_percent"`
}

func (w Window) Complete() bool {
	return w.WithinFivePercent && w.WallNS >= 0 && w.SumNS >= 0 && w.SumNS <= w.WallNS &&
		w.ResidualNS == w.WallNS-w.SumNS && w.ResidualNS <= w.WallNS/20
}

func (p *Coalesced) Matches(receipt, mutationID, rpcID string, requested, applied uint64) bool {
	if p == nil || receipt == "" || mutationID == "" || rpcID == "" || requested == 0 || applied <= requested {
		return false
	}
	s, w := p.Status, p.Winner
	return s.Receipt == receipt && s.MutationID == mutationID && s.RPCID == rpcID &&
		s.Requested == requested && s.Applied == applied && s.Key != "" && s.Ticket == requested &&
		s.Source == "mcp_edit" && s.Clock == "monotonic" && s.Terminal && s.PUnavailable && s.BUnavailable && !s.CoalescedAbsent &&
		s.Refusal == "serving_attempt_binding_mismatch" && s.BindingPredicate == "direct_physical_window_missing" &&
		s.BackgroundStage == "returned" && s.AcceptedStage == "no_accepted_witness" &&
		s.BackgroundRequest == requested && s.BackgroundLast == requested && s.NativeHandoffs != nil && *s.NativeHandoffs == 0 &&
		s.WireID > 0 && digest(s.SHA256) &&
		w.Key == s.Key+"/physical-"+strconv.FormatUint(applied, 10)+"-retry-0" && w.Ticket == applied &&
		w.Source == "canonical_coalesced_attempt" && w.Clock == "monotonic" && w.Terminal &&
		w.Requested == requested && w.Applied == applied && w.Scope == "independent_physical_winner_serving_coalesced_receipt" &&
		w.BackgroundCoverage == "not_established" && w.AttemptTicket == applied && w.AttemptRetry == 0 &&
		w.PartitionComplete && w.PhysicalScope == "accepted_winning_physical_attempt" && p.ServingPhysical.Complete() && coalescedTimingOnlyError(p.AccountingError)
}

func coalescedTimingOnlyError(value string) bool {
	reasons, ok := strings.CutPrefix(value, "A/P/B accounting unavailable: ")
	if !ok || reasons == "" {
		return false
	}
	for _, reason := range strings.Split(reasons, "; ") {
		switch {
		case reason == "requested B unavailable for actual topology/identity", reason == "requested B observed unavailable: serving_attempt_binding_mismatch", reason == "B: missing duration":
		case strings.HasPrefix(reason, "missing B phase "):
			switch strings.TrimPrefix(reason, "missing B phase ") {
			case "scheduling_admission_setup", "initial_timer_arm", "canceled_quiet_wait", "timer_stop", "coalescing_control", "successor_timer_arm", "successor_quiet_wait":
			default:
				return false
			}
		case strings.HasPrefix(reason, "B/P phase mismatch: "):
			switch strings.TrimPrefix(reason, "B/P phase mismatch: ") {
			case "apply", "attempt_finalize", "attempt_setup", "batch_admission", "cohort_admission", "derived", "executor_control", "gate_wait", "lane_wait", "mtimes", "patch_analysis", "patch_inspection", "patch_parse", "resolve", "search_lap":
			default:
				return false
			}
		default:
			return false
		}
	}
	return true
}
