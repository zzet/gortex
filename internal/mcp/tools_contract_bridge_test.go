package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// TestReciprocalRankFusion_HandComputed pins the fusion math to a
// hand-worked example with k=60:
//
//	signal a ranks: x(1), y(2), z(3)
//	signal b ranks: y(1), x(2)
//	signal c ranks: z(1)
//
//	fused(x) = 1/61 + 1/62
//	fused(y) = 1/62 + 1/61
//	fused(z) = 1/63 + 1/61
func TestReciprocalRankFusion_HandComputed(t *testing.T) {
	rankings := map[string][]string{
		"a": {"x", "y", "z"},
		"b": {"y", "x"},
		"c": {"z"},
	}
	fused, perSignal := reciprocalRankFusion(rankings, 60)

	wantX := 1.0/61 + 1.0/62
	wantY := 1.0/62 + 1.0/61
	wantZ := 1.0/63 + 1.0/61

	assert.InDelta(t, wantX, fused["x"], 1e-12)
	assert.InDelta(t, wantY, fused["y"], 1e-12)
	assert.InDelta(t, wantZ, fused["z"], 1e-12)

	// x and y tie exactly; z trails because its second-best rank is 3.
	assert.Equal(t, fused["x"], fused["y"])
	assert.Less(t, fused["z"], fused["x"])

	assert.Equal(t, map[string]int{"a": 1, "b": 2}, perSignal["x"])
	assert.Equal(t, map[string]int{"a": 2, "b": 1}, perSignal["y"])
	assert.Equal(t, map[string]int{"a": 3, "c": 1}, perSignal["z"])
}

func TestReciprocalRankFusion_Empty(t *testing.T) {
	fused, perSignal := reciprocalRankFusion(nil, 60)
	assert.Empty(t, fused)
	assert.Empty(t, perSignal)
}

// setupBridgeWorkspaceRepo writes a repo dir with a shared-workspace
// .gortex.yaml so the two repos' contracts pair across the boundary.
func setupBridgeWorkspaceRepo(t *testing.T, root, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gortex.yaml"),
		[]byte("workspace: acme\nproject: acme\n"), 0o644))
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	return dir
}

// newBridgeTestServer indexes a provider repo (Gin routes) and a
// consumer repo (http.Get of the same path) into one multi-repo graph
// and returns an MCP server whose reconcile pass has materialized the
// bridge subgraph.
func newBridgeTestServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()

	providerRepo := setupBridgeWorkspaceRepo(t, root, "provider-svc", map[string]string{
		"go.mod": "module example.com/provider-svc\n\ngo 1.21\n",
		"main.go": `package main

import "github.com/gin-gonic/gin"

func setupRoutes(r *gin.Engine) {
	r.GET("/api/users", listUsers)
}

func listUsers() {}
`,
	})
	consumerRepo := setupBridgeWorkspaceRepo(t, root, "consumer-svc", map[string]string{
		"go.mod": "module example.com/consumer-svc\n\ngo 1.21\n",
		"client.go": `package main

import "net/http"

func fetchUsers() {
	http.Get("http://api.example.com/api/users")
}
`,
	})

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: providerRepo, Name: "provider-svc"},
			{Path: consumerRepo, Name: "consumer-svc"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	preg := testRegistry()

	g := graph.New()
	mi := indexer.NewMultiIndexer(g, preg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	eng := query.NewEngine(g)
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})
}

type bridgeTestPayload struct {
	Mode   string `json:"mode"`
	Total  int    `json:"total"`
	Symbol string `json:"symbol"`
	Groups []struct {
		BridgeID      string         `json:"bridge_id"`
		CanonicalKey  string         `json:"canonical_key"`
		ContractType  string         `json:"contract_type"`
		Repos         []string       `json:"repos"`
		CrossRepo     bool           `json:"cross_repo"`
		ProviderCount int            `json:"provider_count"`
		ConsumerCount int            `json:"consumer_count"`
		FusedScore    float64        `json:"fused_score"`
		SignalRanks   map[string]int `json:"signal_ranks"`
		MatchedVia    []string       `json:"matched_via"`
		Providers     []struct {
			ContractID string `json:"contract_id"`
			Repo       string `json:"repo"`
			SymbolID   string `json:"symbol_id"`
			FilePath   string `json:"file_path"`
		} `json:"providers"`
		Consumers []struct {
			ContractID string `json:"contract_id"`
			Repo       string `json:"repo"`
			SymbolID   string `json:"symbol_id"`
		} `json:"consumers"`
	} `json:"groups"`
}

