package indexer

import (
	"fmt"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type recordingParseGraphStore struct {
	graph.Store

	mu           sync.Mutex
	batchCalls   int
	removeCalls  int
	removedCount int
	nodeCount    int
	edgeCount    int
	maxBatchNode int
	maxBatchEdge int
	nodeOrder    []string
	events       []string
}

func newRecordingParseGraphStore() *recordingParseGraphStore {
	return &recordingParseGraphStore{Store: graph.New()}
}

func (s *recordingParseGraphStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.mu.Lock()
	s.batchCalls++
	s.nodeCount += len(nodes)
	s.edgeCount += len(edges)
	if len(nodes) > s.maxBatchNode {
		s.maxBatchNode = len(nodes)
	}
	if len(edges) > s.maxBatchEdge {
		s.maxBatchEdge = len(edges)
	}
	for _, node := range nodes {
		if node != nil {
			s.nodeOrder = append(s.nodeOrder, node.ID)
		}
	}
	s.events = append(s.events, "batch")
	s.mu.Unlock()
	s.Store.AddBatch(nodes, edges)
}

func (s *recordingParseGraphStore) RemoveEdgesExact(edges []*graph.Edge) int {
	if len(edges) == 0 {
		return 0
	}
	removed := graph.RemoveEdgesExact(s.Store, edges)
	s.mu.Lock()
	s.removeCalls++
	s.removedCount += removed
	s.events = append(s.events, "remove")
	s.mu.Unlock()
	return removed
}

func (s *recordingParseGraphStore) BulkSetFileMtimes(string, map[string]int64) error {
	return nil
}

func (s *recordingParseGraphStore) recordEvent(event string) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}

func TestParseGraphBatchScaleUsesBoundedWrites(t *testing.T) {
	store := newRecordingParseGraphStore()
	limits := parseGraphBatchLimits{files: 10, nodes: 10_000, edges: 10_000, bytes: 1 << 30}
	batch := newParseGraphBatchWithLimits(store, limits)
	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("n-%03d", i)
		batch.add(
			[]*graph.Node{{ID: id, Kind: graph.KindFunction}},
			[]*graph.Edge{{From: id, To: "target", Kind: graph.EdgeCalls}},
			nil,
		)
	}
	batch.flush()

	if store.batchCalls != 11 {
		t.Fatalf("AddBatch calls = %d, want 11 bounded chunk writes", store.batchCalls)
	}
	if store.nodeCount != 105 || store.edgeCount != 105 {
		t.Fatalf("persisted nodes/edges = %d/%d, want 105/105", store.nodeCount, store.edgeCount)
	}
	if store.maxBatchNode > 10 || store.maxBatchEdge > 10 {
		t.Fatalf("batch exceeded file bound: max nodes/edges = %d/%d", store.maxBatchNode, store.maxBatchEdge)
	}
	for i, id := range store.nodeOrder {
		want := fmt.Sprintf("n-%03d", i)
		if id != want {
			t.Fatalf("node order[%d] = %q, want %q", i, id, want)
		}
	}
}

func TestParseGraphBatchThresholdsFlushBeforeOverfill(t *testing.T) {
	tests := []struct {
		name   string
		limits parseGraphBatchLimits
		nodes  int
		edges  int
		idSize int
	}{
		{name: "nodes", limits: parseGraphBatchLimits{files: 20, nodes: 3, edges: 20, bytes: 1 << 20}, nodes: 2},
		{name: "edges", limits: parseGraphBatchLimits{files: 20, nodes: 20, edges: 3, bytes: 1 << 20}, edges: 2},
		{name: "bytes", limits: parseGraphBatchLimits{files: 20, nodes: 20, edges: 20, bytes: 700}, nodes: 1, idSize: 300},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newRecordingParseGraphStore()
			batch := newParseGraphBatchWithLimits(store, tt.limits)
			for file := 0; file < 2; file++ {
				nodes := make([]*graph.Node, tt.nodes)
				for i := range nodes {
					nodes[i] = &graph.Node{ID: fmt.Sprintf("%0*d-%d-%d", tt.idSize, 0, file, i), Kind: graph.KindFunction}
				}
				edges := make([]*graph.Edge, tt.edges)
				for i := range edges {
					edges[i] = &graph.Edge{From: fmt.Sprintf("f-%d", file), To: fmt.Sprintf("t-%d", i), Kind: graph.EdgeCalls}
				}
				batch.add(nodes, edges, nil)
			}
			batch.flush()
			if store.batchCalls != 2 {
				t.Fatalf("AddBatch calls = %d, want 2 threshold-bounded writes", store.batchCalls)
			}
		})
	}
}

