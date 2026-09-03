package indexer

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// TestLinkedWorktreeRoots reports the on-disk worktree siblings of a
// repo — the lookup the edit-path resolver uses to re-root a write into
// the worktree the file belongs to.
func TestLinkedWorktreeRoots(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	main := t.TempDir()
	runGit(t, main, "init", "-q", "-b", "main")
	runGit(t, main, "config", "user.email", "test@example.com")
	runGit(t, main, "config", "user.name", "Test")
	runGit(t, main, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(main, "main.go"), "package main\nfunc Main() {}\n")
	runGit(t, main, "add", ".")
	runGit(t, main, "commit", "-q", "-m", "init")

	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, main, "worktree", "add", "-q", "-b", "feature", wt)
	writeFile(t, filepath.Join(wt, "feature.go"), "package main\nfunc Feature() {}\n")

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: main, Name: "main-repo"},
		{Path: wt, Name: "wt"},
	}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	// Queried from the main checkout, the linked worktree is listed.
	fromMain := mi.LinkedWorktreeRoots(main)
	require.Len(t, fromMain, 1)
	assert.Equal(t, realpath(t, wt), realpath(t, fromMain[0]))

	// Queried from the worktree itself, the same set comes back —
	// the lookup keys on the shared MainRepoPath, not the argument.
	fromWt := mi.LinkedWorktreeRoots(wt)
	require.Len(t, fromWt, 1)
	assert.Equal(t, realpath(t, wt), realpath(t, fromWt[0]))

	// An unrelated path resolves to no worktree siblings.
	assert.Empty(t, mi.LinkedWorktreeRoots(t.TempDir()))
}
