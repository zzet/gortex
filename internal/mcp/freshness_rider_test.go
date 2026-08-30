package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
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

func freshReq(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

func TestTargetRepoRelFile(t *testing.T) {
	require.Equal(t, "internal/x.go",
		targetRepoRelFile("read_file", freshReq(map[string]any{"path": "internal/x.go"}), ""))
	require.Equal(t, "internal/x.go",
		targetRepoRelFile("read_file", freshReq(map[string]any{"path": "gortex/internal/x.go"}), "gortex"))
	require.Equal(t, "a.go",
		targetRepoRelFile("get_symbol_source", freshReq(map[string]any{"id": "a.go::Foo"}), ""))
	// Non-file tools yield no target.
	require.Equal(t, "",
		targetRepoRelFile("search_symbols", freshReq(map[string]any{"query": "x"}), ""))
	// Empty args yield no target.
	require.Equal(t, "",
		targetRepoRelFile("read_file", freshReq(map[string]any{}), ""))
}

func TestDecorateResultWithFreshness(t *testing.T) {
	rider := map[string]any{"file": "a.go", "stale": true}

	// JSON object: rider attached under "freshness", original keys kept.
	got := decorateResultWithFreshness(mcp.NewToolResultText(`{"x":1}`), rider)
	text, ok := singleTextContent(got)
	require.True(t, ok)
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &obj))
	require.Equal(t, float64(1), obj["x"])
	require.NotNil(t, obj["freshness"])

	// Non-JSON-object payload (GCX/TOON) is left untouched.
	got2 := decorateResultWithFreshness(mcp.NewToolResultText("GCX1 tool=foo\nrow1"), rider)
	text2, _ := singleTextContent(got2)
	require.Equal(t, "GCX1 tool=foo\nrow1", text2)

	// Empty rider is a no-op.
	got3 := decorateResultWithFreshness(mcp.NewToolResultText(`{"x":1}`), nil)
	text3, _ := singleTextContent(got3)
	require.Equal(t, `{"x":1}`, text3)

	// A worktree-mismatch-only rider still attaches.
	got4 := decorateResultWithFreshness(mcp.NewToolResultText(`{"x":1}`),
		map[string]any{"worktree_mismatch": true})
	text4, _ := singleTextContent(got4)
	var obj4 map[string]any
	require.NoError(t, json.Unmarshal([]byte(text4), &obj4))
	require.Equal(t, true, obj4["freshness"].(map[string]any)["worktree_mismatch"])
}

func TestPathWithin(t *testing.T) {
	require.True(t, pathWithin("/a/b/c", "/a/b"))
	require.True(t, pathWithin("/a/b", "/a/b"))
	require.False(t, pathWithin("/a/bc", "/a/b"), "must respect segment boundaries")
	require.False(t, pathWithin("/a", "/a/b"))
	require.False(t, pathWithin("/x/y", "/a/b"))
}

