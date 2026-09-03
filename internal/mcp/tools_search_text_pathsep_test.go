package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/trigram"
)

// TestGraphMatchPathKey pins the two spellings apart: a trigram match path
// is always forward-slash, while a graph node ID joins the repo prefix with
// "/" and keeps the repo-relative remainder in the OS separator.
func TestGraphMatchPathKey(t *testing.T) {
	require.Equal(t, "beta/"+filepath.FromSlash("pkg/sub/main.go"),
		graphMatchPathKey("beta/pkg/sub/main.go", true),
		"a repo-prefixed match keeps the prefix separator and OS-spells the remainder")
	require.Equal(t, filepath.FromSlash("pkg/sub/main.go"),
		graphMatchPathKey("pkg/sub/main.go", false),
		"an unprefixed match is OS-spelled whole")
	require.Equal(t, "beta/main.go", graphMatchPathKey("beta/main.go", true),
		"a repo-root file has one spelling on every platform")
}

// setupNestedRepo writes a repo whose only source file sits below the root,
// which is where the match-path and node-ID spellings diverge.
func setupNestedRepo(t *testing.T, name, workspace, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gortex.yaml"),
		[]byte("workspace: "+workspace+"\nproject: backend\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pkg", "sub", "main.go"),
		[]byte(body), 0o644))
	return dir
}

func nestedRepoServer(t *testing.T, entries []config.RepoEntry) (*Server, *graph.Graph) {
	t.Helper()
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: entries}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	singleton := indexer.New(g, reg, config.IndexConfig{}, zap.NewNop())
	srv := NewServer(query.NewEngine(g), g, singleton, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})
	return srv, g
}

// TestGraphRelPath_LoneRepoForwardSlash covers the read side of the same
// separator gap. resolveFilePath echoes a caller-supplied relative path back
// verbatim, so a forward-slash path — what agents write, and what every tool
// description shows — reached the graph unchanged and missed the node below
// the repo root on Windows: get_file_summary answered file_not_indexed for a
// file that was indexed.
func TestGraphRelPath_LoneRepoForwardSlash(t *testing.T) {
	solo := setupNestedRepo(t, "solo", "shared", "package sub\n\nfunc SoloHandler() {}\n")
	srv, g := nestedRepoServer(t, []config.RepoEntry{{Path: solo, Name: "solo", Project: "backend"}})

	nodeKey := "solo/" + filepath.FromSlash("pkg/sub/main.go")
	require.NotNil(t, g.GetNode(nodeKey), "fixture invariant: a lone repo's node ids carry its prefix")

	require.Equal(t, nodeKey, srv.graphRelPath(context.Background(), "solo/pkg/sub/main.go"),
		"a forward-slash path must be normalised to the graph's spelling")
	require.Equal(t, nodeKey, srv.graphRelPath(context.Background(), nodeKey),
		"an already OS-spelled path is unchanged (idempotent)")

	res := callTool(t, srv, "get_file_summary", map[string]any{"path": "pkg/sub/main.go"})
	require.False(t, res.IsError, "a forward-slash repo-relative path must resolve")
	require.Contains(t, res.Content[0].(mcplib.TextContent).Text, "SoloHandler")
}

// TestFilterTextMatchesByResolvedScope_BelowRepoRoot is the regression for
// search_text returning zero results on Windows. Scope narrowing attributes
// every match to a graph node and fails closed when it cannot. The lookup
// key was the raw forward-slash match path, which equals the node ID only
// for a file at the repo root — so on Windows every match below the root was
// silently dropped and a repo-wide search reported no hits at all.
//
// The existing fail-closed fixture uses a root-level file, where the two
// spellings coincide on every platform; these put the file in a
// subdirectory, and cover both node-ID shapes: prefixed (several tracked
// repos) and a lone repo, whose ids carry its prefix too.
func TestFilterTextMatchesByResolvedScope_BelowRepoRoot(t *testing.T) {
	t.Run("prefixed node ids", func(t *testing.T) {
		alpha := setupNestedRepo(t, "alpha", "shared", "package sub\n\n// marker\nfunc A() {}\n")
		beta := setupNestedRepo(t, "beta", "shared", "package sub\n\n// marker\nfunc B() {}\n")
		srv, g := nestedRepoServer(t, []config.RepoEntry{
			{Path: alpha, Name: "alpha", Project: "backend"},
			{Path: beta, Name: "beta", Project: "backend"},
		})

		nodeKey := "beta/" + filepath.FromSlash("pkg/sub/main.go")
		require.NotNil(t, g.GetNode(nodeKey),
			"fixture invariant: several tracked repos mint prefixed node ids")

		got := srv.filterTextMatchesByResolvedScope(
			context.Background(),
			[]trigram.Match{{Path: "beta/pkg/sub/main.go", Line: 3, Text: "// marker"}},
			ResolvedScope{WorkspaceID: "shared", RepoAllow: map[string]bool{"beta": true}},
		)
		require.Len(t, got, 1,
			"a node-backed match below the repo root must survive narrowing on every platform")
	})

	t.Run("lone repo", func(t *testing.T) {
		solo := setupNestedRepo(t, "solo", "shared", "package sub\n\n// marker\nfunc S() {}\n")
		srv, g := nestedRepoServer(t, []config.RepoEntry{
			{Path: solo, Name: "solo", Project: "backend"},
		})

		require.NotNil(t, g.GetNode("solo/"+filepath.FromSlash("pkg/sub/main.go")),
			"fixture invariant: a lone repo's node ids carry its prefix")

		got := srv.filterTextMatchesByResolvedScope(
			context.Background(),
			[]trigram.Match{{Path: "solo/pkg/sub/main.go", Line: 3, Text: "// marker"}},
			ResolvedScope{WorkspaceID: "shared", RepoAllow: map[string]bool{"solo": true}},
		)
		require.Len(t, got, 1,
			"the unprefixed retry must also cross the separator gap")
	})
}
