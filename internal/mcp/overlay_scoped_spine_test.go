package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The scoped-node spine (scopedNodes / scopedNodesLight /
// scopedNodesByKinds) is what every whole-graph handler iterates. These
// tests pin that it reads the request's reader rather than the server's
// base store, and that two handlers sitting on top of it — audit_health
// and get_architecture — report the overlay's state.

const (
	spineHandlerID = "p/handler.go::Handle"
	spineProcessID = "p/service.go::Process"
	spineRetiredID = "p/service.go::Retired"
	spineServiceFl = "p/service.go"
)

// overlaySpineFixture wires a handler-capable server over a two-file base
// graph plus the layer an editor session would push: service.go is
// re-parsed with Process moved down the file and Retired deleted.
func overlaySpineFixture(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: spineHandlerID, Name: "Handle", Kind: graph.KindFunction, FilePath: "p/handler.go", Language: "go", StartLine: 5})
	g.AddNode(&graph.Node{ID: spineProcessID, Name: "Process", Kind: graph.KindFunction, FilePath: spineServiceFl, Language: "go", StartLine: 7})
	g.AddNode(&graph.Node{ID: spineRetiredID, Name: "Retired", Kind: graph.KindFunction, FilePath: spineServiceFl, Language: "go", StartLine: 30})
	g.AddEdge(&graph.Edge{From: spineHandlerID, To: spineProcessID, Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: spineHandlerID, To: spineRetiredID, Kind: graph.EdgeCalls})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(spineServiceFl, false)
	layer.AddNode(spineServiceFl, &graph.Node{
		ID: spineProcessID, Name: "Process", Kind: graph.KindFunction,
		FilePath: spineServiceFl, Language: "go", StartLine: 70,
	})
	layer.MarkRemoved("Retired", spineRetiredID)

	srv := &Server{
		graph:      g,
		session:    newSessionState(),
		sessions:   newSessionMap(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		toolScopes: newScopeRegistry(),
	}
	return srv, layer
}

// spineLineByID indexes a scoped-node slice by ID → start line, so one
// map answers both "is the symbol there" and "whose payload is it".
func spineLineByID(nodes []*graph.Node) map[string]int {
	out := make(map[string]int, len(nodes))
	for _, n := range nodes {
		out[n.ID] = n.StartLine
	}
	return out
}

// TestScopedNodesSpineReadsThroughRequestReader pins all three scoped-node
// entry points to the request reader. Reverting any of them to s.graph
// serves the on-disk line for the re-parsed symbol and keeps handing out
// the symbol the buffer deleted.
func TestScopedNodesSpineReadsThroughRequestReader(t *testing.T) {
	srv, layer := overlaySpineFixture(t)
	baseCtx := context.Background()
	viewCtx := overlayCtx(t, srv, layer)

	callables := []graph.NodeKind{graph.KindFunction}
	cases := []struct {
		name string
		call func(context.Context) []*graph.Node
	}{
		{"scopedNodes", srv.scopedNodes},
		{"scopedNodesLight", srv.scopedNodesLight},
		{"scopedNodesByKinds", func(ctx context.Context) []*graph.Node {
			return srv.scopedNodesByKinds(ctx, callables)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			onBase := spineLineByID(tc.call(baseCtx))
			assert.Equal(t, 7, onBase[spineProcessID], "a plain request reports the indexed line")
			assert.Contains(t, onBase, spineRetiredID, "a plain request still sees the indexed symbol")

			onView := spineLineByID(tc.call(viewCtx))
			assert.Equal(t, 70, onView[spineProcessID], "the buffer's payload must replace the indexed one")
			assert.NotContains(t, onView, spineRetiredID, "a symbol the buffer deleted must not survive the scan")
			assert.Contains(t, onView, spineHandlerID, "an untouched file keeps its symbols")
		})
	}

	assert.Len(t, srv.graph.AllNodes(), 3, "the overlay request must not mutate the base store")
}

// auditReportFor drives audit_health and decodes its report.
func auditReportFor(t *testing.T, s *Server, ctx context.Context) AuditReport {
	t.Helper()
	res, err := s.handleAuditHealth(ctx, makeReq("audit_health", nil))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "audit_health errored: %s", toolResultText(res))
	var report AuditReport
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &report))
	return report
}

// TestAuditHealthReflectsOverlay is the handler-level proof for the
// scopedNodesByKinds → ComputeAuditReport path: the graded symbol set and
// each row's payload come from the caller's buffers.
func TestAuditHealthReflectsOverlay(t *testing.T) {
	srv, layer := overlaySpineFixture(t)

	lineFor := func(r AuditReport, id string) (int, bool) {
		for _, w := range r.WorstSymbols {
			if w.ID == id {
				return w.Line, true
			}
		}
		return 0, false
	}

	onBase := auditReportFor(t, srv, context.Background())
	assert.Equal(t, 3, onBase.SymbolCount, "a plain request grades every indexed callable")
	line, ok := lineFor(onBase, spineProcessID)
	require.True(t, ok)
	assert.Equal(t, 7, line)
	_, ok = lineFor(onBase, spineRetiredID)
	assert.True(t, ok, "a plain request grades the indexed symbol")

	onView := auditReportFor(t, srv, overlayCtx(t, srv, layer))
	assert.Equal(t, 2, onView.SymbolCount, "the deleted symbol must drop out of the graded set")
	line, ok = lineFor(onView, spineProcessID)
	require.True(t, ok)
	assert.Equal(t, 70, line, "the row must carry the buffer's payload")
	_, ok = lineFor(onView, spineRetiredID)
	assert.False(t, ok, "a symbol the buffer deleted must not be graded")
}

// architectureSnapshotFor drives get_architecture and decodes the payload.
func architectureSnapshotFor(t *testing.T, s *Server, ctx context.Context) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	res, err := s.handleGetArchitecture(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "get_architecture errored: %s", toolResultText(res))
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &out))
	return out
}

// TestArchitectureSnapshotReflectsOverlay covers the counter and
// entry-point sections: both are edge-driven, so a base-store read shows
// the deleted symbol's call site and inflates the entry point's fan-out.
func TestArchitectureSnapshotReflectsOverlay(t *testing.T) {
	srv, layer := overlaySpineFixture(t)

	entryFanOut := func(out map[string]any, id string) (float64, bool) {
		entries, _ := out["entry_points"].([]any)
		for _, raw := range entries {
			row, _ := raw.(map[string]any)
			if row["id"] == id {
				fanOut, _ := row["fan_out"].(float64)
				return fanOut, true
			}
		}
		return 0, false
	}

	onBase := architectureSnapshotFor(t, srv, context.Background())
	baseSummary, _ := onBase["summary"].(map[string]any)
	assert.Equal(t, float64(3), baseSummary["total_nodes"])
	assert.Equal(t, float64(2), baseSummary["total_edges"])
	fanOut, ok := entryFanOut(onBase, spineHandlerID)
	require.True(t, ok, "Handle is the indexed entry point")
	assert.Equal(t, float64(2), fanOut)

	onView := architectureSnapshotFor(t, srv, overlayCtx(t, srv, layer))
	viewSummary, _ := onView["summary"].(map[string]any)
	assert.Equal(t, float64(2), viewSummary["total_nodes"], "the deleted symbol must leave the node total")
	assert.Equal(t, float64(1), viewSummary["total_edges"], "its call site must leave the edge total")
	fanOut, ok = entryFanOut(onView, spineHandlerID)
	require.True(t, ok, "Handle stays the entry point")
	assert.Equal(t, float64(1), fanOut, "the call into the deleted symbol must not be counted")
}
