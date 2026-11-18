package indexer

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/excludes"
)

// search_text enumerates the current checkout with the index walk's own
// exclusion rules: only the MCP config files in the agent-config folders that
// hold one are admitted (never the checkouts under .claude/worktrees/ nor
// other agent state), .git is never source, and per-folder ignore files and
// the user's excludes still apply on top of the agent-config exception.
func TestSourceSearchAppliesTheIndexExclusionRules(t *testing.T) {
	root := t.TempDir()
	idx := newAdmissionTestIndexer(t, root)
	for rel, body := range map[string]string{
		"main.go":                        "package main\n",
		".claude/mcp.json":               `{"mcpServers":{}}`,
		".kiro/settings/mcp.json":        `{"mcpServers":{}}`,
		".claude/worktrees/wt/main.go":   "package main\n",
		".claude/worktrees/wt/.mcp.json": `{"mcpServers":{}}`,
		".claude/state/mcp.json":         `{"mcpServers":{}}`,
		".git/hooks/hook.go":             "package hooks\n",
		"sub/.gortexignore":              "ignored.go\n",
		"sub/ignored.go":                 "package sub\n",
		"sub/kept.go":                    "package sub\n",
	} {
		writeExcludeFixture(t, filepath.Join(root, filepath.FromSlash(rel)), body)
	}
	ctx := context.Background()

	files, err := idx.CurrentSourceSearchFiles(ctx, root, nil, nil, 0, nil)
	require.NoError(t, err)
	sort.Strings(files)
	assert.Equal(t, []string{".claude/mcp.json", ".kiro/settings/mcp.json", "main.go", "sub/kept.go"}, files)

	for rel, want := range map[string]bool{
		".claude/mcp.json":               true,
		".kiro/settings/mcp.json":        true,
		".claude/worktrees/wt/.mcp.json": false,
		".claude/worktrees/wt/main.go":   false,
		".claude/state/mcp.json":         false,
		"sub/ignored.go":                 false,
	} {
		content := []byte(`{"mcpServers":{}}`)
		if filepath.Ext(rel) == ".go" {
			content = []byte("package x\n")
		}
		assert.Equal(t, want, idx.AdmitSourceSearchBuffer(ctx, root, filepath.Join(root, filepath.FromSlash(rel)), content, nil), rel)
	}

	// A caller-supplied policy lifts only the Builtin agent-config lines: the
	// user's own exclude of an agent-config folder still applies.
	patterns := append(append([]string{}, excludes.Builtin...), ".kiro/")
	files, err = idx.CurrentSourceSearchFiles(ctx, root, nil, nil, 0, patterns)
	require.NoError(t, err)
	sort.Strings(files)
	assert.Equal(t, []string{".claude/mcp.json", "main.go", "sub/kept.go"}, files)
	assert.False(t, idx.AdmitSourceSearchBuffer(ctx, root, filepath.Join(root, ".kiro", "settings", "mcp.json"), []byte(`{"mcpServers":{}}`), patterns))
}

func TestSourceSearchPreservesIncludesWithExplicitPatterns(t *testing.T) {
	root := t.TempDir()
	idx := newAdmissionTestIndexer(t, root)
	idx.config.Include = []string{"vendor/keep.go", "vendor/blocked/keep.go"}
	for _, rel := range []string{"vendor/keep.go", "vendor/blocked/keep.go", "dir/keep.go", "main.go"} {
		writeExcludeFixture(t, filepath.Join(root, rel), "package app\n")
	}
	writeExcludeFixture(t, filepath.Join(root, "vendor/blocked/.gortexignore"), "keep.go\n")
	patterns := append(append([]string{}, excludes.Builtin...), "dir/", "!dir/keep.go")
	files, err := idx.CurrentSourceSearchFiles(context.Background(), root, nil, nil, 0, patterns)
	require.NoError(t, err)
	require.Equal(t, []string{"main.go", "vendor/keep.go"}, files)
	for rel, want := range map[string]bool{"vendor/keep.go": true, "vendor/blocked/keep.go": false, "dir/keep.go": false} {
		require.Equal(t, want, idx.AdmitSourceSearchBuffer(context.Background(), root, filepath.Join(root, rel), []byte("package app\n"), patterns), rel)
	}
}
