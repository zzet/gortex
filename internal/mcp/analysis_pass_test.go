package mcp

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/runtimeactivity"
)

// setAnalysisStageHook installs a checkpoint hook for one test.
func setAnalysisStageHook(t *testing.T, hook func(stage string)) {
	t.Helper()
	previous := analysisStageHook
	analysisStageHook = hook
	t.Cleanup(func() { analysisStageHook = previous })
}

// writeOverlayNode commits one graph write to the analysed generation.
func writeOverlayNode(t *testing.T, server *Server, id string) {
	t.Helper()
	server.graph.AddBatch([]*graph.Node{
		{ID: "pkg/overlay.go::" + id, Kind: graph.KindFunction, Name: id, FilePath: "pkg/overlay.go", Language: "go"},
	}, nil)
}

// analysisReads runs every reader of the analysis snapshot once and reports
// whether each answered with a snapshot.
func analysisReads(server *Server) map[string]bool {
	_, _, query := server.activeAnalysisQuery()
	return map[string]bool{
		"communities":   server.getCommunities() != nil,
		"processes":     server.getProcesses() != nil,
		"pagerank":      server.getPageRank() != nil,
		"hits":          server.getHITS() != nil,
		"adjacency":     server.getAdjacency() != nil,
		"auto_concepts": server.getAutoConcepts() != nil,
		"query":         query,
	}
}

// analysisReadsAll is analysisReads plus the readers whose answer on this
// small fixture is empty either way (hotspots): they are only timed.
func analysisReadsAll(server *Server) map[string]bool {
	_ = server.getHotspots()
	return analysisReads(server)
}

// A pass computes without analysisMu. While it runs, every reader of the
// snapshot answers at once: from the installed snapshot while it still matches
// the graph, and without the analysis signals once a write made it stale —
// never by waiting for the pass.
func TestAnalysisReadersNeverWaitForARunningPass(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	server, _ := newAnalysisWiringServer(t, store)
	server.RunAnalysis()
	require.True(t, analysisReads(server)["query"], "fixture precondition: an installed snapshot")

	for round, stale := range []bool{false, true, true} {
		viaRunAnalysis := round == 2
		reached := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		setAnalysisStageHook(t, func(stage string) {
			if stage == "load" {
				once.Do(func() { close(reached) })
				<-release
			}
		})
		if stale {
			writeOverlayNode(t, server, fmt.Sprintf("StaleMaker%d", round))
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			if viaRunAnalysis {
				// The production entry point, over a written graph.
				server.RunAnalysis()
				return
			}
			// Straight to the computation: the no-op rule would skip a pass
			// over the unchanged graph.
			server.populateAnalysis(true, true)
		}()
		select {
		case <-reached:
		case <-time.After(20 * time.Second):
			t.Fatal("the pass never reached its first checkpoint")
		}
		var reads map[string]bool
		answerWithin(t, 2*time.Second, "the analysis readers", func() { reads = analysisReadsAll(server) })
		for reader, answered := range reads {
			if stale {
				require.False(t, answered, "%s answered from a snapshot the graph no longer matches", reader)
			} else {
				require.True(t, answered, "%s did not answer from the matching snapshot while a pass ran", reader)
			}
		}
		close(release)
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Fatal("the pass never finished")
		}
		setAnalysisStageHook(t, nil)
	}
	require.True(t, analysisReads(server)["query"], "the pass after the write installs a matching snapshot")
}

type countingAnalysisStore struct {
	graph.Store
	counts atomic.Int64
}

func (c *countingAnalysisStore) NodeCount() int { c.counts.Add(1); return c.Store.NodeCount() }
func (c *countingAnalysisStore) EdgeCount() int { c.counts.Add(1); return c.Store.EdgeCount() }

// A pass over a graph the installed snapshot still matches does nothing: no
// header load, no counting of the corpus, no new epoch. A write makes the next
// pass run.
func TestAnalysisPassIsANoOpOnAnUnchangedGraph(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	server, _ := newAnalysisWiringServer(t, store)
	server.RunAnalysis()
	server.communityTokenForAnswer() // the token of the graph as it is
	counting := &countingAnalysisStore{Store: server.graph}
	server.graph = counting
	epoch := server.analysisEpoch
	stages := 0
	setAnalysisStageHook(t, func(string) { stages++ })

	server.RunAnalysis()
	require.Equal(t, int64(1), server.analysisPassSkips.Load(), "the unchanged graph was analysed again")
	require.Equal(t, epoch, server.analysisEpoch)
	require.Zero(t, counting.counts.Load(), "the no-op pass counted the corpus")
	require.Zero(t, stages)

	writeOverlayNode(t, server, "Changed")
	server.RunAnalysis()
	require.Equal(t, int64(1), server.analysisPassSkips.Load(), "a written graph was not analysed")
	require.Greater(t, server.analysisEpoch, epoch)
	require.Positive(t, stages)
}

