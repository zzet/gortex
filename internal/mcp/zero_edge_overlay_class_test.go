package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// caveatOf extracts the zero-edge caveat from a handler's JSON response.
func caveatOf(t *testing.T, text string) *graph.ZeroEdgeCaveat {
	t.Helper()
	var resp struct {
		Caveat *graph.ZeroEdgeCaveat `json:"caveat"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	return resp.Caveat
}

// TestFindUsages_OverlayStructuralEvidenceClassifiesLikelyUnused pins the
// exact caveat class for an overlay-only symbol with structural evidence:
// the request's view knows the symbol and its outgoing edge, so an empty
// usage result must classify as likely_unused — classifying against the
// base graph (where the symbol does not exist) reports an extraction gap
// for a symbol the request resolved cleanly.
func TestFindUsages_OverlayStructuralEvidenceClassifiesLikelyUnused(t *testing.T) {
	base := graph.New()
	helper := &graph.Node{ID: "src/util.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "src/util.go", StartLine: 1}
	base.AddNode(helper)

	layer := graph.NewOverlayLayer()
	fresh := &graph.Node{ID: "src/new.go::FreshFn", Kind: graph.KindFunction, Name: "FreshFn", FilePath: "src/new.go", StartLine: 1}
	layer.AddNode("src/new.go", fresh)
	layer.AddEdge(&graph.Edge{From: fresh.ID, To: helper.ID, Kind: graph.EdgeCalls, FilePath: "src/new.go", Line: 3})

	eng := query.NewEngine(base)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, base, nil, nil, zap.NewNop(), nil)
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(base, layer))

	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_usages"
	req.Params.Arguments = map[string]any{"id": fresh.ID}
	res, err := srv.handleFindUsages(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	caveat := caveatOf(t, res.Content[0].(mcplib.TextContent).Text)
	require.NotNil(t, caveat, "an empty result for a resolved symbol must carry a caveat")
	require.Equal(t, graph.ZeroEdgeLikelyUnused, caveat.Class,
		"overlay structural evidence (no incoming, one outgoing) is the likely_unused shape")
}

// TestFindUsages_OverlayWeakEvidenceClassifiesCoverageIncomplete pins the
// exact caveat class when the only incoming evidence lives in the overlay
// and is speculative: the row is hidden from the result by default, so the
// empty answer must say coverage_incomplete — the base graph knows nothing
// about the symbol and would misreport an extraction gap.
func TestFindUsages_OverlayWeakEvidenceClassifiesCoverageIncomplete(t *testing.T) {
	base := graph.New()

	layer := graph.NewOverlayLayer()
	fresh := &graph.Node{ID: "src/new.go::FreshFn", Kind: graph.KindFunction, Name: "FreshFn", FilePath: "src/new.go", StartLine: 1}
	guess := &graph.Node{ID: "src/guess.go::MaybeCalls", Kind: graph.KindFunction, Name: "MaybeCalls", FilePath: "src/guess.go", StartLine: 5}
	layer.AddNode("src/new.go", fresh)
	layer.AddNode("src/guess.go", guess)
	layer.AddEdge(&graph.Edge{
		From: guess.ID, To: fresh.ID, Kind: graph.EdgeCalls, FilePath: "src/guess.go", Line: 7,
		Origin: graph.OriginSpeculative, Meta: map[string]any{graph.MetaSpeculative: true},
	})

	eng := query.NewEngine(base)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, base, nil, nil, zap.NewNop(), nil)
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(base, layer))

	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_usages"
	req.Params.Arguments = map[string]any{"id": fresh.ID}
	res, err := srv.handleFindUsages(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	caveat := caveatOf(t, res.Content[0].(mcplib.TextContent).Text)
	require.NotNil(t, caveat)
	require.Equal(t, graph.ZeroEdgeCoverageIncomplete, caveat.Class,
		"a hidden speculative usage is weak evidence, not proof of unuse and not a gap")
}

// TestGetCallers_TombstonedCallerClassifiesLikelyUnused pins the caveat
// when the overlay tombstones the file holding a symbol's only base
// caller: the request's view has zero callers, so the empty result must
// carry a likely_unused caveat — classifying against the base graph still
// sees the deleted caller's edge and answers with no caveat at all.
func TestGetCallers_TombstonedCallerClassifiesLikelyUnused(t *testing.T) {
	base := graph.New()
	target := &graph.Node{ID: "src/core.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "src/core.go", StartLine: 1}
	caller := &graph.Node{ID: "src/old.go::OnlyCaller", Kind: graph.KindFunction, Name: "OnlyCaller", FilePath: "src/old.go", StartLine: 3}
	helper := &graph.Node{ID: "src/util.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "src/util.go", StartLine: 1}
	base.AddNode(target)
	base.AddNode(caller)
	base.AddNode(helper)
	base.AddEdge(&graph.Edge{From: caller.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "src/old.go", Line: 5})
	// An outgoing edge keeps the tombstoned view out of the
	// extraction-gap shape: no incoming, some structure → likely_unused.
	base.AddEdge(&graph.Edge{From: target.ID, To: helper.ID, Kind: graph.EdgeCalls, FilePath: "src/core.go", Line: 2})

	layer := graph.NewOverlayLayer()
	layer.MarkFile("src/old.go", true)

	eng := query.NewEngine(base)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, base, nil, nil, zap.NewNop(), nil)
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(base, layer))

	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_callers"
	req.Params.Arguments = map[string]any{"id": target.ID}
	res, err := srv.handleGetCallers(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	caveat := caveatOf(t, res.Content[0].(mcplib.TextContent).Text)
	require.NotNil(t, caveat,
		"zero callers in the request's view must carry a caveat even when the base graph still holds a deleted caller")
	require.Equal(t, graph.ZeroEdgeLikelyUnused, caveat.Class)
}
