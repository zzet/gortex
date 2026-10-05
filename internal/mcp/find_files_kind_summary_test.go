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

// Hide only the new summary capability, retaining the existing scoped fallback.
type findFilesFullProjectionStore struct {
	graph.Store
	selected graph.ScopedNodeProjectionSequencer
}

func (s findFilesFullProjectionStore) NodesInScopeSeq(repos, files []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	return s.selected.NodesInScopeSeq(repos, files, kinds...)
}
func (s findFilesFullProjectionStore) NodesLightInScopeSeq(repos, files []string) iter.Seq[*graph.Node] {
	return s.selected.NodesLightInScopeSeq(repos, files)
}

func TestFindFilesKindSummaryPreservesPublicResultsAndSelectedFallback(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "summary.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	var nodes []*graph.Node
	for i := 0; i < 300; i++ {
		for _, repo := range []string{"a", "b", ""} {
			path := fmt.Sprintf("docs/README-%03d.md", i)
			if repo != "" {
				path = repo + "/" + path
			}
			nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%s-row-%03d", repo, i), Kind: graph.KindFile,
				FilePath: path, RepoPrefix: repo, WorkspaceID: "ws", ProjectID: "p", Language: "markdown",
				Meta: map[string]any{"doc": "retrieval metadata excluded by summary"}})
		}
	}
	nodes = append(nodes,
		&graph.Node{ID: "a/README.md", Kind: graph.KindFile, FilePath: "a/README.md", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "markdown"},
		&graph.Node{ID: "unowned", Kind: graph.KindFile, FilePath: "README.md", WorkspaceID: "ws", ProjectID: "p", Language: "text"},
		&graph.Node{ID: "wrong-project", Kind: graph.KindFile, FilePath: "README.md", WorkspaceID: "ws", ProjectID: "other"},
		&graph.Node{ID: "wrong-workspace", Kind: graph.KindFile, FilePath: "README.md", WorkspaceID: "other", ProjectID: "p"},
		&graph.Node{ID: "ordinary-code", Kind: graph.KindFunction, FilePath: "a/README.md", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p"},
	)
	require.NoError(t, store.AddBatchChecked(nodes, nil))
	tracked := &findFilesProjectionStore{Store: store}
	srv, _ := overlaySpineFixture(t)
	srv.scopeWorkspace, srv.scopeProject = "ws", "p"
	installContractCoreKindTestRuntime(t, srv)
	read := func(args map[string]any) map[string]any {
		result, err := srv.handleFindFiles(context.Background(), makeReq("find_files", args))
		require.NoError(t, err)
		require.False(t, result.IsError)
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &out))
		return out
	}
	for _, args := range []map[string]any{
		{"query": "README.md", "repo": "a", "limit": 1},
		{"query": "README", "repo": "a", "limit": 500},
		{"query": "README", "repo": "a", "path": "docs", "limit": 2},
		{"query": "rdm", "fuzzy": true, "repo": "a", "glob": "**/*.md", "limit": 3},
	} {
		srv.graph = tracked
		require.Implements(t, (*graph.ScopedKindSummarySequencer)(nil), srv.readerFor(context.Background()))
		before := tracked.summaryReads
		got := read(args)
		require.Equal(t, before+1, tracked.summaryReads)
		require.Zero(t, tracked.fullReads, "the primary summary path must not hydrate full scoped rows")
		require.Zero(t, tracked.globalReads)
		srv.graph = findFilesFullProjectionStore{Store: store, selected: store}
		_, optional := srv.readerFor(context.Background()).(graph.ScopedKindSummarySequencer)
		require.False(t, optional)
		require.Equal(t, read(args), got, "public ranking, order, language, scope and truncation must match full rows")
	}
	srv.graph = tracked
	limited := read(map[string]any{"query": "README", "repo": "a", "limit": 1})
	require.Equal(t, true, limited["truncated"])
	all := read(map[string]any{"query": "README.md", "repo": "a", "limit": 10})
	require.Equal(t, float64(2), all["count"])
	files := all["files"].([]any)
	require.Equal(t, "a/README.md", files[0].(map[string]any)["id"])
	require.Equal(t, "markdown", files[0].(map[string]any)["language"])
	require.Equal(t, "unowned", files[1].(map[string]any)["id"])
	require.Equal(t, "text", files[1].(map[string]any)["language"])

	// A selected overlay lacks the optional summary trait and must use its
	// existing composed projection, preserving replacement and deletion masks.
	layer := graph.NewOverlayLayer()
	layer.MarkFile("a/README.md", true)
	layer.MarkFile("a/next/README.md", false)
	layer.AddNode("a/next/README.md", &graph.Node{ID: "a/next/README.md", Kind: graph.KindFile, FilePath: "a/next/README.md", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "overlay"})
	ctx := overlayCtx(t, srv, layer)
	_, optional := srv.readerFor(ctx).(graph.ScopedKindSummarySequencer)
	require.False(t, optional, "never reach through an overlay to a physical base summary")
	before := tracked.summaryReads
	result, err := srv.handleFindFiles(ctx, makeReq("find_files", map[string]any{"query": "README.md", "repo": "a", "path": "next"}))
	require.NoError(t, err)
	out := decodeFindFiles(t, result)
	require.Equal(t, 1, out.Count)
	require.Equal(t, "a/next/README.md", out.Files[0].ID)
	require.Equal(t, "overlay", out.Files[0].Language)
	require.Equal(t, before, tracked.summaryReads)
}

func TestContractCoreKindSummaryCapabilityStaysConditional(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "traits.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	base := struct {
		graph.Reader
		graph.ScopedNodeProjectionSequencer
		graph.ScopedKindSummarySequencer
	}{store, store, store}
	filtered := struct {
		graph.Reader
		graph.ScopedNodeProjectionSequencer
		graph.ScopedKindSummarySequencer
		graph.FilteredContainingNameReader
	}{store, store, store, store}
	bounded := struct {
		graph.Reader
		graph.ScopedNodeProjectionSequencer
		graph.ScopedKindSummarySequencer
		graph.BoundedFileNodeReader
	}{store, store, store, store}
	for _, selected := range []graph.Reader{base, filtered, bounded, store} {
		reader := newContractCoreEdges(selected, context.Background(), nil)
		require.Implements(t, (*graph.ScopedKindSummarySequencer)(nil), reader)
		require.Implements(t, (*graph.ScopedNodeProjectionSequencer)(nil), reader)
		_, wantFiltered := selected.(graph.FilteredContainingNameReader)
		_, gotFiltered := reader.(graph.FilteredContainingNameReader)
		require.Equal(t, wantFiltered, gotFiltered)
		_, wantBounded := selected.(graph.BoundedFileNodeReader)
		_, gotBounded := reader.(graph.BoundedFileNodeReader)
		require.Equal(t, wantBounded, gotBounded)
		_, edges := reader.(graph.ScopedProjectionSequencer)
		require.False(t, edges, "summary forwarding must not expose physical edge traversal")
	}
	hidden := newContractCoreEdges(findFilesFullProjectionStore{Store: store, selected: store}, context.Background(), nil)
	_, summary := hidden.(graph.ScopedKindSummarySequencer)
	require.False(t, summary)
	_, delta := any((*graph.DeltaWriter)(nil)).(graph.ScopedKindSummarySequencer)
	require.False(t, delta, "a composed delta deliberately retains full-row fallback")
}
