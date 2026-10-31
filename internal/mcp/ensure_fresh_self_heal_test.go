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

// TestEnsureFresh_MultiRepoSelfHealsStaleFile is the regression test for the
// stale-read class: in multi-repo mode ensureFresh used to return early and do
// nothing, so a file changed on disk since indexing kept serving its old body
// through the index-backed read tools. It must now route the path to its owning
// per-repo indexer, detect the drift, and re-index — leaving the graph current.
func TestEnsureFresh_MultiRepoSelfHealsStaleFile(t *testing.T) {
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

	refreshed := srv.ensureFresh([]string{"repo-a/main.go"})
	assert.Equal(t, []string{"repo-a/main.go"}, refreshed,
		"multi-repo ensureFresh must re-index the drifted file (pre-fix it returned nil)")
	assert.NotEmpty(t, g.FindNodesByName("HelloAgain"),
		"the graph reflects the new on-disk content after self-heal")

	// A follow-up call is a no-op: the recorded mtime advanced, so the file is
	// no longer classified stale and is not re-indexed again.
	assert.Empty(t, srv.ensureFresh([]string{"repo-a/main.go"}),
		"an already-fresh file is not re-indexed a second time")
}

// TestEnsureFresh_SingleRepoSelfHealsStaleFile covers the single-repo path with
// no active watcher: the on-read freshness check still re-indexes a drifted
// file so the graph does not serve a stale body.
func TestEnsureFresh_SingleRepoSelfHealsStaleFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Hello() {}\n"), 0o644))

	g := graph.New()
	reg := testRegistry()
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil) // watcher nil → on-read refresh active

	require.Empty(t, g.FindNodesByName("HelloAgain"))

	bumpFile(t, filepath.Join(dir, "main.go"),
		"package main\n\nfunc Hello() {}\n\nfunc HelloAgain() {}\n")

	refreshed := srv.ensureFresh([]string{"main.go"})
	assert.Equal(t, []string{"main.go"}, refreshed)
	assert.NotEmpty(t, g.FindNodesByName("HelloAgain"),
		"single-repo self-heal re-indexed the drifted file")
}
