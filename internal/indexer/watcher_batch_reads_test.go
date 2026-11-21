package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// watcherReadCountingStore proves watcher/indexing helpers drive the Store's
// bounded batch APIs. Embedding keeps this spy exact: any accidental return to
// a point lookup is counted while every unrelated operation delegates.
type watcherReadCountingStore struct {
	graph.Store
	getNodeCalls       int
	getNodesBatchCalls int
	getInCalls         int
	getInBatchCalls    int
	getOutCalls        int
	getOutBatchCalls   int
}

func (s *watcherReadCountingStore) GetNode(id string) *graph.Node {
	s.getNodeCalls++
	return s.Store.GetNode(id)
}

func (s *watcherReadCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.getNodesBatchCalls++
	return s.Store.GetNodesByIDs(ids)
}

func (s *watcherReadCountingStore) GetInEdges(id string) []*graph.Edge {
	s.getInCalls++
	return s.Store.GetInEdges(id)
}

func (s *watcherReadCountingStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.getInBatchCalls++
	return s.Store.GetInEdgesByNodeIDs(ids)
}

func (s *watcherReadCountingStore) GetOutEdges(id string) []*graph.Edge {
	s.getOutCalls++
	return s.Store.GetOutEdges(id)
}

func (s *watcherReadCountingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.getOutBatchCalls++
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *watcherReadCountingStore) resetReads() {
	s.getNodeCalls = 0
	s.getNodesBatchCalls = 0
	s.getInCalls = 0
	s.getInBatchCalls = 0
	s.getOutCalls = 0
	s.getOutBatchCalls = 0
}

func TestRestubIncomingRefsUsesOneInboundBatch(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "defs.go::F", Kind: graph.KindFunction, Name: "F", FilePath: "defs.go"},
		{ID: "defs.go::G", Kind: graph.KindFunction, Name: "G", FilePath: "defs.go"},
		{ID: "defs.go::local", Kind: graph.KindFunction, Name: "local", FilePath: "defs.go"},
		{ID: "caller.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "caller.go"},
		{ID: "caller.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "caller.go"},
	}, []*graph.Edge{
		{From: "caller.go::A", To: "defs.go::F", Kind: graph.EdgeCalls, Origin: graph.OriginLSPResolved},
		{From: "caller.go::B", To: "defs.go::G", Kind: graph.EdgeReferences, Origin: graph.OriginLSPResolved},
		{From: "defs.go::local", To: "defs.go::F", Kind: graph.EdgeCalls, Origin: graph.OriginLSPResolved},
	})

	counted := &watcherReadCountingStore{Store: g}
	idx := newTestIndexer(counted)
	_, err := idx.restubIncomingRefs("defs.go")
	require.NoError(t, err)

	if counted.getInCalls != 0 || counted.getInBatchCalls != 1 {
		t.Fatalf("incoming reads: point=%d batch=%d, want 0/1", counted.getInCalls, counted.getInBatchCalls)
	}
	assertTarget := func(from, want string) {
		t.Helper()
		for _, e := range g.GetOutEdges(from) {
			if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences {
				if e.To != want {
					t.Fatalf("%s target = %q, want %q", from, e.To, want)
				}
				return
			}
		}
		t.Fatalf("edge from %s not found", from)
	}
	assertTarget("caller.go::A", graph.UnresolvedMarker+"F")
	assertTarget("caller.go::B", graph.UnresolvedMarker+"G")
	assertTarget("defs.go::local", "defs.go::F") // intra-file edge is evicted with its source
}

