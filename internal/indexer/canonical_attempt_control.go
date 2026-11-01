package indexer

import "time"

func isAttemptControl(lap canonicalPatchLap) bool {
	return lap >= patchAttemptSetup && lap <= patchAttemptFinalize
}

// These transitions reuse the SAME bounded ordered accumulator. The current
// control interval is actually open while setup/inspection/control/finalization
// runs, and pauses at actual named-operation endpoints; nothing is a residual.
// All locked helpers require PublicationStamps.mu.
func (p *canonicalPatchLaps) openAttemptControlLocked(at time.Time) {
	if !p.controlObserved || p.controlFinished {
		return
	}
	p.beginLapLocked(p.controlKind, at)
}

func (p *canonicalPatchLaps) closeAttemptControlLocked(at time.Time) {
	if p.active != 1 || !isAttemptControl(p.activeKind) {
		p.invalid = true
		return
	}
	p.endLapLocked(p.activeKind, p.activeStarted, at)
}

func (s *PublicationStamps) startAttemptControl(at time.Time) {
	s.startAttemptControlKind(at, patchAttemptSetup)
}

func (s *PublicationStamps) startAttemptControlKind(at time.Time, kind canonicalPatchLap) {
	if s == nil || s.patchLaps == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.patchLaps
	if !isAttemptControl(kind) || at.IsZero() || p.controlObserved || p.active != 0 || !p.lastEnd.IsZero() {
		p.invalid = true
		return
	}
	p.controlObserved, p.controlKind, p.attemptStarted = true, kind, at
	p.openAttemptControlLocked(at)
}

func (s *PublicationStamps) switchAttemptControlAt(kind canonicalPatchLap, at time.Time) {
	if s == nil || s.patchLaps == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.patchLaps
	if !p.controlObserved || p.controlFinished {
		return
	}
	if !isAttemptControl(kind) || at.IsZero() {
		p.invalid = true
		return
	}
	p.closeAttemptControlLocked(at)
	p.controlKind = kind
	p.openAttemptControlLocked(at)
}

func (s *PublicationStamps) switchAttemptControl(kind canonicalPatchLap) bool {
	if s == nil || s.patchLaps == nil {
		return false
	}
	s.mu.Lock()
	observed := s.patchLaps.controlObserved && !s.patchLaps.controlFinished && s.patchLaps.controlKind != kind
	s.mu.Unlock()
	if !observed {
		return false
	}
	s.switchAttemptControlAt(kind, time.Now())
	return true
}

func (p *canonicalPatchLaps) finishAttemptControlLocked(at time.Time) {
	if !p.controlObserved || p.controlFinished {
		return
	}
	p.closeAttemptControlLocked(at)
	p.controlFinished, p.attemptFinished = true, at
}

func (p *canonicalPatchLaps) beginObservedOuterLocked(kind canonicalPatchLap, at time.Time) {
	if !p.controlObserved || p.controlFinished {
		return
	}
	p.closeAttemptControlLocked(at)
	p.beginLapLocked(kind, at)
}

func (p *canonicalPatchLaps) endObservedOuterLocked(kind canonicalPatchLap, start, end time.Time) {
	if !p.controlObserved || p.controlFinished {
		return
	}
	p.endLapLocked(kind, start, end)
	p.openAttemptControlLocked(end)
}
