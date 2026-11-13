package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/zzet/gortex/bench/receiptproof"
)

// Capture only the latest owned status. A later invalid/accepted status clears
// this candidate just as it clears the separate proven-superseded candidate.
func coalescedFunctionalStatus(status map[string]any, raw json.RawMessage, wireID int64, accounting editProbeAccounting) *receiptproof.Coalesced {
	if p := nativeNoOpFunctionalStatus(status, raw, wireID, accounting); p != nil {
		return p
	}
	pub, _ := status["publication_phases"].(map[string]any)
	timing, _ := status["phase_timing"].(map[string]any)
	bg, _ := status["requested_background_unavailable"].(map[string]any)
	ack, _ := status["acknowledgement"].(map[string]any)
	winner, _ := status["coalesced_publication"].(map[string]any)
	wp, _ := winner["publication_phases"].(map[string]any)
	wt, _ := winner["timings"].(map[string]any)
	physical, _ := wt["phase_timing"].(map[string]any)
	pa, _ := physical["patch_accounting"].(map[string]any)
	_, observed := timing["patch_wall_ms"]
	if pa["execution_disposition"] == "native_mtime_duplicate" || !observed || timing["patch_wall_ms"] != nil || timing["patch_accounting"] != nil || status["requested_background"] != nil || status["read_assisted_publication"] != nil ||
		status["graph_status"] != "fresh" || status["graph_status_terminal"] != true || physical["clock"] != "monotonic" ||
		accounting.A == nil || accounting.P == nil || accounting.B != nil || accounting.R != nil || accounting.Shared != nil || accounting.State != "terminal_unavailable_background" {
		return nil
	}
	text := func(m map[string]any, key string) string { value, _ := m[key].(string); return value }
	number := func(m map[string]any, key string) uint64 {
		value, err := strconvEditProbeUint(m[key])
		if err != nil {
			return ^uint64(0)
		}
		return value
	}
	handoffs := number(bg, "observed_native_handoffs")
	p := &receiptproof.Coalesced{
		Status:          receiptproof.Status{Receipt: text(status, "receipt"), MutationID: text(status, "mutation_id"), RPCID: text(ack, "rpc_id"), Requested: number(status, "reindex_generation"), Applied: number(status, "applied_generation"), Key: text(pub, "key"), Ticket: number(pub, "ticket"), Source: text(pub, "source"), Clock: text(pub, "clock"), Terminal: pub["terminal"] == true, PUnavailable: true, BUnavailable: true, Refusal: text(bg, "reason"), BindingPredicate: text(bg, "binding_predicate"), BackgroundStage: text(bg, "background_stage"), AcceptedStage: text(bg, "accepted_attempt_stage"), BackgroundRequest: number(bg, "requested_generation"), BackgroundLast: number(bg, "last_observed_generation"), NativeHandoffs: &handoffs, WireID: wireID, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))},
		Winner:          receiptproof.CoalescedWinner{Key: text(wp, "key"), Ticket: number(wp, "ticket"), Source: text(wp, "source"), Clock: text(wp, "clock"), Terminal: wp["terminal"] == true, Requested: number(winner, "requested_generation"), Applied: number(winner, "applied_generation"), Scope: text(winner, "scope"), BackgroundCoverage: text(winner, "background_coverage"), AttemptTicket: number(pa, "attempt_ticket"), AttemptRetry: number(pa, "attempt_retry"), PartitionComplete: pa["observed_partition_complete"] == true, PhysicalScope: text(pa, "scope")},
		ServingPhysical: receiptproof.Window(*accounting.P), AccountingError: "A/P/B accounting unavailable: " + strings.Join(accounting.Reasons, "; "),
	}
	if bg["scope"] != "requested_background_observation" || bg["publication_key"] != pub["key"] || !p.Matches(p.Status.Receipt, p.Status.MutationID, accounting.RPCID, accounting.Requested, accounting.Applied) {
		return nil
	}
	return p
}

func classifyCoalescedFunctionalRecord(record *editProbeRecord, scope *editProbePhysicalWriteScope) bool {
	if !sharedFunctionalRecord(record, scope) || record.coalescedStatus == nil || record.Superseded != nil || record.FunctionalUnavailable != nil || len(record.Errors) != 1 || record.Errors[0] != record.coalescedStatus.AccountingError ||
		!qualifiedFunctionalAccounting(record.Accounting, record.coalescedStatus) {
		return false
	}
	p := record.coalescedStatus
	if !p.Matches(record.Receipt, record.MutationID, record.Accounting.RPCID, record.Accounting.Requested, record.Accounting.Applied) {
		return false
	}
	copy := *p
	record.CoalescedFunctional, record.Outcome, record.Errors = &copy, receiptproof.CoalescedFunctionalOutcome, nil
	// P was measured for the independent winner, not the original requested edit.
	record.Accounting.P, record.Accounting.B = nil, nil
	return true
}

func sharedFunctionalRecord(record *editProbeRecord, scope *editProbePhysicalWriteScope) bool {
	if record == nil || scope == nil || scope.Kind != "shared_worktree_file" || scope.File == "" || scope.Editors < 2 || record.CheckoutID != "" || record.Route != "" || record.Outcome != "diagnostic_failure" ||
		record.ACKStatus != "applied" || record.ACKDiskStatus != "committed" || record.MutationID == "" || record.ACKMutationID != record.MutationID || record.ACKCommitSeq == 0 || record.ACKReadbackExact == nil ||
		!record.GraphTerminal || record.StaleClaimedFresh || !coalescedFreshTiers(record.MutationTiers) || !coalescedMetricObserved(record.Ack) || !coalescedMetricObserved(record.Readback) ||
		record.EvidenceBytes <= 0 || record.EvidenceLimit != editProbeRawLimit || record.EvidenceBytes > record.EvidenceLimit {
		return false
	}
	digest, err := hex.DecodeString(record.ACKContentSHA256)
	if err != nil || len(digest) != 32 {
		return false
	}
	for _, name := range []string{"source", "summary", "editing_context", "callers", "call_chain", "search_visible", "text"} {
		if !coalescedMetricObserved(record.FirstCorrect[name]) {
			return false
		}
	}
	seen := make(map[string]bool)
	for _, read := range record.Readbacks {
		if read.ObservationOnly {
			continue
		}
		if seen[read.Kind] || !read.Correct || read.Error != "" || !coalescedMetricObserved(read.Latency) {
			return false
		}
		seen[read.Kind] = true
	}
	for _, name := range []string{"source", "file", "summary", "search_visible", "text"} {
		if !seen[name] {
			return false
		}
	}
	return true
}

func coalescedMetricObserved(m Metric) bool {
	return m.Unit == "ms" && m.Value != nil && !math.IsNaN(*m.Value) && !math.IsInf(*m.Value, 0) && *m.Value >= 0
}

// Reuse the serialized raw tier observations rather than deriving freshness from
// the outcome label. Missing, pending, failed or malformed tiers cannot qualify.
func coalescedFreshTiers(tiers *editProbeLiveTiers) bool {
	if tiers == nil || tiers.State != "observed" || tiers.Reason != "" {
		return false
	}
	previous := time.Time{}
	for _, stamp := range []string{tiers.ACK, tiers.FileGraph, tiers.Relations, tiers.Derived} {
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || at.IsZero() || !previous.IsZero() && at.Before(previous) {
			return false
		}
		previous = at
	}
	for _, tier := range []string{"file_graph", "relations", "derived"} {
		if tiers.Tiers[tier] != "fresh" {
			return false
		}
	}
	return true
}
