package mcp

import (
	"context"
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
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// newSingleRepoServer indexes one repo through the MultiIndexer — the
// daemon/serverstack shape where multiIndexer is always non-nil — and
// returns the server plus the repo root. Its nodes carry RepoPrefix
// "myrepo": a lone repo is the first tracked repo, not a special mode.
func newSingleRepoServer(t *testing.T) (*Server, *graph.Graph, string) {
	t.Helper()
	dir := setupMiniRepo(t, "myrepo")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: dir, Name: "myrepo"}},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})
	return srv, g, dir
}

// Resolving a node's source path is the gate in front of every source read
// — and of savings recording — so it must work for the lone repo's own
// nodes.
func TestResolveNodePath_LoneRepoNode(t *testing.T) {
	srv, g, dir := newSingleRepoServer(t)

	node := g.GetNode("myrepo/main.go::Hello")
	require.NotNil(t, node, "a lone repo's node IDs carry its prefix")
	require.Equal(t, "myrepo", node.RepoPrefix)

	abs, err := srv.resolveNodePath(context.Background(), node)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "main.go"), abs)
}

// Bare repo-relative paths must keep resolving on a solo daemon. This is the
// agent-facing compatibility guarantee of always-prefixed IDs: nothing
// forces an agent to learn the prefix before it can name a file.
func TestResolveFilePath_LoneRepoBareRelative(t *testing.T) {
	srv, _, dir := newSingleRepoServer(t)

	abs, rel, err := srv.resolveFilePath(context.Background(), "main.go")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "main.go"), abs)
	// relPath comes back in the GRAPH's spelling, not the caller's. Every
	// downstream node lookup keys on it, and graph file keys are prefixed —
	// echoing the bare input back resolved the bytes and then reported
	// file_not_indexed for a file that was indexed.
	assert.Equal(t, "myrepo/main.go", rel)

	// A file that does NOT exist yet — a write_file / edit_file create
	// target — must resolve too. Existence-gated anchoring alone would
	// refuse this, which is the regression the sole-repo lookup prevents.
	abs, rel, err = srv.resolveFilePath(context.Background(), "internal/brand_new.go")
	require.NoError(t, err, "creating a new file by bare path must work in a solo workspace")
	assert.Equal(t, filepath.Join(dir, "internal", "brand_new.go"), abs)
	assert.Equal(t, "myrepo/internal/brand_new.go", rel)

	// Containment still enforced: escaping the lone root is refused.
	_, _, err = srv.resolveFilePath(context.Background(), "../escape.go")
	require.Error(t, err)

	// The prefixed form keeps working.
	abs, _, err = srv.resolveFilePath(context.Background(), "myrepo/main.go")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "main.go"), abs)
}

func TestResolveFilePath_MultiRepoBareRelativeStillAmbiguous(t *testing.T) {
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
	_, err = mi.IndexAll()
	require.NoError(t, err)

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	_, _, err = srv.resolveFilePath(context.Background(), "main.go")
	require.Error(t, err, "bare-relative path with two tracked repos stays ambiguous")
}
