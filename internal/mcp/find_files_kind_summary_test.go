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

// Without a project in scope, find_files reads file locations and must answer
// exactly as full rows do. Fourteen hits tie on score, depth and
// repository-relative path; the twelve repository ties have descending IDs
// but ascending languages, so the covering index order is not the ID order
// the unstable sort's tie outcome depends on.
func TestFindFilesLocationPathMatchesFullRowFallback(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "locations.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	var nodes []*graph.Node
	for i := 0; i < 300; i++ {
		for _, repo := range []string{"a", "b", ""} {
			path := fmt.Sprintf("pkg/dir_%d/file_%03d.go", i%4, i)
			if repo != "" {
				path = repo + "/" + path
			}
			nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%s-row-%03d", repo, 299-i), Kind: graph.KindFile,
				FilePath: path, RepoPrefix: repo, WorkspaceID: "ws", ProjectID: "p", Language: fmt.Sprintf("go-%d", i%3)})
		}
	}
	for i := 0; i < 12; i++ {
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("tie-%02d", 11-i), Kind: graph.KindFile, FilePath: "a/docs/README.md",
			RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: fmt.Sprintf("markdown-%02d", i)})
	}
	nodes = append(nodes,
		&graph.Node{ID: "tie-unowned", Kind: graph.KindFile, FilePath: "docs/README.md", WorkspaceID: "ws", ProjectID: "p", Language: "text"},
		&graph.Node{ID: "tie-wrong-project", Kind: graph.KindFile, FilePath: "docs/README.md", WorkspaceID: "ws", ProjectID: "other"},
		&graph.Node{ID: "wrong-workspace", Kind: graph.KindFile, FilePath: "docs/README.md", WorkspaceID: "other", ProjectID: "p"},
		&graph.Node{ID: "b-docs", Kind: graph.KindFile, FilePath: "b/docs/README.md", RepoPrefix: "b", WorkspaceID: "ws", ProjectID: "p"},
		&graph.Node{ID: "a/docs/guide/README.md", Kind: graph.KindFile, FilePath: "a/docs/guide/README.md", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "markdown"},
		&graph.Node{ID: "a/docs/README.md::Heading", Kind: graph.KindFunction, FilePath: "a/docs/README.md", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p"},
	)
	require.NoError(t, store.AddBatchChecked(nodes, nil))
	tracked := &findFilesProjectionStore{Store: store}
	full := findFilesFullProjectionStore{Store: store, selected: store}
	srv, _ := overlaySpineFixture(t)
	srv.scopeWorkspace = "ws"
	installContractCoreKindTestRuntime(t, srv)
	read := func(ctx context.Context, args map[string]any) string {
		t.Helper()
		result, err := srv.handleFindFiles(ctx, makeReq("find_files", args))
		require.NoError(t, err)
		require.False(t, result.IsError)
		return result.Content[0].(mcplib.TextContent).Text
	}
	ids := func(text string) []string {
		t.Helper()
		var out findFilesResult
		require.NoError(t, json.Unmarshal([]byte(text), &out))
		got := make([]string, 0, len(out.Files))
		for _, file := range out.Files {
			got = append(got, file.ID)
		}
		return got
	}
	for _, args := range []map[string]any{
		{"query": "README.md", "repo": "a", "limit": 5},
		{"query": "README.md", "repo": "a", "limit": 13},
		{"query": "README", "repo": "a", "limit": 500},
		{"glob": "**/README.md", "repo": "a", "limit": 12},
		{"query": "file_1", "repo": "a", "path": "pkg/dir_1", "limit": 7},
		{"query": "fl", "fuzzy": true, "repo": "a", "glob": "pkg/**/*.go", "limit": 9},
	} {
		srv.graph = tracked
		locations, summaries := tracked.locationReads, tracked.summaryReads
		got := read(context.Background(), args)
		require.Equal(t, locations+1, tracked.locationReads, "%v", args)
		require.Equal(t, summaries, tracked.summaryReads)
		require.Zero(t, tracked.fullReads)
		require.Zero(t, tracked.globalReads)
		srv.graph = full
		require.Equal(t, read(context.Background(), args), got, "%v: location rows must answer as full rows do", args)
	}
	srv.graph = tracked
	all := ids(read(context.Background(), map[string]any{"query": "README.md", "repo": "a", "limit": 500}))
	require.Len(t, all, 15, "twelve repository ties, the unowned tie, the other-project tie and the deeper guide")
	require.Equal(t, "a/docs/guide/README.md", all[14])
	require.Contains(t, all, "tie-wrong-project", "no project in scope admits another project's row")
	require.NotContains(t, all, "wrong-workspace")
	require.NotContains(t, all, "b-docs")

	// A project narrows the scope check by a column location rows lack, so
	// the call keeps the summary read and still drops the other project.
	srv.scopeProject = "p"
	locations, summaries := tracked.locationReads, tracked.summaryReads
	args := map[string]any{"query": "README.md", "repo": "a", "limit": 500}
	scoped := read(context.Background(), args)
	require.Equal(t, locations, tracked.locationReads)
	require.Equal(t, summaries+1, tracked.summaryReads)
	require.NotContains(t, ids(scoped), "tie-wrong-project")
	require.Len(t, ids(scoped), 14)
	srv.graph = full
	require.Equal(t, read(context.Background(), args), scoped)
	srv.scopeProject = ""

	// An overlay masks and replaces base files, so it never offers the
	// physical location read.
	srv.graph = tracked
	layer := graph.NewOverlayLayer()
	layer.MarkFile("a/docs/guide/README.md", true)
	layer.MarkFile("a/next/README.md", false)
	layer.AddNode("a/next/README.md", &graph.Node{ID: "a/next/README.md", Kind: graph.KindFile, FilePath: "a/next/README.md", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "overlay"})
	ctx := overlayCtx(t, srv, layer)
	_, exposed := srv.readerFor(ctx).(graph.ScopedFileLocationSequencer)
	require.False(t, exposed, "never reach through an overlay to the physical location read")
	locations = tracked.locationReads
	overlaid := ids(read(ctx, args))
	require.Equal(t, locations, tracked.locationReads)
	require.Contains(t, overlaid, "a/next/README.md")
	require.NotContains(t, overlaid, "a/docs/guide/README.md", "the overlay's deletion mask still applies")
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
		require.Implements(t, (*graph.ScopedFileLocationSequencer)(nil), reader, "file locations ride on the summary capability")
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
	_, locations := hidden.(graph.ScopedFileLocationSequencer)
	require.False(t, locations, "no summary capability, no file locations")
	// DeltaWriter now composes compact summaries. The optional capability must
	// retain its selected full-row fallback and both live-layer deletion masks
	// and additions, without exposing physical edge traversal through the wrapper.
	require.NoError(t, store.AddBatchChecked([]*graph.Node{
		{ID: "a/kept.go", Kind: graph.KindFile, FilePath: "a/kept.go", RepoPrefix: "a", Language: "go"},
		{ID: "a/removed.go", Kind: graph.KindFile, FilePath: "a/removed.go", RepoPrefix: "a", Language: "go"},
		{ID: "b/foreign.go", Kind: graph.KindFile, FilePath: "b/foreign.go", RepoPrefix: "b", Language: "go"},
		{ID: "a/kept.go::fn", Kind: graph.KindFunction, FilePath: "a/kept.go", RepoPrefix: "a"},
	}, nil))
	locationIDs := func(reader graph.Reader) []string {
		t.Helper()
		locations, ok := reader.(graph.ScopedFileLocationSequencer)
		require.True(t, ok)
		var got []string
		for node := range locations.FileLocationsInScopeSeq([]string{"a"}) {
			got = append(got, node.ID+"|"+node.Language)
		}
		return got
	}
	// A selected summary reader without its own location read serves the
	// wrapper's file locations from its summary rows.
	require.Equal(t, []string{"a/kept.go|go", "a/removed.go|go"}, locationIDs(newContractCoreEdges(base, context.Background(), nil)))
	fullOnly := struct {
		graph.Reader
		graph.ScopedProjectionSequencer
	}{store, store}
	for _, selected := range []graph.Reader{store, fullOnly} {
		delta := graph.NewDeltaWriter(selected, nil)
		delta.EvictFiles([]string{"a/removed.go"})
		delta.AddNode(&graph.Node{ID: "a/added.go", Kind: graph.KindFile, FilePath: "a/added.go", RepoPrefix: "a", Language: "python"})
		wrapped := newContractCoreEdges(delta, context.Background(), nil)
		summary, ok := wrapped.(graph.ScopedKindSummarySequencer)
		require.True(t, ok)
		var got []string
		for node := range summary.NodesLightByKindsInScopeSeq([]string{"a"}, nil, graph.KindFile) {
			got = append(got, node.ID+"|"+node.Language)
		}
		require.Equal(t, []string{"a/added.go|python", "a/kept.go|go"}, got)
		_, physical := any(delta).(graph.ScopedFileLocationSequencer)
		require.False(t, physical, "a masking delta never offers the physical location read")
		require.Equal(t, got, locationIDs(wrapped), "wrapped file locations keep the delta's masks and additions")
		_, edges := wrapped.(graph.ScopedProjectionSequencer)
		require.False(t, edges, "composed summaries must not expose physical edge traversal")
	}
}
