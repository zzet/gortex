package mcp

import (
	"context"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

// --- walk-cache scope (unit) ---

// The content-addressed walk key describes the seed neighbourhood, not which
// snapshot produced it. Two views of one repository can therefore produce the
// same walk key; the scope is what keeps their entries apart.
func TestWalkCacheScope_SeparatesViewsAndSources(t *testing.T) {
	const walkKey = "deadbeef"

	base := pprCacheScope{source: pprWalkSourceSharedAnalysis, cacheable: true}
	viewA := pprCacheScope{source: pprWalkSourceSelectedReader, view: "view\x00fpA\x00/wt/a", cacheable: true}
	viewB := pprCacheScope{source: pprWalkSourceSelectedReader, view: "view\x00fpB\x00/wt/b", cacheable: true}

	require.NotEmpty(t, base.key(walkKey))
	assert.Equal(t, base.key(walkKey), base.key(walkKey), "one scope must key one walk stably")
	assert.NotEqual(t, viewA.key(walkKey), viewB.key(walkKey),
		"two views at different generations must never share a walk entry")
	assert.NotEqual(t, base.key(walkKey), viewA.key(walkKey),
		"a selected view must not share the shared corpus's walk entry")

	// Two snapshots of different provenance over the same view are different
	// snapshots and must not share either.
	sameViewOtherSource := pprCacheScope{source: pprWalkSourceSharedAnalysis, view: viewA.view, cacheable: true}
	assert.NotEqual(t, viewA.key(walkKey), sameViewOtherSource.key(walkKey))

	// One view, two bounded shapes: the walk key describes the seed
	// neighbourhood, so a narrower and a wider bound over one view can carry
	// the same walk key over materially different graphs. The root set is
	// part of the snapshot's identity.
	narrow := viewA.withRoots("rootsNarrow")
	wide := viewA.withRoots("rootsWide")
	assert.NotEqual(t, narrow.key(walkKey), wide.key(walkKey),
		"two bounded shapes over one view must not share a walk entry")
	assert.NotEqual(t, viewA.key(walkKey), narrow.key(walkKey),
		"a whole-graph scope and a bounded scope are different snapshots")

	// "" is the cache's do-not-store key.
	assert.Empty(t, pprCacheScope{}.key(walkKey), "the zero scope must be uncacheable")
	assert.Empty(t, base.key(""), "an empty walk key stays empty")
	assert.Empty(t, pprCacheScope{}.withRoots("r").key(walkKey),
		"naming a root set must not make an uncacheable scope cacheable")
}

// The unscoped wrapper takes no context, so it can name neither the view the
// request read nor the bound its caller put on the snapshot. Its callers'
// snapshots are built from readerFor (analysis_lazy_consumers.go:174), which
// is view- AND editor-buffer-aware, so sharing one namespace across them is
// the same mixed-view hazard the scoped path closes. It must not cache.
func TestPersonalizedPageRank_UnscopedWrapperNeverSharesEntries(t *testing.T) {
	srv := &Server{pprCache: newPPRWalkCache()}
	snap := analysis.BuildAdjacencySnapshot(walkTestGraph(t))
	seeds := []string{"a.go::A"}
	require.NotEmpty(t, snap.WalkCacheKey(seeds, 0), "fixture must produce a content key")

	require.NotEmpty(t, srv.personalizedPageRank(snap, seeds))
	require.NotEmpty(t, srv.personalizedPageRank(snap, seeds))

	hits, _, size, _, _ := srv.pprCache.stats()
	assert.Zero(t, size, "a walk whose snapshot identity cannot be named must not enter the shared cache")
	assert.Zero(t, hits, "…and must never be served from it either")
}

func TestWalkCacheScope_FromRequestContext(t *testing.T) {
	bg := context.Background()

	unrouted := walkCacheScope(bg, pprWalkSourceSharedAnalysis)
	require.True(t, unrouted.cacheable)
	assert.Empty(t, unrouted.view, "the shared corpus is the empty view identity")

	routed := walkCacheScope(withRequestView(bg, &requestView{
		reader:   graph.New(),
		viewRoot: "/wt/a",
		rider:    &graphview.ViewRider{GraphID: "g1", CheckoutID: "c1", ViewFingerprint: "fp1"},
	}), pprWalkSourceSelectedReader)
	require.True(t, routed.cacheable)
	assert.NotEmpty(t, routed.view, "a routed request must name its selected snapshot")
	assert.NotEqual(t, unrouted.key("k"), routed.key("k"))

	other := walkCacheScope(withRequestView(bg, &requestView{
		reader:   graph.New(),
		viewRoot: "/wt/b",
		rider:    &graphview.ViewRider{GraphID: "g1", CheckoutID: "c2", ViewFingerprint: "fp2"},
	}), pprWalkSourceSelectedReader)
	assert.NotEqual(t, routed.key("k"), other.key("k"),
		"two checkouts of one graph must key differently")

	// Editor buffers are per-session mutable state no durable identity names.
	overlaid := walkCacheScope(WithOverlayView(bg, graph.NewOverlaidView(graph.New(), nil)), pprWalkSourceSelectedReader)
	assert.False(t, overlaid.cacheable, "a buffer-composed request must not share walk entries")
	assert.Empty(t, overlaid.key("k"))
}

// The cache itself must hold one entry per (snapshot, walk), and serve a
// scope only its own entry.
func TestPPRWalkCache_ScopedEntriesNeverCrossViews(t *testing.T) {
	srv := &Server{pprCache: newPPRWalkCache()}
	snap := analysis.BuildAdjacencySnapshot(walkTestGraph(t))
	seeds := []string{"a.go::A"}
	require.NotEmpty(t, snap.WalkCacheKey(seeds, 0), "fixture must produce a content key")

	viewA := pprCacheScope{source: pprWalkSourceSelectedReader, view: "view\x00fpA", cacheable: true}
	viewB := pprCacheScope{source: pprWalkSourceSelectedReader, view: "view\x00fpB", cacheable: true}

	require.NotEmpty(t, srv.personalizedPageRankScoped(viewA, snap, seeds))
	require.NotEmpty(t, srv.personalizedPageRankScoped(viewB, snap, seeds))

	hits, misses, size, _, _ := srv.pprCache.stats()
	assert.Equal(t, 2, size, "identical walks under two view identities must hold two entries")
	assert.Zero(t, hits, "the second view must not hit the first view's entry")
	assert.EqualValues(t, 2, misses)

	// The same view hits its own entry.
	require.NotEmpty(t, srv.personalizedPageRankScoped(viewA, snap, seeds))
	hits, _, size, _, _ = srv.pprCache.stats()
	assert.EqualValues(t, 1, hits, "a repeat walk on the same view must hit")
	assert.Equal(t, 2, size)

	// An uncacheable scope computes without touching the cache at all.
	require.NotEmpty(t, srv.personalizedPageRankScoped(pprCacheScope{}, snap, seeds))
	_, _, size, _, _ = srv.pprCache.stats()
	assert.Equal(t, 2, size, "an uncacheable scope must not add an entry")
}

// walkTestGraph is a two-package call chain with enough structure for a
// seeded walk to spread mass beyond the seed.
func walkTestGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New()
	for _, n := range []struct{ id, name, file string }{
		{"a.go::A", "A", "a.go"},
		{"b.go::B", "B", "b.go"},
		{"c.go::C", "C", "c.go"},
	} {
		g.AddNode(&graph.Node{ID: n.id, Kind: graph.KindFunction, Name: n.name, FilePath: n.file, Language: "go"})
	}
	g.AddEdge(&graph.Edge{From: "a.go::A", To: "b.go::B", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2})
	g.AddEdge(&graph.Edge{From: "b.go::B", To: "c.go::C", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 2})
	return g
}

