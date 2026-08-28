package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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

// newWorktreeMCPServer builds a Server whose MultiIndexer has already
// indexed the supplied config repos. Used to exercise the runtime
// track_repository path for git-worktree instancing.
func newWorktreeMCPServer(t *testing.T, repos ...config.RepoEntry) (*Server, *indexer.MultiIndexer) {
	t.Helper()
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: repos}
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
	return srv, mi
}

func gitInitWorktreeRepo(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	gitInit(t, dir, "init", "-q", "-b", "main")
	gitInit(t, dir, "config", "user.email", "test@example.com")
	gitInit(t, dir, "config", "user.name", "Test")
	gitInit(t, dir, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib.go"),
		[]byte("package lib\n\nfunc Canonical() {}\n"), 0o644))
	gitInit(t, dir, "add", ".")
	gitInit(t, dir, "commit", "-q", "-m", "init")
}

func trackRepoPrefix(t *testing.T, srv *Server, args map[string]any) (string, string) {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = args
	res, err := srv.handleTrackRepository(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "track_repository failed: %+v", res.Content)
	var payload struct {
		Prefix string `json:"prefix"`
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(extractTextFromContent(t, res.Content)), &payload))
	return payload.Prefix, payload.Status
}

// TestTrackRepository_WorktreeAutoInstance exercises the issue #47 flow
// end-to-end through the MCP track_repository tool: a worktree whose
// `.gortex.yaml` declares a different workspace is auto-tracked as an
// independent instance, and the tool reports the derived prefix.
func TestTrackRepository_WorktreeAutoInstance(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	canon := filepath.Join(t.TempDir(), "oas-orm")
	gitInitWorktreeRepo(t, canon)

	// A placeholder repo keeps the indexer in multi-repo (prefixed) mode.
	placeholder := filepath.Join(t.TempDir(), "placeholder")
	require.NoError(t, os.MkdirAll(placeholder, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(placeholder, "p.go"),
		[]byte("package p\n\nfunc P() {}\n"), 0o644))

	srv, mi := newWorktreeMCPServer(t,
		config.RepoEntry{Path: canon},
		config.RepoEntry{Path: placeholder},
	)
	require.NotNil(t, mi.GetMetadata("oas-orm"))

	// Linked worktree, colliding basename, declaring a distinct workspace.
	wt := filepath.Join(t.TempDir(), "oas-orm")
	gitInit(t, canon, "worktree", "add", "-q", "-b", "task", wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".gortex.yaml"),
		[]byte("workspace: task-ws\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "feature.go"),
		[]byte("package lib\n\nfunc Feature() {}\n"), 0o644))

	prefix, status := trackRepoPrefix(t, srv, map[string]any{"path": wt})
	assert.Equal(t, "tracked", status)
	assert.Equal(t, "oas-orm@task-ws", prefix, "worktree must get a derived instance prefix")

	assert.NotNil(t, mi.GetMetadata("oas-orm@task-ws"), "worktree instance tracked")
	assert.NotNil(t, mi.GetMetadata("oas-orm"), "canonical must remain tracked")
}

// TestTrackRepository_WorktreeForcedFlag covers the explicit
// `as_worktree` directive on a worktree that declares no workspace: the
// branch name (sanitised) becomes the instance tag.
func TestTrackRepository_WorktreeForcedFlag(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	canon := filepath.Join(t.TempDir(), "oas-orm")
	gitInitWorktreeRepo(t, canon)

	placeholder := filepath.Join(t.TempDir(), "placeholder")
	require.NoError(t, os.MkdirAll(placeholder, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(placeholder, "p.go"),
		[]byte("package p\n\nfunc P() {}\n"), 0o644))

	srv, mi := newWorktreeMCPServer(t,
		config.RepoEntry{Path: canon},
		config.RepoEntry{Path: placeholder},
	)

	wt := filepath.Join(t.TempDir(), "oas-orm")
	gitInit(t, canon, "worktree", "add", "-q", "-b", "feature/login", wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "feature.go"),
		[]byte("package lib\n\nfunc Feature() {}\n"), 0o644))

	prefix, status := trackRepoPrefix(t, srv, map[string]any{"path": wt, "as_worktree": true})
	assert.Equal(t, "tracked", status)
	assert.Equal(t, "oas-orm@feature-login", prefix,
		"forced worktree without a declared workspace is tagged by its sanitised branch")
	assert.NotNil(t, mi.GetMetadata("oas-orm@feature-login"))
	assert.NotNil(t, mi.GetMetadata("oas-orm"))
}