// A write during a pass supersedes the attempt: it stops at the next
// checkpoint instead of computing the remaining sub-analyses for a result the
// revision gate would refuse, and the next attempt publishes.
func TestAnalysisPassStopsAtTheNextCheckpointWhenTheGraphIsWritten(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	server, _ := newAnalysisWiringServer(t, store)
	var stages []string
	wrote := false
	setAnalysisStageHook(t, func(stage string) {
		stages = append(stages, stage)
		if stage == "processes" && !wrote {
			wrote = true
			writeOverlayNode(t, server, "MidPass")
		}
	})
	metrics := server.populateAnalysis(true, true)
	require.Equal(t, 2, metrics.attempts)
	require.Equal(t, 1, metrics.superseded)
	require.Equal(t, "processes", metrics.supersededAt)
	require.Equal(t, []string{
		"load", "leiden_communities", "processes",
		"load", "leiden_communities", "processes", "pagerank", "adjacency", "auto_concepts", "hits", "save",
	}, stages, "the superseded attempt went on past its checkpoint")
	_, _, ok := server.activeAnalysisQuery()
	require.True(t, ok, "the second attempt did not publish")
}

// A background pass waits out an edit cycle and a tool call in flight at its
// checkpoints; a pass a tool call runs itself does not.
func TestBackgroundAnalysisPassYieldsToEditCyclesAndToolCalls(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	server, _ := newAnalysisWiringServer(t, store)
	var editing atomic.Bool
	server.analysisYield = editing.Load

	editing.Store(true)
	go func() {
		time.Sleep(300 * time.Millisecond)
		editing.Store(false)
	}()
	started := time.Now()
	metrics := server.populateAnalysis(true, true)
	require.GreaterOrEqual(t, time.Since(started), 250*time.Millisecond, "the pass ran through an edit cycle")
	require.Positive(t, metrics.yields)
	require.GreaterOrEqual(t, metrics.yielded, 250*time.Millisecond)

	writeOverlayNode(t, server, "ToolCall")
	runtimeactivity.Begin("mcp")
	go func() {
		time.Sleep(300 * time.Millisecond)
		runtimeactivity.End("mcp")
	}()
	metrics = server.populateAnalysis(true, true)
	require.Positive(t, metrics.yields, "the pass did not yield to a tool call in flight")

	writeOverlayNode(t, server, "Foreground")
	editing.Store(true)
	defer editing.Store(false)
	started = time.Now()
	metrics = server.populateAnalysis(true, false)
	require.Zero(t, metrics.yields, "a pass a tool call runs itself waited for itself")
	require.Less(t, time.Since(started), analysisYieldCap)
}

// With the view-scoped analysis clock, a write to another generation (a
// worktree edit) neither stales the installed snapshot nor makes the next pass
// compute; a write to the analysed view does.
func TestAWriteToAnotherGenerationLeavesTheAnalysisCurrent(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	server, _ := newAnalysisWiringServer(t, store)
	server.RunAnalysis()
	stages := 0
	setAnalysisStageHook(t, func(string) { stages++ })

	store.AtGeneration(analysisWiringViewGeneration+40).AddBatch([]*graph.Node{
		{ID: "x/y.go::Foreign", Kind: graph.KindFunction, Name: "Foreign", FilePath: "x/y.go", Language: "go"},
	}, nil)
	_, _, ok := server.activeAnalysisQuery()
	require.True(t, ok, "a write to another generation staled the analysis")
	server.RunAnalysis()
	require.Zero(t, stages, "a write to another generation made the pass compute")
	require.Equal(t, int64(1), server.analysisPassSkips.Load())

	writeOverlayNode(t, server, "OwnView")
	server.RunAnalysis()
	require.Positive(t, stages, "a write to the analysed view did not make the pass compute")
}

// ViewGeneration keeps the wrapped handle's generation axis: the analysis is
// keyed on the generation s.graph reads.
func (c *countingAnalysisStore) ViewGeneration() int64 {
	if scoped, ok := c.Store.(viewGenerationScoped); ok {
		return scoped.ViewGeneration()
	}
	return 0
}

// A tool that changes the graph (index_repository, reindex_repository) asks
// for the rollups through startBackgroundAnalysis: the call returns at once,
// the pass runs in the background — where it yields — and a request made
// while it runs gets one more pass after it.
func TestToolTriggeredAnalysisRunsInTheBackground(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	server, _ := newAnalysisWiringServer(t, store)
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var loads atomic.Int32
	setAnalysisStageHook(t, func(stage string) {
		if stage == "load" && loads.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
	})
	done := make(chan struct{}, 4)
	previous := backgroundAnalysisDoneHook
	backgroundAnalysisDoneHook = func() { done <- struct{}{} }
	t.Cleanup(func() { backgroundAnalysisDoneHook = previous })

	started := time.Now()
	require.True(t, server.startBackgroundAnalysis("reindex"))
	require.Less(t, time.Since(started), time.Second, "the tool waited for the pass")
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the background pass never started")
	}
	// A second request while the first pass runs joins it and asks for one
	// more pass once it ends.
	writeOverlayNode(t, server, "DuringPass")
	require.False(t, server.startBackgroundAnalysis("reindex"))
	close(release)
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the background passes never finished")
	}
	// The follow-up pass runs; over a graph the first pass already covered
	// it is the no-op pass.
	require.GreaterOrEqual(t, int64(loads.Load())+server.analysisPassSkips.Load(), int64(2),
		"the request made during the pass got no pass of its own")
	_, _, ok := server.activeAnalysisQuery()
	require.True(t, ok, "the last pass did not publish the written graph's analysis")
}
