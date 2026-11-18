package mcp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type heldWarmHotspotStore struct {
	*store_sqlite.Store
	owner   *Server
	ctx     context.Context
	entered chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (s *heldWarmHotspotStore) BindReadContext(ctx context.Context) graph.Reader {
	return &heldWarmHotspotStore{Store: s.WithReadContext(ctx), owner: s.owner, ctx: ctx, entered: s.entered, release: s.release, once: s.once}
}

func (s *heldWarmHotspotStore) NodeIDsByKinds(kinds []graph.NodeKind) []string {
	// The ordinary analysis publication must finish first. Only the actual
	// contextual hotspot scorer over that installed generation is held.
	if s.ctx != nil && s.ctx.Done() != nil && s.owner.analysisSnapshotStillCurrent() {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.ctx.Done():
		case <-s.release:
		}
		return nil
	}
	return s.Store.NodeIDsByKinds(kinds)
}

func TestBackgroundCloseCancelsActualOutlineWarmupBeforeRankingPublication(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	s, view := newAnalysisWiringServer(t, seedAnalysisWiringStore(t))
	logs, entries := observer.New(zap.InfoLevel)
	s.logger = zap.New(logs)
	entered, release := make(chan struct{}), make(chan struct{})
	s.graph = &heldWarmHotspotStore{Store: view, owner: s, entered: entered, release: release, once: &sync.Once{}}
	pass := make(chan struct{})
	t.Cleanup(func() { s.BeginBackgroundClose(); close(release); s.DrainBackground(); <-pass })
	go func() { defer close(pass); s.RunAnalysis() }()
	awaitMaintenanceDone(t, entered)
	require.True(t, s.analysisPassActive.Load())
	s.BeginBackgroundClose()
	drained := make(chan struct{})
	go func() { s.DrainBackground(); close(drained) }()
	awaitMaintenanceDone(t, pass)
	awaitMaintenanceDone(t, drained)
	s.analysisMu.RLock()
	ready := s.hotspotsReady
	s.analysisMu.RUnlock()
	require.False(t, ready, "cancelled/empty scorer must not install a ready ranking")
	require.False(t, s.analysisPassActive.Load())
	require.Empty(t, entries.FilterMessage("mcp: analysis pass complete").All(), "cancelled preparation is not settled bootstrap")
}

type observedPreparationContext struct {
	context.Context
	calls   atomic.Int32
	waiting chan struct{}
}

func (c *observedPreparationContext) Done() <-chan struct{} {
	if c.calls.Add(1) == 2 {
		close(c.waiting)
	}
	return c.Context.Done()
}

func TestOutlineWarmupCancellationDoesNotWaitForLazyHotspotBuild(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	s, _ := newAnalysisWiringServer(t, seedAnalysisWiringStore(t))
	s.RunAnalysis()
	s.analysisMu.Lock()
	s.hotspotsReady = false
	s.analysisMu.Unlock()
	entered, release, lazyDone, warmDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var builds atomic.Int32
	s.hotspotsFn = func(graph.Reader, *analysis.CommunityResult, float64) []analysis.HotspotEntry {
		builds.Add(1)
		close(entered)
		<-release
		return []analysis.HotspotEntry{{ID: "complete-lazy-ranking"}}
	}
	base, cancel := context.WithCancel(t.Context())
	ctx := &observedPreparationContext{Context: base, waiting: make(chan struct{})}
	warmStarted := false
	t.Cleanup(func() {
		cancel()
		unblock()
		awaitMaintenanceDone(t, lazyDone)
		if warmStarted {
			awaitMaintenanceDone(t, warmDone)
		}
	})
	go func() { defer close(lazyDone); _ = s.getHotspots() }()
	awaitMaintenanceDone(t, entered)
	warmStarted = true
	go func() { defer close(warmDone); _ = s.warmOutlineHotspotsContext(ctx) }()
	awaitMaintenanceDone(t, ctx.waiting) // Own context actually reached contended wait.
	cancel()
	awaitMaintenanceDone(t, warmDone)
	select {
	case <-lazyDone:
		t.Fatal("cancelled warmup replaced joining the actual lazy owner")
	default:
	}
	s.analysisMu.RLock()
	ready := s.hotspotsReady
	s.analysisMu.RUnlock()
	require.False(t, ready)
	unblock()
	awaitMaintenanceDone(t, lazyDone)
	require.EqualValues(t, 1, builds.Load(), "cancelled waiter must not restart scoring")
	require.Equal(t, []analysis.HotspotEntry{{ID: "complete-lazy-ranking"}}, s.getHotspots(), "successful lazy publication stays unchanged")
}
