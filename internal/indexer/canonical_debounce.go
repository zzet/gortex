package indexer

import "time"

// Only the initial exact-generation timer owns this quiet-wait pair. Retry
// backoff and the callback's claim lock/cohort admission are different work.
type canonicalDebounceWait struct {
	started, entered time.Time
}

type canonicalDebounceEvidence struct {
	startedNS, enteredNS int64
	entered              bool
}

func (p canonicalDebounceWait) freeze(origin time.Time) canonicalDebounceEvidence {
	return canonicalDebounceEvidence{startedNS: p.started.Sub(origin).Nanoseconds(), enteredNS: p.entered.Sub(origin).Nanoseconds(), entered: !p.entered.IsZero()}
}

func (p canonicalDebounceEvidence) project(offsets map[PublicationPhase]int64, terminal bool, out *EditPhaseTimings) {
	out.Milliseconds["debounce"], out.States["debounce"] = nil, "not_observed"
	if p.startedNS < 0 || p.entered && p.enteredNS < p.startedNS {
		out.States["debounce"] = "invalid_order"
		return
	}
	if !p.entered {
		if !terminal {
			out.States["debounce"] = "pending"
		}
		return
	}
	enqueue, hasEnqueue := offsets[PublicationTicketEnqueued]
	if !hasEnqueue {
		// A native serving record has no request-admission timestamp. Its
		// independently observed arm start is the applicable lower boundary.
		enqueue, hasEnqueue = offsets[PublicationTimerArmStarted]
	}
	cycle, hasCycle := offsets[PublicationCycleStarted]
	if !hasEnqueue || !hasCycle {
		return
	}
	if enqueue < 0 || p.startedNS < enqueue || p.enteredNS > cycle {
		out.States["debounce"] = "invalid_order"
		return
	}
	if terminal {
		last := PublicationTicketCompleted
		if _, failed := offsets[PublicationTicketFailed]; failed {
			last = PublicationTicketFailed
		}
		end, known := offsets[last]
		if !known {
			return
		}
		if p.enteredNS > end {
			out.States["debounce"] = "invalid_order"
			return
		}
	}
	ms := float64(p.enteredNS-p.startedNS) / 1e6
	out.Milliseconds["debounce"], out.States["debounce"] = &ms, "measured"
}
