package receiptproof

import "strings"

// FunctionalUnavailableOutcome certifies the shared edit's functional result.
// It makes no assertion about who executed indexing, coalescing or supersession.
const FunctionalUnavailableOutcome = "qualified_functional_unavailable_timings"

type FunctionalUnavailable struct {
	Status                 Status `json:"status"`
	PhysicalEvidenceAbsent bool   `json:"physical_evidence_absent_observed"`
	ReadAssistedAbsent     bool   `json:"read_assisted_absent_observed"`
	AccountingError        string `json:"original_accounting_error"`
}

func (p *FunctionalUnavailable) Matches(receipt, mutationID, rpcID string, requested, applied uint64) bool {
	if p == nil || receipt == "" || mutationID == "" || rpcID == "" || requested == 0 || applied != requested {
		return false
	}
	s := p.Status
	unavailable := s.Refusal == "serving_attempt_binding_mismatch" && s.BindingPredicate == "direct_physical_window_missing" && s.BackgroundStage == "returned" && s.AcceptedStage == "no_accepted_witness" ||
		s.Refusal == "preexisting_path_work" && s.BindingPredicate == "" && s.BackgroundStage == "" && s.AcceptedStage == ""
	return s.Receipt == receipt && s.MutationID == mutationID && s.RPCID == rpcID && s.Requested == requested && s.Applied == applied &&
		s.Key != "" && s.Ticket == requested && s.Source == "mcp_edit" && s.Clock == "monotonic" && s.Terminal &&
		s.PUnavailable && s.BUnavailable && s.CoalescedAbsent && p.PhysicalEvidenceAbsent && p.ReadAssistedAbsent && unavailable &&
		s.BackgroundRequest == requested && s.BackgroundLast == requested && s.NativeHandoffs != nil && *s.NativeHandoffs == 0 &&
		s.WireID > 0 && digest(s.SHA256) && functionalUnavailableTimingOnlyError(p.AccountingError, s.Refusal)
}

func functionalUnavailableTimingOnlyError(value, refusal string) bool {
	reasons, ok := strings.CutPrefix(value, "A/P/B accounting unavailable: ")
	if !ok || reasons == "" {
		return false
	}
	matchedRefusal := false
	for _, reason := range strings.Split(reasons, "; ") {
		matchedRefusal = matchedRefusal || reason == "requested B observed unavailable: "+refusal
		switch reason {
		case "applicable physical P unavailable", "P: missing duration":
			continue
		}
		if coalescedTimingOnlyError("A/P/B accounting unavailable: " + reason) {
			// B/P mismatches mean some original physical phase was observed.
			if strings.HasPrefix(reason, "B/P phase mismatch: ") {
				return false
			}
			continue
		}
		name, missing := strings.CutPrefix(reason, "missing applicable P phase ")
		if !missing {
			return false
		}
		switch name {
		case "gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "attempt_setup", "patch_inspection", "executor_control", "attempt_finalize", "cohort_admission", "lane_wait":
		default:
			return false
		}
	}
	return matchedRefusal
}

// OriginalPhysicalPhases includes optional control/deep fields too, so an
// incomplete retained execution cannot hide behind an absent whole window.
func OriginalPhysicalPhases() []string {
	return []string{"debounce", "cohort_admission", "batch_admission", "lane_wait", "gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "attempt_setup", "patch_inspection", "executor_control", "attempt_finalize", "patch_analysis"}
}

func AcknowledgementPhase(name string) bool {
	switch name {
	case "dispatcher_queue", "middleware", "parse", "disk_write", "publish", "reindex_wait", "edit_control", "caller_result":
		return true
	default:
		return false
	}
}

// UnavailablePhaseReason accepts only absence states emitted by the collector,
// never a measured producer state or a malformed numeric observation.
func UnavailablePhaseReason(name, reason string) bool {
	return reason == "producer phase state not_observed" || reason == "producer phase state unreported" ||
		reason == "status phases_ms omits "+name || reason == "status exposes no phases_ms"
}
