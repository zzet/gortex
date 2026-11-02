package indexer

import "time"

// EditResolveMutexWait is a diagnostic subset of resolve, not another disjoint
// patch lap. Milliseconds sums the observed acquisitions of this execution.
type EditResolveMutexWait struct {
	Milliseconds *float64 `json:"milliseconds"`
	State        string   `json:"state"`
	SubsetOf     string   `json:"subset_of"`
	Acquisitions uint64   `json:"acquisitions"`
}

type mutationResolveMutexWait struct {
	total                 time.Duration
	acquisitions          uint64
	first, last           time.Time
	spanStarted, spanLast time.Time
	invalid               bool
}

type mutationResolveMutexEvidence struct {
	totalNS, firstNS, lastNS int64
	acquisitions             uint64
	invalid, incomplete      bool
}

// Only the existing canonical physical P identity may own this diagnostic.
// The separate read-repair collector intentionally has no such ticket/origin.
func (s *PublicationStamps) mutationResolveMutexObserver() func(time.Time, time.Time) {
	if s == nil || s.patchLaps == nil || s.patchLaps.ticket == 0 || s.patchLaps.origin.IsZero() {
		return nil
	}
	return s.observeMutationResolveMutexWait
}

func (s *PublicationStamps) observeMutationResolveMutexWait(started, acquired time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.patchLaps
	d := &p.resolveMutexWait
	if d.acquisitions == ^uint64(0) {
		d.invalid = true
		return
	}
	d.acquisitions++
	if p.active != 1 || p.activeKind != patchResolve || started.IsZero() || acquired.IsZero() || acquired.Before(started) || started.Before(p.activeStarted) {
		d.invalid = true
		return
	}
	elapsed := acquired.Sub(started)
	if elapsed < 0 || d.total > time.Duration(1<<63-1)-elapsed {
		d.invalid = true
		return
	}
	d.total += elapsed
	if d.first.IsZero() || started.Before(d.first) {
		d.first = started
	}
	if acquired.After(d.last) {
		d.last = acquired
	}
	if d.spanStarted.IsZero() {
		d.spanStarted = p.activeStarted
	}
	if d.spanStarted != p.activeStarted {
		d.invalid = true
	}
	if acquired.After(d.spanLast) {
		d.spanLast = acquired
	}
}

// Validate each span separately; a later resolve span must not make an
// acquisition in an intervening bookkeeping gap look enclosed.
func (d *mutationResolveMutexWait) finishResolveSpan(started, ended time.Time) {
	if !d.spanStarted.IsZero() && (d.spanStarted != started || d.spanLast.After(ended)) {
		d.invalid = true
	}
	d.spanStarted, d.spanLast = time.Time{}, time.Time{}
}

func (d mutationResolveMutexWait) freeze(origin time.Time) mutationResolveMutexEvidence {
	out := mutationResolveMutexEvidence{totalNS: int64(d.total), acquisitions: d.acquisitions, invalid: d.invalid, incomplete: !d.spanStarted.IsZero()}
	if !d.first.IsZero() {
		out.firstNS = d.first.Sub(origin).Nanoseconds()
		out.lastNS = d.last.Sub(origin).Nanoseconds()
	}
	return out
}

func (d mutationResolveMutexEvidence) project(resolve canonicalLapEvidence, enclosingInvalid bool) *EditResolveMutexWait {
	if d.acquisitions == 0 && !d.invalid {
		return nil
	}
	out := &EditResolveMutexWait{State: "not_observed", SubsetOf: "resolve", Acquisitions: d.acquisitions}
	if d.invalid || enclosingInvalid || d.firstNS < resolve.firstNS || d.lastNS > resolve.lastNS || d.totalNS < 0 {
		out.State = "invalid_order"
	} else if !d.incomplete && resolve.begun > 0 && resolve.begun == resolve.done && !resolve.incomplete {
		ms := float64(d.totalNS) / 1e6
		out.Milliseconds, out.State = &ms, "measured"
	}
	return out
}

// Keep the original P evidence equality: diagnostic subsets never establish
// or invalidate the disjoint accounting witness.
func (p canonicalPatchEvidence) sameAccounting(other canonicalPatchEvidence) bool {
	return p.laps == other.laps && p.invalid == other.invalid
}
