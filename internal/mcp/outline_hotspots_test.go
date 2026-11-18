package mcp

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

func TestAnalysisWarmsOneCurrentOutlineRankingAndReportsCost(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	store := seedAnalysisWiringStore(t)
	s, _ := newAnalysisWiringServer(t, store)
	logs, entries := observer.New(zap.InfoLevel)
	s.logger = zap.New(logs)
	var builds atomic.Int32
	s.hotspotsFn = func(graph.Reader, *analysis.CommunityResult, float64) []analysis.HotspotEntry {
		builds.Add(1)
		return []analysis.HotspotEntry{{ID: "pkg/overlay.go::OverlayAlpha"}}
	}
	require.Nil(t, s.outlineHotspots(), "before-first-analysis behavior stays empty")
	require.Zero(t, builds.Load())
	s.RunAnalysis()
	require.EqualValues(t, 1, builds.Load())
	require.Len(t, s.outlineHotspots(), 1)
	require.EqualValues(t, 1, builds.Load(), "outline must reuse the exact global producer")
	s.RunAnalysis()
	require.EqualValues(t, 1, builds.Load(), "a current/no-op pass must not rescore")
	complete := entries.FilterMessage("mcp: analysis pass complete").All()
	require.Len(t, complete, 1)
	require.Contains(t, complete[0].ContextMap(), "hotspots_warmup")
}

func TestOutlineWarmRankingRejectsSupersededGraph(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	store := seedAnalysisWiringStore(t)
	s, _ := newAnalysisWiringServer(t, store)
	var builds atomic.Int32
	s.hotspotsFn = func(graph.Reader, *analysis.CommunityResult, float64) []analysis.HotspotEntry {
		builds.Add(1)
		writeOverlayNode(t, s, "superseding-write")
		return []analysis.HotspotEntry{{ID: "stale"}}
	}
	s.RunAnalysis()
	require.EqualValues(t, 1, builds.Load())
	require.Nil(t, s.outlineHotspots(), "a write during global scoring must reject publication")
	s.analysisMu.RLock()
	ready := s.hotspotsReady
	s.analysisMu.RUnlock()
	require.False(t, ready)
	require.Zero(t, s.warmOutlineHotspots(), "never warm an already stale snapshot")
	require.EqualValues(t, 1, builds.Load())
}

func TestOutlineRankingReusesRevisionProofWithoutCorpusCounts(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	s, _ := newAnalysisWiringServer(t, store)
	s.graph = store
	s.RunAnalysis()
	counter := &countingAnalysisStore{Store: s.graph}
	s.graph = counter
	// The backend still supplies the same complete revision clock, so the
	// retained rank needs neither NodeCount nor EdgeCount to establish liveness.
	_ = s.outlineHotspots()
	require.Zero(t, counter.counts.Load())
}