func callBridge(t *testing.T, srv *Server, args map[string]any) bridgeTestPayload {
	t.Helper()
	req := mcplib.CallToolRequest{}
	if args == nil {
		args = map[string]any{}
	}
	args["action"] = "bridge"
	req.Params.Arguments = args
	res, err := srv.handleContracts(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "bridge call failed: %+v", res.Content)

	var payload bridgeTestPayload
	require.NoError(t, json.Unmarshal([]byte(extractTextFromContent(t, res.Content)), &payload))
	return payload
}

// TestHandleContractBridges_RankMode runs the fused ranking over a
// real two-repo index and asserts the grouped response shape: bridge
// identity, both sides resolved with repo + symbol + location, and
// per-signal ranks riding on the fused score.
func TestHandleContractBridges_RankMode(t *testing.T) {
	srv := newBridgeTestServer(t)

	payload := callBridge(t, srv, map[string]any{"query": "users api"})
	require.Equal(t, "rank", payload.Mode)
	require.GreaterOrEqual(t, payload.Total, 1)
	require.NotEmpty(t, payload.Groups)

	top := payload.Groups[0]
	assert.Equal(t, "bridge::acme::acme::http::GET::/api/users", top.BridgeID)
	assert.Equal(t, "GET /api/users", top.CanonicalKey)
	assert.Equal(t, "http", top.ContractType)
	assert.Equal(t, []string{"consumer-svc", "provider-svc"}, top.Repos)
	assert.True(t, top.CrossRepo)
	assert.Greater(t, top.FusedScore, 0.0)
	assert.Contains(t, top.SignalRanks, "text")
	assert.Contains(t, top.SignalRanks, "degree")

	require.NotEmpty(t, top.Providers, "provider side must be resolved")
	prov := top.Providers[0]
	assert.Equal(t, "provider-svc", prov.Repo)
	assert.Equal(t, "provider-svc/main.go::listUsers", prov.SymbolID)
	assert.Equal(t, "provider-svc/main.go", prov.FilePath)

	require.NotEmpty(t, top.Consumers, "consumer side must be resolved")
	cons := top.Consumers[0]
	assert.Equal(t, "consumer-svc", cons.Repo)
	assert.Equal(t, "consumer-svc/client.go::fetchUsers", cons.SymbolID)
}

// TestHandleContractBridges_RankMode_SymbolAdjacency: passing a symbol
// adds the graph-adjacency signal to the fusion.
func TestHandleContractBridges_RankMode_SymbolAdjacency(t *testing.T) {
	srv := newBridgeTestServer(t)

	payload := callBridge(t, srv, map[string]any{
		"query":  "users",
		"symbol": "consumer-svc/client.go::fetchUsers",
	})
	require.NotEmpty(t, payload.Groups)
	top := payload.Groups[0]
	assert.Equal(t, "bridge::acme::acme::http::GET::/api/users", top.BridgeID)
	assert.Contains(t, top.SignalRanks, "adjacency",
		"symbol-anchored call must rank the adjacency signal: %v", top.SignalRanks)
	assert.Equal(t, 1, top.SignalRanks["adjacency"])
}

// TestHandleContractBridges_ImpactMode: from the consumer symbol, the
// bridge it participates in must surface as cross-service blast
// radius, with the anchoring contract recorded.
func TestHandleContractBridges_ImpactMode(t *testing.T) {
	srv := newBridgeTestServer(t)

	payload := callBridge(t, srv, map[string]any{
		"mode":   "impact",
		"symbol": "consumer-svc/client.go::fetchUsers",
	})
	require.Equal(t, "impact", payload.Mode)
	require.Equal(t, 1, payload.Total)
	top := payload.Groups[0]
	assert.Equal(t, "bridge::acme::acme::http::GET::/api/users", top.BridgeID)
	assert.Equal(t, []string{"http::GET::/api/users"}, top.MatchedVia)

	// The provider symbol works symmetrically — its route's bridge is
	// its blast radius too.
	payload = callBridge(t, srv, map[string]any{
		"mode":   "impact",
		"symbol": "provider-svc/main.go::listUsers",
	})
	require.Equal(t, 1, payload.Total)
	assert.Equal(t, "bridge::acme::acme::http::GET::/api/users", payload.Groups[0].BridgeID)
}