// --- production entrypoint: context_closure rank=proximity ---

// closureViewGraph is a selected view's content: the same seed symbol, but a
// callee the indexed corpus has never contained.
func closureViewGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New()
	add := func(id, name, file string) {
		g.AddNode(&graph.Node{
			ID: id, Kind: graph.KindFunction, Name: name, FilePath: file,
			Language: "go", StartLine: 1, EndLine: 3,
			Meta: map[string]any{"signature": "func " + name + "()"},
		})
	}
	add("a.go::A", "A", "a.go")
	add("z.go::Z", "Z", "z.go")
	g.AddEdge(&graph.Edge{From: "a.go::A", To: "z.go::Z", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2})
	return g
}

func callClosureWithContext(t *testing.T, srv *Server, ctx context.Context, args map[string]any) map[string]any {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "context_closure"
	req.Params.Arguments = args
	res, err := findAndCallHandler(srv, "context_closure", ctx, req)
	require.NoError(t, err)
	return extractTextResult(t, res)
}

func memberProximity(t *testing.T, out map[string]any, id string) (float64, bool) {
	t.Helper()
	members, ok := out["members"].([]any)
	require.True(t, ok, "members must be a list")
	for _, m := range members {
		row := m.(map[string]any)
		if row["id"] == id {
			p, hasP := row["proximity"].(float64)
			require.True(t, hasP, "proximity ranking must attach a score to %s", id)
			return p, true
		}
	}
	return 0, false
}

