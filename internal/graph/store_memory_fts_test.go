package graph_test

// FTS sidecar tests for the memory backend (issue davalillo/gortex#21). The
// engine under test is a real store_sqlite.Store on a ":memory:" DSN — the
// same construction the daemon uses — so these tests exercise the exact
// delegation surface and ranking SQL the sqlite backend runs. The tests live
// in the external graph_test package because store_sqlite imports graph; an
// in-package test file could not reference the engine type without an import
// cycle.

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func newMemoryStoreWithFTSEngine(t *testing.T) *graph.MemoryStore {
	t.Helper()
	engine, err := store_sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("open fts engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return graph.NewMemoryStore(graph.WithFTSEngine(engine))
}

func ftsTestNode(id, name, file, repo string) *graph.Node {
	return &graph.Node{
		ID:        id,
		Kind:      graph.KindFunction,
		Name:      name,
		FilePath:  file,
		RepoPrefix: repo,
		StartLine: 1,
		EndLine:   10,
		Language:  "go",
	}
}

// TestMemoryStoreFTSSearchRoundTrip covers the full read surface: ranked
// hits, bundle materialization from the resident graph (node + in/out edges),
// incremental delete, and repo reset.
func TestMemoryStoreFTSSearchRoundTrip(t *testing.T) {
	s := newMemoryStoreWithFTSEngine(t)

	s.AddNode(ftsTestNode("a", "AlphaWidget", "x.go", ""))
	s.AddNode(ftsTestNode("b", "BetaWidget", "x.go", ""))
	s.AddNode(ftsTestNode("c", "GammaWidget", "y.go", ""))
	s.AddNode(ftsTestNode("d", "Delta", "y.go", ""))
	s.AddEdge(&graph.Edge{From: "a", To: "b", Kind: graph.EdgeCalls})
	s.AddEdge(&graph.Edge{From: "b", To: "c", Kind: graph.EdgeCalls})

	items := []graph.SymbolFTSItem{
		{NodeID: "a", Tokens: "alpha widget"},
		{NodeID: "b", Tokens: "beta widget"},
		{NodeID: "c", Tokens: "gamma widget"},
		{NodeID: "d", Tokens: "delta"},
	}
	if err := s.BulkUpsertSymbolFTS("", items); err != nil {
		t.Fatalf("BulkUpsertSymbolFTS: %v", err)
	}
	if err := s.BuildSymbolIndex(); err != nil {
		t.Fatalf("BuildSymbolIndex: %v", err)
	}

	count, err := s.SymbolFTSCount()
	if err != nil {
		t.Fatalf("SymbolFTSCount: %v", err)
	}
	if count != 4 {
		t.Fatalf("SymbolFTSCount = %d, want 4", count)
	}

	hits, err := s.SearchSymbols("widget", 10)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.NodeID] = true
	}
	for _, want := range []string{"a", "b", "c"} {
		if !got[want] {
			t.Errorf("SearchSymbols(widget) missing %q; got %v", want, hits)
		}
	}
	if got["d"] {
		t.Errorf("SearchSymbols(widget) returned 'd' — no widget token in its row")
	}

	// Bundles carry the node body AND the resident graph's edges — this is
	// the store-boundary step the engine cannot do (it holds no node rows).
	bundles, err := s.SearchSymbolBundles("widget", 10)
	if err != nil {
		t.Fatalf("SearchSymbolBundles: %v", err)
	}
	byID := map[string]graph.SymbolBundle{}
	for _, b := range bundles {
		if b.Node == nil {
			t.Fatalf("bundle with nil node")
		}
		byID[b.Node.ID] = b
	}
	if len(byID) != 3 {
		t.Fatalf("SearchSymbolBundles(widget) = %d bundles, want 3", len(byID))
	}
	if b := byID["a"]; len(b.OutEdges) != 1 || b.OutEdges[0].To != "b" {
		t.Errorf("bundle[a].OutEdges = %v, want one edge a→b", b.OutEdges)
	}
	if b := byID["b"]; len(b.InEdges) != 1 || b.InEdges[0].From != "a" {
		t.Errorf("bundle[b].InEdges = %v, want one edge a→b", b.InEdges)
	}

	// Incremental delete removes exactly the supplied documents.
	if err := s.BatchDeleteSymbolFTS([]string{"a"}); err != nil {
		t.Fatalf("BatchDeleteSymbolFTS: %v", err)
	}
	hits, err = s.SearchSymbols("widget", 10)
	if err != nil {
		t.Fatalf("SearchSymbols after delete: %v", err)
	}
	for _, h := range hits {
		if h.NodeID == "a" {
			t.Errorf("deleted doc 'a' still matches")
		}
	}

	// Repo reset wipes the whole corpus.
	if err := s.ResetSymbolFTS(""); err != nil {
		t.Fatalf("ResetSymbolFTS: %v", err)
	}
	count, err = s.SymbolFTSCount()
	if err != nil {
		t.Fatalf("SymbolFTSCount after reset: %v", err)
	}
	if count != 0 {
		t.Fatalf("SymbolFTSCount after reset = %d, want 0", count)
	}
}

