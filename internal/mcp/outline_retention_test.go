package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// Exercise real CWD routing and tool middleware, not an injected request reader
// or hotspot list. Primary/dedicated CWDs read generation zero; automatic linked
// checkouts compose layers and must preserve the selected-reader fallback.
func TestOutlineRepeatedPrimaryCWDUsesGenerationAndRetainedHotspots(t *testing.T) {
	stack := newViewStack(t)
	s := stack.srv
	t.Cleanup(s.DrainBackground)
	// The routing fixture alone has too few connected callables to form a
	// detected community. Seed one real population so the proof exercises
	// SQL membership, rather than the empty-generation shortcut.
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := 0; i < 80; i++ {
		id := fmt.Sprintf("repo/analysis.go::Handler%03d", i)
		nodes = append(nodes, &graph.Node{ID: id, Name: fmt.Sprintf("Handler%03d", i), Kind: graph.KindFunction,
			RepoPrefix: "repo", WorkspaceID: "main-ws", ProjectID: "repo", FilePath: "repo/analysis.go", Language: "go"})
		if i > 0 {
			edges = append(edges, &graph.Edge{From: nodes[i-1].ID, To: id, Kind: graph.EdgeCalls})
		}
	}
	stack.store.AddBatch(nodes, edges)
	counted := &outlineCountingReader{Store: stack.store}
	s.graph = counted
	installContractCoreKindTestRuntime(t, s)
	s.analysisMu.Lock()
	metrics := s.populateAnalysisLocked()
	s.analysisMu.Unlock()
	require.NoError(t, metrics.cacheSaveErr)
	require.Positive(t, s.analysisGeneration.CommunityCount, "the fixture must exercise published community rows")
	require.True(t, s.releaseTransientAnalysisIfIdle())
	var builds atomic.Int32
	s.hotspotsFn = func(reader graph.Reader, communities *analysis.CommunityResult, threshold float64) []analysis.HotspotEntry {
		builds.Add(1)
		return analysis.FindHotspots(reader, communities, threshold)
	}
	var first string
	for i := 0; i < 3; i++ {
		assertNoMaterializedAnalysis(t, s)
		result, err := stack.callWithView(t, stack.repoRoot, "get_repo_outline", map[string]any{"format": "json"},
			func(ctx context.Context) (*mcplib.CallToolResult, error) {
				reader := s.readerFor(ctx)
				_, wrapped := reader.(contractCoreWrapped)
				require.True(t, wrapped, "use the daemon's production contract-core reader")
				view := requestViewFromContext(ctx)
				require.False(t, view.routed(), "a primary CWD must keep the indexed corpus")
				_, bound := s.sessionScopeOptions(ctx)
				require.True(t, bound)
				opts, _ := s.scopeOptionsWithRepoNarrow(ctx, repoAllowFromContext(ctx))
				_, supported, err := s.outlineGenerationCommunities(ctx, reader, graph.NodeScope{
					WorkspaceID: opts.WorkspaceID, ProjectID: opts.ProjectID, RepoAllow: opts.RepoAllow,
				})
				require.NoError(t, err)
				require.True(t, supported, "the actual primary CWD must reach the generation query")
				return s.handleGetRepoOutline(ctx, makeReq("get_repo_outline", map[string]any{"format": "json"}))
			})
		require.NoError(t, err)
		require.False(t, result.IsError)
		got := viewResultText(t, result)
		if i == 0 {
			first = got
		} else {
			require.JSONEq(t, first, got)
		}
		require.EqualValues(t, 1, builds.Load(), "transient release must not rebuild the eligible exact ranking")
		assertNoMaterializedAnalysis(t, s)
		require.True(t, s.hotspotsReady)
	}
	require.Positive(t, counted.communityQueries)
	before := counted.communityQueries
	result, err := stack.callWithView(t, stack.worktreeRoot, "get_repo_outline", map[string]any{"format": "json"},
		func(ctx context.Context) (*mcplib.CallToolResult, error) {
			_, supported, err := s.outlineGenerationCommunities(ctx, s.readerFor(ctx), graph.NodeScope{})
			require.NoError(t, err)
			require.False(t, supported, "automatic checkout composition must retain membership lookup fallback")
			return s.handleGetRepoOutline(ctx, makeReq("get_repo_outline", map[string]any{"format": "json"}))
		})
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Equal(t, before, counted.communityQueries)
	assertNoMaterializedAnalysis(t, s)
	// A real node mutation changes the token even if counts stay the same.
	node := stack.store.GetNode("repo/edit.go::Old")
	require.NotNil(t, node)
	node.Name = "Renamed"
	stack.store.AddNode(node)
	require.Nil(t, s.getHotspots(), "a retained ranking must fail closed after same-count payload mutation")
}

