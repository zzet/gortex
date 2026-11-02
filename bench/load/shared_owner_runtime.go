package main

import (
	"context"
	"strings"
)

// accountingForStatus is the runtime boundary at which an actual owned child
// can be associated with a reply. Pure wire decoding cannot supply that bond.
func (r *editProbeRuntime) accountingForStatus(ctx context.Context, status map[string]any, receipt, tool string) editProbeAccounting {
	shared := false
	for name := range status {
		shared = shared || strings.HasPrefix(name, "shared_owner_accounting_v")
	}
	if !shared {
		return checkEditProbeAccountingForTool(status, receipt, tool)
	}
	if tool == "edit.symbol" && status["tool"] != "edit_symbol" {
		return editProbeAccounting{State: "unavailable_shared_owner", Reasons: []string{"status tool does not match offered edit.symbol"}}
	}
	out := checkEditProbeAcknowledgement(status, receipt, "reindex_generation", nil, tool == "edit.symbol")
	out.State = "unavailable_shared_owner"
	if _, ok := status["shared_owner_accounting_v1"].(map[string]any); !ok {
		out.Reasons = append(out.Reasons, "complete typed shared owner proof unavailable")
		return out
	}
	role := ""
	if r.phase != nil && r.phase.EditProbe != nil {
		role = r.phase.EditProbe.Role
	}
	r.sharedMu.Lock()
	defer r.sharedMu.Unlock()
	// A new actual owned child may establish its own binding. A raw old reply
	// still has to match that child's PID/birth and full current membership.
	if r.sharedBinding == nil || r.p != nil && r.p.process != r.sharedBinding.command && r.p.startCount > r.sharedBinding.startCount {
		binding, err := captureEditProbeSharedBinding(ctx, r.p, role)
		if err != nil {
			out.Reasons = append(out.Reasons, err.Error())
			return out
		}
		r.sharedBinding = binding
	}
	proof, proofErr := checkEditProbeSharedProof(status, receipt, r.sharedBinding, r.p, role)
	ref, err := r.sharedReport.observe(proof, proofErr)
	if err != nil {
		out.Reasons = append(out.Reasons, err.Error())
		if proof.ownerKey != "" {
			out.State = "terminal_unavailable_shared_owner"
		}
		return out
	}
	out.Shared = ref
	if len(out.Reasons) == 0 && out.A != nil {
		out.State = "observed_shared_owner_windows_checks_pass"
	}
	return out
}

func (r *editProbeRuntime) finishSharedOwners() *editProbeSharedOwnerReport {
	r.sharedMu.Lock()
	defer r.sharedMu.Unlock()
	if len(r.sharedReport.Definitions) == 0 {
		return nil
	}
	// All probe writers have joined before finish. This moves report data into
	// the final report; neither it nor a dictionary lookup proves a new reply.
	r.sharedReport.summarize()
	return &r.sharedReport
}

func invalidateSharedAccounting(record *editProbeRecord, report *editProbeSharedOwnerReport) {
	if record.Accounting.Shared == nil {
		return
	}
	if err := report.definitionError(record.Accounting.Shared); err != nil {
		record.Accounting.State = "terminal_unavailable_shared_owner"
		record.Accounting.Reasons = append(record.Accounting.Reasons, err.Error())
		record.Errors = append(record.Errors, "shared owner accounting invalidated: "+err.Error())
		if record.Outcome == "complete" {
			record.Outcome = "diagnostic_failure"
		}
	}
}
