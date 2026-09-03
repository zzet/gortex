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

// The graph-algorithm analyzers (components, clusters, role,
// connectivity_health) run whole-graph kernels over whatever reader the
// request carries. These tests pin that: with an overlay-active request the
// partition, the fan counts and the hydrated rows all come from the caller's
// buffers, and a symbol the buffer deleted never reaches the response.

// analyzerJSON drives one analyzer handler and decodes its JSON payload.
func analyzerJSON(
	t *testing.T,
	ctx context.Context,
	label string,
	args map[string]any,
	call func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error),
) map[string]any {
	t.Helper()
	res, err := call(ctx, makeReq("analyze", args))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "%s errored: %s", label, toolResultText(res))
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &out))
	return out
}

// idSetFrom collects the string ids under key in a decoded row.
func idSetFrom(row map[string]any, key string) map[string]bool {
	raw, _ := row[key].([]any)
	set := make(map[string]bool, len(raw))
	for _, v := range raw {
		if id, ok := v.(string); ok {
			set[id] = true
		}
	}
	return set
}

// firstRow returns the first element of a decoded list payload.
func firstRow(t *testing.T, out map[string]any, key string) map[string]any {
	t.Helper()
	rows, _ := out[key].([]any)
	require.NotEmpty(t, rows, "expected at least one %s row", key)
	row, ok := rows[0].(map[string]any)
	require.True(t, ok)
	return row
}

// TestAnalyzeWCCReflectsOverlay pins runComponents to the request reader.
// Reverted to the base store, the weakly connected component still carries
// the symbol the buffer deleted and reports the indexed size.
func TestAnalyzeWCCReflectsOverlay(t *testing.T) {
	srv, layer := overlaySpineFixture(t)
	call := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return srv.handleAnalyzeConnectedComponents(ctx, req, false)
	}

	onBase := idSetFrom(firstRow(t, analyzerJSON(t, context.Background(), "wcc", nil, call), "components"), "members")
	assert.Len(t, onBase, 3, "a plain request walks every indexed symbol")
	assert.True(t, onBase[spineRetiredID], "a plain request keeps the indexed symbol")

	viewCtx := overlayCtx(t, srv, layer)
	onView := idSetFrom(firstRow(t, analyzerJSON(t, viewCtx, "wcc", nil, call), "components"), "members")
	assert.Len(t, onView, 2, "the deleted symbol must leave the component")
	assert.False(t, onView[spineRetiredID], "a symbol the buffer deleted must not be a member")
	assert.True(t, onView[spineHandlerID] && onView[spineProcessID], "the surviving symbols stay connected")

	assert.Len(t, srv.graph.AllNodes(), 3, "the overlay request must not mutate the base store")
}

// roleRowsByID indexes an analyze role response by symbol id.
func roleRowsByID(out map[string]any) map[string]map[string]any {
	symbols, _ := out["symbols"].([]any)
	rows := make(map[string]map[string]any, len(symbols))
	for _, raw := range symbols {
		row, _ := raw.(map[string]any)
		id, _ := row["symbol_id"].(string)
		rows[id] = row
	}
	return rows
}

// TestAnalyzeRoleReflectsOverlay pins the fan-count lookups — and the
// widened classifyRole they feed — to the request reader. Reverted to the
// base store the entry point keeps the call site into the symbol the buffer
// deleted, reporting fan_out 2 for a buffer that has one call left.
func TestAnalyzeRoleReflectsOverlay(t *testing.T) {
	srv, layer := overlaySpineFixture(t)

	onBase := roleRowsByID(analyzerJSON(t, context.Background(), "role", nil, srv.handleAnalyzeRole))
	require.Contains(t, onBase, spineHandlerID)
	assert.Equal(t, float64(2), onBase[spineHandlerID]["fan_out"], "both indexed call sites count")
	assert.Contains(t, onBase, spineRetiredID, "a plain request classifies the indexed symbol")

	viewCtx := overlayCtx(t, srv, layer)
	onView := roleRowsByID(analyzerJSON(t, viewCtx, "role", nil, srv.handleAnalyzeRole))
	require.Contains(t, onView, spineHandlerID)
	assert.Equal(t, float64(1), onView[spineHandlerID]["fan_out"],
		"the call into the deleted symbol must not be counted")
	assert.Equal(t, "entry", onView[spineHandlerID]["role"])
	assert.NotContains(t, onView, spineRetiredID, "a symbol the buffer deleted must not be classified")
	require.Contains(t, onView, spineProcessID)
	assert.Equal(t, float64(70), onView[spineProcessID]["start_line"], "the row carries the buffer's payload")
}

