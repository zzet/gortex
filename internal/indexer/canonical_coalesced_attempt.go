package indexer

import (
	"fmt"
	"time"
	"weak"
)

// A successor is its own physical observation. It never rebinds the edit's
// ticket or copies accepted-P evidence into a receipt it merely serves.
func (p *PublicationPhaseRecord) beginCoalescedAttempt(generation uint64, retry int, entered time.Time) *PublicationPhaseRecord {
	if p == nil || generation == 0 || retry < 0 || entered.IsZero() {
		return nil
	}
	owner := p.owner.Value()
	if owner == nil {
		return nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	p.mu.Lock()
	if owner.byKey[p.key] != p || !p.canonicalBound || p.ticket >= generation || entered.Before(p.origin) {
		p.mu.Unlock()
		return nil
	}
	checkout, key := p.checkoutID, fmt.Sprintf("%s/physical-%d-retry-%d", p.key, generation, retry)
	p.mu.Unlock()
	if owner.byKey[key] != nil {
		return nil // a callback identity cannot be rebound to another origin
	}
	record := owner.beginLocked(checkout, key, "canonical_coalesced_attempt", entered)
	// This origin is the observed native arm (or callback entry when absent),
	// not request admission. Bind identity without inventing ticket_enqueued.
	record.mu.Lock()
	record.canonicalBound, record.ticket = true, generation
	record.mu.Unlock()
	return record
}

func (p *PublicationPhaseRecord) linkCoalescedWinner(winner *PublicationPhaseRecord, requested, applied uint64) {
	if p == nil || winner == nil || p == winner || requested == 0 || requested >= applied {
		return
	}
	owner := p.owner.Value()
	if owner == nil || winner.owner.Value() != owner {
		return
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	// Keys may have changed while the callback ran. Membership is checked under
	// the existing recorder->record order, not by a stale captured key.
	p.mu.Lock()
	defer p.mu.Unlock()
	winner.mu.Lock()
	defer winner.mu.Unlock()
	if owner.byKey[p.key] != p || owner.byKey[winner.key] != winner || !p.canonicalBound ||
		p.ticket != requested || winner.ticket != applied || winner.acceptedAttempt == nil || p.attemptWindow != nil || p.coalescedLinked {
		return
	}
	for _, mark := range p.marks {
		if mark.phase == PublicationTicketFailed {
			return
		}
	}
	p.coalescedWinner = weak.Make(winner)
	p.coalescedLinked = true
}

// CoalescedPublication is a separate actual winner, not the requested edit's P
// window or a complete per-edit background window. An evicted/rebound owner or
// winner remains unavailable. The weak link cannot retain bounded history.
type CoalescedPublication struct {
	RequestedGeneration uint64                     `json:"requested_generation"`
	AppliedGeneration   uint64                     `json:"applied_generation"`
	Scope               string                     `json:"scope"`
	BackgroundCoverage  string                     `json:"background_coverage"`
	Publication         PublicationPhaseSnapshot   `json:"publication_phases"`
	Timings             EditPhaseTimings           `json:"timings"`
	Background          *RequestedBackgroundTiming `json:"-"` // same fenced immutable observation, projected once
}

func (p *PublicationPhaseRecord) CoalescedPublication() *CoalescedPublication {
	winner, _ := p.CoalescedPublicationEvidence()
	return winner
}

// CoalescedPublicationEvidence captures the related winner/background or an
// actual immutable refusal under one recorder-membership fence. A refusal is
// available even when no independent winner could bind; it is never coverage.
func (p *PublicationPhaseRecord) CoalescedPublicationEvidence() (*CoalescedPublication, *RequestedBackgroundUnavailable) {
	if p == nil {
		return nil, nil
	}
	owner := p.owner.Value()
	if owner == nil {
		return nil, nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	p.mu.Lock()
	winner, requested, key, background := p.coalescedWinner.Value(), p.ticket, p.key, p.background
	var unavailable *RequestedBackgroundUnavailable
	if p.backgroundUnavailable != nil {
		copy := *p.backgroundUnavailable
		unavailable = &copy
	}
	p.mu.Unlock()
	if owner.byKey[key] != p {
		return nil, nil
	}
	if unavailable != nil && unavailable.RequestedGeneration != requested {
		unavailable = nil
	}
	if unavailable != nil {
		// The recorder membership above proves this is the same retained owner
		// after a legitimate final-key binding. Project only the copied diagnostic;
		// neither its frozen timing evidence nor earlier caller snapshots change.
		unavailable.PublicationKey = key
	}
	if winner == nil {
		return nil, unavailable
	}
	snapshot := winner.Snapshot()
	if owner.byKey[snapshot.Key] != winner || snapshot.Ticket <= requested {
		return nil, unavailable
	}
	out := &CoalescedPublication{RequestedGeneration: requested, AppliedGeneration: snapshot.Ticket,
		Scope: "independent_physical_winner_serving_coalesced_receipt", BackgroundCoverage: "not_established",
		Publication: snapshot, Timings: snapshot.EditTimings()}
	if background != nil && background.requested == requested && background.applied == snapshot.Ticket && snapshot.attemptWindow != nil && *snapshot.attemptWindow == background.physical {
		out.Background = background.project(out.Timings.Window.PatchAccounting)
	}
	if out.Background != nil {
		unavailable = nil
	}
	return out, unavailable
}