// TestMemoryStoreFTSRepoScoped pins the narrowing INSIDE the FTS query (the
// sqlite-side contract): scoped search never returns out-of-repo rows even
// when they rank higher.
func TestMemoryStoreFTSRepoScoped(t *testing.T) {
	s := newMemoryStoreWithFTSEngine(t)

	s.AddNode(ftsTestNode("r1/a", "AlphaWidget", "x.go", "repo1"))
	s.AddNode(ftsTestNode("r2/b", "BetaWidget", "y.go", "repo2"))

	items := []graph.SymbolFTSItem{
		{NodeID: "r1/a", Tokens: "alpha widget"},
		{NodeID: "r2/b", Tokens: "beta widget"},
	}
	// BulkUpsertSymbolFTS takes the owning repo prefix as a parameter — one
	// call per repository, mirroring the indexer's cold path.
	if err := s.BulkUpsertSymbolFTS("repo1", items[:1]); err != nil {
		t.Fatalf("BulkUpsertSymbolFTS(repo1): %v", err)
	}
	if err := s.BulkUpsertSymbolFTS("repo2", items[1:]); err != nil {
		t.Fatalf("BulkUpsertSymbolFTS(repo2): %v", err)
	}
	if err := s.BuildSymbolIndex(); err != nil {
		t.Fatalf("BuildSymbolIndex: %v", err)
	}

	bundles, err := s.SearchSymbolBundlesRepoScoped("widget", []string{"repo2"}, 10)
	if err != nil {
		t.Fatalf("SearchSymbolBundlesRepoScoped: %v", err)
	}
	if len(bundles) != 1 || bundles[0].Node == nil || bundles[0].Node.ID != "r2/b" {
		t.Fatalf("scoped search = %+v, want only r2/b", bundles)
	}
}

// TestMemoryStoreFTSEnginelessInert pins the pre-#21 fallback: an engine-less
// MemoryStore answers every FTS capability inertly (no panic, no results)
// instead of lying about a search backend it cannot serve.
func TestMemoryStoreFTSEnginelessInert(t *testing.T) {
	s := graph.NewMemoryStore()
	s.AddNode(ftsTestNode("a", "AlphaWidget", "x.go", ""))

	if hits, err := s.SearchSymbols("widget", 10); err != nil || len(hits) != 0 {
		t.Fatalf("engine-less SearchSymbols = %v, %v; want empty, nil", hits, err)
	}
	if bundles, err := s.SearchSymbolBundles("widget", 10); err != nil || len(bundles) != 0 {
		t.Fatalf("engine-less SearchSymbolBundles = %v, %v; want empty, nil", bundles, err)
	}
	if count, err := s.SymbolFTSCount(); err != nil || count != 0 {
		t.Fatalf("engine-less SymbolFTSCount = %d, %v; want 0, nil", count, err)
	}
	if err := s.UpsertSymbolFTS("a", "alpha"); err != nil {
		t.Fatalf("engine-less UpsertSymbolFTS = %v; want nil", err)
	}
}
