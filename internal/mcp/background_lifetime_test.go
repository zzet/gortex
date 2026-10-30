package mcp

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func awaitMaintenanceDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("admitted maintenance owner did not actually finish")
	}
}

func TestBackgroundCloseCancelsAnalysisWithoutPublishingOrDroppingItsOwner(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	s, _ := newAnalysisWiringServer(t, store)
	s.RunAnalysis()
	epoch, previous := s.analysisEpoch, s.analysisGeneration
	writeOverlayNode(t, s, "ForceShutdownAttempt")
	sender := &fakeSpecificSender{}
	s.graphInvalidatedBroadcaster = newGraphInvalidatedBroadcaster(sender, zap.NewNop())
	s.graphInvalidatedBroadcaster.subscribe("shutdown")
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	unlock := func() { unblock.Do(func() { close(release) }) }
	t.Cleanup(unlock)
	var stages atomic.Int32
	setAnalysisStageHook(t, func(stage string) {
		stages.Add(1)
		if stage == "load" {
			close(entered)
			<-release
		}
	})
	t.Cleanup(func() { s.BeginBackgroundClose(); unlock(); s.DrainBackground() })
	pass := make(chan struct{})
	go func() { defer close(pass); s.RunAnalysis() }()
	awaitMaintenanceDone(t, entered)
	s.BeginBackgroundClose() // Must return despite the held pass hook.
	drained := make(chan struct{}, 8)
	for range 8 {
		go func() { s.DrainBackground(); drained <- struct{}{} }()
	}
	select {
	case <-drained:
		t.Fatal("cancellation was substituted for actual pass completion")
	default:
	}
	require.False(t, s.startBackgroundAnalysis("late"))
	unlock()
	awaitMaintenanceDone(t, pass)
	for range 8 {
		awaitMaintenanceDone(t, drained)
	}
	require.Equal(t, epoch, s.analysisEpoch, "canceled computation installed a partial/empty replacement")
	require.Equal(t, previous, s.analysisGeneration)
	require.Empty(t, sender.snapshot(), "cancellation emitted a settled graph receipt")
	require.Equal(t, int32(1), stages.Load(), "canceled attempt retried or ran a later stage")
	s.RunAnalysis()
	require.Equal(t, int32(1), stages.Load(), "terminal close admitted a synchronous pass")
	require.False(t, s.analysisPassActive.Load())
}

func TestBackgroundCloseOwnsAnalysisAlreadyWaitingForSerialPassLock(t *testing.T) {
	s, _ := newAnalysisWiringServer(t, seedAnalysisWiringStore(t))
	s.analysisRunMu.Lock()
	var once sync.Once
	unlock := func() { once.Do(s.analysisRunMu.Unlock) }
	t.Cleanup(unlock)
	pass := make(chan struct{})
	go func() { defer close(pass); s.RunAnalysis() }()
	require.Eventually(t, func() bool {
		s.backgroundMaintenanceMu.Lock()
		defer s.backgroundMaintenanceMu.Unlock()
		return s.backgroundContext != nil
	}, 3*time.Second, time.Millisecond)
	s.BeginBackgroundClose()
	drained := make(chan struct{})
	go func() { s.DrainBackground(); close(drained) }()
	select {
	case <-drained:
		t.Fatal("serial-lock waiter escaped ownership")
	default:
	}
	unlock()
	awaitMaintenanceDone(t, pass)
	awaitMaintenanceDone(t, drained)
	require.Zero(t, s.analysisEpoch, "canceled queued pass still computed/published")
}

func TestBackgroundAnalysisCoalescerStopsAgainLoopOnTerminalClose(t *testing.T) {
	s, _ := newAnalysisWiringServer(t, seedAnalysisWiringStore(t))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unlock)
	var stages atomic.Int32
	setAnalysisStageHook(t, func(string) { stages.Add(1); close(entered); <-release })
	t.Cleanup(func() { s.BeginBackgroundClose(); unlock(); s.DrainBackground() })
	require.True(t, s.startBackgroundAnalysis("first"))
	awaitMaintenanceDone(t, entered)
	require.False(t, s.startBackgroundAnalysis("coalesced"))
	s.BeginBackgroundClose()
	unlock()
	s.DrainBackground()
	require.Equal(t, int32(1), stages.Load())
	require.False(t, s.analysisRun.running.Load())
	require.Zero(t, s.analysisRun.startedAt.Load())
	require.False(t, s.startBackgroundAnalysis("after close"))
}

type heldCochangeRead struct {
	graph.Store
	entered, release, exited chan struct{}
	scans                    atomic.Int32
}

func (g *heldCochangeRead) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		g.scans.Add(1)
		close(g.entered)
		defer close(g.exited)
		<-g.release // Legacy iterator: cancellation cannot replace its actual return.
		for e := range g.Store.EdgesByKind(kind) {
			if !yield(e) {
				return
			}
		}
	}
}

