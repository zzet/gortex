package indexer

import "time"

// Checkout receipt accounting.
//
// A watcher ticket's receipt closes its physical attempt P and its requested
// background B from the point attempt that served it
// (canonical_attempt_window.go, canonical_requested_background.go). A checkout
// ticket is served instead by one coordinator refresh cycle, which builds and
// routes a working-tree generation. It arms no requested-background timer and
// runs no point executor, so neither producer ever runs for it. Without this
// evidence the receipt could not say whether a B window might still arrive,
// and a client waiting on one could only time out.
//
// So a checkout ticket's completion records, on the publication record its
// edit was admitted with, the same two pieces the primary's producers would:
//
//   - P, the window of the cycle that served the ticket, from its start to the
//     ticket's completion. The cycle's builder phases are timed on the
//     publication timeline and are not patch laps, so the patch-lap partition
//     reads not_observed and the disposition names the checkout refresh
//     cycle. Only a ticket the cycle owed at its start gets one: a ticket that
//     rode a cycle already in flight was not what that cycle ran for, as a
//     watcher's coalesced receipt does not borrow its winner's P. A ticket
//     carried across deferred, rescheduled, or retried cycles gets no P
//     either: the final cycle's window alone is not the ticket's first-try
//     physical attempt.
//   - B, the frozen refusal of a requested-background binding: no measured B
//     window exists for a checkout ticket, and none can arrive later.
//
// Both are in the ticket's admission-sequence domain, which is also the domain
// of the receipt's requested and applied generations. Only a committed
// single-file write's ticket records them. A batch and recovery tickets keep
// their own accounting.

// checkoutRefreshCycle names a P whose serving attempt was a checkout refresh
// cycle, and the binding predicate that refuses its requested background.
const checkoutRefreshCycle = "checkout_refresh_cycle"

// recordCheckoutPublication records the P window and the B refusal of the
// checkout ticket bound to p, before the ticket's completion mark, so a reader
// that sees the record terminal also sees both. cycleStarted is the start of
// the cycle that served the ticket, zero when that cycle did not run for it.
// A record that is no longer a member of its recorder, already terminal, bound
// to a watcher attempt or to another ticket, or that already holds P or B
// evidence is left unchanged.
func (p *PublicationPhaseRecord) recordCheckoutPublication(ticket uint64, cycleStarted, completed time.Time) {
	if p == nil || ticket == 0 || completed.IsZero() {
		return
	}
	owner := p.owner.Value()
	if owner == nil {
		return
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if owner.byKey[p.key] != p || p.terminal || p.canonicalBound || p.ticket != ticket ||
		p.attemptWindow != nil || p.background != nil || p.backgroundUnavailable != nil || p.coalescedWinner.Value() != nil {
		return
	}
	carried := false
	for _, mark := range p.marks {
		if mark.phase == PublicationCycleStarted && mark.at.Before(cycleStarted) {
			carried = true
			break
		}
	}
	if !carried && !cycleStarted.IsZero() && !cycleStarted.Before(p.origin) && !completed.Before(cycleStarted) {
		p.attemptWindow = &canonicalAttemptWindow{
			startNS: cycleStarted.Sub(p.origin).Nanoseconds(), endNS: completed.Sub(p.origin).Nanoseconds(),
			ticket: ticket, checkoutCycle: true,
		}
	}
	refusal := RequestedBackgroundUnavailable{
		Scope: "requested_background_observation", Reason: "serving_attempt_binding_mismatch",
		PublicationKey: p.key, RequestedGeneration: ticket, LastObservedGeneration: ticket,
	}
	refusal = canonicalBindingUnavailableLocked(refusal, checkoutRefreshCycle, p)
	p.backgroundUnavailable = &refusal
}

// accountsCheckoutPublication reports whether a completing ticket records its
// P and B (recordCheckoutPublication): a committed single-file write's, which
// carries tier evidence from its acknowledgement on.
func (r *checkoutRefreshRequest) accountsCheckoutPublication() bool {
	return r.record != nil && len(r.batchFiles) == 0 && r.contentHash != "" &&
		!r.ticket.Ticket.TierState().AckAt.IsZero()
}
