package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/trigram"
)

// The overlay layer builder mints nodes for the buffers an editor pushes.
// Readers gate a node with query.QueryOptions.ScopeAllows, which reads
// WorkspaceID and falls back to RepoPrefix only when it is empty. A repo
// whose workspace slug equals its prefix hides a missing stamp; these tests
// use a repo named "repo-a" inside workspace "acme", where the fallback
// resolves to the wrong slug and an unstamped overlay node is dropped by
// every scope-narrowed read.

const (
	stampWorkspace = "acme"
	stampRepo      = "repo-a"
	stampFilePath  = stampRepo + "/main.go"
	stampKeptID    = stampFilePath + "::Handle"
	stampAddedID   = stampFilePath + "::Added"
)

// stampFixture is a single tracked repo whose declared workspace slug
// differs from its repo prefix, indexed for real so the base nodes carry
// the slugs the indexer stamps.
type stampFixture struct {
	srv  *Server
	root string
}

func newOverlayStampFixture(t *testing.T) stampFixture {
	t.Helper()

	root := filepath.Join(t.TempDir(), stampRepo)
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"),
		[]byte("workspace: "+stampWorkspace+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"),
		[]byte("package main\n\nfunc Handle() {}\n"), 0o644))

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: root, Name: stampRepo}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	bm := search.NewNull()
	mi := indexer.NewMultiIndexer(g, testRegistry(), bm, cm, zap.NewNop())
	_, err = mi.IndexScoped("", "")
	require.NoError(t, err)

	eng := query.NewEngine(g)
	eng.SetSearch(bm)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer:  mi,
		ConfigManager: cm,
	})
	return stampFixture{srv: srv, root: root}
}

// buildStampLayer pushes one buffer that keeps the indexed symbol and adds
// a second one, and returns the constructed layer.
func (f stampFixture) buildStampLayer(t *testing.T) *graph.OverlayLayer {
	t.Helper()
	layer, paths, err := f.srv.constructOverlayLayer(context.Background(), []daemon.OverlayFile{{
		Path:    filepath.Join(f.root, "main.go"),
		Content: "package main\n\nfunc Handle() {}\n\nfunc Added() {}\n",
	}})
	require.NoError(t, err)
	require.NotNil(t, layer)
	require.Equal(t, []string{stampFilePath}, paths)
	return layer
}

// stampBaseNode reads one indexed node straight off the base store.
func (f stampFixture) stampBaseNode(t *testing.T, id string) *graph.Node {
	t.Helper()
	n := f.srv.graph.GetNode(id)
	require.NotNil(t, n, "fixture base node %q missing", id)
	return n
}

// TestOverlayLayerStampsWorkspaceIdentity is the identity half: every node
// the layer mints must carry the same workspace / project slugs as the base
// nodes of the same repo, whether it re-emits an indexed symbol, replaces
// the file node, or is new in the buffer.
func TestOverlayLayerStampsWorkspaceIdentity(t *testing.T) {
	f := newOverlayStampFixture(t)

	baseSymbol := f.stampBaseNode(t, stampKeptID)
	require.Equal(t, stampWorkspace, baseSymbol.WorkspaceID,
		"fixture is pointless unless the base slug differs from the prefix")
	require.NotEqual(t, baseSymbol.RepoPrefix, baseSymbol.WorkspaceID)

	layer := f.buildStampLayer(t)
	view := graph.NewOverlaidView(f.srv.graph, layer)

	minted := view.GetFileNodes(stampFilePath)
	require.NotEmpty(t, minted)
	ids := make(map[string]bool, len(minted))
	for _, n := range minted {
		ids[n.ID] = true
		assert.Equal(t, baseSymbol.WorkspaceID, n.WorkspaceID,
			"layer node %q must carry the repo's workspace slug", n.ID)
		assert.Equal(t, baseSymbol.ProjectID, n.ProjectID,
			"layer node %q must carry the repo's project slug", n.ID)
		assert.Equal(t, stampRepo, n.RepoPrefix)
	}
	assert.True(t, ids[stampKeptID], "the re-emitted symbol must be in the layer")
	assert.True(t, ids[stampAddedID], "the symbol new in the buffer must be in the layer")

	// The file node is what search_text attributes a match through, so it
	// carries the stamp too.
	fileNode := view.GetNode(stampFilePath)
	require.NotNil(t, fileNode, "the layer must re-emit the covered file node")
	assert.Equal(t, stampWorkspace, fileNode.WorkspaceID)
}

// TestOverlayLayerStampsNewFileFromIndexerBinding covers the last step of
// the precedence chain: a file that only exists in the buffer has no base
// nodes to copy an identity from, so the stamp comes from the repo
// indexer's own binding — the same value it stamps at indexing time.
func TestOverlayLayerStampsNewFileFromIndexerBinding(t *testing.T) {
	f := newOverlayStampFixture(t)
	graphPath := stampRepo + "/fresh.go"

	layer, paths, err := f.srv.constructOverlayLayer(context.Background(), []daemon.OverlayFile{{
		Path:    filepath.Join(f.root, "fresh.go"),
		Content: "package main\n\nfunc Fresh() {}\n",
	}})
	require.NoError(t, err)
	require.NotNil(t, layer)
	require.Equal(t, []string{graphPath}, paths)

	nodes := graph.NewOverlaidView(f.srv.graph, layer).GetFileNodes(graphPath)
	require.NotEmpty(t, nodes, "a buffer for a file with no base nodes still mints nodes")
	for _, n := range nodes {
		assert.Equal(t, stampWorkspace, n.WorkspaceID, "new-file node %q", n.ID)
		assert.Equal(t, stampRepo, n.ProjectID, "new-file node %q", n.ID)
	}
}

// TestOverlayNodesSurviveScopeNarrowedRead is the reader half: a session
// bound to the repo narrows every whole-graph read to workspace "acme", and
// the buffer's symbols must survive that narrow. Without the stamp the
// layer nodes fall back to the repo prefix and the scoped spine drops them,
// so an editor session sees the file as empty.
func TestOverlayNodesSurviveScopeNarrowedRead(t *testing.T) {
	f := newOverlayStampFixture(t)
	layer := f.buildStampLayer(t)

	ctx := sessionCtx("s-stamp", f.root)
	ws, _, bound := f.srv.sessionScope(ctx)
	require.True(t, bound)
	require.Equal(t, stampWorkspace, ws, "the session narrows on the declared slug")

	viewCtx := WithOverlayView(ctx, graph.NewOverlaidView(f.srv.graph, layer))
	scoped := make(map[string]bool)
	for _, n := range f.srv.scopedNodes(viewCtx) {
		scoped[n.ID] = true
	}
	assert.True(t, scoped[stampKeptID], "the re-emitted symbol must survive the workspace narrow")
	assert.True(t, scoped[stampAddedID], "the symbol new in the buffer must survive the workspace narrow")

	// search_text attributes each match to a graph node and fail-closed
	// drops anything it cannot prove in scope. The covered file resolves to
	// the layer's file node, so the stamp decides whether the whole buffer's
	// matches survive.
	resolved := ResolvedScope{WorkspaceID: ws, RepoAllow: map[string]bool{stampRepo: true}}
	match := trigram.Match{Path: stampFilePath, Line: 5, Text: "func Added() {}"}
	kept := f.srv.filterTextMatchesByResolvedScope(viewCtx, []trigram.Match{match}, resolved)
	assert.Equal(t, []trigram.Match{match}, kept,
		"a match in a covered file must not be dropped by the scope filter")
}
