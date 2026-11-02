package indexer

import "time"

// These are the fixed canonical patch laps, not arbitrary trace categories.
// One attempt owns bounded scalar state, regardless of files/chunks/retries.
type canonicalPatchLap uint8

const (
	patchGateWait canonicalPatchLap = iota
	patchParse
	patchApply
	patchSearch
	patchResolve
	patchMtimes
	patchDerived
	// Supplementary actual work, kept private until its additive projection is
	// reviewed. It is not a replacement for any requested patch lap.
	patchBookkeeping
	canonicalDeepLapCount
)

const (
	patchAttemptSetup canonicalPatchLap = canonicalDeepLapCount + iota
	patchInspection
	patchExecutorControl
	patchAttemptFinalize
	patchCohortAdmission
	patchBatchAdmission
	patchRepositoryLane
	canonicalPatchLapCount
)

var canonicalPatchLapNames = [...]string{
	"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived",
}

type canonicalLapTotal struct {
	total       time.Duration
	first, last time.Time
	begun, done uint64
	incomplete  bool
}

type canonicalPatchLaps struct {
	laps                             [canonicalPatchLapCount]canonicalLapTotal
	lastEnd                          time.Time
	active                           int
	activeKind                       canonicalPatchLap
	activeStarted                    time.Time
	controlObserved, controlFinished bool
	controlKind                      canonicalPatchLap
	attemptStarted, attemptFinished  time.Time
	invalid                          bool
	nativeMtimeDuplicate             bool      // observed early return before the executor
	origin, enqueued                 time.Time // immutable original admission identity, for attempt logs
	ticket                           uint64
}

// This disposition belongs only to the native mtime gate, not to a failed or
// otherwise unobserved executor. It adds no clock read or allocation.
func (s *PublicationStamps) recordNativeMtimeDuplicate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.patchLaps; p != nil {
		if p.nativeMtimeDuplicate {
			p.invalid = true
		}
		p.nativeMtimeDuplicate = true
	}
}

func (p *PublicationPhaseRecord) newCanonicalPatchStamps() *PublicationStamps {
	p.mu.Lock()
	defer p.mu.Unlock()
	laps := &canonicalPatchLaps{origin: p.origin, ticket: p.ticket}
	for _, mark := range p.marks {
		if mark.phase == PublicationTicketEnqueued {
			laps.enqueued = mark.at
			break
		}
	}
	return &PublicationStamps{patchLaps: laps}
}

type canonicalLapSpan struct {
	stamps  *PublicationStamps
	lap     canonicalPatchLap
	started time.Time
}

func (s *PublicationStamps) beginPatchLap(lap canonicalPatchLap) canonicalLapSpan {
	if s == nil || s.patchLaps == nil {
		return canonicalLapSpan{} // no clock/allocation on unobserved paths
	}
	return s.beginPatchLapAt(lap, time.Now())
}

func (s *PublicationStamps) beginPatchLapAt(lap canonicalPatchLap, at time.Time) canonicalLapSpan {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.patchLaps
	if p.controlObserved && !p.controlFinished && p.active == 1 && isAttemptControl(p.activeKind) {
		p.closeAttemptControlLocked(at)
	}
	p.beginLapLocked(lap, at)
	return canonicalLapSpan{stamps: s, lap: lap, started: at}
}

func (p *canonicalPatchLaps) beginLapLocked(lap canonicalPatchLap, at time.Time) {
	v := &p.laps[lap]
	if at.IsZero() || p.active != 0 || !p.lastEnd.IsZero() && at.Before(p.lastEnd) {
		p.invalid = true
	}
	if v.begun == 0 {
		v.first = at
	}
	v.begun++
	if p.active == 0 {
		p.activeKind, p.activeStarted = lap, at
	}
	p.active++
}

func (p *canonicalPatchLaps) endLapLocked(lap canonicalPatchLap, start, at time.Time) {
	v := &p.laps[lap]
	if at.IsZero() || at.Before(start) || p.active != 1 || p.activeKind != lap || !p.activeStarted.Equal(start) {
		p.invalid = true
	} else if elapsed := at.Sub(start); elapsed < 0 || v.total > time.Duration(1<<63-1)-elapsed {
		p.invalid = true
	} else {
		v.total += elapsed
	}
	v.last = at
	v.done++
	p.active--
	p.lastEnd = at
}

func (span *canonicalLapSpan) end() time.Time {
	if span.stamps == nil {
		return time.Time{}
	}
	return span.endAt(time.Now())
}