func TestAffectedByShapeReadsAreFileBatchedAndEquivalent(t *testing.T) {
	g := graph.New()
	f := &graph.Node{ID: "defs.ts::F", Kind: graph.KindFunction, Name: "F", FilePath: "defs.ts", Meta: map[string]any{"signature": "F()"}}
	h := &graph.Node{ID: "defs.ts::H", Kind: graph.KindFunction, Name: "H", FilePath: "defs.ts", Meta: map[string]any{"signature": "H()"}}
	pF := &graph.Node{ID: "defs.ts::F.x", Kind: graph.KindParam, Name: "x", FilePath: "defs.ts", Meta: map[string]any{"position": 0, "type": "number"}}
	pH := &graph.Node{ID: "defs.ts::H.y", Kind: graph.KindParam, Name: "y", FilePath: "defs.ts", Meta: map[string]any{"position": 0, "type": "string", "variadic": true}}
	g.AddBatch([]*graph.Node{
		{ID: "defs.ts", Kind: graph.KindFile, Name: "defs.ts", FilePath: "defs.ts"},
		f, h, pF, pH,
		{ID: "caller.ts::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "caller.ts"},
	}, []*graph.Edge{
		{From: pF.ID, To: f.ID, Kind: graph.EdgeParamOf},
		{From: pH.ID, To: h.ID, Kind: graph.EdgeParamOf},
		{From: f.ID, To: "unresolved::number", Kind: graph.EdgeReturns, Meta: map[string]any{"position": 0}},
		{From: h.ID, To: "unresolved::string", Kind: graph.EdgeReturns, Meta: map[string]any{"position": 0}},
		{From: "caller.ts::Caller", To: f.ID, Kind: graph.EdgeCalls},
	})

	// The focused helper is the semantic reference implementation; the hot path
	// must produce the exact same composed shape from its shared adjacency.
	wantF := symbolShapeFor(g, f) + "\n"
	wantH := symbolShapeFor(g, h) + "\n"
	counted := &watcherReadCountingStore{Store: g}
	idx := newTestIndexer(counted)
	snap := idx.snapshotAffectedBy("defs.ts")
	if snap == nil {
		t.Fatal("snapshot is nil")
	}
	if got := snap.symbols[stableSymbolKey(f)].shape; got != wantF {
		t.Fatalf("F shape = %q, want %q", got, wantF)
	}
	if got := snap.symbols[stableSymbolKey(h)].shape; got != wantH {
		t.Fatalf("H shape = %q, want %q", got, wantH)
	}
	if counted.getInCalls != 0 || counted.getOutCalls != 0 || counted.getNodeCalls != 0 {
		t.Fatalf("shape point reads: in=%d out=%d node=%d, want all zero",
			counted.getInCalls, counted.getOutCalls, counted.getNodeCalls)
	}
	if counted.getInBatchCalls != 1 || counted.getOutBatchCalls != 1 || counted.getNodesBatchCalls != 0 {
		t.Fatalf("shape batches: in=%d out=%d nodes=%d, want 1/1/0 (params reused from file nodes)",
			counted.getInBatchCalls, counted.getOutBatchCalls, counted.getNodesBatchCalls)
	}

	counted.resetReads()
	delta := affectedByDelta(counted, snap, g.GetFileNodes("defs.ts"))
	if len(delta) != 0 {
		t.Fatalf("unchanged shapes produced delta %v", delta)
	}
	if counted.getInCalls != 0 || counted.getOutCalls != 0 || counted.getNodeCalls != 0 ||
		counted.getInBatchCalls != 1 || counted.getOutBatchCalls != 1 || counted.getNodesBatchCalls != 0 {
		t.Fatalf("delta reads: point(in/out/node)=%d/%d/%d batch(in/out/nodes)=%d/%d/%d",
			counted.getInCalls, counted.getOutCalls, counted.getNodeCalls,
			counted.getInBatchCalls, counted.getOutBatchCalls, counted.getNodesBatchCalls)
	}
}

func TestIncrementalReusePrefetchesTargetsOnce(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go"},
		{ID: "a.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "a.go"},
		{ID: "lib.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "lib.go"},
	}, []*graph.Edge{
		{From: "a.go::A", To: "lib.go::Target", Kind: graph.EdgeCalls, Origin: graph.OriginLSPResolved, Tier: graph.ResolvedBy(graph.OriginLSPResolved), Confidence: 1},
		{From: "a.go::B", To: "lib.go::Target", Kind: graph.EdgeReferences, Origin: graph.OriginLSPResolved, Tier: graph.ResolvedBy(graph.OriginLSPResolved), Confidence: 1},
	})
	counted := &watcherReadCountingStore{Store: g}
	reuse, unresolved, _ := captureIncrementalState(counted, "a.go")
	if len(unresolved) != 0 || len(reuse) != 2 {
		t.Fatalf("capture sizes: reuse=%d unresolved=%d, want 2/0", len(reuse), len(unresolved))
	}
	if counted.getNodeCalls != 0 || counted.getNodesBatchCalls != 1 || counted.getOutBatchCalls != 1 {
		t.Fatalf("capture reads: node=%d nodeBatch=%d outBatch=%d, want 0/1/1",
			counted.getNodeCalls, counted.getNodesBatchCalls, counted.getOutBatchCalls)
	}

	counted.resetReads()
	edges := []*graph.Edge{
		{From: "a.go::A", To: graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls},
		{From: "a.go::B", To: graph.UnresolvedMarker + "Target", Kind: graph.EdgeReferences},
	}
	if reused := applyResolvedOutEdges(counted, edges, reuse, nil); reused != 2 {
		t.Fatalf("reused = %d, want 2", reused)
	}
	if counted.getNodeCalls != 0 || counted.getNodesBatchCalls != 1 {
		t.Fatalf("apply target reads: point=%d batch=%d, want 0/1", counted.getNodeCalls, counted.getNodesBatchCalls)
	}
	for _, e := range edges {
		if e.To != "lib.go::Target" || e.Origin != graph.OriginLSPResolved {
			t.Fatalf("reused edge = %+v, want Target with preserved LSP provenance", e)
		}
	}
}

func TestTestMetadataReadsAreBatchedAndPreserveRunnerSemantics(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "tests/test_a.py", Kind: graph.KindFile, Name: "tests/test_a.py", FilePath: "tests/test_a.py", Language: "python"},
		{ID: "tests/widget_spec.rb", Kind: graph.KindFile, Name: "tests/widget_spec.rb", FilePath: "tests/widget_spec.rb", Language: "ruby"},
		{ID: "src/lib.rs", Kind: graph.KindFile, Name: "src/lib.rs", FilePath: "src/lib.rs", Language: "rust"},
		{ID: "tests/test_a.py::test_a", Kind: graph.KindFunction, Name: "test_a", FilePath: "tests/test_a.py", Language: "python"},
		{ID: "tests/widget_spec.rb::works", Kind: graph.KindFunction, Name: "works", FilePath: "tests/widget_spec.rb", Language: "ruby"},
		{ID: "src/lib.rs::it_works", Kind: graph.KindFunction, Name: "it_works", FilePath: "src/lib.rs", Language: "rust"},
		{ID: "src/lib.rs::subject", Kind: graph.KindFunction, Name: "subject", FilePath: "src/lib.rs", Language: "rust"},
		{ID: "annotation::rust::test", Kind: graph.KindType, Name: "test", Language: "rust"},
	}, []*graph.Edge{
		{From: "tests/test_a.py", To: "unresolved::import::unittest", Kind: graph.EdgeImports},
		{From: "tests/widget_spec.rb", To: "unresolved::import::rspec/core", Kind: graph.EdgeImports},
		{From: "src/lib.rs::it_works", To: "annotation::rust::test", Kind: graph.EdgeAnnotated},
		{From: "tests/test_a.py::test_a", To: "src/lib.rs::subject", Kind: graph.EdgeCalls},
		{From: "tests/widget_spec.rb::works", To: "src/lib.rs::subject", Kind: graph.EdgeCalls},
		{From: "src/lib.rs::it_works", To: "src/lib.rs::subject", Kind: graph.EdgeCalls},
	})

	counted := &watcherReadCountingStore{Store: g}
	marked, emitted := markTestSymbolsAndEmitEdges(counted)
	if marked != 3 || emitted != 3 {
		t.Fatalf("mark/emit = %d/%d, want 3/3", marked, emitted)
	}
	if counted.getOutCalls != 0 || counted.getNodeCalls != 0 {
		t.Fatalf("test metadata point reads: out=%d node=%d, want zero", counted.getOutCalls, counted.getNodeCalls)
	}
	if counted.getOutBatchCalls != 1 || counted.getNodesBatchCalls != 1 {
		t.Fatalf("test metadata batches: file adjacency=%d annotation nodes=%d, want 1/1",
			counted.getOutBatchCalls, counted.getNodesBatchCalls)
	}
	wantRunner := map[string]string{
		"tests/test_a.py":             "unittest",
		"tests/widget_spec.rb":        "rspec",
		"src/lib.rs::it_works":        "cargo-test",
		"tests/test_a.py::test_a":     "unittest",
		"tests/widget_spec.rb::works": "rspec",
	}
	for id, want := range wantRunner {
		n := g.GetNode(id)
		if n == nil {
			t.Fatalf("node %s missing", id)
		}
		if got, _ := n.Meta["test_runner"].(string); got != want {
			t.Errorf("%s runner = %q, want %q", id, got, want)
		}
	}
}
