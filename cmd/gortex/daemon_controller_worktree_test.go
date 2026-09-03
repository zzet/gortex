package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// wtGit runs one git command in dir with the developer's own configuration
// pinned out of the way.
//
// The null device is os.DevNull rather than a "/dev/null" literal: that
// literal names no file on Windows, so the isolation it is there to provide
// depends on git's own path emulation rather than on the platform.
func wtGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
}

// buildWorktreeController builds a realController whose MultiIndexer has
// indexed a canonical checkout plus a linked worktree (declaring a
// distinct workspace, with a colliding basename, so both would otherwise
// resolve to the same prefix). Returns the controller, indexer, config
// manager, and the two checkout paths.
func buildWorktreeController(t *testing.T) (*realController, *indexer.MultiIndexer, *config.ConfigManager, string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	canon := filepath.Join(t.TempDir(), "oas-orm")
	require.NoError(t, os.MkdirAll(canon, 0o755))
	wtGit(t, canon, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(canon, "lib.go"),
		[]byte("package lib\n\nfunc C() {}\n"), 0o644))
	wtGit(t, canon, "add", ".")
	wtGit(t, canon, "commit", "-q", "-m", "init")

	wt := filepath.Join(t.TempDir(), "oas-orm")
	wtGit(t, canon, "worktree", "add", "-q", "-b", "task", wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".gortex.yaml"),
		[]byte("workspace: task-ws\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "feature.go"),
		[]byte("package lib\n\nfunc F() {}\n"), 0o644))

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: canon}, {Path: wt}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	// An in-memory graph has no checkout catalog, so the lifecycle runs in
	// its degraded shape here: no identities, and the same index / watcher /
	// config side effects the reload diff has always had.
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer:  mi,
		ConfigManager: cm,
		Graph:         g,
		Logger:        zap.NewNop(),
	})
	require.NoError(t, err)

	c := &realController{
		graph:         g,
		multiIndexer:  mi,
		configManager: cm,
		lifecycle:     lifecycle,
		logger:        zap.NewNop(),
	}
	return c, mi, cm, canon, wt
}

func reloadResult(t *testing.T, c *realController) (added, removed int) {
	t.Helper()
	raw, err := c.Reload(context.Background())
	require.NoError(t, err)
	var res struct {
		Added   int `json:"added"`
		Removed int `json:"removed"`
	}
	require.NoError(t, json.Unmarshal(raw, &res))
	return res.Added, res.Removed
}

// TestReload_KeepsWorktreeInstance is the regression for the reload
// untrack-diff: a worktree tracked under a derived `<base>@<workspace>`
// prefix must not be untracked on a no-op reload just because its config
// entry's basename matches the canonical's prefix.
func TestReload_KeepsWorktreeInstance(t *testing.T) {
	c, mi, _, _, _ := buildWorktreeController(t)
	require.NotNil(t, mi.GetMetadata("oas-orm"))
	require.NotNil(t, mi.GetMetadata("oas-orm@task-ws"))

	added, removed := reloadResult(t, c)
	assert.Equal(t, 0, added)
	assert.Equal(t, 0, removed, "reload must not untrack the worktree instance")
	assert.NotNil(t, mi.GetMetadata("oas-orm@task-ws"), "worktree instance survives reload")
	assert.NotNil(t, mi.GetMetadata("oas-orm"), "canonical survives reload")
}

// TestReload_UntracksRemovedWorktree confirms a worktree instance is
// dropped when its entry leaves the config — matched by root path, not a
// recomputed prefix.
func TestReload_UntracksRemovedWorktree(t *testing.T) {
	c, mi, cm, _, wt := buildWorktreeController(t)
	require.NotNil(t, mi.GetMetadata("oas-orm@task-ws"))

	require.NoError(t, cm.Global().RemoveRepo(wt))
	require.NoError(t, cm.Global().Save())

	_, removed := reloadResult(t, c)
	assert.Equal(t, 1, removed)
	assert.Nil(t, mi.GetMetadata("oas-orm@task-ws"), "removed worktree is untracked")
	assert.NotNil(t, mi.GetMetadata("oas-orm"), "canonical remains tracked")
}
