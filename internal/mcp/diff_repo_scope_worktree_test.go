package mcp

import (
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// TestChangesetHelpersKeepTheRepoPrefixForAWorktree: the review helpers that
// take a resolved root and prefix map a worktree's diff under the tracked
// repo's prefix, even though the worktree sits outside every tracked root.
func TestChangesetHelpersKeepTheRepoPrefixForAWorktree(t *testing.T) {
	// Given a repo tracked under "repo" whose HEAD plants an inverted
	// err-check, and a linked worktree branched from the base commit that
	// commits the same edit.
	dir, file := reviewGitRepo(t)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: dir, Name: "repo"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, testRegistry(), search.NewNull(), cm, zap.NewNop())
	for _, entry := range cm.Global().ReposSnapshot() {
		_, err := mi.TrackRepoCtx(t.Context(), entry)
		require.NoError(t, err)
	}
	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{ConfigManager: cm, MultiIndexer: mi})
	edited, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file)))
	require.NoError(t, err)
	worktree := filepath.Join(t.TempDir(), "wt")
	runGitT(t, dir, "worktree", "add", "--detach", worktree, "base-ref")
	require.NoError(t, os.WriteFile(filepath.Join(worktree, filepath.FromSlash(file)), edited, 0o644))
	runGitT(t, worktree, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-am", "edit")
	ctx := withRequestView(t.Context(), &requestView{
		kind:         requestViewKindWorktree,
		viewRoot:     worktree,
		reader:       g,
		materialized: &graphview.RepoView{ID: graphview.RepoViewID{RepoPrefix: "repo"}},
	})
	root, prefix := srv.diffRepoScope(ctx, "repo")
	require.Equal(t, worktree, root)
	require.Equal(t, "repo", prefix)
	request := func(args map[string]any) mcplib.CallToolRequest {
		req := mcplib.CallToolRequest{}
		req.Params.Arguments = args
		return req
	}

	// When each helper maps the worktree's changeset against the base commit.
	base := request(map[string]any{"base": "base-ref"})
	posted, err := srv.postReviewFindingsFor(ctx, base, root, prefix)
	require.NoError(t, err)
	critiqued, err := srv.critiqueFindingsFor(ctx, base, root, prefix)
	require.NoError(t, err)
	_, _, symbolIDs, err := srv.resolveReviewerChangeset(ctx, base, root, prefix)
	require.NoError(t, err)

	// Then the changed file joins its graph node under "repo/".
	require.NotEmpty(t, posted, "post_review finds the planted bug")
	require.NotEmpty(t, critiqued, "critique_review finds the planted bug")
	require.Contains(t, symbolIDs, "repo/"+file+"::Load")
}
