package mcp

import (
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// The high-level legs are exclusive. Snapshot contains its batch reads plus
// SnapshotCompute; cache lookup/store include mutex wait and map/LRU work.
type centralityTimingRecorder struct {
	rerank.CentralityTiming
	started, last time.Time
}

func newCentralityTimingRecorder(observer func(rerank.CentralityTiming)) *centralityTimingRecorder {
	if observer == nil {
		return nil
	}
	now := time.Now()
	return &centralityTimingRecorder{CentralityTiming: rerank.CentralityTiming{Calls: 1}, started: now, last: now}
}

func (t *centralityTimingRecorder) mark(leg *time.Duration) {
	now := time.Now()
	*leg += now.Sub(t.last)
	t.last = now
}

func (t *centralityTimingRecorder) finish(observer func(rerank.CentralityTiming)) {
	t.mark(&t.Bookkeeping)
	t.Total = t.last.Sub(t.started)
	t.SnapshotCompute = t.Snapshot - t.NodeRead - t.EdgeRead
	observer(t.CentralityTiming)
}

// BuildBoundedAdjacencySnapshot uses only the two basic batch methods. The
// reader beneath this decorator is ALREADY request-bound: contextual method
// selection, cancellation, selected masks and core edge filtering stay there.
// Nested filtering and its endpoint classification count as outgoing-read work.
type centralityObservedReader struct {
	graph.Reader
	timing *centralityTimingRecorder
}

func (r centralityObservedReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	start := time.Now()
	rows := r.Reader.GetNodesByIDs(ids)
	r.timing.NodeRead += time.Since(start)
	r.timing.NodeReads++
	r.timing.NodeIDs += len(ids)
	r.timing.NodeRows += len(rows)
	return rows
}

func (r centralityObservedReader) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	start := time.Now()
	rows := r.Reader.GetOutEdgesByNodeIDs(ids)
	r.timing.EdgeRead += time.Since(start)
	r.timing.EdgeReads++
	r.timing.EdgeIDs += len(ids)
	for _, edges := range rows {
		r.timing.EdgeRows += len(edges)
	}
	return rows
}
