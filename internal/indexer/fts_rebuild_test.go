package indexer

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/progress"
)

// allNodesCountStore wraps a graph.Store and counts AllNodes walks. The
// memory backend's warm-boot FTS rebuild used to walk the whole store once
// PER REPO (O(repos × nodes), ~9 s on the 5-repo / 226k-node corpus) — this
// wrapper is how the one-pass rewrite proves it walks each distinct graph
// exactly once.
type allNodesCountStore struct {
	graph.Store
	calls int32
}

func (w *allNodesCountStore) AllNodes() []*graph.Node {
	atomic.AddInt32(&w.calls, 1)
	return w.Store.AllNodes()
}

func (w *allNodesCountStore) BatchUpsertSymbolFTS(items []graph.SymbolFTSItem) error {
	return w.Store.(graph.SymbolFTSBatchUpserter).BatchUpsertSymbolFTS(items)
}

func (w *allNodesCountStore) BuildSymbolIndex() error {
	return w.Store.(graph.SymbolSearcher).BuildSymbolIndex()
}

func ftsRebuildTestStore(t *testing.T) (*graph.MemoryStore, *allNodesCountStore) {
	t.Helper()
	eng, err := store_sqlite.Open(filepath.Join(t.TempDir(), "fts-engine.sqlite"))
	if err != nil {
		t.Fatalf("open fts engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	mem := graph.NewMemoryStore(graph.WithFTSEngine(eng))
	return mem, &allNodesCountStore{Store: mem}
}

func ftsRebuildTestNodes(mem *graph.MemoryStore) {
	nodes := []*graph.Node{
		{ID: "app/a.go::Alpha", Kind: graph.KindFunction, Name: "AlphaWidget", Language: "go", RepoPrefix: "app"},
		{ID: "app/a.go::AlphaHelper", Kind: graph.KindFunction, Name: "AlphaWidgetHelper", Language: "go", RepoPrefix: "app"},
		{ID: "beta/b.go::BetaWidget", Kind: graph.KindType, Name: "BetaWidget", Language: "go", RepoPrefix: "beta"},
		{ID: "beta/b.go::SkipFile", Kind: graph.KindFile, Name: "SkipFile", Language: "go", RepoPrefix: "beta"},
		{ID: "dep::ext::BetaWidget", Kind: graph.KindFunction, Name: "BetaWidgetExternal", Language: "go"},
	}
	for _, n := range nodes {
		mem.AddNode(n)
	}
}

// TestMultiIndexerRebuildSymbolFTS_OnePassPerGraph: with two indexers over
// one shared store, the warm-boot rebuild must walk the store's AllNodes
// surface exactly once (partitioning by prefix) and produce a corpus where
// every repo's scoped search finds its own symbols and the deliberate
// unowned globals — identical to what the per-repo walk produced.
func TestMultiIndexerRebuildSymbolFTS_OnePassPerGraph(t *testing.T) {
	mem, wrapped := ftsRebuildTestStore(t)
	ftsRebuildTestNodes(mem)

	idxApp := &Indexer{graph: wrapped, repoPrefix: "app"}
	idxBeta := &Indexer{graph: wrapped, repoPrefix: "beta"}
	mi := &MultiIndexer{indexers: map[string]*Indexer{"app": idxApp, "beta": idxBeta}, logger: zap.NewNop()}

	if err := mi.RebuildSymbolFTS(progress.Nop{}); err != nil {
		t.Fatalf("RebuildSymbolFTS: %v", err)
	}

	if got := atomic.LoadInt32(&wrapped.calls); got != 1 {
		t.Fatalf("AllNodes walked %d times, want exactly 1 (one pass per distinct graph)", got)
	}

	// Each repo's scoped search sees its own symbols; no cross-repo leak.
	// The dep:: global is NOT in the rebuild corpus by design: no indexer
	// owns the empty prefix (that bucket is partitioned but unconsumed),
	// matching the old per-repo walk — dep:: docs are written by the
	// incremental index path and survive boots because the rebuild only
	// upserts, it never resets the corpus.
	appHits, err := mem.SearchSymbolsRepoScoped("widget", []string{"app"}, 50)
	if err != nil {
		t.Fatalf("scoped(app): %v", err)
	}
	appFound := map[string]bool{}
	for _, h := range appHits {
		appFound[h.NodeID] = true
	}
	if !appFound["app/a.go::Alpha"] || !appFound["app/a.go::AlphaHelper"] {
		t.Fatalf("scoped(app) = %+v, want both app symbols", appHits)
	}
	if appFound["beta/b.go::BetaWidget"] {
		t.Fatalf("scoped(app) leaked a beta symbol: %+v", appHits)
	}

	betaHits, err := mem.SearchSymbolsRepoScoped("widget", []string{"beta"}, 50)
	if err != nil {
		t.Fatalf("scoped(beta): %v", err)
	}
	betaFound := map[string]bool{}
	for _, h := range betaHits {
		betaFound[h.NodeID] = true
	}
	if !betaFound["beta/b.go::BetaWidget"] {
		t.Fatalf("scoped(beta) = %+v, want BetaWidget", betaHits)
	}
	if betaFound["dep::ext::BetaWidget"] {
		t.Fatalf("rebuild must not write unowned-global docs (unchanged behavior): %+v", betaHits)
	}

	// Admission still applies: the KindFile node is not searchable.
	unscoped, err := mem.SearchSymbols("SkipFile", 10)
	if err != nil {
		t.Fatalf("unscoped: %v", err)
	}
	if len(unscoped) != 0 {
		t.Fatalf("KindFile node surfaced in FTS: %+v", unscoped)
	}
}

// TestPartitionNodesByPrefix: the one-pass partition keeps only non-nil
// nodes, keys them by their exact repo prefix, and preserves AllNodes order
// inside each bucket (the chunking order the per-repo walk produced).
func TestPartitionNodesByPrefix(t *testing.T) {
	app1 := &graph.Node{ID: "app/1", RepoPrefix: "app"}
	app2 := &graph.Node{ID: "app/2", RepoPrefix: "app"}
	beta := &graph.Node{ID: "beta/1", RepoPrefix: "beta"}
	got := partitionNodesByPrefix([]*graph.Node{app1, nil, beta, app2})
	if len(got["app"]) != 2 || got["app"][0] != app1 || got["app"][1] != app2 {
		t.Fatalf("app bucket = %+v, want [app1 app2] in order", got["app"])
	}
	if len(got["beta"]) != 1 || got["beta"][0] != beta {
		t.Fatalf("beta bucket = %+v, want [beta]", got["beta"])
	}
	if _, ok := got[""]; ok {
		t.Fatalf("nil nodes must be dropped, got a %q bucket", "")
	}
}
