package mcp

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/runtimeactivity"
)

// The analysis pass (RunAnalysis) is a whole-graph computation: six
// sub-analyses (Leiden communities, processes, PageRank, the adjacency
// snapshot, auto concepts, HITS) plus the durable save, each a full scan of the
// corpus. On the live store a pass that computes takes 12–25 minutes per
// attempt, and it used to run under analysisMu from start to end, so every
// reader of the analysis snapshot — symbol search, communities, processes,
// hotspots, impact — waited for the whole of it.
//
// Four rules bound it now:
//
//   - the pass computes without analysisMu; only the install takes it, for as
//     long as a few field stores. Readers answer from the installed snapshot
//     while it matches the graph, and without analysis signals once it does
//     not — which is what they answered before the first pass;
//   - a pass over a graph the installed snapshot still matches is a no-op
//     (analysisSnapshotStillCurrent), decided from the store's write revision
//     without counting the corpus;
//   - a pass is interruptible between sub-analyses: once a graph write has
//     moved the store's analysis revision, the result could never be
//     published (CommitAnalysisSnapshot refuses it), so the attempt stops at
//     the next checkpoint instead of computing the remaining sub-analyses for
//     nothing;
//   - a background pass yields between sub-analyses while an edit cycle holds
//     the build lane or a tool call is in flight, up to analysisYieldCap per
//     checkpoint so a busy daemon still finishes the pass.

// analysisYieldPoll is how often a yielding pass looks again.
const analysisYieldPoll = 25 * time.Millisecond

// analysisYieldCap bounds one checkpoint's wait.
const analysisYieldCap = 30 * time.Second

// analysisStageHook, when set, runs at every checkpoint before the yield and
// the revision check. Tests only.
var analysisStageHook func(stage string)

// analysisSnapshotStillCurrent reports whether the installed snapshot still
// describes the graph, so a pass would compute (or load) the same thing again.
// It reads the lock-free answer receipt and the revision-gated token: no
// corpus count while the graph has not been written.
func (s *Server) analysisSnapshotStillCurrent() bool {
	if !s.analysisMu.TryRLock() {
		return false
	}
	epoch, token := s.analysisEpoch, s.communitiesToken
	ready, header := s.analysisGenerationReady, s.analysisGeneration
	hasSnapshot := s.communities != nil
	s.analysisMu.RUnlock()
	if epoch == 0 {
		return false
	}
	writer, _ := s.analysisGenerationBackends()
	if writer != nil {
		if !ready || header.GenerationID <= 0 || header.FormatVersion != analysisGenerationFormatVersion ||
			header.GraphRevision != token.analysisRevision {
			return false
		}
	} else if !hasSnapshot {
		return false
	}
	return token == s.communityTokenForAnswer()
}

// analysisPassControl is one attempt's checkpoint state.
type analysisPassControl struct {
	s          *Server
	writer     graph.AnalysisGenerationStore
	expected   uint64
	background bool
	metrics    *analysisRunMetrics
}

// checkpoint runs between two sub-analyses. It reports false when the attempt
// is superseded: the store was written since the attempt read its revision.
func (c *analysisPassControl) checkpoint(stage string) bool {
	if hook := analysisStageHook; hook != nil {
		hook(stage)
	}
	if c.background {
		c.s.yieldAnalysisPass(c.metrics)
	}
	if c.writer != nil && c.writer.AnalysisMutationRevision() != c.expected {
		return false
	}
	return true
}

// analysisShouldYield reports whether a background pass should wait: an edit
// cycle holds the build lane, or a tool call is in flight.
func (s *Server) analysisShouldYield() bool {
	if yield := s.analysisYield; yield != nil && yield() {
		return true
	}
	if s.editLaneBusy() {
		return true
	}
	return runtimeactivity.Current().ByKind["mcp"] > 0
}

// yieldAnalysisPass waits, up to analysisYieldCap, while the pass should yield.
func (s *Server) yieldAnalysisPass(metrics *analysisRunMetrics) {
	if !s.analysisShouldYield() {
		return
	}
	started := time.Now()
	metrics.yields++
	for s.analysisShouldYield() && time.Since(started) < analysisYieldCap {
		time.Sleep(analysisYieldPoll)
	}
	metrics.yielded += time.Since(started)
}

// editLaneBusyForTest, when set, stands in for the lifecycle's edit-cycle
// signal. Tests only.
var editLaneBusyForTest func() bool

// editLaneBusy reports whether an edit cycle holds the build lane.
func (s *Server) editLaneBusy() bool {
	if hook := editLaneBusyForTest; hook != nil {
		return hook()
	}
	lc := s.lifecycle
	return lc != nil && lc.EditCycleActive()
}

// analysisPruneLaneWait bounds how long the generation prune waits for an edit
// cycle to release the build lane before skipping its round.
var analysisPruneLaneWait = 2 * time.Minute

// analysisPruneStoodDown counts prune rounds skipped for a busy lane (tests
// and diagnostics).
var analysisPruneStoodDown atomic.Int64

// waitForQuietEditLane waits, up to limit, while an edit cycle holds the build
// lane, and reports whether the lane is free.
func (s *Server) waitForQuietEditLane(limit time.Duration) bool {
	started := time.Now()
	for s.editLaneBusy() {
		if time.Since(started) >= limit {
			return false
		}
		time.Sleep(analysisYieldPoll)
	}
	return true
}

// cancelWhenEditCycleStarts calls cancel as soon as an edit cycle holds the
// build lane; stop ends the watch.
func (s *Server) cancelWhenEditCycleStarts(cancel context.CancelFunc) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(analysisYieldPoll)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if s.editLaneBusy() {
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}
