package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// TestSearchText_PathFilterAppliesBeforeLimit pins the issue-#827 fix at
// the tool boundary: a path-scoped search_text must return scoped
// matches at a limit that the unscoped ordering fills with
// out-of-scope files, instead of wiping the page to zero. The fixture
// names files so lexicographic order puts two docs and other/ ahead of
// src/ — limit 3 is exactly the old post-filter wipeout shape.
func TestSearchText_PathFilterAppliesBeforeLimit(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"ADoc.md":    "Needle in the readme\n",
		"ZDoc.md":    "Needle in the changelog\n",
		"other/c.go": "package other\n\nvar NeedleC = 1\n",
		"src/a.go":   "package src\n\nvar NeedleA = 1\n",
		"src/b.go":   "package src\n\nvar NeedleB = 1\n",
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)

	// Sanity: the unscoped head at limit 3 holds no src/ hit — otherwise
	// this test no longer exercises the wipeout shape.
	unscoped := searchTextResponse(t, srv, map[string]any{"query": "Needle", "limit": 3})
	matches, _ := unscoped["matches"].([]any)
	require.NotEmpty(t, matches)
	for _, m := range matches {
		path, _ := m.(map[string]any)["path"].(string)
		require.NotContains(t, path, "src/",
			"fixture drifted: a src/ hit reached the unscoped head at limit 3")
	}

	out := searchTextResponse(t, srv, map[string]any{"query": "Needle", "path": "src", "limit": 3})

	require.Equal(t, float64(2), out["count"],
		"the scoped page must hold the scoped matches, not the empty remainder of a global cut")
	scoped, ok := out["matches"].([]any)
	require.True(t, ok)
	require.Len(t, scoped, 2)
	for _, m := range scoped {
		path, _ := m.(map[string]any)["path"].(string)
		require.Contains(t, path, "src/")
	}
	_, truncated := out["_truncated_by_limit"]
	require.False(t, truncated,
		"the scoped search stopped on corpus exhaustion, not on the limit")
}

// TestSearchText_MultiRepoPathFormsSurviveTheInSearchFilter pins the
// #845 review fix: the in-search path restriction must accept every
// spelling that worked under the old post-filter — including the
// repo-prefixed form every graph tool returns (the one an agent copies
// back) — instead of silently zeroing the result. The fixture mirrors
// the review's: repos alpha and beta each hold pkg/sub/main.go with a
// Needle, so `alpha/pkg/sub` must match alpha alone and the other
// spellings must match both repos.
func TestSearchText_MultiRepoPathFormsSurviveTheInSearchFilter(t *testing.T) {
	alpha := setupNestedRepo(t, "alpha", "shared", "package sub\n\nvar Needle = 1\n")
	beta := setupNestedRepo(t, "beta", "shared", "package sub\n\nvar Needle = 1\n")
	srv, _ := nestedRepoServer(t, []config.RepoEntry{
		{Path: alpha, Name: "alpha", Project: "backend"},
		{Path: beta, Name: "beta", Project: "backend"},
	})

	cases := []struct {
		path  string
		count int
	}{
		{"pkg/sub", 2},
		{"alpha/pkg/sub", 1},
		{"./pkg/sub", 2},
		{"pkg\\sub", 2},
		{"/pkg/sub", 2},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			out := searchTextResponse(t, srv, map[string]any{
				"query": "Needle", "path": tc.path, "limit": 10,
			})
			require.Equal(t, float64(tc.count), out["count"],
				"path form %q must not silently zero the scoped result", tc.path)
			_, truncated := out["_truncated_by_limit"]
			require.False(t, truncated)
		})
	}
}