// TestAnalyzeSpectralClustersReflectsOverlay pins the spectral partitioning
// and the per-cluster member hydration to the request reader.
func TestAnalyzeSpectralClustersReflectsOverlay(t *testing.T) {
	srv, layer := overlaySpineFixture(t)
	args := map[string]any{"algorithm": "spectral", "min_size": float64(2)}

	baseRow := firstRow(t, analyzerJSON(t, context.Background(), "clusters", args, srv.handleAnalyzeClusters), "clusters")
	assert.Equal(t, float64(3), baseRow["size"], "a plain request clusters every indexed symbol")
	assert.True(t, idSetFrom(baseRow, "member_sample")[spineRetiredID], "the indexed symbol is a member")

	viewCtx := overlayCtx(t, srv, layer)
	viewRow := firstRow(t, analyzerJSON(t, viewCtx, "clusters", args, srv.handleAnalyzeClusters), "clusters")
	assert.Equal(t, float64(2), viewRow["size"], "the deleted symbol must leave the cluster")
	assert.False(t, idSetFrom(viewRow, "member_sample")[spineRetiredID],
		"a symbol the buffer deleted must not be a member")
}

// TestAnalyzeConnectivityHealthReflectsOverlay pins the degree lookups inside
// analysis.GraphConnectivity to the request reader: with the call into the
// deleted symbol gone, the entry point drops from a two-edge node to a leaf.
func TestAnalyzeConnectivityHealthReflectsOverlay(t *testing.T) {
	srv, layer := overlaySpineFixture(t)

	onBase := analyzerJSON(t, context.Background(), "connectivity_health", nil, srv.handleAnalyzeConnectivityHealth)
	assert.Equal(t, float64(3), onBase["nominal_nodes"])
	assert.Equal(t, float64(2), onBase["leaf"], "only the two called symbols are leaves on the index")

	viewCtx := overlayCtx(t, srv, layer)
	onView := analyzerJSON(t, viewCtx, "connectivity_health", nil, srv.handleAnalyzeConnectivityHealth)
	assert.Equal(t, float64(2), onView["nominal_nodes"], "the deleted symbol leaves the scanned set")
	assert.Equal(t, float64(2), onView["leaf"],
		"the entry point becomes a leaf once its call into the deleted symbol is gone")
	assert.Equal(t, float64(0), onView["isolated"])
}

const (
	impCoreFile = "p/core.go"
	impCoreID   = impCoreFile + "::Core"
	impKeptID   = "p/keep.go::KeptCaller"
	impDropFile = "p/drop.go"
	impDropID   = impDropFile + "::DroppedCaller"
)

// overlayImpactFixture wires a symbol with two indexed callers plus the
// layer an editor session would push once one of those call sites is gone:
// drop.go is re-parsed empty, so its caller and its call edge disappear.
func overlayImpactFixture(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: impCoreID, Name: "Core", Kind: graph.KindFunction, FilePath: impCoreFile, Language: "go", StartLine: 5, EndLine: 9})
	g.AddNode(&graph.Node{ID: impKeptID, Name: "KeptCaller", Kind: graph.KindFunction, FilePath: "p/keep.go", Language: "go", StartLine: 3, EndLine: 6})
	g.AddNode(&graph.Node{ID: impDropID, Name: "DroppedCaller", Kind: graph.KindFunction, FilePath: impDropFile, Language: "go", StartLine: 3, EndLine: 6})
	g.AddEdge(&graph.Edge{From: impKeptID, To: impCoreID, Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: impDropID, To: impCoreID, Kind: graph.EdgeCalls})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(impDropFile, false)
	layer.MarkRemoved("DroppedCaller", impDropID)

	return reviewFamilyServer(t, g), layer
}

// TestExplainChangeImpactReflectsOverlay pins the blast-radius read
// (analysis.AnalyzeImpactContext) to the request reader. The precomputed
// reach index is built over the base corpus and is therefore skipped under
// an overlay; the live walk that replaces it must count the buffer's
// callers, not the indexed ones, or the pre-edit safety gate reports a
// dependent the caller has already removed.
func TestExplainChangeImpactReflectsOverlay(t *testing.T) {
	srv, layer := overlayImpactFixture(t)

	depthOneIDs := func(out map[string]any) map[string]bool {
		byDepth, _ := out["by_depth"].(map[string]any)
		rows, _ := byDepth["1"].([]any)
		ids := make(map[string]bool, len(rows))
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			if id, ok := row["id"].(string); ok {
				ids[id] = true
			}
		}
		return ids
	}
	run := func(ctx context.Context) map[string]any {
		return analyzerJSON(t, ctx, "explain_change_impact",
			map[string]any{"ids": impCoreID}, srv.handleEnhancedChangeImpact)
	}

	onBase := run(context.Background())
	assert.Equal(t, float64(2), onBase["total_affected"], "both indexed callers count")
	baseIDs := depthOneIDs(onBase)
	assert.True(t, baseIDs[impKeptID] && baseIDs[impDropID], "a plain request reports both callers")

	onView := run(overlayCtx(t, srv, layer))
	assert.Equal(t, float64(1), onView["total_affected"],
		"the caller the buffer deleted must leave the blast radius")
	viewIDs := depthOneIDs(onView)
	assert.True(t, viewIDs[impKeptID], "the surviving caller stays a direct dependent")
	assert.False(t, viewIDs[impDropID], "a caller the buffer deleted must not be reported")

	assert.Len(t, srv.graph.AllNodes(), 3, "the overlay request must not mutate the base store")
}
