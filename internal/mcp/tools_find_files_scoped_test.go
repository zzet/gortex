package mcp

import (
	"context"
	"encoding/json"
	"iter"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type findFilesProjectionStore struct {
	*store_sqlite.Store
	globalReads    int
	projectedRepos []string
	fullReads      int
	summaryReads   int
}

func (s *findFilesProjectionStore) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		s.globalReads++
		s.Store.NodesByKind(kind)(yield)
	}
}

func (s *findFilesProjectionStore) NodesInScopeSeq(repos, files []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	s.fullReads++
	s.projectedRepos = append(s.projectedRepos, repos...)
	return s.Store.NodesInScopeSeq(repos, files, kinds...)
}

func (s *findFilesProjectionStore) NodesLightByKindsInScopeSeq(repos, files []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	s.summaryReads++
	s.projectedRepos = append(s.projectedRepos, repos...)
	return s.Store.NodesLightByKindsInScopeSeq(repos, files, kinds...)
}

func TestFindFilesScopedProjectionPreservesScopeAndOverlay(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "files.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "a/README.md", Kind: graph.KindFile, FilePath: "a/README.md", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "markdown"},
		{ID: "z-root", Kind: graph.KindFile, FilePath: "README.md", WorkspaceID: "ws", ProjectID: "p", Language: "text"},
		{ID: "a/deep/README.md", Kind: graph.KindFile, FilePath: "a/deep/README.md", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "markdown"},
		{ID: "loose/README.md", Kind: graph.KindFile, FilePath: "loose/README.md", WorkspaceID: "ws", ProjectID: "p", Language: "text"},
		{ID: "b/README.md", Kind: graph.KindFile, FilePath: "b/README.md", RepoPrefix: "b", WorkspaceID: "ws", ProjectID: "p"},
		{ID: "outside/README.md", Kind: graph.KindFile, FilePath: "outside/README.md", WorkspaceID: "other", ProjectID: "p"},
		{ID: "wrong-project/README.md", Kind: graph.KindFile, FilePath: "wrong-project/README.md", WorkspaceID: "ws", ProjectID: "other"},
	}, nil)
	tracked := &findFilesProjectionStore{Store: store}
	srv, _ := overlaySpineFixture(t)
	srv.graph = tracked
	srv.scopeWorkspace, srv.scopeProject = "ws", "p"
	require.Implements(t, (*graph.ScopedProjectionSequencer)(nil), srv.readerFor(context.Background()))
	read := func(ctx context.Context, args map[string]any) map[string]any {
		res, err := srv.handleFindFiles(ctx, makeReq("find_files", args))
		require.NoError(t, err)
		require.False(t, res.IsError)
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
		return out
	}
	args := map[string]any{"query": "README.md", "repo": "a", "limit": 2}
	projected := read(context.Background(), args)
	require.Equal(t, 0, tracked.globalReads)
	require.Equal(t, []string{"", "a"}, tracked.projectedRepos)
	require.Equal(t, true, projected["truncated"])
	files := projected["files"].([]any)
	require.Equal(t, "a/README.md", files[0].(map[string]any)["id"])
	require.Equal(t, "markdown", files[0].(map[string]any)["language"])
	require.Equal(t, "z-root", files[1].(map[string]any)["id"])

	// Readers without the optional projection retain the original semantics.
	srv.graph = struct{ graph.Store }{store}
	require.Equal(t, projected, read(context.Background(), args))
	args["limit"] = 100
	all := read(context.Background(), args)
	require.Equal(t, float64(4), all["count"])
	require.Equal(t, false, all["truncated"])

	// A request overlay must mask the base file and serve its replacement.
	srv.graph = tracked
	layer := graph.NewOverlayLayer()
	layer.MarkFile("a/README.md", true)
	layer.MarkFile("a/new/README.md", false)
	layer.AddNode("a/new/README.md", &graph.Node{ID: "a/new/README.md", Kind: graph.KindFile, FilePath: "a/new/README.md", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "overlay"})
	overlaid := read(overlayCtx(t, srv, layer), args)
	ids := make([]string, 0)
	for _, file := range overlaid["files"].([]any) {
		ids = append(ids, file.(map[string]any)["id"].(string))
	}
	require.Equal(t, []string{"z-root", "a/deep/README.md", "loose/README.md", "a/new/README.md"}, ids)
	args["path"] = "new"
	pathOnly := read(overlayCtx(t, srv, layer), args)
	require.Equal(t, float64(1), pathOnly["count"])
	require.Equal(t, "overlay", pathOnly["files"].([]any)[0].(map[string]any)["language"])
}
