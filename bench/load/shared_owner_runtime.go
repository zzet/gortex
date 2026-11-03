package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

func editProbeHasSharedOwnerFields(status map[string]any) bool {
	for name := range status {
		if strings.HasPrefix(name, "shared_owner_accounting_v") {
			return true
		}
	}
	return false
}

// accountingForStatus is the runtime boundary at which an actual owned child
// can be associated with a reply. Pure wire decoding cannot supply that bond.
func (r *editProbeRuntime) accountingForStatus(ctx context.Context, status map[string]any, receipt, tool string) editProbeAccounting {
	if !editProbeHasSharedOwnerFields(status) {
		return checkEditProbeAccountingForTool(status, receipt, tool)
	}
	if tool == "edit.symbol" && status["tool"] != "edit_symbol" {
		return editProbeAccounting{State: "unavailable_shared_owner", Reasons: []string{"status tool does not match offered edit.symbol"}}
	}
	out := checkEditProbeAcknowledgement(status, receipt, "reindex_generation", nil, tool == "edit.symbol")
	out.State = "unavailable_shared_owner"
	ref, credible, err := r.referenceForSharedStatus(ctx, status, receipt, "")
	if err != nil {
		out.Reasons = append(out.Reasons, err.Error())
		if credible {
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

// Both physical point-status and exact move-child callers use this same
// current-domain/member validator. The report dictionary is never authority.
func (r *editProbeRuntime) referenceForSharedStatus(ctx context.Context, status map[string]any, receipt, originalMovePath string) (*editProbeSharedReference, bool, error) {
	if _, ok := status["shared_owner_accounting_v1"].(map[string]any); !ok {
		return nil, false, errors.New("complete typed shared owner proof unavailable")
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
			return nil, false, err
		}
		r.sharedBinding = binding
	}
	proof, proofErr := checkEditProbeSharedProof(status, receipt, r.sharedBinding, r.p, role)
	if originalMovePath != "" && proof.member.Path != "" {
		relative, err := filepath.Rel(r.sharedBinding.root, proof.member.Path)
		want := editProbeMovePath{Relative: filepath.ToSlash(relative), Qualified: r.sharedBinding.prefix + "/" + filepath.ToSlash(relative), Absolute: proof.member.Path}
		if err != nil || !movePathMatches(originalMovePath, want) {
			return nil, proof.ownerKey != "", errors.New("move original path does not match its current physical member")
		}
	}
	ref, err := r.sharedReport.observe(proof, proofErr)
	return ref, proof.ownerKey != "", err
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

func invalidateSharedAccounting(record *editProbeRecord, report *editProbeSharedOwnerReport, required bool) {
	if record.MoveAccounting != nil {
		for _, file := range record.MoveAccounting.Files {
			if file.Shared == nil {
				continue
			}
			if err := report.definitionError(file.Shared); err != nil {
				record.MoveAccounting.State = "unavailable_move_shared_owner"
				record.MoveAccounting.Reasons = append(record.MoveAccounting.Reasons, err.Error())
				if required {
					record.Errors = append(record.Errors, "move shared owner accounting invalidated: "+err.Error())
					if record.Outcome == "complete" {
						record.Outcome = "diagnostic_failure"
					}
				}
			}
		}
	}
	if record.Accounting.Shared == nil {
		return
	}
	if err := report.definitionError(record.Accounting.Shared); err != nil {
		record.Accounting.State = "terminal_unavailable_shared_owner"
		record.Accounting.Reasons = append(record.Accounting.Reasons, err.Error())
		if required {
			record.Errors = append(record.Errors, "shared owner accounting invalidated: "+err.Error())
			if record.Outcome == "complete" {
				record.Outcome = "diagnostic_failure"
			}
		}
	}
}