func TestGetHotspotsRetainedRealRankingCoalescesAfterRelease(t *testing.T) {
	store, _ := buildAnalysisCacheTestGraph(t, 80)
	defer store.Close()
	s, metrics := populateAnalysisForTest(store)
	require.NoError(t, metrics.cacheSaveErr)
	var builds atomic.Int32
	s.hotspotsFn = func(reader graph.Reader, communities *analysis.CommunityResult, threshold float64) []analysis.HotspotEntry {
		builds.Add(1)
		return analysis.FindHotspots(reader, communities, threshold)
	}
	want := s.getHotspots()
	require.True(t, s.releaseTransientAnalysisIfIdle())
	assertNoMaterializedAnalysis(t, s)
	var wg sync.WaitGroup
	results := make(chan []analysis.HotspotEntry, 24)
	for range 24 {
		wg.Go(func() { results <- s.getHotspots() })
	}
	wg.Wait()
	close(results)
	for got := range results {
		require.Equal(t, want, got)
	}
	require.EqualValues(t, 1, builds.Load())
	assertNoMaterializedAnalysis(t, s)
}

func TestGetHotspotsOversizeResultIsCompleteAndRebuiltAfterRelease(t *testing.T) {
	store, _ := buildAnalysisCacheTestGraph(t, 80)
	defer store.Close()
	var edges []*graph.Edge
	for i := 1; i < 80; i++ {
		edges = append(edges, &graph.Edge{From: fmt.Sprintf("repo::pkg%d::HandleRequest%d", i/20, i), To: "repo::pkg0::HandleRequest0", Kind: graph.EdgeCalls})
	}
	store.AddBatch(nil, edges)
	s, metrics := populateAnalysisForTest(store)
	require.NoError(t, metrics.cacheSaveErr)
	var builds int
	s.hotspotsFn = func(reader graph.Reader, communities *analysis.CommunityResult, threshold float64) []analysis.HotspotEntry {
		builds++
		// Keep the real algorithm's IDs/order/scores, but give every result
		// enough display bytes to exercise the oversize lifetime policy.
		entries := analysis.FindHotspots(reader, communities, threshold)
		for i := range entries {
			entries[i].Name = strings.Repeat("x", hotspotResultRetentionBytes)
		}
		return entries
	}
	first := s.getHotspots()
	require.NotEmpty(t, first)
	require.False(t, hotspotResultRetainable(first))
	s.releaseTransientAnalysisIfIdle()
	require.False(t, s.hotspotsReady)
	require.Nil(t, s.hotspots)
	second := s.getHotspots()
	require.Equal(t, first, second, "the complete ranking must survive for each caller, without truncation")
	require.Equal(t, 2, builds)
}

func TestGetHotspotsTransientReleaseDuringRealBuildPreservesPartition(t *testing.T) {
	store, _ := buildAnalysisCacheTestGraph(t, 80)
	defer store.Close()
	s, metrics := populateAnalysisForTest(store)
	require.NoError(t, metrics.cacheSaveErr)
	want := analysis.FindHotspots(store, s.getCommunities(), 0)
	var builds int
	s.hotspotsFn = func(reader graph.Reader, communities *analysis.CommunityResult, threshold float64) []analysis.HotspotEntry {
		builds++
		require.NotNil(t, communities)
		require.NotEmpty(t, communities.NodeToComm)
		// Existing build seam deterministically models another request's
		// release after this build took its immutable partition snapshot.
		require.True(t, s.releaseTransientAnalysisIfIdle())
		assertNoMaterializedAnalysis(t, s)
		return analysis.FindHotspots(reader, communities, threshold)
	}
	require.Equal(t, want, s.getHotspots())
	s.releaseTransientAnalysisIfIdle()
	assertNoMaterializedAnalysis(t, s)
	require.Equal(t, want, s.getHotspots())
	require.Equal(t, 1, builds)
}
