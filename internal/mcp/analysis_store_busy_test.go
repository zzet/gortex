package mcp

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
	"github.com/zzet/gortex/internal/viewmetrics"
)

var rerankBusyTestIDs = []string{"pkg/overlay.go::OverlayAlpha", "pkg/overlay.go::OverlayBeta"}

func newRerankBusyTestServer(t *testing.T) *Server {
	t.Helper()
	store := seedAnalysisWiringStore(t)
	server, _ := newAnalysisWiringServer(t, store)
	server.RunAnalysis()
	server.DrainBackground()
	_, _, ok := server.activeAnalysisQueryForAnswer()
	require.True(t, ok)
	return server
}

// Hold the actual SQLite writer mutex from another goroutine through the
// publication callback, without changing or invalidating the analysis receipt.
func holdAnalysisStoreWriter(t *testing.T, writer graph.AnalysisGenerationStore) func() {
	t.Helper()
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		writer.CommitAnalysisSnapshot(writer.AnalysisMutationRevision(), func() {
			close(locked)
			<-release
		})
	}()
	select {
	case <-locked:
	case <-done:
		t.Fatal("fixture could not hold the writer mutex")
	}
	unlock := func() {
		select {
		case <-done:
		default:
			close(release)
			<-done
		}
	}
	t.Cleanup(unlock)
	return unlock
}

func TestRerankAnalysisMetricsNeverWaitForStoreWriter(t *testing.T) {
	server := newRerankBusyTestServer(t)
	want := server.rerankAnalysisMetrics(rerankBusyTestIDs)
	require.NotEmpty(t, want)
	writer, _ := server.analysisGenerationBackends()
	unlock := holdAnalysisStoreWriter(t, writer)
	before := viewmetrics.Read().Counters[viewmetrics.SearchRerankAnalysisSkippedBusyTotal]
	var got map[string]rerank.AnalysisMetric
	answerWithin(t, 50*time.Millisecond, "rerank analysis metrics behind the store writer", func() {
		got = server.rerankAnalysisMetrics(rerankBusyTestIDs)
	})
	require.Nil(t, got)
	require.Equal(t, before+1, viewmetrics.Read().Flat()[viewmetrics.SearchRerankAnalysisSkippedBusyTotal])
	unlock()
	require.Equal(t, want, server.rerankAnalysisMetrics(rerankBusyTestIDs))
}

func TestAnswerTopAnalysisMetricBusyReadIsNotCached(t *testing.T) {
	server := newRerankBusyTestServer(t)
	writer, _ := server.analysisGenerationBackends()
	_, header, ok := server.activeAnalysisQueryForAnswer()
	require.True(t, ok)
	unlock := holdAnalysisStoreWriter(t, writer)
	var value float64
	var err error
	answerWithin(t, 50*time.Millisecond, "top metric behind the store writer", func() {
		value, err = server.answerTopAnalysisMetricValue(header, graph.AnalysisMetricAuthority)
	})
	require.Zero(t, value)
	require.ErrorIs(t, err, graph.ErrAnalysisStoreBusy)
	cache := server.answerAnalysisCacheFor(header.GenerationID)
	cache.mu.Lock()
	_, cached := cache.topMetric[graph.AnalysisMetricAuthority]
	cache.mu.Unlock()
	require.False(t, cached, "busy read poisoned the generation cache")
	unlock()

	require.NotEmpty(t, server.rerankAnalysisMetrics(rerankBusyTestIDs))
	cache.mu.Lock()
	value, cached = cache.topMetric[graph.AnalysisMetricAuthority]
	cache.mu.Unlock()
	require.True(t, cached)
	require.Positive(t, value)

	// A cache hit needs no store read, even while the writer is busy again.
	holdAnalysisStoreWriter(t, writer)
	var next float64
	answerWithin(t, 50*time.Millisecond, "cached maximum", func() {
		next, err = server.answerTopAnalysisMetricValue(header, graph.AnalysisMetricAuthority)
	})
	require.NoError(t, err)
	require.Equal(t, value, next)
}

// Keep only the established interfaces on this wrapper to exercise backends
// without the optional try-read capability.
type blockingRerankAnalysisStore struct {
	graph.Store
	graph.AnalysisGenerationStore
	graph.AnalysisQueryStore
	viewGeneration int64
	pointReads     int
	topReads       int
}

func (s *blockingRerankAnalysisStore) ViewGeneration() int64 { return s.viewGeneration }

func (s *blockingRerankAnalysisStore) AnalysisNodeMetrics(generationID int64, ids []string) ([]graph.AnalysisNodeMetric, error) {
	s.pointReads++
	return s.AnalysisQueryStore.AnalysisNodeMetrics(generationID, ids)
}

func (s *blockingRerankAnalysisStore) TopAnalysisNodeMetrics(generationID int64, metric graph.AnalysisMetric, limit int, cursor *graph.AnalysisMetricCursor) ([]graph.AnalysisNodeMetric, *graph.AnalysisMetricCursor, error) {
	s.topReads++
	return s.AnalysisQueryStore.TopAnalysisNodeMetrics(generationID, metric, limit, cursor)
}

