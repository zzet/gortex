package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

func decodeRenameErrorResp(t *testing.T, res *mcplib.CallToolResult) map[string]any {
	t.Helper()
	require.True(t, res.IsError, "%v", res.Content)
	require.NotEmpty(t, res.Content)
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	return resp
}

func TestRenameSymbol_UnindexedRecovery(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	const source = "package main\n\nfunc Late() {}\nfunc Use() { Late() }\n"
	path := filepath.Join(dir, "late.go")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))

	for _, dryRun := range []bool{false, true} {
		res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
			"id": "late.go::Late", "new_name": "Renamed", "dry_run": dryRun,
		})
		require.True(t, res.IsError)
		resp := decodeRenameErrorResp(t, res)
		require.Equal(t, "symbol_not_indexed", resp["error_code"])
		recovery := resp["data"].(map[string]any)
		require.Equal(t, "refused", recovery["status"])
		require.Equal(t, false, recovery["semantic_rename_complete"])
		require.Equal(t, false, recovery["written"])
		require.Equal(t, dryRun, recovery["dry_run"])
		require.EqualValues(t, 1, recovery["occurrences"])
		require.Contains(t, recovery["warning"], "Only the parsed declaration is anchored")

		fallback := recovery["safe_fallback"].(map[string]any)
		require.Equal(t, "edit", fallback["tool"])
		require.Equal(t, "declaration_only", fallback["scope"])
		request := fallback["request"].(map[string]any)
		require.Equal(t, "file", request["operation"])
		require.Equal(t, "late.go", request["target"].(map[string]any)["file"])
		require.Equal(t, "func Late() {}\n", request["match"])
		require.Equal(t, "func Renamed() {}\n", request["replacement"])
		guard := request["guard"].(map[string]any)
		require.EqualValues(t, 1, guard["expected_occurrences"])
		require.NotEmpty(t, guard["base_sha"])
		require.NotEmpty(t, fallback["guidance"])

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, source, string(got))
	}
}

func TestRenameSymbol_IgnoredRecovery(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	const source = "package main\n\nfunc Ignored() {}\n"
	path := filepath.Join(dir, "ignored.go")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))
	srv.indexer.SetExcludePatterns([]string{"ignored.go"})
	_, err := srv.indexer.Index(dir)
	require.NoError(t, err)

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id": "ignored.go::Ignored", "new_name": "Renamed",
	})
	require.True(t, res.IsError)
	resp := decodeRenameErrorResp(t, res)
	require.Equal(t, "symbol_not_indexed", resp["error_code"])
	require.EqualValues(t, 1, resp["data"].(map[string]any)["occurrences"])
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, source, string(got))
}

func TestRenameSymbol_RecoveryExecutesDeclarationOnly(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	const source = "package main\n\ntype Run struct{}\nfunc (r *Run) Run() {}\nfunc Use(r *Run) { r.Run() }\n"
	path := filepath.Join(dir, "late.go")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id": "late.go::Run.Run", "new_name": "Execute",
	})
	resp := decodeRenameErrorResp(t, res)
	recovery := resp["data"].(map[string]any)
	require.Equal(t, "Run", recovery["declaration_name"])
	fallback := recovery["safe_fallback"].(map[string]any)
	request := fallback["request"].(map[string]any)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit"
	req.Params.Arguments = request
	editResult, err := srv.handleFacade(context.Background(), "edit", req)
	require.NoError(t, err)
	require.False(t, editResult.IsError, toolResultText(editResult))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "package main\n\ntype Run struct{}\nfunc (r *Run) Execute() {}\nfunc Use(r *Run) { r.Run() }\n", string(got))
}

func TestRenameSymbol_SubIdentifierIsNotADeclaration(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	const source = "const foo$bar = 1;\n"
	path := filepath.Join(dir, "late.js")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id": "late.js::foo", "new_name": "renamed",
	})
	require.True(t, res.IsError)
	require.Contains(t, toolResultText(res), "symbol not found: late.js::foo")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, source, string(got))
}

func TestRenameSymbol_IgnoredMinifiedRecoveryIsRefused(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	source := "const foo=1;function use(){return " + strings.Repeat("foo+", 4096) + "0}\n"
	path := filepath.Join(dir, "ignored.js")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))
	srv.indexer.SetExcludePatterns([]string{"ignored.js"})
	_, err := srv.indexer.Index(dir)
	require.NoError(t, err)

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id": "ignored.js::foo", "new_name": "renamed",
	})
	require.True(t, res.IsError)
	require.Contains(t, toolResultText(res), "symbol not found: ignored.js::foo")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, source, string(got))
}