// TestSearchText_RepoQualifiedPathSkipsTheOtherRepos pins the #845
// round-2 fix: when every filter is repo-qualified, a repo none of the
// filters names must be skipped outright, not searched unscoped. Beta
// holding five needles at limit 3 makes the old fallback visible: its
// unscoped hits inflated rawMatches past the limit and tripped a false
// _truncated_by_limit on what is a complete result.
func TestSearchText_RepoQualifiedPathSkipsTheOtherRepos(t *testing.T) {
	alpha := setupNestedRepo(t, "alpha", "shared", "package sub\n\nvar Needle = 1\n")
	beta := setupNestedRepo(t, "beta", "shared", "package sub\n\nvar Needle = 1\n")
	for i := 0; i < 4; i++ {
		require.NoError(t, os.WriteFile(
			filepath.Join(beta, fmt.Sprintf("extra%d.go", i)),
			[]byte("package extra\n\n// Needle in the notes\n"), 0o644))
	}
	srv, _ := nestedRepoServer(t, []config.RepoEntry{
		{Path: alpha, Name: "alpha", Project: "backend"},
		{Path: beta, Name: "beta", Project: "backend"},
	})

	for _, tc := range []struct{ name, path string }{
		{"repo-qualified prefix", "alpha/pkg/sub"},
		{"repo named whole", "alpha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := searchTextResponse(t, srv, map[string]any{
				"query": "Needle", "path": tc.path, "limit": 3,
			})
			require.Equal(t, float64(1), out["count"],
				"only alpha matches; beta must not be searched unscoped")
			_, truncated := out["_truncated_by_limit"]
			require.False(t, truncated,
				"the result is complete — beta's discarded hits must not inflate the raw count past the limit")
		})
	}
}

// TestSearchText_MultiRepoRegexpRoutingAppliesBeforeLimit pins the
// #845 round-2 gap on the multi-repo regexp branch: the routed path
// prefixes must reach GrepRegexpForReposPaths, so the restriction
// lands inside each per-repo search before its limit cut — what the
// single-indexer regexp test already pins. With the routing dropped
// (nil prefixes), alpha's out-of-scope head consumes its cut at limit
// 1 and the scoped result wipes to zero.
func TestSearchText_MultiRepoRegexpRoutingAppliesBeforeLimit(t *testing.T) {
	alpha := setupNestedRepo(t, "alpha", "shared", "package sub\n\nvar Needle = 1\n")
	beta := setupNestedRepo(t, "beta", "shared", "package sub\n\nvar Needle = 1\n")
	// Both sort ahead of pkg/sub/main.go, so an unscoped cut at limit 2
	// keeps only out-of-scope hits and the scoped result wipes to zero.
	require.NoError(t, os.WriteFile(filepath.Join(alpha, "a1.go"),
		[]byte("package filler\n\n// Needle in the readme\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(alpha, "a2.go"),
		[]byte("package filler\n\n// Needle in the changelog\n"), 0o644))
	srv, _ := nestedRepoServer(t, []config.RepoEntry{
		{Path: alpha, Name: "alpha", Project: "backend"},
		{Path: beta, Name: "beta", Project: "backend"},
	})

	out := searchTextResponse(t, srv, map[string]any{
		"query": "Needle", "regexp": true, "path": "alpha/pkg/sub", "limit": 2,
	})
	require.Equal(t, float64(1), out["count"],
		"the regexp branch must apply the per-repo path restriction before the limit cut")
	scoped, ok := out["matches"].([]any)
	require.True(t, ok)
	require.Len(t, scoped, 1)
	path, _ := scoped[0].(map[string]any)["path"].(string)
	require.Contains(t, path, "alpha/pkg/sub/")
	_, truncated := out["_truncated_by_limit"]
	require.False(t, truncated)
}

// TestSearchText_RegexpPathFilterAppliesBeforeLimit pins the #845 review
// fix for regexp queries: the regexp branches previously passed an empty
// pathPrefix to the searchers, so the path filter ran after the limit
// cut and a scoped regexp search could wipe to zero exactly like the
// literal one did before #827.
func TestSearchText_RegexpPathFilterAppliesBeforeLimit(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"ADoc.md":    "Needle in the readme\n",
		"ZDoc.md":    "Needle in the changelog\n",
		"other/c.go": "package other\n\nvar NeedleC = 1\n",
		"src/a.go":   "package src\n\nvar NeedleA = 1\n",
		"src/b.go":   "package src\n\nvar NeedleB = 1\n",
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)

	out := searchTextResponse(t, srv, map[string]any{
		"query": "Needle", "regexp": true, "path": "src", "limit": 3,
	})
	require.Equal(t, float64(2), out["count"],
		"the regexp branch must apply the path filter before the limit cut")
	scoped, ok := out["matches"].([]any)
	require.True(t, ok)
	require.Len(t, scoped, 2)
	for _, m := range scoped {
		path, _ := m.(map[string]any)["path"].(string)
		require.Contains(t, path, "src/")
	}
	_, truncated := out["_truncated_by_limit"]
	require.False(t, truncated)
}
