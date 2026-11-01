package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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
	edgeReads      int
	projectedRepos []string
	fullReads      int
	summaryReads   int
	locationReads  int
	locationRows   int
}

func (s *findFilesProjectionStore) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		s.globalReads++
		s.Store.NodesByKind(kind)(yield)
	}
}

func (s *findFilesProjectionStore) AllNodes() []*graph.Node {
	s.globalReads++
	return s.Store.AllNodes()
}

func (s *findFilesProjectionStore) AllNodesLight() []*graph.Node {
	s.globalReads++
	return s.Store.AllNodesLight()
}

func (s *findFilesProjectionStore) AllEdges() []*graph.Edge {
	s.edgeReads++
	return s.Store.AllEdges()
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

func (s *findFilesProjectionStore) FileLocationsInScopeSeq(repos []string) iter.Seq[*graph.Node] {
	s.locationReads++
	s.projectedRepos = append(s.projectedRepos, repos...)
	return func(yield func(*graph.Node) bool) {
		for node := range s.Store.FileLocationsInScopeSeq(repos) {
			s.locationRows++
			if !yield(node) {
				return
			}
		}
	}
}

// Unrelated repositories holding the same paths must change neither the
// answer nor the rows the location read yields: the read is bounded by the
// requested repository and the unowned files, never by the store.
func TestFindFilesScopedNeverScansAndRowsStayFlat(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "flat.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	readmes := func(repo string) []*graph.Node {
		nodes := make([]*graph.Node, 0, 50)
		for i := range 50 {
			path := fmt.Sprintf("%s/svc_%02d/README.md", repo, i)
			nodes = append(nodes, &graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, RepoPrefix: repo,
				WorkspaceID: "ws", ProjectID: "p", Language: "markdown"})
		}
		return nodes
	}
	require.NoError(t, store.AddBatchChecked(append(readmes("a"),
		&graph.Node{ID: "README.md", Kind: graph.KindFile, FilePath: "README.md", WorkspaceID: "ws", ProjectID: "p", Language: "markdown"}), nil))
	tracked := &findFilesProjectionStore{Store: store}
	srv, _ := overlaySpineFixture(t)
	srv.graph = tracked
	srv.scopeWorkspace = "ws"
	installContractCoreKindTestRuntime(t, srv)
	require.Implements(t, (*graph.ScopedFileLocationSequencer)(nil), srv.readerFor(t.Context()))
	read := func() (string, int) {
		t.Helper()
		tracked.projectedRepos, tracked.locationReads, tracked.locationRows = nil, 0, 0
		res, err := srv.handleFindFiles(t.Context(), makeReq("find_files", map[string]any{"query": "README.md", "repo": "a", "limit": 500}))
		require.NoError(t, err)
		require.False(t, res.IsError)
		require.Zero(t, tracked.globalReads, "a scoped find_files read the whole store")
		require.Zero(t, tracked.edgeReads, "a scoped find_files read every edge")
		require.Zero(t, tracked.fullReads)
		require.Zero(t, tracked.summaryReads, "no project narrows the scope, so file locations suffice")
		require.Equal(t, 1, tracked.locationReads)
		require.Equal(t, []string{"", "a"}, tracked.projectedRepos)
		return res.Content[0].(mcplib.TextContent).Text, tracked.locationRows
	}
	baseline, baselineRows := read()
	var out findFilesResult
	require.NoError(t, json.Unmarshal([]byte(baseline), &out))
	require.Len(t, out.Files, 51)
	require.Equal(t, 51, baselineRows, "the requested repository's files and the unowned one")

	for _, repo := range []string{"b", "c", "d", "e", "f"} {
		require.NoError(t, store.AddBatchChecked(readmes(repo), nil))
	}
	payload, rows := read()
	require.JSONEq(t, baseline, payload, "unrelated repositories changed the scoped answer")
	require.Equal(t, baselineRows, rows, "unrelated repositories changed the rows the location read yielded")
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
