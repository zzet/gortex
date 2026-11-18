package indexer

import "time"

// Two fixed admission envelopes, not a tracing vocabulary or interval history.
// Both precede the executor and remain separate from its deep patch laps.
type canonicalAdmissionKind uint8

const (
	canonicalCohortAdmission canonicalAdmissionKind = iota
	canonicalBatchAdmission
	canonicalAdmissionCount
)

var canonicalAdmissionNames = [...]string{"cohort_admission", "batch_admission"}

type canonicalAdmissionPair struct {
	started, finished time.Time
	invalid           bool
}

type canonicalAdmissionSpan struct {
	stamps *PublicationStamps
	kind   canonicalAdmissionKind
}

type canonicalAdmissionEvidence struct {
	pairs [canonicalAdmissionCount]struct {
		startNS, endNS             int64
		started, finished, invalid bool
	}
}

func (s *PublicationStamps) beginCanonicalAdmission(kind canonicalAdmissionKind) canonicalAdmissionSpan {
	if s == nil || s.patchLaps == nil {
		return canonicalAdmissionSpan{}
	}
	return s.beginCanonicalAdmissionAt(kind, time.Now())
}

func (s *PublicationStamps) beginCanonicalAdmissionAt(kind canonicalAdmissionKind, at time.Time) canonicalAdmissionSpan {
	s.mu.Lock()
	defer s.mu.Unlock()
	pair := &s.admissions[kind]
	if at.IsZero() || !pair.started.IsZero() {
		pair.invalid = true
	} else {
		pair.started = at
	}
	s.patchLaps.beginObservedOuterLocked(patchCohortAdmission+canonicalPatchLap(kind), at)
	return canonicalAdmissionSpan{stamps: s, kind: kind}
}

func (span *canonicalAdmissionSpan) end() {
	if span.stamps == nil {
		return
	}
	span.endAt(time.Now())
}

func (span *canonicalAdmissionSpan) endAt(at time.Time) {
	s := span.stamps
	if s == nil {
		return
	}
	span.stamps = nil
	s.mu.Lock()
	defer s.mu.Unlock()
	pair := &s.admissions[span.kind]
	if at.IsZero() || pair.started.IsZero() || at.Before(pair.started) || !pair.finished.IsZero() {
		pair.invalid = true
		return
	}
	pair.finished = at
	s.patchLaps.endObservedOuterLocked(patchCohortAdmission+canonicalPatchLap(span.kind), pair.started, at)
}

func (s *PublicationStamps) freezeCanonicalAdmissions(origin time.Time) canonicalAdmissionEvidence {
	var out canonicalAdmissionEvidence
	for i, pair := range s.admissions {
		v := &out.pairs[i]
		v.started, v.finished, v.invalid = !pair.started.IsZero(), !pair.finished.IsZero(), pair.invalid
		if v.started {
			v.startNS = pair.started.Sub(origin).Nanoseconds()
		}
		if v.finished {
			v.endNS = pair.finished.Sub(origin).Nanoseconds()
		}
	}
	return out
}

func (p canonicalAdmissionEvidence) project(offsets map[PublicationPhase]int64, terminal bool, lane *canonicalLaneWaitEvidence, out *EditPhaseTimings) {
	enqueue, hasEnqueue := offsets[PublicationTicketEnqueued]
	cycle, hasCycle := offsets[PublicationCycleStarted]
	executor, hasExecutor := offsets[PublicationPatchExecutionStarted]
	invalid := false
	for _, pair := range p.pairs {
		if pair.invalid || pair.started && pair.startNS < 0 || pair.finished && (!pair.started || pair.endNS < pair.startNS) {
			invalid = true
		}
	}
	cohort, batch := p.pairs[canonicalCohortAdmission], p.pairs[canonicalBatchAdmission]
	if cohort.finished && batch.started && cohort.endNS > batch.startNS {
		invalid = true
	}
	if lane != nil && lane.started {
		for _, pair := range p.pairs {
			if pair.finished && pair.endNS > lane.startNS {
				invalid = true
			}
		}
	}
	for i, name := range canonicalAdmissionNames {
		pair := p.pairs[i]
		out.Milliseconds[name], out.States[name] = nil, "not_observed"
		if invalid {
			out.States[name] = "invalid_order"
			continue
		}
		if !pair.started {
			continue
		}
		if !pair.finished {
			if !terminal {
				out.States[name] = "pending"
			}
			continue
		}
		if !hasEnqueue || !hasCycle || !hasExecutor {
			continue
		}
		if enqueue < 0 || cycle < enqueue || pair.startNS < cycle || pair.endNS > executor {
			out.States[name] = "invalid_order"
			continue
		}
		if terminal {
			last := PublicationTicketCompleted
			if _, failed := offsets[PublicationTicketFailed]; failed {
				last = PublicationTicketFailed
			}
			end, known := offsets[last]
			if !known {
				continue
			}
			if pair.endNS > end {
				out.States[name] = "invalid_order"
				continue
			}
		}
		ms := float64(pair.endNS-pair.startNS) / 1e6
		out.Milliseconds[name], out.States[name] = &ms, "measured"
	}
}