// A request that selected a view must be ranked by a snapshot of the graph it
// actually read. The shared analysis pass's CSR describes the indexed corpus
// only: scoring a selected view with it gives every symbol the view added a
// zero, and gives the base's own symbols mass the view may not have.
func TestContextClosureProximity_RanksTheSelectedSnapshot(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedClosureGraph(t, srv)
	srv.RunAnalysis() // the shared snapshot now describes the base corpus

	// Sanity: on the base corpus the shared snapshot still ranks normally.
	baseOut := callClosureWithContext(t, srv, context.Background(), map[string]any{
		"symbols":    "a.go::A",
		"edge_kinds": "calls,references",
		"rank":       "proximity",
	})
	baseB, ok := memberProximity(t, baseOut, "b.go::B")
	require.True(t, ok, "the base closure must contain b.go::B")
	assert.Greater(t, baseB, 0.0, "the shared snapshot must still rank an unrouted request")

	// A routed request reads a view whose callee the base corpus never had.
	viewCtx := withRequestView(context.Background(), &requestView{
		reader:   closureViewGraph(t),
		viewRoot: "/wt/a",
		rider:    &graphview.ViewRider{GraphID: "g1", CheckoutID: "c1", ViewFingerprint: "fp1"},
	})
	viewOut := callClosureWithContext(t, srv, viewCtx, map[string]any{
		"symbols":    "a.go::A",
		"edge_kinds": "calls,references",
		"rank":       "proximity",
	})
	viewZ, ok := memberProximity(t, viewOut, "z.go::Z")
	require.True(t, ok, "the selected view's closure must contain z.go::Z")
	assert.Greater(t, viewZ, 0.0,
		"a selected view's members must be scored by the view's own snapshot, not the shared corpus's")

	// The base's own callee is not in the selected view at all, so it must not
	// be ranked into the view's answer.
	_, hasB := memberProximity(t, viewOut, "b.go::B")
	assert.False(t, hasB, "a symbol only the shared corpus has must not appear in the selected view's closure")
}

// Two requests reading the same content through different identities must not
// share a walk-cache entry, and a repeat of one of them must hit its own.
func TestContextClosureProximity_CacheEntriesAreKeyedByView(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedClosureGraph(t, srv)
	srv.RunAnalysis()

	args := map[string]any{
		"symbols":    "a.go::A",
		"edge_kinds": "calls,references",
		"rank":       "proximity",
	}
	// Two views of one repository reading byte-identical content: the same
	// reader, so the same snapshot, so the same content-addressed walk key.
	// Only the selected identity differs — which is exactly the case a cache
	// keyed by walk content alone cannot tell apart.
	shared := closureViewGraph(t)
	viewCtx := func(checkout, fingerprint, root string) context.Context {
		return withRequestView(context.Background(), &requestView{
			reader:   shared,
			viewRoot: root,
			rider: &graphview.ViewRider{
				GraphID: "g1", CheckoutID: checkout, ViewFingerprint: fingerprint,
			},
		})
	}
	ctxA := viewCtx("c1", "fp1", "/wt/a")
	ctxB := viewCtx("c2", "fp2", "/wt/b")

	callClosureWithContext(t, srv, ctxA, args)
	_, _, size, _, _ := srv.pprCache.stats()
	require.Equal(t, 1, size, "the first view must cache exactly one walk")

	callClosureWithContext(t, srv, ctxB, args)
	hits, _, size, _, _ := srv.pprCache.stats()
	assert.Equal(t, 2, size, "a second view of the same content must cache its own walk")
	assert.Zero(t, hits, "one view must never hit another view's entry")

	// A repeat of the first view hits its own entry.
	callClosureWithContext(t, srv, ctxA, args)
	hits, _, size, _, _ = srv.pprCache.stats()
	assert.EqualValues(t, 1, hits, "a repeat of the same view must hit its own entry")
	assert.Equal(t, 2, size)
}

