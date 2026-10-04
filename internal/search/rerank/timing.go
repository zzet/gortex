package rerank

import "time"

// TimingStage separates an engine ranking pass from the final handler pass.
type TimingStage uint8

const (
	TimingOuter TimingStage = iota
	TimingInner
)

// Timing records mutually exclusive preparation legs and scoring/sort work.
// Prepare is their containing total, not another cost to add to those legs.
// Observers are request-local; the zero/nil observer path reads no clocks.
type Timing struct {
	Stage                                                                          TimingStage
	CentralityWork                                                                 CentralityTiming
	Prepare, Metrics, Bookkeeping, Outgoing, Incoming, MergeFan, Centrality        time.Duration
	Scoring                                                                        time.Duration
	PrepareCalls, ScoringCalls, Candidates, MissingOut, MissingIn, OutRows, InRows int
}

// Add accumulates observed passes without mixing their stage label.
func (t *Timing) Add(other Timing) {
	t.CentralityWork.Add(other.CentralityWork)
	t.Prepare += other.Prepare
	t.Metrics += other.Metrics
	t.Bookkeeping += other.Bookkeeping
	t.Outgoing += other.Outgoing
	t.Incoming += other.Incoming
	t.MergeFan += other.MergeFan
	t.Centrality += other.Centrality
	t.Scoring += other.Scoring
	t.PrepareCalls += other.PrepareCalls
	t.ScoringCalls += other.ScoringCalls
	t.Candidates += other.Candidates
	t.MissingOut += other.MissingOut
	t.MissingIn += other.MissingIn
	t.OutRows += other.OutRows
	t.InRows += other.InRows
}

type prepareTiming struct {
	Timing
	started, last time.Time
}

func newPrepareTiming(observer func(Timing)) *prepareTiming {
	if observer == nil {
		return nil
	}
	now := time.Now()
	return &prepareTiming{Timing: Timing{PrepareCalls: 1}, started: now, last: now}
}

func (t *prepareTiming) mark(leg *time.Duration) {
	if t == nil {
		return
	}
	now := time.Now()
	*leg += now.Sub(t.last)
	t.last = now
}

func (t *prepareTiming) finish(observer func(Timing)) {
	if t == nil {
		return
	}
	t.Prepare = t.last.Sub(t.started)
	observer(t.Timing)
}

// CentralityTiming splits the callback measured by Timing.Centrality. Snapshot
// contains NodeRead + EdgeRead + SnapshotCompute; never add both levels.
// Cache lookup/store durations include mutex waiting and the cache operation.
type CentralityTiming struct {
	Total, ReaderSetup, Snapshot, NodeRead, EdgeRead, SnapshotCompute  time.Duration
	ScopeKey, CacheLookup, Walk, CacheStore, Bookkeeping               time.Duration
	Calls, NodeReads, EdgeReads, NodeIDs, EdgeIDs, NodeRows, EdgeRows  int
	MemoCalls, CacheHits, CacheMisses, CacheDisabled, CacheUncacheable int
	SnapshotNodes, SnapshotEdges, Truncated                            int
}

func (t *CentralityTiming) Add(o CentralityTiming) {
	t.Total += o.Total
	t.ReaderSetup += o.ReaderSetup
	t.Snapshot += o.Snapshot
	t.NodeRead += o.NodeRead
	t.EdgeRead += o.EdgeRead
	t.SnapshotCompute += o.SnapshotCompute
	t.ScopeKey += o.ScopeKey
	t.CacheLookup += o.CacheLookup
	t.Walk += o.Walk
	t.CacheStore += o.CacheStore
	t.Bookkeeping += o.Bookkeeping
	t.Calls += o.Calls
	t.NodeReads += o.NodeReads
	t.EdgeReads += o.EdgeReads
	t.NodeIDs += o.NodeIDs
	t.EdgeIDs += o.EdgeIDs
	t.NodeRows += o.NodeRows
	t.EdgeRows += o.EdgeRows
	t.MemoCalls += o.MemoCalls
	t.CacheHits += o.CacheHits
	t.CacheMisses += o.CacheMisses
	t.CacheDisabled += o.CacheDisabled
	t.CacheUncacheable += o.CacheUncacheable
	t.SnapshotNodes += o.SnapshotNodes
	t.SnapshotEdges += o.SnapshotEdges
	t.Truncated += o.Truncated
}
