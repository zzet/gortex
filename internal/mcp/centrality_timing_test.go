package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

type centralityTimingReadCounts struct{ nodes, edges, nodeIDs, edgeIDs int }

type centralityTimingReader struct {
	graph.Store
	counts centralityTimingReadCounts
	cancel context.CancelFunc
}

// The request-bound reader must choose these contextual capabilities BEFORE
// the telemetry decorator is installed. Plain batches must never be selected.
func (r *centralityTimingReader) GetNodesByIDs([]string) map[string]*graph.Node {
	panic("lost contextual node capability")
}
func (r *centralityTimingReader) GetOutEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	panic("lost contextual edge capability")
}
func (r *centralityTimingReader) GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*graph.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.counts.nodes++
	r.counts.nodeIDs += len(ids)
	return r.Store.GetNodesByIDs(ids), nil
}
func (r *centralityTimingReader) GetOutEdgesByNodeIDsContext(ctx context.Context, ids []string, _ int) (map[string][]*graph.Edge, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	r.counts.edges++
	r.counts.edgeIDs += len(ids)
	rows := r.Store.GetOutEdgesByNodeIDs(ids)
	if r.cancel != nil {
		r.cancel()
	}
	return rows, false, nil
}

func TestCentralityTimingPreservesCheckedReadsScoresAndCache(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		label := "miss_then_hit"
		if canceled {
			label = "canceled_build"
		}
		t.Run(label, func(t *testing.T) {
			run := func(observed bool) ([]rerank.CentralityResult, centralityTimingReadCounts, []rerank.CentralityTiming, int64, int64, int) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				reader := &centralityTimingReader{Store: walkTestGraph(t)}
				if canceled {
					reader.cancel = cancel
				}
				cache := newPPRWalkCache()
				cache.enabled = true
				srv := &Server{graph: reader, pprCache: cache}
				var receipts []rerank.CentralityTiming
				var observe func(rerank.CentralityTiming)
				if observed {
					observe = func(v rerank.CentralityTiming) { receipts = append(receipts, v) }
				}
				var results []rerank.CentralityResult
				for range 2 {
					results = append(results, srv.boundedCentralityForRequestObserved(ctx, []string{"a.go::A"}, []string{"a.go::A", "b.go::B", "c.go::C"}, observe))
				}
				hits, misses, size, _, _ := cache.stats()
				return results, reader.counts, receipts, hits, misses, size
			}
			plain, reads, _, hits, misses, size := run(false)
			got, gotReads, timing, gotHits, gotMisses, gotSize := run(true)
			require.Equal(t, plain, got)
			require.Equal(t, reads, gotReads)
			require.Equal(t, []int64{hits, misses, int64(size)}, []int64{gotHits, gotMisses, int64(gotSize)})
			require.Len(t, timing, 2)
			var combined rerank.CentralityTiming
			for _, v := range timing {
				require.Equal(t, 1, v.Calls)
				require.Equal(t, v.Total, v.ReaderSetup+v.Snapshot+v.ScopeKey+v.CacheLookup+v.Walk+v.CacheStore+v.Bookkeeping)
				require.Equal(t, v.Snapshot, v.NodeRead+v.EdgeRead+v.SnapshotCompute)
				combined.Add(v)
			}
			if canceled {
				require.Empty(t, got[0].Scores)
				require.Empty(t, got[1].Scores)
				require.Zero(t, gotSize)
				require.Zero(t, combined.Walk)
				require.Equal(t, 1, gotReads.edges)
				require.Zero(t, combined.CacheHits+combined.CacheMisses)
			} else {
				require.NotEmpty(t, got[0].Scores)
				require.Equal(t, 1, timing[0].CacheMisses)
				require.Equal(t, 1, timing[1].CacheHits)
				require.Zero(t, timing[1].Walk)
				require.Positive(t, timing[1].NodeReads, "a walk hit must honestly report the fresh CSR reads")
				require.Equal(t, gotReads.nodes, combined.NodeReads)
				require.Equal(t, gotReads.edges, combined.EdgeReads)
				require.Equal(t, gotReads.nodeIDs, combined.NodeIDs)
				require.Equal(t, gotReads.edgeIDs, combined.EdgeIDs)
				require.Equal(t, got[0].NodeCount, timing[0].SnapshotNodes)
				require.Equal(t, got[0].EdgeCount, timing[0].SnapshotEdges)
			}
		})
	}
	require.Nil(t, newCentralityTimingRecorder(nil))
	fields := symbolCentralityTimingFields(rerank.CentralityTiming{CacheLookup: 2500 * time.Microsecond})
	require.Equal(t, 2.5, fields["cache_lookup_including_wait_ms"])
}