// closureChainGraph is a selected view's content: a five-symbol call chain,
// each symbol in its own package, so a walk from the head reaches the tail in
// four hops. Distinct packages matter — the content-addressed walk key folds
// the seed's package root plus its 1-HOP out-neighbour packages only, so two
// bounded snapshots of this chain that differ further out still produce the
// same walk key.
func closureChainGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New()
	chain := []struct{ id, name, file string }{
		{"pa/a.go::A", "A", "pa/a.go"},
		{"pb/b.go::B", "B", "pb/b.go"},
		{"pc/c.go::C", "C", "pc/c.go"},
		{"pd/d.go::D", "D", "pd/d.go"},
		{"pe/e.go::E", "E", "pe/e.go"},
	}
	for _, n := range chain {
		g.AddNode(&graph.Node{
			ID: n.id, Kind: graph.KindFunction, Name: n.name, FilePath: n.file,
			Language: "go", StartLine: 1, EndLine: 3,
			Meta: map[string]any{"signature": "func " + n.name + "()"},
		})
	}
	for i := 0; i+1 < len(chain); i++ {
		g.AddEdge(&graph.Edge{
			From: chain[i].id, To: chain[i+1].id, Kind: graph.EdgeCalls,
			FilePath: chain[i].file, Line: 2,
		})
	}
	return g
}

func chainViewContext(checkout, fingerprint, root string, reader graph.Reader) context.Context {
	return withRequestView(context.Background(), &requestView{
		reader:   reader,
		viewRoot: root,
		rider: &graphview.ViewRider{
			GraphID: "g1", CheckoutID: checkout, ViewFingerprint: fingerprint,
		},
	})
}

// Every closure member the response emits a proximity for must be inside the
// snapshot the walk ran over. A CSR rooted on the SEEDS only reaches
// proximityAdjacencyDepth hops, while members come from ImportClosure at the
// request's own max_depth — so a member further out would be absent from the
// snapshot, score nothing, and be emitted as proximity 0: a number a caller
// cannot tell from "unreachable", and one that ties every truncated member at
// the bottom of the ranking. Rooting the CSR on the member set is what makes
// the emitted score real.
func TestContextClosureProximity_ScoresMembersBeyondTheSeedHorizon(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedClosureGraph(t, srv)
	srv.RunAnalysis()

	ctx := chainViewContext("c1", "fp1", "/wt/a", closureChainGraph(t))
	out := callClosureWithContext(t, srv, ctx, map[string]any{
		"symbols":    "pa/a.go::A",
		"edge_kinds": "calls,references",
		"rank":       "proximity",
		"max_depth":  6,
	})

	// B and C are inside a seed-rooted depth-2 horizon; D and E are not.
	scores := make([]float64, 0, 4)
	for _, id := range []string{"pb/b.go::B", "pc/c.go::C", "pd/d.go::D", "pe/e.go::E"} {
		p, ok := memberProximity(t, out, id)
		require.True(t, ok, "%s must be a closure member at max_depth=6", id)
		assert.Greater(t, p, 0.0,
			"%s is beyond a seed-rooted bounded horizon and must still carry a real score, not a fabricated zero", id)
		scores = append(scores, p)
	}
	for i := 0; i+1 < len(scores); i++ {
		assert.Greater(t, scores[i], scores[i+1],
			"proximity must fall with distance along the chain (index %d), not flatten to a tie at zero", i)
	}
}

