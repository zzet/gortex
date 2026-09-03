package query

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

// overlayBundleBackend is a bundle-capable backend whose payload is
// frozen at base state — the shape every disk backend has, since the
// search corpus indexes the base graph and knows nothing about a
// per-request overlay.
type overlayBundleBackend struct {
	bundles []search.SymbolBundle
}

func (b *overlayBundleBackend) Add(string, ...string) {}
func (b *overlayBundleBackend) Remove(string)         {}
func (b *overlayBundleBackend) Search(string, int) []search.SearchResult {
	out := make([]search.SearchResult, 0, len(b.bundles))
	for _, bundle := range b.bundles {
		out = append(out, search.SearchResult{ID: bundle.Node.ID, Score: bundle.Score})
	}
	return out
}
func (b *overlayBundleBackend) Count() int { return len(b.bundles) }
func (b *overlayBundleBackend) Close()     {}

func (b *overlayBundleBackend) SearchSymbolBundles(string, int) []search.SymbolBundle {
	out := make([]search.SymbolBundle, len(b.bundles))
	copy(out, b.bundles)
	return out
}

const (
	overlayBundleFile   = "repo/buffer.go"
	overlayBundleKeptID = overlayBundleFile + "::Widget"
	overlayBundleGoneID = overlayBundleFile + "::WidgetLegacy"
	overlayBundleCaller = "repo/caller.go::Caller"
)

// overlayBundleFixture returns the base graph, a bundle backend built
// from it, and the overlay layer that replaces one indexed symbol and
// deletes the other.
func overlayBundleFixture(t *testing.T) (*graph.Graph, *overlayBundleBackend, *graph.OverlayLayer, *graph.Node) {
	t.Helper()
	base := graph.New()
	kept := &graph.Node{
		ID: overlayBundleKeptID, Name: "Widget", Kind: graph.KindFunction,
		FilePath: overlayBundleFile, RepoPrefix: "repo", StartLine: 10,
	}
	gone := &graph.Node{
		ID: overlayBundleGoneID, Name: "WidgetLegacy", Kind: graph.KindFunction,
		FilePath: overlayBundleFile, RepoPrefix: "repo", StartLine: 20,
	}
	caller := &graph.Node{
		ID: overlayBundleCaller, Name: "Caller", Kind: graph.KindFunction,
		FilePath: "repo/caller.go", RepoPrefix: "repo",
	}
	base.AddNode(kept)
	base.AddNode(gone)
	base.AddNode(caller)
	callKept := &graph.Edge{From: overlayBundleCaller, To: overlayBundleKeptID, Kind: graph.EdgeCalls, FilePath: "repo/caller.go", Line: 5}
	callGone := &graph.Edge{From: overlayBundleCaller, To: overlayBundleGoneID, Kind: graph.EdgeCalls, FilePath: "repo/caller.go", Line: 6}
	base.AddEdge(callKept)
	base.AddEdge(callGone)

	backend := &overlayBundleBackend{bundles: []search.SymbolBundle{
		{Node: kept, Score: 10, InEdges: []*graph.Edge{callKept}},
		{Node: gone, Score: 9, InEdges: []*graph.Edge{callGone}},
	}}

	replacement := &graph.Node{
		ID: overlayBundleKeptID, Name: "Widget", Kind: graph.KindFunction,
		FilePath: overlayBundleFile, RepoPrefix: "repo", StartLine: 44,
	}
	layer := graph.NewOverlayLayer()
	layer.MarkFile(overlayBundleFile, false)
	layer.AddNode(overlayBundleFile, replacement)
	layer.MarkRemoved("WidgetLegacy", overlayBundleGoneID)
	return base, backend, layer, replacement
}

func candidateByID(cands []*rerank.Candidate, id string) *rerank.Candidate {
	for _, c := range cands {
		if c != nil && c.Node != nil && c.Node.ID == id {
			return c
		}
	}
	return nil
}

