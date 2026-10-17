package indexer

import (
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// The lifecycle's analysis lane.
//
// A tracked-set change reruns the query surface's graph-wide rollups
// (LifecycleNotifier.RunAnalysis): a whole-graph pass, tens of seconds on a
// large workspace. It used to run inline, on whichever goroutine noticed the
// change — a family reconcile's retry timer, the coordinator that observed a
// worktree go away — and on a daemon with one core it starved edit cycles and
// timed out tool calls while it ran. Two rules now bound it:
//
//   - scope: a family reconcile that forgot a checkout without changing the
//     tracked repository set (an automatic worktree added or removed next to
//     its primary) changes nothing the rollups read, so it invalidates the
//     session scopes and runs no analysis. Only a change of the tracked
//     repository set (a repository tracked, evicted or retired) reruns them;
//   - lane: the analysis never runs on the caller. Requests are coalesced onto
//     one low-priority worker, which starts the pass only after a quiet
//     interval and only while no edit cycle holds the build lane (the same
//     predicate the background checkpoints yield to), re-checking before it
//     starts.

// analysisRequest is one reason to rerun the rollups. baseline is the tracked
// repository set's fingerprint when the change began; an empty baseline is a
// change of the set itself (a track, an eviction, a removal sweep).
type analysisRequest struct {
	unconditional bool
	baselines     map[string]struct{}
}

func (r *analysisRequest) merge(o analysisRequest) {
	if o.unconditional {
		r.unconditional = true
	}
	for b := range o.baselines {
		if r.baselines == nil {
			r.baselines = make(map[string]struct{})
		}
		r.baselines[b] = struct{}{}
	}
}

func (r analysisRequest) empty() bool {
	return !r.unconditional && len(r.baselines) == 0
}

// lifecycleAnalysisStats is what the lane did, for tests and diagnostics.
type lifecycleAnalysisStats struct {
	Requests         int
	Runs             int
	SkippedUnchanged int
	EditCycleWaits   int
}

// lifecycleAnalysisLane is the single low-priority worker the lifecycle's
// analysis requests coalesce onto.
type lifecycleAnalysisLane struct {
	mu      sync.Mutex
	pending analysisRequest
	running bool
	closed  bool
	wake    chan struct{}
	done    chan struct{}
	started bool
	stats   lifecycleAnalysisStats
	// idle is closed and replaced whenever the lane has nothing pending and
	// nothing running (waitAnalysisIdle).
	idle       chan struct{}
	idleClosed bool
	// stop is closed by closeAnalysisLane.
	stop chan struct{}
}

// analysisQuiet is how long the lane waits after the last request before it
// starts a pass, so a burst of topology events costs one pass.
var analysisQuiet = 2 * time.Second

// analysisEditCyclePoll is how often a lane waiting out an edit cycle looks
// again.
var analysisEditCyclePoll = 50 * time.Millisecond

func newLifecycleAnalysisLane() *lifecycleAnalysisLane {
	idle := make(chan struct{})
	close(idle)
	return &lifecycleAnalysisLane{wake: make(chan struct{}, 1), done: make(chan struct{}), idle: idle, idleClosed: true}
}

// repoSetFingerprint is the tracked repository set as the rollups read it:
// every indexed repository's prefix and root.
func (l *CheckoutLifecycle) repoSetFingerprint() string {
	if l == nil || l.mi == nil {
		return ""
	}
	meta := l.mi.AllMetadata()
	rows := make([]string, 0, len(meta))
	for prefix, m := range meta {
		root := ""
		if m != nil {
			root = m.RootPath
		}
		rows = append(rows, prefix+"\x00"+root)
	}
	sort.Strings(rows)
	return strings.Join(rows, "\x01")
}

// editCycleHoldsBuildLane is the edit-cycle predicate over the lifecycle's
// build gate: an edit's synchronous republish, or a checkout cycle admitted
// at interactive priority, holds the lane.
func (l *CheckoutLifecycle) editCycleHoldsBuildLane() bool {
	if l.analysisEditCycle != nil {
		return l.analysisEditCycle()
	}
	gate := l.buildGate()
	if gate == nil {
		return false
	}
	st := gate.Stats()
	if !st.Active || st.Holder == nil {
		return false
	}
	switch st.Holder.Kind {
	case "checkout_mutation":
		return true
	case "checkout_cycle":
		return st.Holder.Priority == viewmetrics.BuildPriorityInteractive
	}
	return false
}

// EditCycleActive reports the edit-cycle predicate (editCycleHoldsBuildLane)
// to the analysis pass, which yields to it between its sub-analyses.
func (l *CheckoutLifecycle) EditCycleActive() bool {
	if l == nil {
		return false
	}
	return l.editCycleHoldsBuildLane()
}

// requestAnalysis hands a rollup rerun to the lane. It never runs the pass on
// the caller.
func (l *CheckoutLifecycle) requestAnalysis(req analysisRequest) {
	lane := l.analysis
	lane.mu.Lock()
	if lane.closed {
		lane.mu.Unlock()
		return
	}
	lane.stats.Requests++
	lane.pending.merge(req)
	if lane.idleClosed {
		lane.idle = make(chan struct{})
		lane.idleClosed = false
	}
	if !lane.started {
		lane.started = true
		go l.runAnalysisLane()
	}
	lane.mu.Unlock()
	select {
	case lane.wake <- struct{}{}:
	default:
	}
}

// runAnalysisLane is the lane's worker.
func (l *CheckoutLifecycle) runAnalysisLane() {
	lane := l.analysis
	defer close(lane.done)
	for {
		select {
		case <-lane.wake:
		case <-lane.stopped():
			return
		}
		// Quiet interval: further requests restart it.
		for quiet := true; quiet; {
			select {
			case <-lane.wake:
			case <-time.After(analysisQuiet):
				quiet = false
			case <-lane.stopped():
				return
			}
		}
		// Low priority: never start while an edit cycle holds the build lane.
		waited := false
		for l.editCycleHoldsBuildLane() {
			waited = true
			select {
			case <-time.After(analysisEditCyclePoll):
			case <-lane.stopped():
				return
			}
		}
		lane.mu.Lock()
		if waited {
			lane.stats.EditCycleWaits++
		}
		req := lane.pending
		lane.pending = analysisRequest{}
		lane.running = !req.empty()
		lane.mu.Unlock()
		if !req.empty() {
			l.runRequestedAnalysis(req)
		}
		lane.mu.Lock()
		lane.running = false
		if lane.pending.empty() && !lane.idleClosed {
			close(lane.idle)
			lane.idleClosed = true
		}
		lane.mu.Unlock()
	}
}

// runRequestedAnalysis runs the rollups when the request's scope reaches
// them: unconditionally for a tracked-set change, and for a family change
// only when the tracked repository set moved since the change began.
func (l *CheckoutLifecycle) runRequestedAnalysis(req analysisRequest) {
	lane := l.analysis
	if !req.unconditional {
		current := l.repoSetFingerprint()
		moved := false
		for b := range req.baselines {
			if b != current {
				moved = true
				break
			}
		}
		if !moved {
			lane.mu.Lock()
			lane.stats.SkippedUnchanged++
			lane.mu.Unlock()
			return
		}
	}
	l.mu.RLock()
	notifier := l.notifier
	l.mu.RUnlock()
	if notifier == nil {
		return
	}
	started := time.Now()
	notifier.RunAnalysis()
	lane.mu.Lock()
	lane.stats.Runs++
	lane.mu.Unlock()
	l.logger.Info("checkout lifecycle: tracked-set analysis ran on the maintenance lane",
		zap.Duration("elapsed", time.Since(started)))
}

func (lane *lifecycleAnalysisLane) stopped() <-chan struct{} {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if lane.closed {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return lane.stopCh()
}

// stopCh is the channel close closes; created lazily under mu.
func (lane *lifecycleAnalysisLane) stopCh() chan struct{} {
	if lane.stop == nil {
		lane.stop = make(chan struct{})
	}
	return lane.stop
}

// closeAnalysisLane stops the lane and joins its worker. A pending request is
// dropped: the process is going away.
func (l *CheckoutLifecycle) closeAnalysisLane() {
	lane := l.analysis
	if lane == nil {
		return
	}
	lane.mu.Lock()
	if lane.closed {
		lane.mu.Unlock()
		return
	}
	lane.closed = true
	close(lane.stopCh())
	started := lane.started
	lane.mu.Unlock()
	if started {
		<-lane.done
	}
}

// waitAnalysisIdle waits until the lane has nothing pending or running, or
// the timeout passes; it reports whether the lane went idle. A test seam.
func (l *CheckoutLifecycle) waitAnalysisIdle(timeout time.Duration) bool {
	lane := l.analysis
	lane.mu.Lock()
	idle := lane.idle
	lane.mu.Unlock()
	select {
	case <-idle:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (l *CheckoutLifecycle) analysisStats() lifecycleAnalysisStats {
	lane := l.analysis
	lane.mu.Lock()
	defer lane.mu.Unlock()
	return lane.stats
}
