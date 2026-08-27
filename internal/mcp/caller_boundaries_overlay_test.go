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

type callersBoundaryResp struct {
	Boundaries []graph.EpistemicBoundary `json:"boundaries"`
	LowerBound bool                      `json:"lower_bound"`
}

func getCallersBoundaries(t *testing.T, srv *Server, ctx context.Context, id string) callersBoundaryResp {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_callers"
	req.Params.Arguments = map[string]any{"id": id}
	res, err := srv.handleGetCallers(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	var resp callersBoundaryResp
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	return resp
}

// TestGetCallers_OverlayAddedBoundaryIsVisible pins that the epistemic
// boundary lookup reads the request-scoped view: an overlay-only
// implementation of an interface method must surface its dispatch
// boundary and lower_bound — reading the base graph returns the
// overlay-only implementation in the result while boundaries stay
// empty and lower_bound stays false, a self-contradictory answer.
func TestGetCallers_OverlayAddedBoundaryIsVisible(t *testing.T) {
	base := graph.New()
	iface := &graph.Node{ID: "src/iface.go::Iface.Do", Kind: graph.KindMethod, Name: "Do", FilePath: "src/iface.go", StartLine: 1}
	base.AddNode(iface)

	layer := graph.NewOverlayLayer()
	impl := &graph.Node{ID: "src/new.go::FreshImpl.Do", Kind: graph.KindMethod, Name: "Do", FilePath: "src/new.go", StartLine: 1}
	layer.AddNode("src/new.go", impl)
	layer.AddEdge(&graph.Edge{From: impl.ID, To: iface.ID, Kind: graph.EdgeImplements, FilePath: "src/new.go", Line: 1})

	eng := query.NewEngine(base)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, base, nil, nil, zap.NewNop(), nil)
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(base, layer))

	resp := getCallersBoundaries(t, srv, ctx, impl.ID)
	require.NotEmpty(t, resp.Boundaries,
		"an overlay-added implements edge is a dispatch boundary the request's view can see")
	require.True(t, resp.LowerBound,
		"interface dispatch into the overlay-only implementation makes the caller count a floor")
}

// TestGetCallers_TombstonedBoundaryIsGone pins the reverse orientation:
// when the overlay tombstones the file carrying the implements edge,
// the boundary must disappear with it — the base graph would keep
// reporting a dispatch floor through an interface relationship the
// session already deleted.
func TestGetCallers_TombstonedBoundaryIsGone(t *testing.T) {
	base := graph.New()
	iface := &graph.Node{ID: "src/iface.go::Iface.Do", Kind: graph.KindMethod, Name: "Do", FilePath: "src/iface.go", StartLine: 1}
	impl := &graph.Node{ID: "src/impl.go::Impl.Do", Kind: graph.KindMethod, Name: "Do", FilePath: "src/impl.go", StartLine: 1}
	base.AddNode(iface)
	base.AddNode(impl)
	base.AddEdge(&graph.Edge{From: impl.ID, To: iface.ID, Kind: graph.EdgeImplements, FilePath: "src/impl.go", Line: 1})

	// Sanity: without an overlay the boundary is reported.
	eng := query.NewEngine(base)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, base, nil, nil, zap.NewNop(), nil)
	plain := getCallersBoundaries(t, srv, context.Background(), impl.ID)
	require.NotEmpty(t, plain.Boundaries, "fixture: base graph must report the dispatch boundary")

	layer := graph.NewOverlayLayer()
	layer.MarkFile("src/impl.go", true)
	// The session's buffer re-adds the symbol without the interface.
	fresh := &graph.Node{ID: "src/impl.go::Impl.Do", Kind: graph.KindMethod, Name: "Do", FilePath: "src/impl.go", StartLine: 1}
	layer.AddNode("src/impl.go", fresh)
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(base, layer))

	resp := getCallersBoundaries(t, srv, ctx, impl.ID)
	require.Empty(t, resp.Boundaries,
		"a tombstoned implements edge is no dispatch boundary in the request's view")
	require.False(t, resp.LowerBound)
}
