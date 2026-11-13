package main

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/zzet/gortex/bench/receiptproof"
)

// The checker validates functional observations itself; an outcome label or
// higher generation alone cannot turn missing/incorrect evidence into success.
func coalescedFunctionalEditLoopRecord(wire editLoopWire, index int) bool {
	r := wire.Edits[index]
	p := r.CoalescedFunctional
	return sharedFunctionalEditLoopRecord(wire, index) && r.FunctionalUnavailable == nil && p.Matches(r.Receipt, r.MutationID, r.Accounting.RPCID, r.Accounting.Requested, r.Accounting.Applied) &&
		(r.Accounting.State == "terminal_unavailable_background" || p.NativeNoOp != nil && r.Accounting.State == "unavailable") && p.AccountingError == "A/P/B accounting unavailable: "+strings.Join(r.Accounting.Reasons, "; ")
}

func sharedFunctionalEditLoopRecord(wire editLoopWire, index int) bool {
	r := wire.Edits[index]
	return absentEditLoopTiming(r.Accounting.P) && absentEditLoopTiming(r.Accounting.B) && absentEditLoopTiming(r.Accounting.R) && absentEditLoopTiming(r.Accounting.Shared) && sharedFunctionalEditLoopObservations(wire, index)
}

// Functional observations are common to qualified shared-worktree outcomes;
// each outcome separately validates its applicable physical timing owner.
func sharedFunctionalEditLoopObservations(wire editLoopWire, index int) bool {
	r := wire.Edits[index]
	if !sharedPhysicalEditLoop(wire) || r.Superseded != nil || r.Accounting.A == nil ||
		r.ACKExact == nil || r.ACKStatus != "applied" || r.ACKDiskStatus != "committed" || r.MutationID == "" || r.ACKMutationID != r.MutationID || r.ACKCommitSeq == 0 ||
		r.GraphTerminal == nil || !*r.GraphTerminal || r.StaleClaimedFresh == nil || *r.StaleClaimedFresh ||
		r.EvidenceBytes <= 0 || r.EvidenceBytes > r.EvidenceLimit || r.EvidenceLimit != 8<<20 && r.EvidenceLimit != 16<<20 {
		return false
	}
	tiers := r.MutationTiers
	if tiers == nil || tiers.State != "observed" || tiers.Reason != "" {
		return false
	}
	if !orderedEditLoopTierStamps(tiers.ACK, tiers.FileGraph, tiers.Relations, tiers.Derived) {
		return false
	}
	for _, tier := range []string{"file_graph", "relations", "derived"} {
		if tiers.Tiers[tier] != "fresh" {
			return false
		}
	}
	a := r.Accounting.A
	if !(receiptproof.Window{WallNS: a.WallNS, SumNS: a.SumNS, ResidualNS: a.ResidualNS, WithinFivePercent: a.WithinFivePercent}).Complete() {
		return false
	}
	digest, err := hex.DecodeString(r.ACKContentSHA256)
	if err != nil || len(digest) != 32 {
		return false
	}
	observed := func(m metric) bool { return m.Value != nil && finiteNonnegative(*m.Value) && m.Unit == "ms" }
	if !observed(r.ACK) || !observed(r.Readback) || !observed(r.OneRead) {
		return false
	}
	for _, name := range []string{"source", "summary", "editing_context", "callers", "call_chain", "search_visible", "text"} {
		if !observed(r.FirstCorrect[name]) {
			return false
		}
	}
	seen := make(map[string]bool)
	for _, read := range r.Readbacks {
		if read.ObservationOnly {
			continue
		}
		if seen[read.Kind] || read.Correct == nil || !*read.Correct || read.Error != "" || !observed(read.Latency) {
			return false
		}
		seen[read.Kind] = true
	}
	for _, kind := range []string{"source", "file", "summary", "search_visible", "text"} {
		if !seen[kind] {
			return false
		}
	}
	return true
}

// Both the serialized observation and the last raw status must carry this
// same complete ordered timeline. One cannot mask contradictions in the other.
func orderedEditLoopTierStamps(stamps ...string) bool {
	previous := time.Time{}
	for _, stamp := range stamps {
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || at.IsZero() || !previous.IsZero() && at.Before(previous) {
			return false
		}
		previous = at
	}
	return len(stamps) == 4
}
