package store_sqlite

import (
	"errors"
	"log"
	"sync"
	"time"
)

// WALPassiveStats describes the real periodic PASSIVE producer since open.
// Refusals are not admitted Attempts. FramesBackfilledTotal is an observed
// prefix-advance lower bound; reset/unreadable observations are counted
// separately instead of being invented as zero-progress checkpoints.
type WALPassiveStats struct {
	Attempts, Complete, Incomplete, ZeroProgress     int64
	DeferredBuildLane                                int64
	LongestBuildLaneDeferral                         time.Duration
	RefusedLease, RefusedInflight, StoodAsideReclaim int64
	YieldCancelled, Failed                           int64
	FramesBackfilledTotal, FramesUnobserved          int64
	LastOKAt                                         time.Time
	LastOutcome                                      string
}

type walPassiveState struct {
	mu             sync.Mutex
	stats          WALPassiveStats
	deferralSince  time.Time
	lastSuccessLog time.Time
}

// WALPassiveStats never queries SQL, stats files or waits on the writer/read
// pool. The independent short counter mutex is never held during I/O or logs.
func (s *Store) WALPassiveStats() WALPassiveStats {
	if s.coreless() {
		return WALPassiveStats{LastOutcome: "unavailable"}
	}
	state := &s.walPassive
	state.mu.Lock()
	out := state.stats
	if !state.deferralSince.IsZero() {
		out.LongestBuildLaneDeferral = max(out.LongestBuildLaneDeferral, time.Since(state.deferralSince))
	}
	state.mu.Unlock()
	if out.LastOutcome == "" {
		out.LastOutcome = "not_attempted"
	}
	return out
}

func (s *walPassiveState) update(fn func(*WALPassiveStats)) {
	s.mu.Lock()
	fn(&s.stats)
	s.mu.Unlock()
}

func (s *walPassiveState) deferred(now time.Time, count bool) {
	s.mu.Lock()
	if s.deferralSince.IsZero() {
		s.deferralSince = now
	}
	if count {
		s.stats.DeferredBuildLane++
	}
	s.stats.LastOutcome = "deferred_build_lane"
	s.mu.Unlock()
}

func (s *walPassiveState) endDeferral(now time.Time) {
	s.mu.Lock()
	if s.deferralSince.IsZero() {
		s.mu.Unlock()
		return
	}
	duration := max(time.Duration(0), now.Sub(s.deferralSince))
	s.stats.LongestBuildLaneDeferral = max(s.stats.LongestBuildLaneDeferral, duration)
	s.deferralSince = time.Time{}
	s.mu.Unlock()
	log.Printf("store_sqlite: wal checkpoint deferral ended mode=PASSIVE deferred_for=%s", duration.Round(time.Millisecond))
}

func (s *walPassiveState) refused(err error) {
	if errors.Is(err, errWALCheckpointYieldedToCycle) {
		s.deferred(time.Now(), true)
		return
	}
	s.update(func(st *WALPassiveStats) {
		switch {
		case errors.Is(err, errWALCheckpointDeferredBulk):
			st.RefusedLease++
			st.LastOutcome = "refused_lease"
		case errors.Is(err, errWALCheckpointInFlight):
			st.RefusedInflight++
			st.LastOutcome = "refused_inflight"
		default:
			st.Failed++
			st.LastOutcome = "admission_failed"
		}
	})
}

func (s *walPassiveState) yielded() {
	s.update(func(st *WALPassiveStats) {
		st.YieldCancelled++
		st.LastOutcome = "yield_cancelled"
	})
}

func (s *walPassiveState) observed(out passiveCheckpointResult) {
	now := time.Now()
	s.mu.Lock()
	previous := s.stats.LastOutcome
	outcome := "failed"
	switch {
	case out.stopped:
		outcome = "stopped"
	case out.complete:
		s.stats.Complete++
		s.stats.LastOKAt = now.UTC()
		outcome = "complete"
	case errors.Is(out.err, errSQLiteCheckpointIncomplete):
		s.stats.Incomplete++
		outcome = "incomplete"
	default:
		s.stats.Failed++
	}
	if out.complete || errors.Is(out.err, errSQLiteCheckpointIncomplete) {
		if out.framesKnown {
			s.stats.FramesBackfilledTotal += out.frames
			if out.result.WALFrames > 0 && out.frames == 0 {
				s.stats.ZeroProgress++
				outcome = "zero_progress"
			}
		} else {
			s.stats.FramesUnobserved++
		}
	}
	s.stats.LastOutcome = outcome
	emit := out.complete && (previous != outcome || s.lastSuccessLog.IsZero() || now.Sub(s.lastSuccessLog) >= time.Minute)
	if emit {
		s.lastSuccessLog = now
	}
	s.mu.Unlock()
	if emit {
		log.Printf("store_sqlite: wal checkpoint complete mode=PASSIVE outcome=%s frames_advanced=%d frames_observed=%t", outcome, out.frames, out.framesKnown)
	}
}
