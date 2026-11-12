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
	window, accepted, reason := p.record.completePhysicalAttempt(p.stamps, start, end, retry)
	if logger == nil {
		return
	}
	if !accepted {
		// Key is observed after completion released its locks. It is not a
		// frozen guard-time identity or authority for physical accounting.
		if key := p.record.Key(); key != "" {
			logger.Info("graph patch attempt timing unavailable",
				zap.String("patch_lap_scope", "physical_attempt_timing_unavailable"),
				zap.String("publication_key", key),
				zap.Int("attempt_retry", retry),
				zap.String("attempt_rejection_reason", reason))
		}
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

func (p *PublicationPhaseRecord) completePhysicalAttempt(stamps *PublicationStamps, start, end time.Time, retry int) (canonicalAttemptWindow, bool, string) {
	unavailable := canonicalAttemptWindow{}
	switch {
	case p == nil:
		return unavailable, false, "nil_record"
	case stamps == nil:
		return unavailable, false, "nil_stamps"
	case start.IsZero():
		return unavailable, false, "callback_start_unobserved"
	case end.IsZero():
		return unavailable, false, "callback_end_unobserved"
	case retry < 0:
		return unavailable, false, "invalid_retry"
	}
	window := canonicalAttemptWindow{startNS: start.Sub(p.origin).Nanoseconds(), endNS: end.Sub(p.origin).Nanoseconds(), retry: retry}
	switch {
	case window.startNS < 0:
		return unavailable, false, "callback_before_origin"
	case window.endNS < window.startNS:
		return unavailable, false, "callback_return_before_entry"
	}
	stamps.mu.Lock()
	hasCycle := false
	valid := stamps.patchLaps != nil
	reason := ""
	if !valid {
		reason = "patch_laps_unobserved"
	}
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
			reason = "captured_callback_window_mismatch"
		}
	}
	for _, mark := range stamps.marks {
		if mark.phase == PublicationCycleStarted {
			hasCycle = true
			if mark.at.Before(start) {
				valid = false
				if reason == "" {
					reason = "cycle_before_callback_entry"
				}
			}
		}
		if mark.at.After(end) {
			valid = false
			if reason == "" {
				reason = "stamp_after_callback_return"
			}
		}
	}
	stamps.mu.Unlock()
	if !valid || !hasCycle {
		if reason == "" {
			reason = "cycle_start_unobserved"
		}
		return unavailable, false, reason
	}
	// A weak membership owner does not prolong the entire recorder's lifetime.
	// An absent owner is unavailable evidence, never a fabricated zero.
	owner := p.owner.Value()
	if owner == nil {
		return unavailable, false, "recorder_owner_unavailable"
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case owner.byKey[p.key] != p:
		return unavailable, false, "recorder_membership_mismatch"
	case !p.canonicalBound:
		return unavailable, false, "canonical_ticket_unbound"
	case p.ticket != ticket:
		return unavailable, false, "attempt_ticket_mismatch"
	case p.acceptedAttempt != stamps:
		return unavailable, false, "accepted_stamps_mismatch"
	case p.attemptWindow != nil:
		return unavailable, false, "physical_window_already_recorded"
	}
	for _, mark := range p.marks {
		if mark.phase == PublicationTicketFailed {
			return unavailable, false, "publication_ticket_failed"
		}
	}
	if window.accounting.observed {
		if p.patchLaps == nil || !p.patchLaps.sameAccounting(lateDeep) || p.admissions == nil || *p.admissions != lateAdmissions ||
			(p.laneWait == nil && (lateLane.started || lateLane.invalid)) || (p.laneWait != nil && *p.laneWait != lateLane) ||
			!window.accounting.agreesWithAdmissions(lateAdmissions, lateLane) {
			window.accounting.invalid = true
		}
	}
	p.attemptWindow = &window // a fresh immutable observation, including after terminal
	p.acceptedAttempt = nil
	return window, true, ""
}
