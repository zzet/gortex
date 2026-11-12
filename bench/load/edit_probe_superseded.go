package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/bench/receiptproof"
)

// Only a latest, positively observed terminal status supplies this candidate.
// A later missing, malformed, foreign or accepted status discards it.
func supersededStatus(status map[string]any, raw json.RawMessage, wireID int64) *receiptproof.Status {
	publication, _ := status["publication_phases"].(map[string]any)
	timing, _ := status["phase_timing"].(map[string]any)
	background, _ := status["requested_background_unavailable"].(map[string]any)
	ack, _ := status["acknowledgement"].(map[string]any)
	requested, reqErr := strconvEditProbeUint(status["reindex_generation"])
	applied, appliedErr := strconvEditProbeUint(status["applied_generation"])
	ticket, ticketErr := strconvEditProbeUint(publication["ticket"])
	bgRequested, bgErr := strconvEditProbeUint(background["requested_generation"])
	last, lastErr := strconvEditProbeUint(background["last_observed_generation"])
	handoffs, handoffErr := strconvEditProbeUint(background["observed_native_handoffs"])
	_, pObserved := timing["patch_wall_ms"]
	if reqErr != nil || appliedErr != nil || ticketErr != nil || bgErr != nil || lastErr != nil || handoffErr != nil ||
		requested == 0 || applied != requested || status["graph_status"] != "fresh" || status["graph_status_terminal"] != true ||
		publication["source"] != "mcp_edit" || publication["clock"] != "monotonic" || publication["terminal"] != true || ticket != requested ||
		!pObserved || timing["patch_wall_ms"] != nil || timing["patch_accounting"] != nil || status["requested_background"] != nil ||
		status["coalesced_publication"] != nil || status["read_assisted_publication"] != nil ||
		background["scope"] != "requested_background_observation" || background["publication_key"] != publication["key"] ||
		background["reason"] != "serving_attempt_binding_mismatch" || background["binding_predicate"] != "direct_physical_window_missing" ||
		background["background_stage"] != "returned" || background["accepted_attempt_stage"] != "no_accepted_witness" ||
		bgRequested != requested || last != applied || handoffs != 0 || wireID <= 0 {
		return nil
	}
	receipt, _ := status["receipt"].(string)
	mutation, _ := status["mutation_id"].(string)
	rpc, _ := ack["rpc_id"].(string)
	key, _ := publication["key"].(string)
	if receipt == "" || mutation == "" || rpc == "" || key == "" {
		return nil
	}
	return &receiptproof.Status{Receipt: receipt, MutationID: mutation, RPCID: rpc, Requested: requested, Applied: applied, Key: key, Ticket: ticket,
		Source: "mcp_edit", Clock: "monotonic", Terminal: true, PUnavailable: true, BUnavailable: true, CoalescedAbsent: true,
		Refusal: "serving_attempt_binding_mismatch", BindingPredicate: "direct_physical_window_missing", BackgroundStage: "returned", AcceptedStage: "no_accepted_witness",
		BackgroundRequest: bgRequested, BackgroundLast: last, NativeHandoffs: &handoffs, WireID: wireID, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}
}