// TestGatherSymbolCandidates_OverlayReaderRehydratesBundles is the
// leakage regression: a bundle backend hands back the pre-edit node for
// a symbol the buffer changed and the full record of one the buffer
// deleted. Under an overlay reader the engine must re-read every bundle
// hit through that reader — the replaced symbol comes back with the
// overlay's payload, the deleted one never surfaces.
func TestGatherSymbolCandidates_OverlayReaderRehydratesBundles(t *testing.T) {
	base, backend, layer, replacement := overlayBundleFixture(t)
	engine := NewEngine(base)
	engine.SetSearch(backend)

	view := graph.NewOverlaidView(base, layer)
	rctx := &rerank.Context{Graph: view}
	opts := QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}
	got := engine.WithReader(view).GatherSymbolCandidates("Widget", 5, opts, rctx)

	if c := candidateByID(got, overlayBundleGoneID); c != nil {
		t.Fatalf("a symbol the overlay deleted came back from the bundle path: %#v", c.Node)
	}
	kept := candidateByID(got, overlayBundleKeptID)
	if kept == nil {
		t.Fatalf("the replaced symbol is missing from %d candidates", len(got))
	}
	if kept.Node != replacement {
		t.Fatalf("candidate carries the base payload (line %d), want the overlay's (line %d)",
			kept.Node.StartLine, replacement.StartLine)
	}
	if len(got) != 1 {
		t.Fatalf("gather returned %d candidates, want only the replacement", len(got))
	}
	if rctx.CachePreSeeded() {
		t.Fatal("base edge bundles were seeded into the rerank context under an overlay reader")
	}
}

// TestGatherSymbolCandidates_TombstoneReaderDropsEveryBundleHit covers
// the tombstoned-file variant: the whole buffer is gone, so no bundle
// hit from it may reach the candidate slice.
func TestGatherSymbolCandidates_TombstoneReaderDropsEveryBundleHit(t *testing.T) {
	base, backend, _, _ := overlayBundleFixture(t)
	engine := NewEngine(base)
	engine.SetSearch(backend)

	layer := graph.NewOverlayLayer()
	layer.MarkFile(overlayBundleFile, true)
	layer.MarkRemoved("Widget", overlayBundleKeptID)
	layer.MarkRemoved("WidgetLegacy", overlayBundleGoneID)
	view := graph.NewOverlaidView(base, layer)

	opts := QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}
	got := engine.WithReader(view).GatherSymbolCandidates("Widget", 5, opts, &rerank.Context{Graph: view})
	if len(got) != 0 {
		t.Fatalf("gather over a tombstoned file returned %d candidates, want none", len(got))
	}
}

// TestGatherSymbolCandidates_BaseReaderKeepsBundleFastPath guards the
// other side: with no overlay installed the bundle payload is still
// taken verbatim and its edges still pre-seed the rerank context.
func TestGatherSymbolCandidates_BaseReaderKeepsBundleFastPath(t *testing.T) {
	base, backend, layer, _ := overlayBundleFixture(t)
	engine := NewEngine(base)
	engine.SetSearch(backend)

	rctx := &rerank.Context{Graph: base}
	opts := QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}
	got := engine.GatherSymbolCandidates("Widget", 5, opts, rctx)
	if len(got) != 2 {
		t.Fatalf("base gather returned %d candidates, want both bundle hits", len(got))
	}
	kept := candidateByID(got, overlayBundleKeptID)
	if kept == nil || kept.Node != backend.bundles[0].Node {
		t.Fatalf("base gather did not hand back the bundle's own payload")
	}
	if !rctx.CachePreSeeded() {
		t.Fatal("the bundle edge pre-seed was skipped without an overlay reader")
	}

	// Undoing the swap restores the fast path too.
	restored := engine.WithReader(graph.NewOverlaidView(base, layer)).WithReader(base)
	freshCtx := &rerank.Context{Graph: base}
	if got := restored.GatherSymbolCandidates("Widget", 5, opts, freshCtx); len(got) != 2 {
		t.Fatalf("gather after restoring the base reader returned %d candidates, want 2", len(got))
	}
	if !freshCtx.CachePreSeeded() {
		t.Fatal("restoring the base reader left the bundle pre-seed disabled")
	}
}
