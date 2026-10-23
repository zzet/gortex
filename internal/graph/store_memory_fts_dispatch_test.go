package graph_test

// Tests for the lean-mirror dispatch branch in MemoryStore.ensureFTSEngineNodes
// (issue davalillo/gortex#36, finding R3-dispatch-branch-untested): an engine
// implementing FTSNodeMirror must take the lean MirrorFTSNodes write, while an
// engine offering only FTSEngine falls back to AddBatch — both on the
// incremental upsert path the daemon's warmup and reconcile drives.
//
// The fakes are hand-rolled counters, not store_sqlite engines: the dispatch
// contract is about which write the MemoryStore issues, not about SQL
// behaviour (pinned separately in store_sqlite's mirror tests).

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// dispatchFakeEngine is a graph.FTSEngine that records the node writes it
// receives through AddBatch and (optionally) MirrorFTSNodes.
type dispatchFakeEngine struct {
	mirrored   []*graph.Node
	addBatched []*graph.Node
	addBatchedEdges []*graph.Edge
}

func (e *dispatchFakeEngine) UpsertSymbolFTS(nodeID, tokens string) error { return nil }
func (e *dispatchFakeEngine) BulkUpsertSymbolFTS(repoPrefix string, items []graph.SymbolFTSItem) error {
	return nil
}
func (e *dispatchFakeEngine) BuildSymbolIndex() error { return nil }
func (e *dispatchFakeEngine) SearchSymbols(query string, limit int) ([]graph.SymbolHit, error) {
	return nil, nil
}
func (e *dispatchFakeEngine) SearchSymbolsRepoScoped(query string, repoAllow []string, limit int) ([]graph.SymbolHit, error) {
	return nil, nil
}
func (e *dispatchFakeEngine) BatchUpsertSymbolFTS(items []graph.SymbolFTSItem) error { return nil }
func (e *dispatchFakeEngine) BatchDeleteSymbolFTS(nodeIDs []string) error            { return nil }
func (e *dispatchFakeEngine) ResetSymbolFTS(repoPrefix string) error                 { return nil }
func (e *dispatchFakeEngine) ReplaceSymbolFTS(repoPrefix string, produce func(emit func([]graph.SymbolFTSItem) error) error) error {
	return nil
}
func (e *dispatchFakeEngine) SymbolFTSCount() (int, error) { return 0, nil }
func (e *dispatchFakeEngine) GetSymbolFTSNormalization(repoPrefix string) (mode string, ok bool, err error) {
	return "", false, nil
}
func (e *dispatchFakeEngine) SetSymbolFTSNormalization(repoPrefix, mode string) error { return nil }
func (e *dispatchFakeEngine) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	e.addBatched = append(e.addBatched, nodes...)
	e.addBatchedEdges = append(e.addBatchedEdges, edges...)
}

// mirrorFakeEngine additionally implements graph.FTSNodeMirror.
type mirrorFakeEngine struct {
	dispatchFakeEngine
}

func (e *mirrorFakeEngine) MirrorFTSNodes(nodes []*graph.Node) error {
	e.mirrored = append(e.mirrored, nodes...)
	return nil
}

var (
	_ graph.FTSEngine     = (*dispatchFakeEngine)(nil)
	_ graph.FTSEngine     = (*mirrorFakeEngine)(nil)
	_ graph.FTSNodeMirror = (*mirrorFakeEngine)(nil)
)

// seedDispatchGraph registers two nodes on the resident graph so
// ensureFTSEngineNodes can resolve the FTS items' node rows.
func seedDispatchGraph(t *testing.T, s *graph.MemoryStore) {
	t.Helper()
	s.AddNode(ftsTestNode("app/a.go::Alpha", "AlphaWidget", "a.go", "app"))
	s.AddNode(ftsTestNode("app/b.go::Beta", "BetaWidget", "b.go", "app"))
}