// TestHandleContractBridges_Errors covers the argument-validation
// paths: impact without symbol, unknown mode, unknown symbol.
func TestHandleContractBridges_Errors(t *testing.T) {
	srv := newBridgeTestServer(t)

	for name, args := range map[string]map[string]any{
		"impact without symbol": {"action": "bridge", "mode": "impact"},
		"unknown mode":          {"action": "bridge", "mode": "sideways"},
		"unknown symbol":        {"action": "bridge", "mode": "impact", "symbol": "nope.go::missing"},
	} {
		req := mcplib.CallToolRequest{}
		req.Params.Arguments = args
		res, err := srv.handleContracts(context.Background(), req)
		require.NoError(t, err, name)
		assert.True(t, res.IsError, "%s should return a tool error", name)
	}
}

// TestHandleContractBridges_RepoScope: the repo filter keeps bridges
// touching the named repo and drops the rest.
func TestHandleContractBridges_RepoScope(t *testing.T) {
	srv := newBridgeTestServer(t)

	payload := callBridge(t, srv, map[string]any{"query": "users", "repo": "provider-svc"})
	require.NotEmpty(t, payload.Groups, "bridge touches provider-svc, must stay in scope")

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"action": "bridge", "query": "users", "repo": "unrelated-repo"}
	res, err := srv.handleContracts(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, res.IsError, "no bridges touch unrelated-repo — expect the empty-scope error")
}

// TestCollectBridgeGroups_BoundaryScopedParticipants guards the read-
// side half of the workspace-isolation fix: registry.ByID returns every
// same-ID record across all workspaces, so the participant resolver must
// filter to the bridge's own (workspace, project) boundary. Without the
// filter, a bridge for workspace "acme" would list "globex"'s provider
// as a participant.
func TestCollectBridgeGroups_BoundaryScopedParticipants(t *testing.T) {
	g := graph.New()

	// One contract node ID, but two registry records in different
	// workspaces — exactly the shape registry.ByID returns.
	contractID := "http::GET::/api/users"
	g.AddNode(&graph.Node{ID: contractID, Kind: graph.KindContract, Name: contractID, Language: "contract"})

	reg := contracts.NewRegistry()
	reg.Add(contracts.Contract{
		ID: contractID, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		SymbolID: "acme-api/routes.go::list", FilePath: "acme-api/routes.go", Line: 10,
		RepoPrefix: "acme-api", WorkspaceID: "acme", ProjectID: "acme",
	})
	reg.Add(contracts.Contract{
		ID: contractID, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		SymbolID: "globex-api/routes.go::list", FilePath: "globex-api/routes.go", Line: 22,
		RepoPrefix: "globex-api", WorkspaceID: "globex", ProjectID: "globex",
	})

	// A bridge node pinned to the acme boundary, linked to the shared
	// contract node — the materialiser's output for the acme group.
	g.AddNode(&graph.Node{
		ID:          "bridge::acme::acme::" + contractID,
		Kind:        graph.KindContractBridge,
		Name:        "GET /api/users",
		FilePath:    indexer.ContractBridgeFilePath,
		Language:    "contract",
		RepoPrefix:  "acme-api",
		WorkspaceID: "acme",
		Meta: map[string]any{
			"contract_type": "http", "canonical_key": "GET /api/users",
			"workspace": "acme", "project": "acme",
			"repos": []string{"acme-api"}, "provider_count": 1, "consumer_count": 0,
		},
	})
	g.AddEdge(&graph.Edge{
		From: "bridge::acme::acme::" + contractID, To: contractID,
		Kind: graph.EdgeBridges, Meta: map[string]any{"side": "provider"},
	})

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	srv.SetContractRegistry(reg)

	groups := srv.collectBridgeGroups(context.Background(), nil)
	require.Len(t, groups, 1)
	grp := groups[0]
	require.Len(t, grp.Providers, 1,
		"only the acme-boundary provider must be listed, not globex's same-ID record: %+v", grp.Providers)
	assert.Equal(t, "acme-api", grp.Providers[0].Repo)
	assert.Equal(t, "acme-api/routes.go::list", grp.Providers[0].SymbolID)
}