func TestRenameSymbol_OneLineCandidateBudgetRefusesRecovery(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	source := "package main; func Many() {}; func Use() { " + strings.Repeat("Many(); ", maxUnindexedRenameCandidates) + "Many() }\n"
	path := filepath.Join(dir, "late.go")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))

	extracted, err := srv.indexer.ExtractSource(context.Background(), "late.go", []byte(source))
	require.NoError(t, err)
	defer extracted.ReleaseTree()
	found := false
	for _, node := range extracted.Nodes {
		_, suffix, ok := strings.Cut(node.ID, "::")
		if ok && suffix == "Many" {
			found = true
			break
		}
	}
	require.True(t, found, "fixture must reach the candidate-budget gate with a parsed declaration")

	require.Nil(t, srv.unindexedRenameRecovery(context.Background(), "late.go::Many", "Renamed", false))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, source, string(got))
}

func TestRenameSymbol_CancelledRecoveryDoesNotParseOrOfferFallback(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	const source = "package main\n\nfunc Late() {}\n"
	path := filepath.Join(dir, "late.go")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Nil(t, srv.unindexedRenameRecovery(ctx, "late.go::Late", "Renamed", false))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, source, string(got))
}

func TestRenameSymbol_SymlinkRecoveryIsRefused(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	const source = "package main\n\nfunc Linked() {}\n"
	targetPath := filepath.Join(dir, "symlink-target.go")
	linkPath := filepath.Join(dir, "late.go")
	require.NoError(t, os.WriteFile(targetPath, []byte(source), 0o644))
	if err := os.Symlink("symlink-target.go", linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id": "late.go::Linked", "new_name": "Renamed",
	})
	require.True(t, res.IsError)
	require.Contains(t, toolResultText(res), "symbol not found: late.go::Linked")

	got, err := os.ReadFile(targetPath)
	require.NoError(t, err)
	require.Equal(t, source, string(got))
	info, err := os.Lstat(linkPath)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
}

func setupMultiRepoRenameRecoveryServer(
	t *testing.T,
	repoYAML string,
	baseCfg *config.IndexConfig,
) (*Server, string) {
	t.Helper()
	repo := setupMiniRepo(t, "repo-a")
	if repoYAML != "" {
		require.NoError(t, os.WriteFile(filepath.Join(repo, ".gortex.yaml"), []byte(repoYAML), 0o644))
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	global := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repo, Name: "repo-a"}}}
	global.SetConfigPath(configPath)
	require.NoError(t, global.Save())
	manager, err := config.NewConfigManager(configPath)
	require.NoError(t, err)

	registry := testRegistry()
	store := graph.New()
	multi := indexer.NewMultiIndexer(store, registry, search.NewNull(), manager, zap.NewNop())
	registerMCPFixtureRepos(t, multi, manager)

	var base *indexer.Indexer
	if baseCfg != nil {
		base = indexer.New(store, registry, *baseCfg, zap.NewNop())
		t.Cleanup(base.Close)
	}
	srv := NewServer(query.NewEngine(store), store, base, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: manager,
		MultiIndexer:  multi,
	})
	return srv, repo
}

func TestRenameSymbol_UnindexedRecoveryUsesOwningRepoIndexer(t *testing.T) {
	srv, repo := setupMultiRepoRenameRecoveryServer(t, "", nil)
	const source = "package main\n\nfunc Late() {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(repo, "late.go"), []byte(source), 0o644))

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id": "repo-a/late.go::Late", "new_name": "Renamed",
	})
	resp := decodeRenameErrorResp(t, res)
	require.Equal(t, "symbol_not_indexed", resp["error_code"])
	recovery := resp["data"].(map[string]any)
	fallback := recovery["safe_fallback"].(map[string]any)
	request := fallback["request"].(map[string]any)
	require.Equal(t, "repo-a/late.go", request["target"].(map[string]any)["file"])
}

func TestRenameSymbol_UnindexedRecoveryHonorsOwningRepoLimit(t *testing.T) {
	base := config.Default().Index
	base.MaxFileSize = 0
	srv, repo := setupMultiRepoRenameRecoveryServer(t, "index:\n  max_file_size: 16\n", &base)
	const source = "package main\n\nfunc Restricted() {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(repo, "late.go"), []byte(source), 0o644))

	owner, ownerRel := srv.indexerForRel("repo-a/late.go")
	require.NotNil(t, owner)
	result, err := owner.ExtractSource(t.Context(), ownerRel, []byte(source))
	require.Nil(t, result)
	require.ErrorContains(t, err, "bounded extraction limit (16 bytes)")

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id": "repo-a/late.go::Restricted", "new_name": "Renamed",
	})
	require.True(t, res.IsError)
	text := toolResultText(res)
	require.Contains(t, text, "symbol not found: repo-a/late.go::Restricted")
	require.NotContains(t, text, "safe_fallback")
}

func TestRenameSymbol_InvalidTargetStaysNotFound(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "late.go"), []byte("package main\n\nfunc Late() {}\n"), 0o644))
	for _, id := range []string{"missing.go::Missing", "target.go::Missing", "late.go::Typo"} {
		res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
			"id": id, "new_name": "Renamed",
		})
		require.True(t, res.IsError)
		require.Contains(t, toolResultText(res), "symbol not found: "+id)
	}
	target, err := os.ReadFile(filepath.Join(dir, "target.go"))
	require.NoError(t, err)
	require.Equal(t, renameTargetSrc, string(target))
}
