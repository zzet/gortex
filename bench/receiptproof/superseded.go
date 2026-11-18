// Package receiptproof defines the one explicit superseded-receipt evidence
// shared by the edit producer and its budget checker. It has no I/O authority.
package receiptproof

import (
	"encoding/hex"
	"path/filepath"
	"strings"
)

const SupersededOutcome = "proven_superseded"

type Superseded struct {
	Status          Status `json:"status"`
	Guard           Guard  `json:"guard"`
	AccountingError string `json:"original_accounting_error"`
}

type Status struct {
	Receipt           string  `json:"receipt"`
	MutationID        string  `json:"mutation_id"`
	RPCID             string  `json:"acknowledgement_rpc_id"`
	Requested         uint64  `json:"requested_generation"`
	Applied           uint64  `json:"applied_generation"`
	Key               string  `json:"publication_key"`
	Ticket            uint64  `json:"publication_ticket"`
	Source            string  `json:"publication_source"`
	Clock             string  `json:"publication_clock"`
	Terminal          bool    `json:"publication_terminal"`
	PUnavailable      bool    `json:"physical_unavailable_observed"`
	BUnavailable      bool    `json:"background_unavailable_observed"`
	CoalescedAbsent   bool    `json:"coalesced_absent_observed"`
	Refusal           string  `json:"background_refusal"`
	BindingPredicate  string  `json:"binding_predicate"`
	BackgroundStage   string  `json:"background_stage"`
	AcceptedStage     string  `json:"accepted_attempt_stage"`
	BackgroundRequest uint64  `json:"background_requested_generation"`
	BackgroundLast    uint64  `json:"background_last_observed_generation"`
	NativeHandoffs    *uint64 `json:"observed_native_handoffs"`
	WireID            int64   `json:"status_wire_id"`
	SHA256            string  `json:"status_sha256"`
}

type Guard struct {
	Message         string `json:"msg"`
	Scope           string `json:"patch_lap_scope"`
	Reason          string `json:"attempt_rejection_reason"`
	Repo            string `json:"repo"`
	Observed        bool   `json:"attempt_guard_observed"`
	Recorder        string `json:"attempt_guard_recorder"`
	Key             string `json:"attempt_guard_publication_key"`
	Ticket          uint64 `json:"attempt_guard_ticket"`
	CollectorTicket uint64 `json:"attempt_guard_collector_ticket"`
	AcceptedStamps  string `json:"attempt_guard_accepted_stamps"`
	WindowPresent   *bool  `json:"attempt_guard_window_present"`
	Terminal        *bool  `json:"attempt_guard_terminal"`
	PatchPresent    *bool  `json:"attempt_guard_patch_present"`
	Retry           *int   `json:"attempt_retry"`
	Completion      bool   `json:"attempt_completion_observed"`
	Disposition     string `json:"attempt_completion_disposition"`
	Fallback        bool   `json:"attempt_completion_fallback"`
	Applied         uint64 `json:"attempt_completion_applied"`
	Pending         uint64 `json:"attempt_completion_pending_at_check"`
	Count           int    `json:"attempt_completion_count"`
	SourceLog       string `json:"source_log"`
	Line            int    `json:"line_number"`
	SHA256          string `json:"line_sha256"`
}

// Matches requires the original receipt status and one frozen covered-completion
// guard. It never promotes missing timing values to measured work.
func (p *Superseded) Matches(receipt, mutationID, rpcID string, requested, applied uint64) bool {
	if p == nil || receipt == "" || mutationID == "" || rpcID == "" || requested == 0 || applied != requested {
		return false
	}
	s, g := p.Status, p.Guard
	return s.Receipt == receipt && s.MutationID == mutationID && s.RPCID == rpcID &&
		s.Requested == requested && s.Applied == applied && s.Key != "" && s.Ticket == requested &&
		s.Source == "mcp_edit" && s.Clock == "monotonic" && s.Terminal && s.PUnavailable && s.BUnavailable && s.CoalescedAbsent &&
		s.Refusal == "serving_attempt_binding_mismatch" && s.BindingPredicate == "direct_physical_window_missing" &&
		s.BackgroundStage == "returned" && s.AcceptedStage == "no_accepted_witness" &&
		s.BackgroundRequest == requested && s.BackgroundLast == applied && s.NativeHandoffs != nil && *s.NativeHandoffs == 0 &&
		s.WireID > 0 && digest(s.SHA256) &&
		g.Message == "graph patch attempt timing unavailable" && g.Scope == "physical_attempt_timing_unavailable" &&
		g.Reason == "accepted_stamps_mismatch" && g.Repo != "" && g.Observed && digest(g.Recorder) &&
		g.Key == s.Key && g.Ticket == requested && g.CollectorTicket == requested && g.AcceptedStamps == "absent" &&
		observedFalse(g.WindowPresent) && observedFalse(g.Terminal) && observedFalse(g.PatchPresent) && g.Retry != nil && *g.Retry == 0 &&
		g.Completion && g.Disposition == "covered_without_acceptance" && g.Fallback && g.Applied == applied && g.Pending > applied && g.Count > 0 &&
		g.SourceLog == filepath.Base(g.SourceLog) && strings.HasPrefix(g.SourceLog, "daemon") && strings.HasSuffix(g.SourceLog, ".log") &&
		g.Line > 0 && digest(g.SHA256) && timingOnlyError(p.AccountingError)
}

func observedFalse(value *bool) bool { return value != nil && !*value }

func digest(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 32
}

// A valid A window alone is insufficient when its owner/phase validation failed.
// Only these unavailable P/B facts may leave the fatal error set.
func timingOnlyError(value string) bool {
	reasons, ok := strings.CutPrefix(value, "A/P/B accounting unavailable: ")
	if !ok || reasons == "" {
		return false
	}
	for _, reason := range strings.Split(reasons, "; ") {
		switch {
		case reason == "applicable physical P unavailable", reason == "P: missing duration", reason == "B: missing duration",
			reason == "requested B unavailable for actual topology/identity", reason == "requested B observed unavailable: serving_attempt_binding_mismatch":
		case strings.HasPrefix(reason, "missing applicable P phase "), strings.HasPrefix(reason, "missing B phase "):
		default:
			return false
		}
	}
	return true
}
