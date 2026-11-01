package mcp

import (
	"context"
	"path/filepath"
	"testing"

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

// TestDiffRepoScope covers the shared repo-scope resolution the diff-driven
// handlers use: an explicit selector (prefix or path) is honoured
// exclusively, then the lone tracked repo, then the session's cwd-bound
// repo. An unknown selector resolves nothing so callers error instead of
// silently diffing another repo.
func TestDiffRepoScope(t *testing.T) {
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
	for _, entry := range cm.Global().ReposSnapshot() {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	ctx := context.Background()

	// Explicit prefix selector.
	root, prefix := srv.diffRepoScope(ctx, "repo-a")
	require.Equal(t, repoA, root)
	require.Equal(t, "repo-a", prefix)

	// Explicit path selector normalizes to the prefix.
	root, prefix = srv.diffRepoScope(ctx, repoA)
	require.Equal(t, repoA, root)
	require.Equal(t, "repo-a", prefix)

	// Unknown prefix and absolute path selectors both resolve nothing — the
	// caller errors instead of falling back to an unrelated active repo.
	for _, selector := range []string{"nope", t.TempDir()} {
		root, prefix = srv.diffRepoScope(ctx, selector)
		require.Empty(t, root)
		require.Empty(t, prefix)
	}

	// No selector + multiple tracked repos + no session binding: ambiguous.
	root, prefix = srv.diffRepoScope(ctx, "")
	require.Empty(t, root)
	require.Empty(t, prefix)

	// No selector + session cwd inside repo-b resolves repo-b.
	root, prefix = srv.diffRepoScope(WithSessionCWD(ctx, repoB), "")
	require.Equal(t, repoB, root)
	require.Equal(t, "repo-b", prefix)
}