// Two closures over ONE view that bounded their snapshot differently are two
// snapshots. Their walk keys agree (the key describes the seed's package and
// its 1-hop neighbours, which both snapshots contain identically), so only the
// root set in the scope keeps the narrow answer from serving the wide one.
func TestContextClosureProximity_BoundedShapeIsPartOfTheCacheIdentity(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedClosureGraph(t, srv)
	srv.RunAnalysis()

	reader := closureChainGraph(t)
	ctx := chainViewContext("c1", "fp1", "/wt/a", reader)
	args := func(depth int) map[string]any {
		return map[string]any{
			"symbols":    "pa/a.go::A",
			"edge_kinds": "calls,references",
			"rank":       "proximity",
			"max_depth":  depth,
		}
	}

	narrow := callClosureWithContext(t, srv, ctx, args(1))
	_, _, size, _, _ := srv.pprCache.stats()
	require.Equal(t, 1, size, "the narrow closure must cache exactly one walk")
	if _, ok := memberProximity(t, narrow, "pe/e.go::E"); ok {
		t.Fatal("fixture broken: max_depth=1 must not reach the tail of the chain")
	}

	wide := callClosureWithContext(t, srv, ctx, args(6))
	hits, _, size, _, _ := srv.pprCache.stats()
	assert.Equal(t, 2, size, "a differently bounded snapshot over the same view must cache its own walk")
	assert.Zero(t, hits, "the wide closure must not be served the narrow closure's scores")
	tail, ok := memberProximity(t, wide, "pe/e.go::E")
	require.True(t, ok, "the wide closure must reach the tail of the chain")
	assert.Greater(t, tail, 0.0)

	// A repeat of the narrow shape hits its own entry.
	callClosureWithContext(t, srv, ctx, args(1))
	hits, _, size, _, _ = srv.pprCache.stats()
	assert.EqualValues(t, 1, hits, "a repeat of one bounded shape must hit its own entry")
	assert.Equal(t, 2, size)
}

// A labelled base selector carries a reader, but that reader is a filter over
// the shared corpus — the same bytes the published analysis pass indexed
// (view_request.go:1114-1119). It must keep the whole-graph CSR: rebuilding a
// bounded one would trade a complete snapshot for a bounded approximation and
// take a private cache namespace, for content the base snapshot already
// describes. routed() is reader-presence and cannot tell the two apart.
func TestRequestProximityAdjacency_BaseNarrowedViewKeepsTheSharedSnapshot(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedClosureGraph(t, srv)
	srv.RunAnalysis()
	require.NotNil(t, srv.getAdjacency(), "the shared analysis snapshot must exist for this test")

	reader := closureChainGraph(t)
	narrowed := withRequestView(context.Background(), &requestView{
		reader:       reader,
		baseNarrowed: true,
		kind:         requestViewKindBase,
		rider:        &graphview.ViewRider{GraphID: "g1"},
	})
	assert.Nil(t, srv.selectedSnapshotReader(narrowed),
		"a labelled base selector reads the shared corpus, not a checkout of its own")

	snap, scope := srv.requestProximityAdjacency(narrowed, []string{"a.go::A"}, []string{"a.go::A", "b.go::B"})
	require.NotNil(t, snap)
	assert.Equal(t, pprWalkSourceSharedAnalysis, scope.source,
		"a base-narrowed request must be ranked by the published whole-graph CSR")
	assert.Empty(t, scope.roots, "a whole-graph snapshot names no root set")

	// Contrast: the same reader behind a view routed to a checkout of its own
	// IS a selected snapshot, and is bounded and namespaced as one.
	routed := chainViewContext("c1", "fp1", "/wt/a", reader)
	require.NotNil(t, srv.selectedSnapshotReader(routed))
	_, routedScope := srv.requestProximityAdjacency(routed, []string{"pa/a.go::A"}, []string{"pa/a.go::A", "pb/b.go::B"})
	assert.Equal(t, pprWalkSourceSelectedReader, routedScope.source)
	assert.NotEmpty(t, routedScope.roots, "a bounded snapshot must name the root set it was built from")
}