func TestParseGraphBatchConcurrentAddsAreCompleteAndBounded(t *testing.T) {
	store := newRecordingParseGraphStore()
	const (
		workers       = 20
		perWorker     = 100
		filesPerBatch = 17
	)
	batch := newParseGraphBatchWithLimits(store, parseGraphBatchLimits{
		files: filesPerBatch, nodes: 100_000, edges: 100_000, bytes: 1 << 30,
	})
	var callbacks int
	var callbackMu sync.Mutex
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for item := 0; item < perWorker; item++ {
				id := fmt.Sprintf("w%02d-%03d", worker, item)
				batch.add(
					[]*graph.Node{{ID: id, Kind: graph.KindFunction}},
					[]*graph.Edge{{From: id, To: "target", Kind: graph.EdgeCalls}},
					func() {
						callbackMu.Lock()
						callbacks++
						callbackMu.Unlock()
					},
				)
			}
		}(worker)
	}
	wg.Wait()
	batch.flush()

	want := workers * perWorker
	if store.nodeCount != want || store.edgeCount != want || callbacks != want {
		t.Fatalf("nodes/edges/callbacks = %d/%d/%d, want %d each",
			store.nodeCount, store.edgeCount, callbacks, want)
	}
	wantCalls := (want + filesPerBatch - 1) / filesPerBatch
	if store.batchCalls != wantCalls {
		t.Fatalf("AddBatch calls = %d, want %d", store.batchCalls, wantCalls)
	}
	if store.maxBatchNode > filesPerBatch || store.maxBatchEdge > filesPerBatch {
		t.Fatalf("concurrent batch exceeded bound: nodes/edges = %d/%d",
			store.maxBatchNode, store.maxBatchEdge)
	}
}

func TestParseGraphBatchDurabilityCallbacksFollowCommit(t *testing.T) {
	store := newRecordingParseGraphStore()
	batch := newParseGraphBatchWithLimits(store, parseGraphBatchLimits{
		files: 4, nodes: 10, edges: 10, bytes: 1 << 20,
	})
	batch.add([]*graph.Node{{ID: "a", Kind: graph.KindFunction}}, nil, func() {
		store.recordEvent("durable-a")
	})
	batch.add([]*graph.Node{{ID: "b", Kind: graph.KindFunction}}, nil, func() {
		store.recordEvent("durable-b")
	})
	if len(store.events) != 0 {
		t.Fatalf("callbacks ran before flush: %v", store.events)
	}
	batch.flush()
	want := []string{"batch", "durable-a", "durable-b"}
	if len(store.events) != len(want) {
		t.Fatalf("events = %v, want %v", store.events, want)
	}
	for i := range want {
		if store.events[i] != want[i] {
			t.Fatalf("events = %v, want %v", store.events, want)
		}
	}
}

func TestParseGraphBatchFlushClearsStagedPointers(t *testing.T) {
	store := newRecordingParseGraphStore()
	batch := newParseGraphBatchWithLimits(store, parseGraphBatchLimits{
		files: 2,
		nodes: 10,
		edges: 10,
		bytes: 1 << 20,
	})
	node := &graph.Node{ID: "retained"}
	if !batch.add([]*graph.Node{node}, nil, func() {}) {
		t.Fatal("durable store did not accept staged graph")
	}
	batch.flush()
	if len(batch.pending) != 0 {
		t.Fatalf("pending length after flush = %d, want 0", len(batch.pending))
	}
	for i, entry := range batch.pending[:cap(batch.pending)] {
		if entry.nodes != nil || entry.edges != nil || entry.onDurable != nil {
			t.Fatalf("pending backing entry %d retained graph pointers: %+v", i, entry)
		}
	}
}

func TestNewParseGraphBatchFallsBackWithoutDurableStore(t *testing.T) {
	if batch := newParseGraphBatch(graph.New()); batch != nil {
		t.Fatal("in-memory store unexpectedly enabled direct-disk graph batching")
	}
}