func classifySupersededRecord(record *editProbeRecord, scope *editProbePhysicalWriteScope, proof *receiptproof.Superseded) bool {
	// As for normal completion, earlier probe errors and pre-ACK mismatches
	// remain observations; final readback/oracle failures and false-fresh
	// content after ACK remain fatal below.
	if record == nil || proof == nil || scope == nil || scope.Kind != "shared_worktree_file" || scope.File == "" || scope.Editors < 2 ||
		record.Outcome != "diagnostic_failure" || len(record.Errors) != 1 ||
		record.Errors[0] != "A/P/B accounting unavailable: "+strings.Join(record.Accounting.Reasons, "; ") ||
		record.Accounting.State != "terminal_unavailable_background" || record.Accounting.A == nil || !record.Accounting.A.WithinFivePercent ||
		record.Accounting.A.WallNS < 0 || record.Accounting.A.SumNS < 0 || record.Accounting.A.SumNS > record.Accounting.A.WallNS ||
		record.Accounting.A.ResidualNS != record.Accounting.A.WallNS-record.Accounting.A.SumNS || record.Accounting.A.ResidualNS > record.Accounting.A.WallNS/20 ||
		record.Accounting.P != nil || record.Accounting.B != nil || record.Accounting.R != nil || record.Accounting.Shared != nil ||
		record.ACKStatus != "applied" || record.ACKDiskStatus != "committed" || record.MutationID == "" || record.ACKMutationID != record.MutationID ||
		record.ACKReadbackExact == nil || record.ACKCommitSeq == 0 || record.Ack.Value == nil || !record.GraphTerminal ||
		record.StaleClaimedFresh ||
		record.EvidenceBytes <= 0 || record.EvidenceLimit != editProbeRawLimit || record.EvidenceBytes > record.EvidenceLimit {
		return false
	}
	digest, err := hex.DecodeString(record.ACKContentSHA256)
	if err != nil || len(digest) != 32 {
		return false
	}
	for _, name := range []string{"source", "summary", "editing_context", "callers", "call_chain", "search_visible", "text"} {
		if record.FirstCorrect[name].Value == nil {
			return false
		}
	}
	seen := make(map[string]bool)
	for _, read := range record.Readbacks {
		if read.ObservationOnly {
			continue
		}
		if seen[read.Kind] || !read.Correct || read.Error != "" || read.Latency.Value == nil {
			return false
		}
		seen[read.Kind] = true
	}
	for _, name := range []string{"source", "file", "summary", "search_visible", "text"} {
		if !seen[name] {
			return false
		}
	}
	// Retain the exact old refusal and unavailable accounting, outside Errors so
	// the new outcome is distinct without erasing why timing is absent.
	candidate := *proof
	candidate.AccountingError = record.Errors[0]
	if !candidate.Matches(record.Receipt, record.MutationID, record.Accounting.RPCID, record.Accounting.Requested, record.Accounting.Applied) {
		return false
	}
	record.Superseded, record.Outcome, record.Errors = &candidate, receiptproof.SupersededOutcome, nil
	return true
}

func (r *editProbeRuntime) supersededGuards() map[string]*receiptproof.Guard {
	if !r.requireAccounting || r.phase.EditProbe.PhysicalWriteScope == nil || r.p == nil || len(r.p.Repos) != 1 {
		return nil
	}
	candidates := make(map[string]uint64)
	for _, state := range r.states {
		state.mu.Lock()
		candidate := state.record.supersededStatus
		if candidate != nil {
			if _, duplicate := candidates[candidate.Key]; duplicate {
				candidates[candidate.Key] = 0
			} else {
				candidates[candidate.Key] = candidate.Ticket
			}
		}
		state.mu.Unlock()
	}
	if len(candidates) == 0 {
		return nil
	}
	guards, err := collectSupersededGuards(r.p.Root, r.p.Repos[0].Prefix, candidates)
	if err != nil {
		return nil
	} // The original timing failure remains fatal.
	return guards
}

// Scan only this run's already-owned daemon logs, once after probe/raw writers
// have joined. Equal tee copies in different files are one observation;
// repeated same-file observations and conflicting guards remain ambiguous.
func collectSupersededGuards(root, repo string, candidates map[string]uint64) (map[string]*receiptproof.Guard, error) {
	result := make(map[string]*receiptproof.Guard)
	ambiguous := make(map[string]bool)
	sources := make(map[string]map[string]bool)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), "daemon") || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		file, err := os.Open(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), maxWireBytes)
		line := 0
		for scanner.Scan() {
			line++
			var guard receiptproof.Guard
			if json.Unmarshal(scanner.Bytes(), &guard) != nil || guard.Message != "graph patch attempt timing unavailable" || guard.Repo != repo ||
				candidates[guard.Key] == 0 || candidates[guard.Key] != guard.Ticket {
				continue
			}
			guard.SourceLog, guard.Line, guard.SHA256 = entry.Name(), line, fmt.Sprintf("%x", sha256.Sum256(scanner.Bytes()))
			if sources[guard.Key] == nil {
				sources[guard.Key] = make(map[string]bool)
			}
			if sources[guard.Key][guard.SourceLog] {
				ambiguous[guard.Key] = true
			}
			sources[guard.Key][guard.SourceLog] = true
			if previous := result[guard.Key]; previous != nil {
				if previous.SHA256 != guard.SHA256 {
					ambiguous[guard.Key] = true
				}
			} else {
				result[guard.Key] = &guard
			}
		}
		if err := errors.Join(scanner.Err(), file.Close()); err != nil {
			return nil, err
		}
	}
	for key := range ambiguous {
		delete(result, key)
	}
	return result, nil
}
