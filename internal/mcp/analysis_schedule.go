package mcp

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

// scheduledAnalysisState is the lifecycle-analysis edge ledger. requested is
// incremented after every graph publication; satisfied records the newest edge
// captured by a completed analysis snapshot. A hold suppresses only background
// lifecycle launches — it never suppresses an explicit on-demand request.
type scheduledAnalysisState struct {
	mu        sync.Mutex
	holds     uint64
	requested uint64
	satisfied uint64
}

type scheduledAnalysisHold struct {
	once   sync.Once
	server *Server
}

// ScheduleAnalysis implements indexer.LifecycleAnalysisScheduler. Calls are
// cheap and non-blocking: one shared background runner covers every request it
// can see at its snapshot boundary, and a request arriving later schedules at
// most one follow-up pass.
func (s *Server) ScheduleAnalysis() {
	if s == nil {
		return
	}
	s.analysisSchedule.mu.Lock()
	s.analysisSchedule.requested++
	held := s.analysisSchedule.holds > 0
	s.analysisSchedule.mu.Unlock()
	if !held {
		s.startBackgroundAnalysis("lifecycle")
	}
}

// HoldScheduledAnalysis delays lifecycle-triggered whole-graph passes until
// the outermost hold is released. It returns an idempotent release/cancel pair:
// release launches one pass when uncovered work exists; cancel removes this
// hold without launching (daemon teardown), while retaining the dirty epoch so
// a later on-demand or runtime notification cannot mistake it for current.
func (s *Server) HoldScheduledAnalysis() (release func(), cancel func()) {
	if s == nil {
		return func() {}, func() {}
	}
	s.analysisSchedule.mu.Lock()
	s.analysisSchedule.holds++
	s.analysisSchedule.mu.Unlock()

	hold := &scheduledAnalysisHold{server: s}
	return func() { hold.finish(true) }, func() { hold.finish(false) }
}

func (h *scheduledAnalysisHold) finish(release bool) {
	if h == nil || h.server == nil {
		return
	}
	h.once.Do(func() {
		s := h.server
		s.analysisSchedule.mu.Lock()
		if s.analysisSchedule.holds > 0 {
			s.analysisSchedule.holds--
		}
		launch := release && s.analysisSchedule.holds == 0 &&
			s.analysisSchedule.requested > s.analysisSchedule.satisfied
		s.analysisSchedule.mu.Unlock()
		if launch {
			s.startBackgroundAnalysis("lifecycle_startup_release")
		}
	})
}

func (s *Server) analysisScheduleSnapshotEpoch() uint64 {
	if s == nil {
		return 0
	}
	s.analysisSchedule.mu.Lock()
	defer s.analysisSchedule.mu.Unlock()
	return s.analysisSchedule.requested
}

func (s *Server) analysisScheduleMarkSatisfied(epoch uint64) {
	if s == nil {
		return
	}
	s.analysisSchedule.mu.Lock()
	if epoch > s.analysisSchedule.satisfied {
		s.analysisSchedule.satisfied = epoch
	}
	s.analysisSchedule.mu.Unlock()
}

func (s *Server) analysisSchedulePending() bool {
	if s == nil {
		return false
	}
	s.analysisSchedule.mu.Lock()
	defer s.analysisSchedule.mu.Unlock()
	return s.analysisSchedule.requested > s.analysisSchedule.satisfied
}

func (s *Server) analysisScheduleShouldLaunch() bool {
	if s == nil {
		return false
	}
	s.analysisSchedule.mu.Lock()
	defer s.analysisSchedule.mu.Unlock()
	return s.analysisSchedule.holds == 0 &&
		s.analysisSchedule.requested > s.analysisSchedule.satisfied
}

// startBackgroundAnalysis is the single launch seam shared by lifecycle and
// on-demand work. It never waits for analysis; concurrent callers join the
// same pass through analysisRun.running.
func (s *Server) startBackgroundAnalysis(reason string) bool {
	if s == nil || !s.analysisRun.running.CompareAndSwap(false, true) {
		return false
	}
	s.analysisRun.startedAt.Store(time.Now().UnixNano())
	go func() {
		started := time.Now()
		defer func() {
			s.analysisRun.lastTook.Store(int64(time.Since(started)))
			s.analysisRun.startedAt.Store(0)
			s.analysisRun.running.Store(false)
			// Store(false) is the hand-off edge. If a lifecycle notification
			// raced the completed snapshot, either its own start wins this CAS
			// or this check starts the one required follow-up — never both.
			if s.analysisScheduleShouldLaunch() {
				s.startBackgroundAnalysis("lifecycle_followup")
			}
		}()
		if s.logger != nil {
			s.logger.Info("analysis: starting background pass", zap.String("reason", reason))
		}
		s.runBackgroundAnalysis()
	}()
	return true
}

func (s *Server) runBackgroundAnalysis() {
	if s.analysisRunOverride != nil {
		epoch := s.analysisScheduleSnapshotEpoch()
		s.analysisRunOverride()
		s.analysisScheduleMarkSatisfied(epoch)
		return
	}
	if !s.backgroundAnalysisNeeded() {
		return
	}
	s.RunAnalysis()
}

// backgroundAnalysisNeeded waits behind a synchronous RunAnalysis caller, then
// rechecks both durable freshness and the lifecycle edge ledger. This closes
// the only duplicate window: a direct analysis may have satisfied the request
// while this background runner was waiting to acquire analysisMu.
func (s *Server) backgroundAnalysisNeeded() bool {
	s.analysisMu.RLock()
	defer s.analysisMu.RUnlock()
	return s.analysisSchedulePending() || !s.analysisSnapshotCurrentLocked()
}
