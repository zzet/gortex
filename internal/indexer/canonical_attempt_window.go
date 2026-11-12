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

// physicalAttemptGuardDiagnostic contains only scalars frozen under the existing
// recorder/record locks. It neither authorizes a window nor retains a record.
type physicalAttemptGuardDiagnostic struct {
	observed       bool
	recorder       string
	key            string
	recordTicket   uint64
	attemptTicket  uint64
	acceptedStamps string
	windowPresent  bool
	terminal       bool
	patchPresent   bool
}

func (p *pointPublicationAttempt) finishPhysicalAttempt(start, end time.Time, retry int, logger *zap.Logger) {
	window, accepted, reason, guard := p.record.completePhysicalAttempt(p.stamps, start, end, retry)
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
				zap.String("attempt_rejection_reason", reason),
				zap.Bool("attempt_guard_observed", guard.observed),
				zap.String("attempt_guard_recorder", guard.recorder),
				zap.String("attempt_guard_publication_key", guard.key),
				zap.Uint64("attempt_guard_ticket", guard.recordTicket),
				zap.Uint64("attempt_guard_collector_ticket", guard.attemptTicket),
				zap.String("attempt_guard_accepted_stamps", guard.acceptedStamps),
				zap.Bool("attempt_guard_window_present", guard.windowPresent),
				zap.Bool("attempt_guard_terminal", guard.terminal),
				zap.Bool("attempt_guard_patch_present", guard.patchPresent),
				zap.Bool("attempt_completion_observed", p.completion.observed),
				zap.String("attempt_completion_disposition", p.completion.kind),
				zap.Bool("attempt_completion_fallback", p.completion.fallback),
				zap.Uint64("attempt_completion_applied", p.completion.appliedGeneration),
				zap.Uint64("attempt_completion_pending_at_check", p.completion.pendingGeneration),
				zap.Int("attempt_completion_count", p.completion.completed))
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

func (p *PublicationPhaseRecord) completePhysicalAttempt(stamps *PublicationStamps, start, end time.Time, retry int) (canonicalAttemptWindow, bool, string, physicalAttemptGuardDiagnostic) {
	unavailable := canonicalAttemptWindow{}
	guard := physicalAttemptGuardDiagnostic{}
	switch {
	case p == nil:
		return unavailable, false, "nil_record", guard
	case stamps == nil:
		return unavailable, false, "nil_stamps", guard
	case start.IsZero():
		return unavailable, false, "callback_start_unobserved", guard
	case end.IsZero():
		return unavailable, false, "callback_end_unobserved", guard
	case retry < 0:
		return unavailable, false, "invalid_retry", guard
	}
	window := canonicalAttemptWindow{startNS: start.Sub(p.origin).Nanoseconds(), endNS: end.Sub(p.origin).Nanoseconds(), retry: retry}
	switch {
	case window.startNS < 0:
		return unavailable, false, "callback_before_origin", guard
	case window.endNS < window.startNS:
		return unavailable, false, "callback_return_before_entry", guard
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
		return unavailable, false, reason, guard
	}
	// A weak membership owner does not prolong the entire recorder's lifetime.
	// An absent owner is unavailable evidence, never a fabricated zero.
	owner := p.owner.Value()
	if owner == nil {
		return unavailable, false, "recorder_owner_unavailable", guard
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	guard = physicalAttemptGuardDiagnostic{
		observed: true, recorder: owner.sharedInstance, key: p.key,
		recordTicket: p.ticket, attemptTicket: ticket,
		acceptedStamps: "other", windowPresent: p.attemptWindow != nil,
		terminal: p.terminal, patchPresent: p.patchLaps != nil,
	}
	switch p.acceptedAttempt {
	case nil:
		guard.acceptedStamps = "absent"
	case stamps:
		guard.acceptedStamps = "same"
	}
	switch {
	case owner.byKey[p.key] != p:
		return unavailable, false, "recorder_membership_mismatch", guard
	case !p.canonicalBound:
		return unavailable, false, "canonical_ticket_unbound", guard
	case p.ticket != ticket:
		return unavailable, false, "attempt_ticket_mismatch", guard
	case p.acceptedAttempt != stamps:
		return unavailable, false, "accepted_stamps_mismatch", guard
	case p.attemptWindow != nil:
		return unavailable, false, "physical_window_already_recorded", guard
	}
	for _, mark := range p.marks {
		if mark.phase == PublicationTicketFailed {
			return unavailable, false, "publication_ticket_failed", guard
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
	return window, true, "", guard
}
