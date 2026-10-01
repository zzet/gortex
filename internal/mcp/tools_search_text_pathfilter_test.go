package mcp

import (
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
