package resolver

import (
	"fmt"
	"iter"
	"runtime"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The source generates rows on demand and the sink retains only counts. Thus
// retained heap measures resolver working state, not an in-memory fixture.
type terminalHeapStore struct {
	*graph.Graph
	count, persisted, maxBatch int
	peak                       uint64
	invalid                    bool
	committed                  map[int]bool
	failAfter                  int
}

func (s *terminalHeapStore) sample() {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	s.peak = max(s.peak, stats.HeapAlloc)
}

func (s *terminalHeapStore) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		for i := range s.count {
			e := &graph.Edge{From: "caller", To: "unresolved::Missing", Kind: graph.EdgeCalls,
				FilePath: "fixture/main.ts", Line: i + 1,
				Meta: map[string]any{"payload": fmt.Sprintf("synthetic-%0256d", i)}}
			if s.committed[e.Line] {
				setEdgeTerminal(e, terminalReasonNoDefinition)
			}
			if !yield(e) {
				return
			}
		}
	}
}

func (s *terminalHeapStore) CountNodesByNameClass([]string, []graph.NodeKind) map[string]graph.NodeNameClassCount {
	s.sample()
	return nil
}

func (s *terminalHeapStore) PersistEdgeTerminalStamps(edges []*graph.Edge) {
	if s.failAfter > 0 && s.persisted >= s.failAfter {
		panic("synthetic interrupted persistence")
	}
	s.sample()
	s.maxBatch = max(s.maxBatch, len(edges))
	for _, edge := range edges {
		if !edgeTerminalFlag(edge) || edge.Meta["payload"] != fmt.Sprintf("synthetic-%0256d", edge.Line-1) {
			s.invalid = true
		}
		if s.committed != nil {
			s.committed[edge.Line] = true
		}
	}
	s.persisted += len(edges)
}

func TestTerminalBoundedStampPartialRecovery(t *testing.T) {
	s := &terminalHeapStore{Graph: graph.New(), count: 700, committed: make(map[int]bool), failAfter: 256}
	r := &Resolver{graph: s}
	interrupted := false
	func() {
		defer func() { interrupted = recover() != nil }()
		r.reconcileTerminalStamps()
	}()
	if !interrupted || s.persisted != 256 {
		t.Fatalf("expected interruption after one bounded durable batch: interrupted=%v persisted=%d", interrupted, s.persisted)
	}
	s.failAfter = 0
	stamped, unstamped := r.reconcileTerminalStamps()
	if stamped != 444 || unstamped != 0 || s.persisted != 700 || s.invalid || s.maxBatch > 256 {
		t.Fatalf("recovery: stamped=%d unstamped=%d persisted=%d invalid=%v maxBatch=%d", stamped, unstamped, s.persisted, s.invalid, s.maxBatch)
	}
	stamped, unstamped = r.reconcileTerminalStamps()
	if stamped != 0 || unstamped != 0 || s.persisted != 700 {
		t.Fatal("a completed retry must be idempotent")
	}
}

func BenchmarkTerminalRetainedWorkingHeap(b *testing.B) {
	for _, count := range []int{10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			for range b.N {
				s := &terminalHeapStore{Graph: graph.New(), count: count}
				s.sample()
				baseline := s.peak
				r := &Resolver{graph: s}
				stamped, unstamped := r.reconcileTerminalStamps()
				if stamped != count || unstamped != 0 || s.persisted != count || s.invalid {
					b.Fatalf("stamped=%d unstamped=%d persisted=%d invalid=%v", stamped, unstamped, s.persisted, s.invalid)
				}
				b.ReportMetric(float64(s.peak-baseline), "peak-live-bytes")
				b.ReportMetric(float64(s.maxBatch), "max-batch-rows")
			}
		})
	}
}
