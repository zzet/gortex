package mcp

import (
	"context"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestFindHotspots_PendingBeforeFirstAnalysisPass pins the on-demand
// contract for kind=hotspots: before the first RunAnalysis pass lands
// (analysisEpoch == 0, so the cached ranking does not exist), the
// handler must report analysis_pending with a retry hint instead of an
// empty ranking — an empty list is indistinguishable from "the graph
// has no hotspots" and starves the god_nodes rollup too. Regression
// for the silent {"hotspots": [], "total": 0} on fresh daemons.
func TestFindHotspots_PendingBeforeFirstAnalysisPass(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedHotspotNodes(t, srv, 12)
	require.GreaterOrEqual(t, srv.graph.NodeCount(), 10,
		"fixture must clear the handler's minimum-size check")

	srv.analysisMu.RLock()
	stale := srv.analysisSnapshotCurrentLocked()
	srv.analysisMu.RUnlock()
	require.False(t, stale, "a fresh test server must not have a current analysis snapshot")

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{}
	res, err := srv.handleFindHotspots(context.Background(), req)
	require.NoError(t, err)
	if res.IsError {
		tc, _ := res.Content[0].(mcplib.TextContent)
		t.Fatalf("handler errored: %s", tc.Text)
	}

	payload := decodeJSONResult(t, res)
	require.Equal(t, "analysis_pending", payload["status"],
		"hotspots must disclose the on-demand pass, not serve an empty cache silently")
	require.Contains(t, payload, "retry_after_seconds")
	hotspots, ok := payload["hotspots"].([]any)
	require.True(t, ok, "the hotspots collection key must stay present and typed")
	require.Empty(t, hotspots)

	msg, _ := payload["message"].(string)
	require.False(t, strings.Contains(msg, "index_repository"),
		"the graph is populated; only the analysis pass is missing")
	require.Contains(t, msg, "nothing to re-index",
		"a pending analysis must not tell a populated graph to re-index")
}

// seedHotspotNodes tops the graph up with plain function nodes so the
// handler's minimum-size check (>= 10 symbols) passes on the small
// setupTestServer fixture.
func seedHotspotNodes(t *testing.T, srv *Server, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := "hotspot_fixture.go::Fn" + strings.Repeat("I", i+1)
		srv.graph.AddNode(&graph.Node{
			ID: id, Kind: graph.KindFunction, Name: "Fn" + strings.Repeat("I", i+1),
			FilePath: "hotspot_fixture.go",
		})
	}
}

// With an explicit threshold the handler computes synchronously, so it
// must not be gated on the background pass.
func TestFindHotspots_ExplicitThresholdSkipsPendingGate(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedHotspotNodes(t, srv, 12)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{
		"threshold": 0.0 + 500.0, // no survivor can pass; synchronous verdict
	}
	res, err := srv.handleFindHotspots(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	payload := decodeJSONResult(t, res)
	require.NotContains(t, payload, "status",
		"an explicit-threshold run is synchronous and must not report analysis_pending")
	require.Equal(t, float64(0), payload["total"])
}