func TestParseGraphBatchPriorAliasRemovalBatchesBeforeCommit(t *testing.T) {
	store := newRecordingParseGraphStore()
	// Seed prior graph rows for two JS/TS files, plus:
	// - an incoming cross-file edge targeting pages/a.ts::runA (must survive)
	// - a sibling-owned edge from pages/a.ts::runA whose FilePath is pages/b.ts
	//   (must survive even though both files are in the same batch, proving
	//   removal enforces e.FilePath == nodeFiles[fromID]).
	store.Store.AddBatch([]*graph.Node{
		{ID: "pages/a.ts::runA", Kind: graph.KindFunction, Name: "runA", FilePath: "pages/a.ts"},
		{ID: "pages/b.ts::runB", Kind: graph.KindFunction, Name: "runB", FilePath: "pages/b.ts"},
		{ID: "other/caller.ts::callA", Kind: graph.KindFunction, Name: "callA", FilePath: "other/caller.ts"},
	}, []*graph.Edge{
		{From: "pages/a.ts::runA", To: "old/auth.ts::fn", Kind: graph.EdgeCalls, FilePath: "pages/a.ts", Line: 2},
		{From: "pages/b.ts::runB", To: "old/auth.ts::fn", Kind: graph.EdgeCalls, FilePath: "pages/b.ts", Line: 2},
		{From: "other/caller.ts::callA", To: "pages/a.ts::runA", Kind: graph.EdgeCalls, FilePath: "other/caller.ts", Line: 5},
		{From: "pages/a.ts::runA", To: "sibling/target.ts::fn", Kind: graph.EdgeCalls, FilePath: "pages/b.ts", Line: 9},
	})

	batch := newParseGraphBatchWithLimits(store, parseGraphBatchLimits{
		files: 4, nodes: 100, edges: 100, bytes: 1 << 20,
	})
	batch.addWithPriorAlias(
		[]*graph.Node{{ID: "pages/a.ts::runA", Kind: graph.KindFunction, Name: "runA", FilePath: "pages/a.ts"}},
		[]*graph.Edge{{From: "pages/a.ts::runA", To: "unresolved::fn", Kind: graph.EdgeCalls, FilePath: "pages/a.ts", Line: 2}},
		"pages/a.ts",
		func() { store.recordEvent("durable-a") },
	)
	batch.addWithPriorAlias(
		[]*graph.Node{{ID: "pages/b.ts::runB", Kind: graph.KindFunction, Name: "runB", FilePath: "pages/b.ts"}},
		[]*graph.Edge{{From: "pages/b.ts::runB", To: "unresolved::fn", Kind: graph.EdgeCalls, FilePath: "pages/b.ts", Line: 2}},
		"pages/b.ts",
		func() { store.recordEvent("durable-b") },
	)
	if len(store.events) != 0 {
		t.Fatalf("writes or callbacks ran before flush: %v", store.events)
	}

	batch.flush()

	if store.removeCalls != 1 || store.removedCount != 2 || store.batchCalls != 1 {
		t.Fatalf("removeCalls/removedCount/batchCalls = %d/%d/%d, want 1/2/1",
			store.removeCalls, store.removedCount, store.batchCalls)
	}
	wantEvents := []string{"remove", "batch", "durable-a", "durable-b"}
	if len(store.events) != len(wantEvents) {
		t.Fatalf("events = %v, want %v", store.events, wantEvents)
	}
	for i := range wantEvents {
		if store.events[i] != wantEvents[i] {
			t.Fatalf("events = %v, want %v", store.events, wantEvents)
		}
	}

	var sawNewA, sawSiblingA bool
	for _, e := range store.GetOutEdges("pages/a.ts::runA") {
		if e.To == "old/auth.ts::fn" {
			t.Fatalf("stale prior alias edge survived on pages/a.ts: %+v", e)
		}
		if e.To == "unresolved::fn" && e.FilePath == "pages/a.ts" {
			sawNewA = true
		}
		if e.To == "sibling/target.ts::fn" && e.FilePath == "pages/b.ts" {
			sawSiblingA = true
		}
	}
	if !sawNewA || !sawSiblingA {
		t.Fatalf("pages/a.ts edges unexpected: sawNewA=%v sawSiblingA=%v", sawNewA, sawSiblingA)
	}

	var sawNewB bool
	for _, e := range store.GetOutEdges("pages/b.ts::runB") {
		if e.To == "old/auth.ts::fn" {
			t.Fatalf("stale prior alias edge survived on pages/b.ts: %+v", e)
		}
		if e.To == "unresolved::fn" && e.FilePath == "pages/b.ts" {
			sawNewB = true
		}
	}
	if !sawNewB {
		t.Fatalf("replacement edge missing on pages/b.ts")
	}

	var sawIncoming bool
	for _, e := range store.GetOutEdges("other/caller.ts::callA") {
		if e.To == "pages/a.ts::runA" {
			sawIncoming = true
		}
	}
	if !sawIncoming {
		t.Fatal("cross-file incoming edge from other/caller.ts was unexpectedly deleted")
	}

	for i, entry := range batch.pending[:cap(batch.pending)] {
		if entry.priorAliasFile != "" || entry.nodes != nil || entry.edges != nil || entry.onDurable != nil {
			t.Fatalf("pending backing entry %d retained state after flush: %+v", i, entry)
		}
	}
}