// TestMultiRepoRiderAndMissingFileFlag proves the F6 contract: the freshness
// rider now works in multi-repo mode (the legacy hard-bail is gone) by routing
// each target to its OWNING per-repo indexer, it reports a deleted-on-disk file
// as a distinct `missing` verdict, and the list-result decorator flags every
// stale / missing hit with per-repo provenance — a signal codegraph suppresses
// in multi-repo and only covers for deletions by blocking the call outright.
func TestMultiRepoRiderAndMissingFileFlag(t *testing.T) {
	repoA := setupMiniRepo(t, "repo-a")
	repoB := setupMiniRepo(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
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
	_, err = mi.IndexAll()
	require.NoError(t, err)

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	readReq := func(path string) mcp.CallToolRequest {
		return freshReq(map[string]any{"path": path})
	}

	// Baseline: freshly indexed multi-repo files draw no rider (no false
	// positives now that the bail is gone — only genuine drift flags).
	require.Nil(t, srv.freshnessRiderFor(context.Background(), "read_file", readReq("repo-a/main.go")),
		"a fresh multi-repo file must not be flagged")

	// Make repo-a/main.go stale (mtime drift) — the rider must now fire in
	// multi-repo mode and carry the OWNING repo prefix.
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(filepath.Join(repoA, "main.go"), future, future))
	staleRider := srv.freshnessRiderFor(context.Background(), "read_file", readReq("repo-a/main.go"))
	require.NotNil(t, staleRider, "stale multi-repo file must be flagged (hard-bail removed)")
	require.Equal(t, true, staleRider["stale"])
	require.Equal(t, "main.go", staleRider["file"])
	require.Equal(t, "repo-a", staleRider["repo"], "stale verdict must name the owning repo")

	// The same canonical drift must not ride on a response served by another
	// checkout/ref, nor may the read trigger canonical self-healing. Those views
	// have their own publication lifecycle; this indexer's mtime map describes
	// only repo-a's canonical root.
	owner, _ := mi.IndexerForFile(filepath.Join(repoA, "main.go"))
	require.NotNil(t, owner)
	require.True(t, owner.IsTrackedStale("main.go"))
	for name, view := range map[string]*requestView{
		"worktree": {reader: g, viewRoot: filepath.Join(t.TempDir(), "worktree")},
		"ref":      {reader: g, files: &refViewFiles{repoDir: repoA, treeOID: "tree"}},
		"grace":    {baseFallback: true},
	} {
		t.Run("routed "+name+" ignores canonical drift", func(t *testing.T) {
			ctx := withRequestView(context.Background(), view)
			require.Nil(t, srv.freshnessRiderFor(ctx, "read_file", readReq("repo-a/main.go")))
			require.Empty(t, srv.ensureFreshForRequest(ctx, []string{"repo-a/main.go"}))
			require.True(t, owner.IsTrackedStale("main.go"),
				"a routed read must not mutate the canonical checkout's freshness state")
		})
	}

	// Delete repo-b/main.go — a tracked file gone from disk must read as the
	// distinct `missing` verdict, not silently fold into not-stale.
	require.NoError(t, os.Remove(filepath.Join(repoB, "main.go")))
	missingRider := srv.freshnessRiderFor(context.Background(), "read_file", readReq("repo-b/main.go"))
	require.NotNil(t, missingRider, "deleted multi-repo file must be flagged")
	require.Equal(t, true, missingRider["missing"])
	require.Equal(t, "repo-b", missingRider["repo"])
	require.Nil(t, missingRider["stale"], "a deleted file is missing, not stale")

	// List decorator: a synthetic search-style result referencing both files
	// gets a freshness block splitting stale vs missing, each with its repo.
	listRes := mcp.NewToolResultText(
		`{"results":[{"name":"Hello","file":"repo-a/main.go"},{"name":"Hi","file":"repo-b/main.go"},{"name":"Ok","file":"repo-a/main.go"}]}`)
	for name, view := range map[string]*requestView{
		"worktree": {reader: g, viewRoot: filepath.Join(t.TempDir(), "worktree")},
		"ref":      {reader: g, files: &refViewFiles{repoDir: repoA, treeOID: "tree"}},
		"grace":    {baseFallback: true},
	} {
		t.Run("list "+name+" ignores canonical drift", func(t *testing.T) {
			ctx := withRequestView(context.Background(), view)
			require.Equal(t, listRes, srv.decorateListResultWithFreshness(ctx, listRes))
		})
	}
	decorated := srv.decorateListResultWithFreshness(context.Background(), listRes)
	text, ok := singleTextContent(decorated)
	require.True(t, ok)
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &obj))
	fresh, ok := obj["freshness"].(map[string]any)
	require.True(t, ok, "list result must carry a freshness block: %s", text)

	staleFiles, ok := fresh["stale_files"].([]any)
	require.True(t, ok, "freshness must list stale_files")
	require.Len(t, staleFiles, 1, "the repeated repo-a hit must be deduped")
	staleEntry := staleFiles[0].(map[string]any)
	require.Equal(t, "main.go", staleEntry["file"])
	require.Equal(t, "repo-a", staleEntry["repo"])

	missingFiles, ok := fresh["missing_files"].([]any)
	require.True(t, ok, "freshness must list missing_files")
	require.Len(t, missingFiles, 1)
	missingEntry := missingFiles[0].(map[string]any)
	require.Equal(t, "main.go", missingEntry["file"])
	require.Equal(t, "repo-b", missingEntry["repo"])

	// A GCX/TOON (non-JSON-object) payload the caller opted into is untouched.
	gcx := mcp.NewToolResultText("GCX1 tool=search_symbols\nrow1")
	require.Equal(t, gcx, srv.decorateListResultWithFreshness(context.Background(), gcx))
}