func TestBackgroundCloseJoinsTheSingleCochangeReaderAndRefusesLateMine(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{{ID: "a", Kind: graph.KindFile, FilePath: "a.go"}, {ID: "b", Kind: graph.KindFile, FilePath: "b.go"}}, []*graph.Edge{{From: "a", To: "b", Kind: graph.EdgeCoChange, Confidence: 1}})
	g := &heldCochangeRead{Store: base, entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
	s := &Server{graph: g, logger: zap.NewNop()}
	var once sync.Once
	unlock := func() { once.Do(func() { close(g.release) }) }
	t.Cleanup(func() { unlock(); s.DrainBackground() })
	var starters sync.WaitGroup
	for range 8 {
		starters.Add(1)
		go func() { defer starters.Done(); s.PrewarmCoChange(); s.ensureCoChange() }()
	}
	starters.Wait()
	awaitMaintenanceDone(t, g.entered)
	s.BeginBackgroundClose()
	drained := make(chan struct{})
	go func() { s.DrainBackground(); close(drained) }()
	select {
	case <-drained:
		t.Fatal("cochange reader still owns the store")
	default:
	}
	unlock()
	awaitMaintenanceDone(t, drained)
	select {
	case <-g.exited:
	default:
		t.Fatal("mine completion did not include iterator return")
	}
	require.False(t, s.coChangeReady(), "truncated read became a complete cache")
	s.PrewarmCoChange()
	s.mineCoChange()
	require.Equal(t, int32(1), g.scans.Load())
}

func TestBackgroundCloseCancelsPruneAndFTSQuietLaneWaits(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	var busy atomic.Bool
	busy.Store(true)
	withEditLane(t, &busy)
	synctest.Test(t, func(t *testing.T) {
		s := &Server{logger: zap.NewNop()}
		t.Cleanup(s.DrainBackground)
		writer := &pruneRecorder{}
		materializer := &graphview.Materializer{Store: store}
		s.scheduleAnalysisGenerationPrune(writer)
		s.queueRouteFTSWarm(materializer, []int64{1, 2})
		synctest.Wait() // Both admitted owners are parked, not doing store work.
		before := time.Now()
		s.BeginBackgroundClose()
		s.DrainBackground()
		require.Less(t, time.Since(before), time.Second, "terminal drain waited for a lane timeout")
		require.Zero(t, writer.calls.Load())
		require.False(t, s.analysisPruneScheduled.Load())
		s.routeFTSWarm.mu.Lock()
		running, pending := s.routeFTSWarm.running, len(s.routeFTSWarm.pending)
		s.routeFTSWarm.mu.Unlock()
		require.False(t, running)
		require.Zero(t, pending)
		s.queueRouteFTSWarm(materializer, []int64{3, 4})
		s.scheduleAnalysisGenerationPrune(writer)
		synctest.Wait()
		require.Zero(t, writer.calls.Load())
		require.False(t, s.routeFTSWarm.running)
	})
}

type canceledGenerationWriter struct {
	graph.AnalysisGenerationStore
	cancel                      context.CancelFunc
	abortErr                    error
	chunks, aborts, activations int
}

func (*canceledGenerationWriter) BeginAnalysisGeneration(uint64, graph.AnalysisGenerationHeader) (int64, bool, error) {
	return 1, true, nil
}
func (w *canceledGenerationWriter) AppendAnalysisCommunities(uint64, int64, []graph.AnalysisCommunitySummary) (bool, error) {
	w.chunks++
	w.cancel()
	return true, nil
}
func (w *canceledGenerationWriter) AbortAnalysisGeneration(int64) error {
	w.aborts++
	return w.abortErr
}
func (w *canceledGenerationWriter) ActivateAnalysisGeneration(uint64, int64) (bool, error) {
	w.activations++
	return true, nil
}

