package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// A drifted read is current in its own view without advancing either base.
func TestDriftedMultiRepoReadProjectsWithoutPublishingBase(t *testing.T) {
	repoA := setupMiniRepo(t, "repo-a")
	repoB := setupMiniRepo(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: repoA, Name: "repo-a"},
		{Path: repoB, Name: "repo-b"},
	}}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())

	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	// These fixtures test path ownership/freshness, not cross-repo batch
	// resolution. Register their explicit roots through the production path.
	for _, entry := range cm.Global().ReposSnapshot() {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}
	require.True(t, mi.IsMultiRepo())

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	require.NotEmpty(t, g.FindNodesByName("Hello"), "baseline symbol indexed")
	require.Empty(t, g.FindNodesByName("HelloAgain"), "the new symbol is not on disk yet")

	// Change the file outside the indexer (as a native edit / external write
	// would), so only an on-read freshness check can notice.
	bumpFile(t, filepath.Join(repoA, "main.go"),
		"package main\n\nfunc Hello() {}\n\nfunc HelloAgain() {}\n")

	t.Cleanup(func() { srv.DrainBackground(); require.NoError(t, mi.Close(context.Background())) })
	ctx, err := srv.ensureProjected(t.Context(), []string{"repo-a/main.go"})
	require.NoError(t, err)
	require.NotNil(t, srv.readerFor(ctx).GetNode("repo-a/main.go::HelloAgain"))
	assert.Empty(t, g.FindNodesByName("HelloAgain"), "read must not publish the shared base")
	assert.True(t, mi.GetIndexer("repo-a").IsTrackedStale("main.go"), "read must not advance indexed file identity")
	assert.Equal(t, g.GetFileNodes("repo-b/main.go"), srv.readerFor(ctx).GetFileNodes("repo-b/main.go"), "unrelated repository is unchanged")
	next, err := srv.ensureProjected(t.Context(), []string{"repo-a/main.go"})
	require.NoError(t, err)
	require.Same(t, requestProjectionLayers(ctx)[0], requestProjectionLayers(next)[0], "reuse the same checked immutable file layer")

}

// A standalone server uses the same projection path, without a watcher.
func TestDriftedStandaloneReadProjectsWithoutPublishingBase(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Hello() {}\n"), 0o644))

	g := graph.New()
	reg := testRegistry()
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil) // no watcher owns publication

	require.Empty(t, g.FindNodesByName("HelloAgain"))

	bumpFile(t, filepath.Join(dir, "main.go"),
		"package main\n\nfunc Hello() {}\n\nfunc HelloAgain() {}\n")

	ctx, err := srv.ensureProjected(t.Context(), []string{"main.go"})
	require.NoError(t, err)
	require.NotNil(t, srv.readerFor(ctx).GetNode("main.go::HelloAgain"))
	assert.Empty(t, g.FindNodesByName("HelloAgain"))
	assert.True(t, idx.IsTrackedStale("main.go"))

}
