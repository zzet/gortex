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
	Prepare, Metrics, Bookkeeping, Outgoing, Incoming, MergeFan, Centrality        time.Duration
	Scoring                                                                        time.Duration
	PrepareCalls, ScoringCalls, Candidates, MissingOut, MissingIn, OutRows, InRows int
}

// Add accumulates observed passes without mixing their stage label.
func (t *Timing) Add(other Timing) {
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