func TestCanceledAnalysisSaveStopsNewChunksAndRetainsActualAbortError(t *testing.T) {
	// This checks caller chunk/publication policy, not physical SQLite cancellation.
	g := graph.New()
	g.AddBatch([]*graph.Node{{ID: "a", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", Language: "go"}, {ID: "b", Kind: graph.KindFunction, Name: "B", FilePath: "a.go", Language: "go"}}, []*graph.Edge{{From: "a", To: "b", Kind: graph.EdgeCalls}})
	s := &Server{graph: g, logger: zap.NewNop()}
	s.populateAnalysis(false, false)
	candidate := persistedAnalysis{communities: s.communities, leiden: s.leidenCache, processes: s.processes, pageRank: s.pageRank, adjacency: s.adjacency, autoConcepts: s.autoConcepts, hits: s.hits}
	require.NotNil(t, candidate.communities)
	require.NotEmpty(t, candidate.communities.Communities, "fixture must reach a real first save chunk")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	abortErr := errors.New("actual generation abort failed")
	writer := &canceledGenerationWriter{cancel: cancel, abortErr: abortErr}
	_, accepted, err := persistAnalysisGenerationContext(ctx, writer, 0, candidate)
	require.False(t, accepted)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, abortErr)
	require.Equal(t, 1, writer.chunks)
	require.Equal(t, 1, writer.aborts)
	require.Zero(t, writer.activations)
	s.DrainBackground()
}

func TestClosedMaintenanceRefusesExplicitControlCochangeBeforeStoreAccess(t *testing.T) {
	s := &Server{} // Deliberately no graph; any store access is invalid.
	s.BeginBackgroundClose()
	_, _, err := s.EnrichCochange(context.Background(), "repo", t.TempDir())
	require.ErrorIs(t, err, context.Canceled)
	s.DrainBackground()
}

func TestBackgroundCloseJoinsAdmittedFTSReaderBeforeDrainReturns(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	s := &Server{logger: zap.NewNop()}
	entered, release := make(chan struct{}), make(chan struct{})
	previous := routeFTSWarmGateForTest
	routeFTSWarmGateForTest = func() { close(entered); <-release }
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { s.BeginBackgroundClose(); unlock(); s.DrainBackground(); routeFTSWarmGateForTest = previous })
	materializer := &graphview.Materializer{Store: store}
	s.queueRouteFTSWarm(materializer, []int64{1, 2})
	awaitMaintenanceDone(t, entered)
	s.BeginBackgroundClose()
	drained := make(chan struct{})
	go func() { s.DrainBackground(); close(drained) }()
	select {
	case <-drained:
		t.Fatal("FTS worker callback escaped actual completion")
	default:
	}
	unlock()
	awaitMaintenanceDone(t, drained)
	s.routeFTSWarm.mu.Lock()
	running := s.routeFTSWarm.running
	s.routeFTSWarm.mu.Unlock()
	require.False(t, running)
}

// relationCancelWriter isolates save-chunk admission, not SQLite cancellation.
// Earlier components are accepted; cancellation follows the first actual
// relation flush, so an outer-loop-only check cannot pass this regression.
type relationCancelWriter struct {
	canceledGenerationWriter
	relationChunks, relationRows int
}

func (*relationCancelWriter) AppendAnalysisCommunities(uint64, int64, []graph.AnalysisCommunitySummary) (bool, error) {
	return true, nil
}
func (*relationCancelWriter) AppendAnalysisNodes(uint64, int64, []graph.AnalysisNodeMetric) (bool, error) {
	return true, nil
}
func (*relationCancelWriter) AppendAnalysisProcesses(uint64, int64, []graph.AnalysisProcessSummary, []graph.AnalysisProcessStep) (bool, error) {
	return true, nil
}
func (*relationCancelWriter) SealAnalysisComponent(uint64, int64, graph.AnalysisComponent, int) (bool, error) {
	return true, nil
}
func (w *relationCancelWriter) AppendAnalysisConcepts(_ uint64, _ int64, _ []graph.AnalysisConcept, relations []graph.AnalysisConceptRelation) (bool, error) {
	if len(relations) > 0 {
		w.relationChunks++
		w.relationRows += len(relations)
		w.cancel()
	}
	return true, nil
}
func TestCanceledAnalysisSaveStopsAfterFirstRelationChunk(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{{ID: "a", Kind: graph.KindFunction, Name: "Alpha", FilePath: "a.go"}, {ID: "b", Kind: graph.KindFunction, Name: "Beta", FilePath: "a.go"}}, []*graph.Edge{{From: "a", To: "b", Kind: graph.EdgeCalls}})
	s := &Server{graph: g, logger: zap.NewNop()}
	t.Cleanup(s.DrainBackground)
	s.populateAnalysis(false, false)
	siblings := make([]string, 3*analysisGenerationWriteChunk)
	for i := range siblings {
		siblings[i] = fmt.Sprintf("relation-%04d", i)
	}
	candidate := persistedAnalysis{communities: s.communities, leiden: s.leidenCache, processes: s.processes, pageRank: s.pageRank, adjacency: s.adjacency, hits: s.hits, autoConcepts: search.RestoreAutoConcepts(search.AutoConceptsPersistenceSnapshot{Related: map[string][]string{"head": siblings}})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &relationCancelWriter{canceledGenerationWriter: canceledGenerationWriter{cancel: cancel}}
	_, accepted, err := persistAnalysisGenerationContext(ctx, w, 0, candidate)
	require.False(t, accepted)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, w.relationChunks, "relations continued flushing after cancellation")
	require.Equal(t, analysisGenerationWriteChunk, w.relationRows)
	require.Equal(t, 1, w.aborts)
	require.Zero(t, w.activations)
}
