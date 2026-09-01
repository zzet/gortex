package mcp

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Analysis is a whole-graph pass — six analyzers over the full node and edge
// corpus — that takes minutes on a large workspace. It must therefore never
// run inline on a tool call: an agent's tool budget is far shorter than the
// pass, so blocking would guarantee a timeout with nothing to show for it.
//
// Instead a consumer that finds the snapshot missing starts the pass in the
// background and gets back a retry hint, so it can come back once the pass
// has had time to finish rather than hanging or being told to run something
// it has already run.
const (
	// analysisRetryFloor keeps a hint from suggesting a busy-poll.
	analysisRetryFloor = 10 * time.Second
	// analysisRetryCeiling keeps one hint inside a plausible agent budget;
	// a longer pass simply asks the caller back more than once.
	analysisRetryCeiling = 55 * time.Second
	// analysisRetryUnknown is the hint used before any pass has completed
	// and left a duration to extrapolate from.
	analysisRetryUnknown = 30 * time.Second
)

// analysisRunState tracks the background pass so concurrent callers start at
// most one and can be told how long it has left.
type analysisRunState struct {
	running   atomic.Bool
	startedAt atomic.Int64 // unix nanos of the in-flight pass
	lastTook  atomic.Int64 // nanos the previous pass took, 0 until one completes
}

// AnalysisAvailability is what a consumer needs to decide between answering
// now and asking the caller to come back.
type AnalysisAvailability struct {
	// Ready reports that the snapshot is current and can be read.
	Ready bool
	// Running reports that a pass is in flight — either one this call
	// started or one already under way.
	Running bool
	// RetryAfter is how long the caller should wait before asking again.
	// Zero when Ready.
	RetryAfter time.Duration
}

// Message renders the availability as an agent-facing sentence. Empty when
// the snapshot is ready.
func (a AnalysisAvailability) Message() string {
	if a.Ready {
		return ""
	}
	secs := int(a.RetryAfter.Round(time.Second) / time.Second)
	if !a.Running {
		return "graph analysis is unavailable and could not be started; retry later"
	}
	return fmt.Sprintf(
		"graph analysis is running in the background — retry this call in ~%ds. "+
			"It is a whole-graph pass, so it takes minutes on a large workspace; "+
			"there is nothing to re-index and no other action to take.", secs)
}

// retryHint converts the observed pass duration into a wait the caller can
// act on: how much of the previous pass's runtime is left, clamped so a hint
// is never a busy-poll and never outlives a plausible tool budget.
func (s *analysisRunState) retryHint() time.Duration {
	last := time.Duration(s.lastTook.Load())
	if last <= 0 {
		return analysisRetryUnknown
	}
	remaining := last
	if started := s.startedAt.Load(); started > 0 {
		if elapsed := time.Since(time.Unix(0, started)); elapsed < last {
			remaining = last - elapsed
		} else {
			// The pass is already past its previous runtime; it should
			// land soon, so ask for the floor rather than a stale estimate.
			remaining = analysisRetryFloor
		}
	}
	return min(max(remaining, analysisRetryFloor), analysisRetryCeiling)
}

// analysisPendingPayload is the body a consumer returns while the pass runs.
// The empty collection under its own key keeps the response shape identical
// to a successful call, so a caller that just iterates results sees "none
// yet" rather than a decode error, while status/retry_after_seconds carry
// the actionable part.
func analysisPendingPayload(a AnalysisAvailability, collection string) map[string]any {
	return map[string]any{
		collection:            []any{},
		"status":              "analysis_pending",
		"retry_after_seconds": int(a.RetryAfter.Round(time.Second) / time.Second),
		"message":             a.Message(),
	}
}

// ensureAnalysis reports whether the analysis snapshot can be read right now,
// starting a background pass when it cannot. It never blocks on the pass.
//
// The daemon deliberately does not compute analysis at startup: it is minutes
// of whole-graph work that most sessions never look at, and computing it
// eagerly both delays readiness and keeps the result resident. This is the
// path that replaces that eager pass.
func (s *Server) ensureAnalysis() AnalysisAvailability {
	// A lifecycle request retained behind a startup hold is stale by definition,
	// even if the last durable analysis generation still matches the currently
	// visible base graph. On-demand callers bypass the hold and start the shared
	// single-flight immediately.
	pendingLifecycle := s.analysisSchedulePending()
	if !pendingLifecycle {
		// RunAnalysis holds analysisMu for the whole pass, so a plain RLock here
		// would queue behind it and block the caller for minutes — the exact
		// failure this path exists to prevent. Failing to take the lock is
		// itself the answer: a writer holds it, so a pass is in flight.
		if !s.analysisMu.TryRLock() {
			return AnalysisAvailability{Running: true, RetryAfter: s.analysisRun.retryHint()}
		}
		ready := s.analysisSnapshotCurrentLocked()
		s.analysisMu.RUnlock()
		if ready {
			return AnalysisAvailability{Ready: true}
		}
	}

	started := s.startBackgroundAnalysis("on-demand")
	return AnalysisAvailability{
		Running:    started || s.analysisRun.running.Load(),
		RetryAfter: s.analysisRun.retryHint(),
	}
}
