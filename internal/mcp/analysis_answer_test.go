package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

// answerWithin runs fn and fails the test when it has not returned within d.
func answerWithin(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s waited for the running analysis pass", what)
	}
}

// A symbol search reads the analysis snapshot (community ids and normalized
// HITS values for the rerank, the concept vocabulary for query expansion).
// RunAnalysis holds analysisMu for its whole pass — minutes on a large
// workspace — and a search that waited for it waited for the whole pass. The
// answer-path reads never wait: while a pass holds the lock they answer from
// the receipt observed before it, as long as that receipt is still current,
// and with the lock free they answer exactly what the locked reads answer.
func TestAnswerPathAnalysisReadsNeverWaitForAnAnalysisPass(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	server, _ := newAnalysisWiringServer(t, store)
	server.RunAnalysis()

	ids := []string{"pkg/overlay.go::OverlayAlpha", "pkg/overlay.go::OverlayBeta"}
	_, lockedHeader, lockedOK := server.activeAnalysisQuery()
	require.True(t, lockedOK, "fixture precondition: an active analysis generation")
	query, header, ok := server.activeAnalysisQueryForAnswer()
	require.True(t, ok)
	require.NotNil(t, query)
	require.Equal(t, lockedHeader, header, "with the lock free the answer path reads the same receipt")
	free := server.rerankAnalysisMetrics(ids)
	require.NotEmpty(t, free)
	lockedRows, err := server.analysisNodeMetricsBatched(ids)
	require.NoError(t, err)
	require.Len(t, free, len(lockedRows))
	freeConcepts := server.autoConceptsForAnswer()
	require.NotNil(t, freeConcepts, "fixture precondition: a published concept vocabulary")

	// A pass is running: the writer holds analysisMu.
	server.analysisMu.Lock()
	var during map[string]rerank.AnalysisMetric
	answerWithin(t, 2*time.Second, "the rerank's analysis metrics", func() {
		during = server.rerankAnalysisMetrics(ids)
	})
	require.Equal(t, free, during, "a current receipt answers the same metrics while the pass runs")
	var concepts *search.AutoConcepts
	answerWithin(t, 2*time.Second, "the expansion vocabulary", func() {
		concepts = server.autoConceptsForAnswer()
	})
	require.Same(t, freeConcepts, concepts)
	server.analysisMu.Unlock()

	// The graph changes and a pass starts: the old receipt is stale, and the
	// answer is "no analysis signal", at once, never a stale ranking.
	store.AtGeneration(analysisWiringViewGeneration).AddBatch([]*graph.Node{
		{ID: "pkg/overlay.go::OverlayGamma", Kind: graph.KindFunction, Name: "OverlayGamma", FilePath: "pkg/overlay.go", Language: "go"},
	}, nil)
	server.analysisMu.Lock()
	answerWithin(t, 2*time.Second, "the rerank's analysis metrics over a changed graph", func() {
		during = server.rerankAnalysisMetrics(ids)
	})
	require.Nil(t, during, "a stale receipt answered a changed graph")
	answerWithin(t, 2*time.Second, "the expansion vocabulary over a changed graph", func() {
		concepts = server.autoConceptsForAnswer()
	})
	require.Nil(t, concepts, "a stale vocabulary answered a changed graph")
	server.analysisMu.Unlock()

	// The next pass publishes a new generation: the per-generation values
	// (the normalization maxima) are read again, never carried over.
	store.AtGeneration(analysisWiringViewGeneration).AddBatch(nil, []*graph.Edge{
		{From: "pkg/overlay.go::OverlayGamma", To: "pkg/overlay.go::OverlayAlpha", Kind: graph.EdgeCalls, FilePath: "pkg/overlay.go", Line: 9},
		{From: "pkg/overlay.go::OverlayBeta", To: "pkg/overlay.go::OverlayAlpha", Kind: graph.EdgeCalls, FilePath: "pkg/overlay.go", Line: 10},
	})
	server.RunAnalysis()
	_, next, ok := server.activeAnalysisQuery()
	require.True(t, ok)
	require.NotEqual(t, header.GenerationID, next.GenerationID, "fixture precondition: a new analysis generation")
	want := map[string]rerank.AnalysisMetric{}
	rows, err := server.analysisNodeMetricsBatched(ids)
	require.NoError(t, err)
	maxAuthority := server.topAnalysisMetricValue(graph.AnalysisMetricAuthority)
	maxHub := server.topAnalysisMetricValue(graph.AnalysisMetricHub)
	for _, row := range rows {
		metric := rerank.AnalysisMetric{CommunityID: row.CommunityID}
		if maxAuthority > 0 {
			metric.Authority = row.Authority / maxAuthority
		}
		if maxHub > 0 {
			metric.Hub = row.Hub / maxHub
		}
		want[row.NodeID] = metric
	}
	require.Equal(t, want, server.rerankAnalysisMetrics(ids), "the answer path kept another generation's values")
}

// countingStore counts the whole-table counts currentCommunityToken takes.
type countingStore struct {
	graph.Store
	counts int
}

func (c *countingStore) NodeCount() int { c.counts++; return c.Store.NodeCount() }
func (c *countingStore) EdgeCount() int { c.counts++; return c.Store.EdgeCount() }

// The answer path's graph token is counted once per graph revision, not once
// per search: between two writes the store's node and edge counts cannot
// move, so a repeated search takes no whole-table count. A write advances the
// revision and the next search counts again.
func TestAnswerPathCountsTheGraphOncePerRevision(t *testing.T) {
	store := seedAnalysisWiringStore(t)
	server, overlay := newAnalysisWiringServer(t, store)
	server.RunAnalysis()
	counting := &countingStore{Store: server.graph}
	server.graph = counting

	first := server.communityTokenForAnswer()
	require.Equal(t, server.currentCommunityToken(), first)
	counting.counts = 0
	for i := 0; i < 5; i++ {
		require.Equal(t, first, server.communityTokenForAnswer())
	}
	require.Zero(t, counting.counts, "an unchanged graph was counted again")

	overlay.AddBatch([]*graph.Node{
		{ID: "pkg/overlay.go::OverlayDelta", Kind: graph.KindFunction, Name: "OverlayDelta", FilePath: "pkg/overlay.go", Language: "go"},
	}, nil)
	next := server.communityTokenForAnswer()
	require.NotZero(t, counting.counts, "a written graph was not counted again")
	require.NotEqual(t, first, next)
	require.Equal(t, server.currentCommunityToken(), next)
}

// ViewGeneration keeps the wrapped handle's generation axis.
func (c *countingStore) ViewGeneration() int64 {
	if scoped, ok := c.Store.(viewGenerationScoped); ok {
		return scoped.ViewGeneration()
	}
	return 0
}
