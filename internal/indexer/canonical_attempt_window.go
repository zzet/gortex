package indexer

import (
	"time"

	"go.uber.org/zap"
)

// Fixed callback-local readings, never a timer history. Initial quiet wait is
// separate; a retry observes callback entry without relabelling its backoff.
type canonicalPointTimer struct {
	debounce   canonicalDebounceWait
	entered    time.Time
	background *canonicalBackgroundOwner
	successor  bool
}

// Immutable once published. Earlier snapshots keep their original nil/value;
// physical mutation return may postdate receipt terminal and the caller ACK.
type canonicalAttemptWindow struct {
	startNS, endNS int64
	retry          int
	ticket         uint64
	accounting     canonicalAttemptEvidence
}

func (p *pointPublicationAttempt) finishPhysicalAttempt(start, end time.Time, retry int, logger *zap.Logger) {
	window, accepted := p.record.completePhysicalAttempt(p.stamps, start, end, retry)
	if !accepted || logger == nil {
		return
	}
	logger.Info("graph patch attempt complete",
		zap.String("patch_lap_scope", "accepted_physical_attempt_complete"),
		zap.String("publication_key", p.record.Key()),
		zap.Uint64("attempt_ticket", window.ticket),
		zap.Int("attempt_retry", window.retry),
		zap.Float64("attempt_wall_ms", float64(window.endNS-window.startNS)/1e6),
		zap.Object("attempt_accounting", window))
}

func (p *PublicationPhaseRecord) completePhysicalAttempt(stamps *PublicationStamps, start, end time.Time, retry int) (canonicalAttemptWindow, bool) {
	unavailable := canonicalAttemptWindow{}
	if p == nil || stamps == nil || start.IsZero() || end.IsZero() || retry < 0 {
		return unavailable, false
	}
	window := canonicalAttemptWindow{startNS: start.Sub(p.origin).Nanoseconds(), endNS: end.Sub(p.origin).Nanoseconds(), retry: retry}
	if window.startNS < 0 || window.endNS < window.startNS {
		return unavailable, false
	}
	stamps.mu.Lock()
	hasCycle := false
	valid := stamps.patchLaps != nil
	var ticket uint64
	var lateDeep canonicalPatchEvidence
	var lateAdmissions canonicalAdmissionEvidence
	var lateLane canonicalLaneWaitEvidence
	if valid {
		stamps.patchLaps.finishAttemptControlLocked(end)
		ticket = stamps.patchLaps.ticket
		window.ticket = ticket
		lateDeep = stamps.patchLaps.freeze(p.origin)
		lateAdmissions = stamps.freezeCanonicalAdmissions(p.origin)
		lateLane = stamps.laneWait.freeze(p.origin)
		window.accounting = stamps.freezeAttemptEvidence(p.origin)
		if window.accounting.observed && (window.accounting.startNS != window.startNS || window.accounting.endNS != window.endNS) {
			valid = false
		}
	}
	for _, mark := range stamps.marks {
		if mark.phase == PublicationCycleStarted {
			hasCycle = true
			if mark.at.Before(start) {
				valid = false
			}
		}
		if mark.at.After(end) {
			valid = false
		}
	}
	stamps.mu.Unlock()
	if !valid || !hasCycle {
		return unavailable, false
	}
	// A weak membership owner does not prolong the entire recorder's lifetime.
	// An absent owner is unavailable evidence, never a fabricated zero.
	owner := p.owner.Value()
	if owner == nil {
		return unavailable, false
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if owner.byKey[p.key] != p || !p.canonicalBound || p.ticket != ticket || p.acceptedAttempt != stamps || p.attemptWindow != nil {
		return unavailable, false
	}
	for _, mark := range p.marks {
		if mark.phase == PublicationTicketFailed {
			return unavailable, false
		}
	}
	if window.accounting.observed {
		if p.patchLaps == nil || *p.patchLaps != lateDeep || p.admissions == nil || *p.admissions != lateAdmissions ||
			(p.laneWait == nil && (lateLane.started || lateLane.invalid)) || (p.laneWait != nil && *p.laneWait != lateLane) ||
			!window.accounting.agreesWithAdmissions(lateAdmissions, lateLane) {
			window.accounting.invalid = true
		}
	}
	p.attemptWindow = &window // a fresh immutable observation, including after terminal
	p.acceptedAttempt = nil
	return window, true
}
