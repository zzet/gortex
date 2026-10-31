package indexer

import (
	"context"
	"time"
)

// A single canonical point attempt owns this pair. It precedes the executor,
// so it is deliberately separate from the executor-contained patch laps.
type canonicalLaneWait struct {
	started, acquired time.Time
	invalid           bool
}

type canonicalLaneWaitEvidence struct {
	startNS, acquiredNS        int64
	started, acquired, invalid bool
}

func beginCanonicalLaneWait(ctx context.Context) *PublicationStamps {
	s := PublicationStampsFrom(ctx)
	if s == nil || s.patchLaps == nil {
		return nil // no clock/allocation on other paths
	}
	s.recordCanonicalLaneWaitAt(false, time.Now())
	return s
}

func (s *PublicationStamps) canonicalLaneAcquired() {
	if s == nil {
		return
	}
	s.recordCanonicalLaneWaitAt(true, time.Now())
}

func (s *PublicationStamps) recordCanonicalLaneWaitAt(acquired bool, at time.Time) {
	if s == nil || s.patchLaps == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pair := &s.laneWait
	if at.IsZero() {
		pair.invalid = true
		return
	}
	if !acquired {
		if !pair.started.IsZero() {
			pair.invalid = true
			return
		}
		pair.started = at
		s.patchLaps.beginObservedOuterLocked(patchRepositoryLane, at)
		return
	}
	if pair.started.IsZero() || !pair.acquired.IsZero() || at.Before(pair.started) {
		pair.invalid = true
		return
	}
	pair.acquired = at
	s.patchLaps.endObservedOuterLocked(patchRepositoryLane, pair.started, at)
}

func (p canonicalLaneWait) freeze(origin time.Time) canonicalLaneWaitEvidence {
	out := canonicalLaneWaitEvidence{started: !p.started.IsZero(), acquired: !p.acquired.IsZero(), invalid: p.invalid}
	if out.started {
		out.startNS = p.started.Sub(origin).Nanoseconds()
	}
	if out.acquired {
		out.acquiredNS = p.acquired.Sub(origin).Nanoseconds()
	}
	return out
}

func (p canonicalLaneWaitEvidence) project(offsets map[PublicationPhase]int64, terminal bool, out *EditPhaseTimings) {
	out.Milliseconds["lane_wait"], out.States["lane_wait"] = nil, "not_observed"
	if p.invalid || p.started && p.startNS < 0 || p.acquired && (!p.started || p.acquiredNS < p.startNS) {
		out.States["lane_wait"] = "invalid_order"
		return
	}
	if !p.started {
		return
	}
	if !p.acquired {
		if !terminal {
			out.States["lane_wait"] = "pending"
		}
		return
	}
	enqueue, hasEnqueue := offsets[PublicationTicketEnqueued]
	cycle, hasCycle := offsets[PublicationCycleStarted]
	executor, hasExecutor := offsets[PublicationPatchExecutionStarted]
	if !hasEnqueue || !hasCycle || !hasExecutor {
		return
	}
	if enqueue < 0 || cycle < enqueue || p.startNS < cycle || p.acquiredNS > executor {
		out.States["lane_wait"] = "invalid_order"
		return
	}
	if terminal {
		last := PublicationTicketCompleted
		if _, failed := offsets[PublicationTicketFailed]; failed {
			last = PublicationTicketFailed
		}
		finished, known := offsets[last]
		if !known {
			return
		}
		if p.acquiredNS > finished {
			out.States["lane_wait"] = "invalid_order"
			return
		}
	}
	// Only a genuine wholly contained pair is additive. This is not the cohort
	// semaphore, batch RLock, cycle-to-admitted envelope or a full patch wall.
	ms := float64(p.acquiredNS-p.startNS) / 1e6
	out.Milliseconds["lane_wait"], out.States["lane_wait"] = &ms, "measured"
}
