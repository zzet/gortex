package review

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/graph"
)

// The pack / grounding entry points take a graph.Reader so a caller
// holding a per-session shadow view can hand it straight in and have the
// pack rendered against the editor's buffers. These tests pin that the
// reader is honoured — narrowing the parameter back to the base store
// would serve the indexed payload and keep a deleted symbol alive.

const (
	orDepFile    = "app/dep.go"
	orChangedID  = "app/svc.go::Changed"
	orDepID      = orDepFile + "::Dep"
	orRetiredID  = orDepFile + "::Retired"
	orLoopFile   = "app/loop.go"
	orLoopSymbol = orLoopFile + "::Query"
)

// overlayPackFixture builds a base graph plus the layer an editor session
// would push for app/dep.go: Dep re-emitted with a new signature and
// Retired deleted.
func overlayPackFixture() (*graph.Graph, *graph.OverlayLayer) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: orChangedID, Kind: graph.KindFunction, Name: "Changed",
		FilePath: "app/svc.go", Language: "go", StartLine: 3, EndLine: 5,
	})
	g.AddNode(&graph.Node{
		ID: orDepID, Kind: graph.KindFunction, Name: "Dep",
		FilePath: orDepFile, Language: "go", StartLine: 3, EndLine: 5,
		Meta: map[string]any{"signature": "func Dep()"},
	})
	g.AddNode(&graph.Node{
		ID: orRetiredID, Kind: graph.KindFunction, Name: "Retired",
		FilePath: orDepFile, Language: "go", StartLine: 8, EndLine: 10,
		Meta: map[string]any{"signature": "func Retired()"},
	})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(orDepFile, false)
	layer.AddNode(orDepFile, &graph.Node{
		ID: orDepID, Kind: graph.KindFunction, Name: "Dep",
		FilePath: orDepFile, Language: "go", StartLine: 3, EndLine: 6,
		Meta: map[string]any{"signature": "func Dep(ctx context.Context) error"},
	})
	layer.MarkRemoved("Retired", orRetiredID)
	return g, layer
}

// outlineSignatures indexes a pack's tier-3 outline by symbol id.
func outlineSignatures(pack *ReviewPack) map[string]string {
	out := make(map[string]string, len(pack.Outline))
	for _, e := range pack.Outline {
		out[e.ID] = e.Signature
	}
	return out
}

// TestBuildReviewPackReadsThroughOverlaidView pins the widened pack
// signature: handed a shadow view it renders the buffer's outline —
// the re-emitted symbol's new signature, and no indexed payload for the
// symbol the buffer deleted.
func TestBuildReviewPackReadsThroughOverlaidView(t *testing.T) {
	g, layer := overlayPackFixture()

	diff := &analysis.DiffResult{
		ChangedFiles:   []string{"app/svc.go"},
		ChangedSymbols: []analysis.ChangedSymbol{{ID: orChangedID, Name: "Changed", Kind: "function", FilePath: "app/svc.go", Line: 3}},
	}
	impact := &analysis.ImpactResult{
		ByDepth: map[int][]analysis.ImpactEntry{
			2: {
				{ID: orDepID, Name: "Dep", Kind: "function", FilePath: orDepFile, Line: 3},
				{ID: orRetiredID, Name: "Retired", Kind: "function", FilePath: orDepFile, Line: 8},
			},
		},
	}

	onBase := outlineSignatures(BuildReviewPack(g, nil, diff, impact, 0))
	assert.Equal(t, "func Dep()", onBase[orDepID], "the base store renders the indexed signature")
	assert.Equal(t, "func Retired()", onBase[orRetiredID], "the base store still knows the indexed symbol")

	view := graph.NewOverlaidView(g, layer)
	onView := outlineSignatures(BuildReviewPack(view, nil, diff, impact, 0))
	assert.Equal(t, "func Dep(ctx context.Context) error", onView[orDepID],
		"the buffer's signature must replace the indexed one")
	assert.Equal(t, "Retired", onView[orRetiredID],
		"a symbol the buffer deleted carries no indexed payload — only the bare name fallback")

	require.Len(t, g.AllNodes(), 3, "rendering through the view must not mutate the base store")
}

// TestGroundReviewMatchesReadsThroughOverlaidView pins the grounding
// entry point: an N+1 row is refuted once the buffer's version of the
// enclosing function no longer carries a loop.
func TestGroundReviewMatchesReadsThroughOverlaidView(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: orLoopSymbol, Kind: graph.KindFunction, Name: "Query",
		FilePath: orLoopFile, Language: "go",
		Meta: map[string]any{"loop_depth": 2},
	})

	// The buffer hoisted the query out of the loop.
	layer := graph.NewOverlayLayer()
	layer.MarkFile(orLoopFile, false)
	layer.AddNode(orLoopFile, &graph.Node{
		ID: orLoopSymbol, Kind: graph.KindFunction, Name: "Query",
		FilePath: orLoopFile, Language: "go",
	})

	matches := []astquery.Match{{Detector: "go-loop-query-call", SymbolID: orLoopSymbol}}

	assert.Len(t, GroundReviewMatches(g, matches), 1,
		"the indexed loop depth keeps the N+1 row")
	assert.Empty(t, GroundReviewMatches(graph.NewOverlaidView(g, layer), matches),
		"the buffer's loop-free body must refute the N+1 row")
}