// TestEnsureFTSEngineNodesPrefersMirror: a mirror-capable engine receives the
// FTS-admitted node stubs through MirrorFTSNodes and AddBatch is never
// invoked; edges are never forwarded either way.
func TestEnsureFTSEngineNodesPrefersMirror(t *testing.T) {
	engine := &mirrorFakeEngine{}
	s := graph.NewMemoryStore(graph.WithFTSEngine(engine))
	seedDispatchGraph(t, s)

	items := []graph.SymbolFTSItem{
		{NodeID: "app/a.go::Alpha", Tokens: "alpha"},
		{NodeID: "app/b.go::Beta", Tokens: "beta"},
	}
	if err := s.BatchUpsertSymbolFTS(items); err != nil {
		t.Fatalf("BatchUpsertSymbolFTS: %v", err)
	}

	if len(engine.addBatched) != 0 {
		t.Errorf("AddBatch called on a mirror-capable engine: %+v", engine.addBatched)
	}
	if len(engine.mirrored) != 2 {
		t.Fatalf("MirrorFTSNodes = %d nodes, want 2", len(engine.mirrored))
	}
	seen := map[string]*graph.Node{}
	for _, n := range engine.mirrored {
		seen[n.ID] = n
	}
	for _, id := range []string{"app/a.go::Alpha", "app/b.go::Beta"} {
		n, ok := seen[id]
		if !ok {
			t.Fatalf("MirrorFTSNodes missing %q: %+v", id, engine.mirrored)
		}
		if n.Name == "" || n.FilePath == "" || n.RepoPrefix != "app" {
			// The engine's filing join derives the owning repo prefix from
			// these columns — a stripped stub would file the doc unowned.
			t.Errorf("mirrored stub %q lost projection: %+v", id, n)
		}
	}
}

// TestEnsureFTSEngineNodesFallsBackToAddBatch: an engine without
// FTSNodeMirror takes the documented AddBatch fallback and is never asked to
// mirror.
func TestEnsureFTSEngineNodesFallsBackToAddBatch(t *testing.T) {
	engine := &dispatchFakeEngine{}
	s := graph.NewMemoryStore(graph.WithFTSEngine(engine))
	seedDispatchGraph(t, s)

	items := []graph.SymbolFTSItem{{NodeID: "app/a.go::Alpha", Tokens: "alpha"}}
	if err := s.BatchUpsertSymbolFTS(items); err != nil {
		t.Fatalf("BatchUpsertSymbolFTS: %v", err)
	}

	if len(engine.mirrored) != 0 {
		t.Errorf("non-mirror engine received MirrorFTSNodes calls: %+v", engine.mirrored)
	}
	if len(engine.addBatched) != 1 || engine.addBatched[0].ID != "app/a.go::Alpha" {
		t.Fatalf("AddBatch = %+v, want the single Alpha stub", engine.addBatched)
	}
	if len(engine.addBatchedEdges) != 0 {
		t.Errorf("AddBatch must never receive edges from the mirror path: %+v", engine.addBatchedEdges)
	}
}

// TestEnsureFTSEngineNodesDedupesAndSkipsMissing: duplicate NodeIDs mirror
// once per distinct id, empty ids and ids with no resident node row are
// dropped, and a fully-empty item set issues no engine writes at all.
func TestEnsureFTSEngineNodesDedupesAndSkipsMissing(t *testing.T) {
	engine := &mirrorFakeEngine{}
	s := graph.NewMemoryStore(graph.WithFTSEngine(engine))
	seedDispatchGraph(t, s)

	items := []graph.SymbolFTSItem{
		{NodeID: "app/a.go::Alpha", Tokens: "alpha"},
		{NodeID: "app/a.go::Alpha", Tokens: "alpha again"},
		{NodeID: "", Tokens: "empty"},
		{NodeID: "app/ghost.go::Ghost", Tokens: "not in the graph"},
	}
	if err := s.BatchUpsertSymbolFTS(items); err != nil {
		t.Fatalf("BatchUpsertSymbolFTS: %v", err)
	}
	if len(engine.mirrored) != 1 || engine.mirrored[0].ID != "app/a.go::Alpha" {
		t.Fatalf("MirrorFTSNodes = %+v, want exactly the resident Alpha stub", engine.mirrored)
	}

	empty := &mirrorFakeEngine{}
	emptyStore := graph.NewMemoryStore(graph.WithFTSEngine(empty))
	if err := emptyStore.BatchUpsertSymbolFTS(nil); err != nil {
		t.Fatalf("BatchUpsertSymbolFTS(nil): %v", err)
	}
	if len(empty.mirrored) != 0 || len(empty.addBatched) != 0 {
		t.Fatalf("empty item set reached the engine: mirror=%d addBatch=%d", len(empty.mirrored), len(empty.addBatched))
	}
}