func wrapRerankAnalysisStore(server *Server) *blockingRerankAnalysisStore {
	writer, query := server.analysisGenerationBackends()
	wrapped := &blockingRerankAnalysisStore{Store: server.graph, AnalysisGenerationStore: writer, AnalysisQueryStore: query, viewGeneration: server.analysisViewGeneration()}
	server.indexer = nil
	server.graph = wrapped
	return wrapped
}

func TestRerankAnalysisMetricsPreserveBlockingBackendParity(t *testing.T) {
	server := newRerankBusyTestServer(t)
	wrapped := wrapRerankAnalysisStore(server)
	_, supportsTry := any(wrapped).(graph.TryAnalysisQueryStore)
	require.False(t, supportsTry)
	want := server.rerankAnalysisMetrics(rerankBusyTestIDs)
	require.NotEmpty(t, want)
	require.Equal(t, 1, wrapped.pointReads)
	require.Equal(t, 2, wrapped.topReads)
	server.graph = wrapped.Store
	server.answerAnalysisValues.topMetric = nil
	require.Equal(t, want, server.rerankAnalysisMetrics(rerankBusyTestIDs))
}

type tryRerankAnalysisStore struct {
	*blockingRerankAnalysisStore
	point func(int64, []string) ([]graph.AnalysisNodeMetric, error)
	top   func(int64, graph.AnalysisMetric, int, *graph.AnalysisMetricCursor) ([]graph.AnalysisNodeMetric, *graph.AnalysisMetricCursor, error)
}

func (s *tryRerankAnalysisStore) TryAnalysisNodeMetrics(generationID int64, ids []string) ([]graph.AnalysisNodeMetric, error) {
	return s.point(generationID, ids)
}

func (s *tryRerankAnalysisStore) TryTopAnalysisNodeMetrics(generationID int64, metric graph.AnalysisMetric, limit int, cursor *graph.AnalysisMetricCursor) ([]graph.AnalysisNodeMetric, *graph.AnalysisMetricCursor, error) {
	return s.top(generationID, metric, limit, cursor)
}

func TestRerankAnalysisMetricsDiscardPartialBusyBatch(t *testing.T) {
	server := newRerankBusyTestServer(t)
	wrapped := wrapRerankAnalysisStore(server)
	pages := 0
	server.graph = &tryRerankAnalysisStore{blockingRerankAnalysisStore: wrapped, point: func(_ int64, ids []string) ([]graph.AnalysisNodeMetric, error) {
		pages++
		if pages == 2 {
			return nil, fmt.Errorf("second page: %w", graph.ErrAnalysisStoreBusy)
		}
		return []graph.AnalysisNodeMetric{{NodeID: ids[0], Authority: 1}}, nil
	}}
	ids := make([]string, analysisGenerationQueryMax+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("candidate-%04d", i)
	}
	before := viewmetrics.Read().Counters[viewmetrics.SearchRerankAnalysisSkippedBusyTotal]
	require.Nil(t, server.rerankAnalysisMetrics(ids), "a partially scored candidate set escaped")
	require.Equal(t, 2, pages)
	require.Zero(t, wrapped.pointReads, "try-capable backend used the blocking read")
	require.Equal(t, before+1, viewmetrics.Read().Counters[viewmetrics.SearchRerankAnalysisSkippedBusyTotal])
}

func TestRerankAnalysisMetricsRequireBothMaxima(t *testing.T) {
	for _, failedMetric := range []graph.AnalysisMetric{graph.AnalysisMetricAuthority, graph.AnalysisMetricHub} {
		t.Run(string(failedMetric), func(t *testing.T) {
			server := newRerankBusyTestServer(t)
			wrapped := wrapRerankAnalysisStore(server)
			busy := true
			reads := map[graph.AnalysisMetric]int{}
			server.graph = &tryRerankAnalysisStore{blockingRerankAnalysisStore: wrapped, point: wrapped.AnalysisQueryStore.AnalysisNodeMetrics, top: func(generationID int64, metric graph.AnalysisMetric, limit int, cursor *graph.AnalysisMetricCursor) ([]graph.AnalysisNodeMetric, *graph.AnalysisMetricCursor, error) {
				reads[metric]++
				if metric == failedMetric && busy {
					return nil, nil, graph.ErrAnalysisStoreBusy
				}
				return wrapped.AnalysisQueryStore.TopAnalysisNodeMetrics(generationID, metric, limit, cursor)
			}}
			before := viewmetrics.Read().Counters[viewmetrics.SearchRerankAnalysisSkippedBusyTotal]
			require.Nil(t, server.rerankAnalysisMetrics(rerankBusyTestIDs))
			require.Equal(t, before+1, viewmetrics.Read().Counters[viewmetrics.SearchRerankAnalysisSkippedBusyTotal])
			busy = false
			require.NotEmpty(t, server.rerankAnalysisMetrics(rerankBusyTestIDs))
			require.Equal(t, 2, reads[failedMetric], "busy maximum was cached instead of retried")
			require.NotEmpty(t, server.rerankAnalysisMetrics(rerankBusyTestIDs))
			require.Equal(t, 2, reads[failedMetric], "successful maximum was not cached")
			require.Zero(t, wrapped.topReads, "try-capable backend used the blocking top read")
		})
	}
}
