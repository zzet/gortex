package receiptproof

import (
	"encoding/json"
	"strings"
	"testing"
)

func supersededFixture() *Superseded {
	no, retry, handoffs := false, 0, uint64(0)
	return &Superseded{
		Status:          Status{Receipt: "commit-a", MutationID: "edit-a", RPCID: "rpc-2", Requested: 3, Applied: 3, Key: "mutation-a", Ticket: 3, Source: "mcp_edit", Clock: "monotonic", Terminal: true, PUnavailable: true, BUnavailable: true, CoalescedAbsent: true, Refusal: "serving_attempt_binding_mismatch", BindingPredicate: "direct_physical_window_missing", BackgroundStage: "returned", AcceptedStage: "no_accepted_witness", BackgroundRequest: 3, BackgroundLast: 3, NativeHandoffs: &handoffs, WireID: 34, SHA256: strings.Repeat("a", 64)},
		Guard:           Guard{Message: "graph patch attempt timing unavailable", Scope: "physical_attempt_timing_unavailable", Reason: "accepted_stamps_mismatch", Repo: "repo", Observed: true, Recorder: strings.Repeat("b", 64), Key: "mutation-a", Ticket: 3, CollectorTicket: 3, AcceptedStamps: "absent", WindowPresent: &no, Terminal: &no, PatchPresent: &no, Retry: &retry, Completion: true, Disposition: "covered_without_acceptance", Fallback: true, Applied: 3, Pending: 4, Count: 1, SourceLog: "daemon-1.stderr.log", Line: 17, SHA256: strings.Repeat("c", 64)},
		AccountingError: "A/P/B accounting unavailable: applicable physical P unavailable; requested B observed unavailable: serving_attempt_binding_mismatch",
	}
}

func TestSupersededProofRequiresExactPositiveStatusAndGuard(t *testing.T) {
	if !supersededFixture().Matches("commit-a", "edit-a", "rpc-2", 3, 3) {
		t.Fatal("exact observed covered receipt must qualify without inventing P/B")
	}
}

func TestSupersededProofKeepsMissingForeignAndAcceptedAttemptsStrict(t *testing.T) {
	changes := map[string]func(*Superseded){
		"receipt":              func(p *Superseded) { p.Status.Receipt = "foreign" },
		"mutation":             func(p *Superseded) { p.Status.MutationID = "foreign" },
		"rpc":                  func(p *Superseded) { p.Status.RPCID = "foreign" },
		"key":                  func(p *Superseded) { p.Guard.Key = "foreign" },
		"ticket":               func(p *Superseded) { p.Guard.Ticket = 4 },
		"collector":            func(p *Superseded) { p.Guard.CollectorTicket = 4 },
		"applied":              func(p *Superseded) { p.Status.Applied = 4 },
		"not_superseded":       func(p *Superseded) { p.Guard.Pending = 3 },
		"no_completion":        func(p *Superseded) { p.Guard.Count = 0 },
		"unobserved":           func(p *Superseded) { p.Guard.Observed = false },
		"missing_reason":       func(p *Superseded) { p.Guard.Reason = "" },
		"foreign_reason":       func(p *Superseded) { p.Guard.Reason = "recorder_membership_mismatch" },
		"missing_handoffs":     func(p *Superseded) { p.Status.NativeHandoffs = nil },
		"native_handoff":       func(p *Superseded) { one := uint64(1); p.Status.NativeHandoffs = &one },
		"selected_only":        func(p *Superseded) { p.Guard.Disposition = "selected_for_absorb" },
		"not_fallback":         func(p *Superseded) { p.Guard.Fallback = false },
		"accepted":             func(p *Superseded) { p.Guard.AcceptedStamps = "same" },
		"missing_window_flag":  func(p *Superseded) { p.Guard.WindowPresent = nil },
		"prior_window":         func(p *Superseded) { yes := true; p.Guard.WindowPresent = &yes },
		"terminal":             func(p *Superseded) { yes := true; p.Guard.Terminal = &yes },
		"patch":                func(p *Superseded) { yes := true; p.Guard.PatchPresent = &yes },
		"retry":                func(p *Superseded) { retry := 1; p.Guard.Retry = &retry },
		"physical_present":     func(p *Superseded) { p.Status.PUnavailable = false },
		"coalesced_present":    func(p *Superseded) { p.Status.CoalescedAbsent = false },
		"unknown_refusal":      func(p *Superseded) { p.Status.Refusal = "unknown" },
		"missing_status_hash":  func(p *Superseded) { p.Status.SHA256 = "" },
		"missing_guard_hash":   func(p *Superseded) { p.Guard.SHA256 = "" },
		"ack_validation_error": func(p *Superseded) { p.AccountingError += "; missing A phase disk_write" },
		"no_original_error":    func(p *Superseded) { p.AccountingError = "" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(supersededFixture())
			if err != nil {
				t.Fatal(err)
			}
			var p Superseded
			if err := json.Unmarshal(data, &p); err != nil {
				t.Fatal(err)
			}
			change(&p)
			if p.Matches("commit-a", "edit-a", "rpc-2", 3, 3) {
				t.Fatal("missing, foreign or accepted evidence qualified")
			}
		})
	}
}
