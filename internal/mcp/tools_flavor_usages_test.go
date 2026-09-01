package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// setupFlavorUsageServer indexes a Go fixture where `helper` is called
// from a struct method (Service.Run) and from a top-level function.
func setupFlavorUsageServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

type Service struct{}

func (s *Service) Run() {
	helper()
}

func TopLevel() {
	helper()
}

func helper() {}
`), 0o644)
	g := graph.New()
	reg := testRegistry()
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil)
	srv.RunAnalysis()
	return srv
}

func flavorUsageNodeID(srv *Server, name string, kind graph.NodeKind) string {
	for _, n := range srv.graph.AllNodes() {
		if n.Name == name && n.Kind == kind {
			return n.ID
		}
	}
	return ""
}

func findUsagesResp(t *testing.T, srv *Server, id, flavor string) map[string]any {
	t.Helper()
	args := map[string]any{"id": id}
	if flavor != "" {
		args["flavor"] = flavor
	}
	res := callTool(t, srv, "find_usages", args)
	require.False(t, res.IsError)
	text := res.Content[0].(mcplib.TextContent).Text
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	return resp
}

func usageEdgeCount(resp map[string]any) int {
	edges, _ := resp["edges"].([]any)
	return len(edges)
}

// TestFindUsages_FlavorOwnerResolution proves the find_usages flavor
// filter resolves a usage's FROM site to its enclosing owner type and
// matches on that type's flavor.
func TestFindUsages_FlavorOwnerResolution(t *testing.T) {
	srv := setupFlavorUsageServer(t)
	helperID := flavorUsageNodeID(srv, "helper", graph.KindFunction)
	require.NotEmpty(t, helperID, "helper function node should exist")

	// Baseline: helper is called from Service.Run and TopLevel.
	base := findUsagesResp(t, srv, helperID, "")
	require.GreaterOrEqual(t, usageEdgeCount(base), 2, "helper has at least two call sites")

	// flavor:struct keeps only the call from inside the Service struct —
	// Run is a method whose enclosing owner is the struct-flavored type.
	structOnly := findUsagesResp(t, srv, helperID, "struct")
	require.Equal(t, 1, usageEdgeCount(structOnly), "only the struct-owned call should survive")

	// flavor:class keeps none — Service is a struct, not a class.
	classOnly := findUsagesResp(t, srv, helperID, "class")
	require.Equal(t, 0, usageEdgeCount(classOnly), "no class-owned call exists")
}

// TestFindUsages_FlavorNilSafety proves a usage whose FROM node is absent
// from the subgraph node set (resolved only via the graph) does not panic
// and the resolution still works.
func TestFindUsages_FlavorNilSafety(t *testing.T) {
	srv := setupFlavorUsageServer(t)
	helperID := flavorUsageNodeID(srv, "helper", graph.KindFunction)
	require.NotEmpty(t, helperID)

	sg := srv.engineFor(context.Background()).FindUsagesScoped(helperID, query.QueryOptions{})
	// Strip the node set entirely so every FROM lookup falls through to
	// the graph reader — the filter must not panic and must still resolve.
	sg.Nodes = nil
	require.NotPanics(t, func() {
		filterUsagesByFlavor(srv.graph, sg, helperID, "struct")
	})
	require.Equal(t, 1, len(sg.Edges), "struct-owned call survives even with no node set")
}

// TestFindUsages_FromFlavorJSON proves the curated find_usages JSON
// promotes from_type_flavor to a top-level field on the caller node.
func TestFindUsages_FromFlavorJSON(t *testing.T) {
	srv := setupFlavorUsageServer(t)
	helperID := flavorUsageNodeID(srv, "helper", graph.KindFunction)
	require.NotEmpty(t, helperID)

	resp := findUsagesResp(t, srv, helperID, "")
	nodes, _ := resp["nodes"].([]any)
	require.NotEmpty(t, nodes)
	foundRun := false
	for _, raw := range nodes {
		n, _ := raw.(map[string]any)
		if name, _ := n["name"].(string); name == "Run" {
			foundRun = true
			require.Equal(t, "struct", n["from_type_flavor"],
				"Run's enclosing owner Service is a struct, surfaced top-level")
		}
	}
	require.True(t, foundRun, "the Service.Run caller node should be present")
}