func (span *canonicalLapSpan) endAt(at time.Time) time.Time {
	s := span.stamps
	if s == nil {
		return time.Time{}
	}
	span.stamps = nil // completion is idempotent; no closure or per-row atomics
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.patchLaps
	p.endLapLocked(span.lap, span.started, at)
	if p.active == 0 {
		p.openAttemptControlLocked(at)
	}
	return at
}

func (s *PublicationStamps) incompletePatchLaps() {
	if s == nil || s.patchLaps == nil {
		return
	}
	s.mu.Lock()
	// The legacy fallback has inseparable extraction/application/search work.
	// Previously measured chunks cannot make these complete for this attempt.
	for _, lap := range [...]canonicalPatchLap{patchParse, patchApply, patchSearch} {
		s.patchLaps.laps[lap].incomplete = true
	}
	s.mu.Unlock()
}

type canonicalLapEvidence struct {
	totalNS, firstNS, lastNS int64
	begun, done              uint64
	incomplete               bool
}

type canonicalPatchEvidence struct {
	laps    [canonicalDeepLapCount]canonicalLapEvidence
	invalid bool
}

// freeze converts only same-process monotonic readings, never wall strings.
// Called under the collector lock; the returned value has no mutable aliases.
func (p *canonicalPatchLaps) freeze(origin time.Time) canonicalPatchEvidence {
	openOuterControl := p.controlObserved && p.active == 1 && isAttemptControl(p.activeKind)
	out := canonicalPatchEvidence{invalid: p.invalid || p.active != 0 && !openOuterControl}
	for i, v := range p.laps[:canonicalDeepLapCount] {
		out.laps[i] = canonicalLapEvidence{totalNS: int64(v.total), begun: v.begun, done: v.done, incomplete: v.incomplete}
		if v.begun != 0 {
			out.laps[i].firstNS, out.laps[i].lastNS = v.first.Sub(origin).Nanoseconds(), v.last.Sub(origin).Nanoseconds()
		}
	}
	return out
}

// project validates global disjointness and enclosing ownership before ANY
// measured scalar becomes visible. Incomplete/missing laps remain unavailable.
func (p *canonicalPatchEvidence) project(offsets map[PublicationPhase]int64, terminal bool, out *EditPhaseTimings) {
	start, hasStart := offsets[PublicationPatchExecutionStarted]
	end, hasEnd := offsets[PublicationPatchExecutionFinished]
	enqueue, hasEnqueue := offsets[PublicationTicketEnqueued]
	invalid := p.invalid || hasEnqueue && enqueue < 0 || hasStart && start < enqueue || hasEnd && end < start
	if terminal {
		last := PublicationTicketCompleted
		if _, failed := offsets[PublicationTicketFailed]; failed {
			last = PublicationTicketFailed
		}
		if finished, ok := offsets[last]; ok && hasEnd && end > finished {
			invalid = true
		}
	}
	var sum int64
	for _, v := range p.laps {
		if v.begun == 0 {
			continue
		}
		if v.totalNS < 0 || v.firstNS < 0 || v.lastNS < v.firstNS || hasStart && v.firstNS < start || hasEnd && v.lastNS > end || v.totalNS > v.lastNS-v.firstNS {
			invalid = true
		}
		sum += v.totalNS
	}
	if hasStart && hasEnd && sum > end-start {
		invalid = true
	}
	// The old derived pair and the new scalar must describe the SAME call,
	// not two intervals that could overlap or be counted twice.
	derived := p.laps[patchDerived]
	if a, ok := offsets[PublicationPatchDerivedStarted]; ok {
		b, hasB := offsets[PublicationPatchDerivedFinished]
		if !hasB || derived.begun != 1 || derived.done != 1 || derived.firstNS != a || derived.lastNS != b || derived.totalNS != b-a {
			invalid = true
		}
	} else if derived.begun != 0 {
		invalid = true
	}
	for i, name := range canonicalPatchLapNames {
		v := p.laps[i]
		out.Milliseconds[name], out.States[name] = nil, "not_observed"
		if invalid {
			out.States[name] = "invalid_order"
			continue
		}
		if !hasStart || !hasEnd || !hasEnqueue || v.begun == 0 || v.begun != v.done || v.incomplete {
			continue
		}
		ms := float64(v.totalNS) / 1e6
		out.Milliseconds[name], out.States[name] = &ms, "measured"
	}
}

func (s *PublicationStamps) beginDerivedPatchLap() canonicalLapSpan {
	span := s.beginPatchLap(patchDerived)
	if span.stamps != nil {
		s.markAt(PublicationPatchDerivedStarted, span.started)
	}
	return span
}

func (span *canonicalLapSpan) endDerived() {
	s := span.stamps
	if at := span.end(); !at.IsZero() {
		s.markAt(PublicationPatchDerivedFinished, at)
	}
}
