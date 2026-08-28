package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// setupRepoWithHTTPProvider creates a tiny Go repo with Gin route declarations.
// Produces http provider contracts on index.
func setupRepoWithHTTPProvider(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "main.go"),
		[]byte(`package main

import "github.com/gin-gonic/gin"

func setupRoutes(r *gin.Engine) {
	r.GET("/api/users", listUsers)
	r.POST("/api/users", createUser)
}

func listUsers()  {}
func createUser() {}
`),
		0o644,
	))
	return dir
}

// setupRepoWithHTTPConsumer creates a tiny Go repo with http.Get calls.
// Produces http consumer contracts on index.
func setupRepoWithHTTPConsumer(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "client.go"),
		[]byte(`package main

import "net/http"

func fetchUsers() {
	http.Get("http://api.example.com/api/users")
}
`),
		0o644,
	))
	return dir
}

// TestHandleContracts_ReflectsRuntimeTrackedRepos is the regression guard for
// the T0.1 bug: the contracts tool previously held a snapshot Registry that
// was bound once at startup. Repos tracked at runtime (via track_repository)
// added contracts to per-repo indexers but were invisible to contracts
// list/check. This test fails unless the handler pulls a fresh merged
// registry on every call.
func TestHandleContracts_ReflectsRuntimeTrackedRepos(t *testing.T) {
	root := t.TempDir()
	providerRepo := setupRepoWithHTTPProvider(t, root, "provider-svc")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: providerRepo, Name: "provider-svc"},
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
	singleton := indexer.New(g, preg, config.IndexConfig{}, zap.NewNop())
	srv := NewServer(eng, g, singleton, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})
	srv.PublishReadiness("ready", true, nil)

	// Baseline: contracts from the provider repo are visible.
	providerCount := contractsTotal(t, srv, nil)
	require.Greater(t, providerCount, 0,
		"expected contracts from provider-svc to be visible before runtime track")

	// Track a new repo at runtime. This is the scenario that used to silently
	// fail: the indexer learned about it, but the contracts tool kept serving
	// the pre-track snapshot.
	consumerRepo := setupRepoWithHTTPConsumer(t, root, "consumer-svc")
	trackReq := mcplib.CallToolRequest{}
	trackReq.Params.Arguments = map[string]any{
		"path": consumerRepo,
		"name": "consumer-svc",
	}
	trackRes, err := srv.handleTrackRepository(context.Background(), trackReq)
	require.NoError(t, err)
	require.NotNil(t, trackRes)
	assert.False(t, trackRes.IsError,
		"track_repository should succeed; got %+v", trackRes.Content)

	// Post-track: total contracts must increase and the consumer repo must
	// contribute at least one contract of its own.
	totalAfter := contractsTotal(t, srv, nil)
	assert.Greater(t, totalAfter, providerCount,
		"contracts total should grow after runtime track; before=%d after=%d",
		providerCount, totalAfter)

	consumerCount := contractsTotal(t, srv, map[string]any{"repo": "consumer-svc"})
	assert.Greater(t, consumerCount, 0,
		"consumer-svc contracts must be visible via repo filter after runtime track")

	providerCountAfter := contractsTotal(t, srv, map[string]any{"repo": "provider-svc"})
	assert.Equal(t, providerCount, providerCountAfter,
		"tracking a new repo must not drop provider-svc contracts")
}

// TestHandleContracts_MatchesGraphContractCount guards against the symptom the
// tuck audit hit directly: graph holds N contract nodes while contracts list
// returns 0. After the fix, the counts must agree within the same registry
// snapshot.
func TestHandleContracts_MatchesGraphContractCount(t *testing.T) {
	root := t.TempDir()
	repo := setupRepoWithHTTPProvider(t, root, "svc-a")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: repo, Name: "svc-a"}},
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
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	graphContractNodes := 0
	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindContract {
			graphContractNodes++
		}
	}
	require.Greater(t, graphContractNodes, 0, "provider repo should produce contract nodes in graph")

	toolTotal := contractsTotal(t, srv, nil)
	assert.Equal(t, graphContractNodes, toolTotal,
		"contracts list must surface every contract-kind node in the graph")
}

// contractsTotal calls handleContracts (action defaults to list) and extracts
// the integer "total" from the JSON payload. Fails the test on any error or
// malformed response.
func contractsTotal(t *testing.T, srv *Server, args map[string]any) int {
	t.Helper()
	req := mcplib.CallToolRequest{}
	if args == nil {
		req.Params.Arguments = map[string]any{}
	} else {
		req.Params.Arguments = args
	}
	res, err := srv.handleContracts(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "contracts list returned error: %+v", res.Content)

	payload := extractTextFromContent(t, res.Content)
	var parsed struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(payload), &parsed),
		"contracts list payload is not JSON: %s", payload)
	return parsed.Total
}

// extractTextFromContent pulls the serialized text body out of an MCP tool
// result. NewToolResultJSON wraps the JSON in a single TextContent block.
func extractTextFromContent(t *testing.T, content []mcplib.Content) string {
	t.Helper()
	require.NotEmpty(t, content, "tool result has no content blocks")
	var b strings.Builder
	for _, c := range content {
		if tc, ok := c.(mcplib.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	out := b.String()
	require.NotEmpty(t, out, "no text content in tool result")
	return out
}
