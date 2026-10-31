package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// setupDeclServer indexes a multi-file Go project where one function is
// called from several sites, so the use-site → declaration join and its
// grouping can be exercised.
func setupDeclServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()

	writeDecl := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}

	writeDecl("app.go", `package app

func compute() int {
	return 42
}

func runOnce() int {
	return compute()
}

func runTwice() int {
	return compute() + compute()
}
`)
	writeDecl("svc/handler.go", `package svc

import "fmt"

func helperFn() string {
	return "ok"
}

func Handle() {
	fmt.Println(helperFn())
}
`)

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

// declResult unmarshals a find_declaration response.
func declResult(t *testing.T, result *mcplib.CallToolResult) map[string]any {
	t.Helper()
	require.False(t, result.IsError, "find_declaration errored: %s", toolResultText(result))
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &payload))
	return payload
}

// declNames returns the set of resolved declaration names in a response.
func declNames(payload map[string]any) map[string]int {
	out := map[string]int{}
	decls, _ := payload["declarations"].([]any)
	for _, d := range decls {
		dm, _ := d.(map[string]any)
		decl, _ := dm["declaration"].(map[string]any)
		name, _ := decl["name"].(string)
		uses, _ := dm["use_sites"].([]any)
		out[name] = len(uses)
	}
	return out
}

func TestFindDeclaration_RequiresUseSite(t *testing.T) {
	srv := setupDeclServer(t)
	result := callTool(t, srv, "find_declaration", map[string]any{})
	require.True(t, result.IsError)
	assert.Contains(t, toolResultText(result), "use_site is required")
}

func TestFindDeclaration_LiteralResolvesDeclaration(t *testing.T) {
	srv := setupDeclServer(t)

	payload := declResult(t, callTool(t, srv, "find_declaration", map[string]any{
		"use_site": "helperFn(",
	}))
	names := declNames(payload)
	assert.Contains(t, names, "helperFn", "literal use site must resolve to the helperFn declaration")
}

func TestFindDeclaration_GroupsMultipleUseSites(t *testing.T) {
	srv := setupDeclServer(t)

	// compute() is called from runOnce (once) and runTwice (twice) — and
	// also appears in its own declaration line "func compute() int".
	payload := declResult(t, callTool(t, srv, "find_declaration", map[string]any{
		"use_site": "compute()",
	}))
	names := declNames(payload)
	require.Contains(t, names, "compute", "compute must be resolved")
	// The three call sites (runOnce x1, runTwice x2) all group under one
	// declaration entry.
	assert.GreaterOrEqual(t, names["compute"], 3,
		"all three compute() call sites must group under one declaration")
	assert.Equal(t, 1, len(declNames(payload)),
		"compute() use sites must collapse to a single declaration group")
}

func TestFindDeclaration_RegexResolvesDeclaration(t *testing.T) {
	srv := setupDeclServer(t)

	// Regex matching the compute call sites.
	payload := declResult(t, callTool(t, srv, "find_declaration", map[string]any{
		"use_site": `compute\(\)`,
		"regex":    true,
	}))
	names := declNames(payload)
	assert.Contains(t, names, "compute", "regex use site must resolve to the compute declaration")
}

func TestFindDeclaration_KindFilter(t *testing.T) {
	srv := setupDeclServer(t)

	// helperFn is a function; filtering to type only must drop it.
	payload := declResult(t, callTool(t, srv, "find_declaration", map[string]any{
		"use_site": "helperFn(",
		"kind":     "type",
	}))
	names := declNames(payload)
	assert.NotContains(t, names, "helperFn", "kind=type filter must drop the function declaration")

	// Filtering to function keeps it.
	payload = declResult(t, callTool(t, srv, "find_declaration", map[string]any{
		"use_site": "helperFn(",
		"kind":     "function",
	}))
	assert.Contains(t, declNames(payload), "helperFn", "kind=function filter must keep the function")
}

func TestFindDeclaration_PathPrefixScoping(t *testing.T) {
	srv := setupDeclServer(t)

	// helperFn is only used in svc/ — a path_prefix of app/ finds nothing.
	payload := declResult(t, callTool(t, srv, "find_declaration", map[string]any{
		"use_site":    "helperFn(",
		"path_prefix": "app",
	}))
	assert.Equal(t, float64(0), payload["count"], "path_prefix=app must exclude svc/handler.go matches")

	// Scoped to svc/ it resolves.
	payload = declResult(t, callTool(t, srv, "find_declaration", map[string]any{
		"use_site":    "helperFn(",
		"path_prefix": "svc/",
	}))
	assert.Contains(t, declNames(payload), "helperFn")
}

func TestFindDeclaration_RegexPathPrefix(t *testing.T) {
	srv := setupDeclServer(t)

	payload := declResult(t, callTool(t, srv, "find_declaration", map[string]any{
		"use_site":    `helperFn\(`,
		"regex":       true,
		"path_prefix": "svc/",
	}))
	assert.Contains(t, declNames(payload), "helperFn")
}

func TestFindDeclaration_NoMatch(t *testing.T) {
	srv := setupDeclServer(t)

	payload := declResult(t, callTool(t, srv, "find_declaration", map[string]any{
		"use_site": "nonexistent_zzz_call(",
	}))
	assert.Equal(t, float64(0), payload["count"])
}

func TestFindDeclaration_BadRegex(t *testing.T) {
	srv := setupDeclServer(t)

	result := callTool(t, srv, "find_declaration", map[string]any{
		"use_site": "[unclosed",
		"regex":    true,
	})
	require.True(t, result.IsError)
	assert.Contains(t, toolResultText(result), "invalid regex")
}

// TestFindDeclaration_MultiRepoFanout pins the regression: in
// multi-repo mode the daemon owns a MultiIndexer and s.indexer has
// no rootPath, so the legacy s.indexer.GrepText path returned no
// matches and find_declaration silently reported `count: 0` for
// use sites that genuinely existed across the workspace. The fix
// routes Stage 1 through the MultiIndexer fan-out so use-site hits
// surface with repo-prefixed paths.
func TestFindDeclaration_MultiRepoFanout(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "alpha"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "alpha", "main.go"), []byte(`package alpha

func uniqueDecl() int { return 1 }

func consumer() int { return uniqueDecl() + uniqueDecl() }
`), 0o644))
	// Second repo so IsMultiRepo() is true; the use_site only lives
	// in alpha but multi-repo dispatch should still find it.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "beta"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "beta", "main.go"), []byte("package beta\n\nfunc placeholder() {}\n"), 0o644))

	repoA := filepath.Join(dir, "alpha")
	repoB := filepath.Join(dir, "beta")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "alpha"},
			{Path: repoB, Name: "beta"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	registerMCPFixtureRepos(t, mi, cm)
	require.True(t, mi.IsMultiRepo())

	eng := query.NewEngine(g)
	singleton := indexer.New(g, reg, config.IndexConfig{}, zap.NewNop())
	srv := NewServer(eng, g, singleton, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	payload := declResult(t, callTool(t, srv, "find_declaration", map[string]any{
		"use_site": "uniqueDecl(",
	}))
	require.Equal(t, float64(1), payload["count"],
		"expected one declaration with two grouped use sites; got %v", payload)
	names := declNames(payload)
	require.Contains(t, names, "uniqueDecl")
	require.Equal(t, 2, names["uniqueDecl"], "two use sites must group under one declaration")
}
