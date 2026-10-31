package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestOutlineSummaryPreparationHasFixedScopesAndNoResponseWarmup(t *testing.T) {
	v := newViewStack(t)
	t.Cleanup(v.srv.DrainBackground)
	installContractCoreKindTestRuntime(t, v.srv)
	scopes := v.srv.knownOutlineScopes()
	require.NotEmpty(t, scopes)
	require.Equal(t, scopes, v.srv.knownOutlineScopes(), "capture is deterministic and independent of session/project state")
	for _, scope := range scopes {
		require.Empty(t, scope.ProjectID)
	}
	v.srv.RunAnalysis()
	require.Empty(t, v.srv.outlineResponses.keys, "explicit analysis prepares aggregates, never an outline response")
	require.Len(t, v.srv.outlineResponses.summaryKeys, len(scopes))
	for _, scope := range scopes {
		key, ok := v.srv.outlineCacheKey(v.srv.readerFor(t.Context()), scope, true)
		require.True(t, ok)
		prepared, ok := v.srv.outlineResponses.getSummary(key)
		require.True(t, ok)
		exact, supported, err := scopedOutlineCountsFor(v.srv.readerFor(t.Context()), scope)
		require.True(t, supported)
		require.NoError(t, err)
		require.Equal(t, exact.TotalNodes, prepared.counts.TotalNodes)
		require.Equal(t, exact.TotalEdges, prepared.counts.TotalEdges)
		require.Equal(t, exact.ByLanguage, prepared.counts.ByLanguage)
		counts := make(map[string]int)
		for _, row := range exact.FileImports {
			counts[row.FilePath] = row.Count
		}
		require.Equal(t, rankImportedFiles(counts, outlineMostImportedLimit), prepared.imports)
		require.NotNil(t, prepared.communities)
	}
	// A cancelled caller or active edit lane does no aggregate SQL work and
	// schedules no background retry. The analysis snapshot itself stays valid.
	v.srv.outlineResponses.clear()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	require.Zero(t, v.srv.warmOutlineSummaries(cancelled).ready)
	v.srv.DrainBackground()
	previous := editLaneBusyForTest
	editLaneBusyForTest = func() bool { return true }
	t.Cleanup(func() { editLaneBusyForTest = previous })
	require.Zero(t, v.srv.warmOutlineSummaries(t.Context()).ready)
	editLaneBusyForTest = previous
	require.Empty(t, v.srv.outlineResponses.summaryKeys)
	require.Positive(t, v.srv.warmOutlineSummaries(t.Context()).ready)
	// A real same-store mutation invalidates every prepared scope. Requests may
	// compute an exact response, but must not rebuild/queue aggregate preparation.
	v.store.AddNode(&graph.Node{ID: "repo/new", Name: "new", Kind: graph.KindFunction, RepoPrefix: "repo", WorkspaceID: "main-ws", FilePath: "repo/new.go"})
	for _, scope := range scopes {
		key, ok := v.srv.outlineCacheKey(v.srv.readerFor(t.Context()), scope, true)
		require.True(t, ok)
		_, ready := v.srv.outlineResponses.getSummary(key)
		require.False(t, ready)
	}
	require.Zero(t, v.srv.warmOutlineSummaries(context.Background()).ready, "stale analysis cannot prepare scopes")
	v.srv.InvalidateSessionScopes()
	require.Empty(t, v.srv.outlineResponses.summaryKeys)
}

func TestOutlinePreparedSectionsOwnAndBoundExactFinalRankings(t *testing.T) {
	cache := outlineResponseCache{}
	counts := graph.OutlineCounts{TotalNodes: 100, TotalEdges: 99, ByLanguage: map[string]int{"go": 100}}
	for i := 0; i < 100; i++ {
		counts.FileImports = append(counts.FileImports, graph.FileImportCountRow{FilePath: fmt.Sprintf("repo/%03d.go", i), Count: i / 2})
	}
	community := map[string]any{"count": 2, "modularity": .5, "top": []map[string]any{{"label": "original"}}}
	key := outlineResponseKey{scope: "ready"}
	bytes, ok := cache.putSummary(key, counts, community)
	require.True(t, ok)
	require.LessOrEqual(t, bytes, outlineCachePayloadBytes)
	counts.ByLanguage["go"] = 0
	community["top"].([]map[string]any)[0]["label"] = "mutated"
	first, ok := cache.getSummary(key)
	require.True(t, ok)
	require.Equal(t, 100, first.counts.ByLanguage["go"])
	require.Len(t, first.imports, outlineMostImportedLimit)
	require.Equal(t, "repo/098.go", first.imports[0]["path"], "equal counts keep ascending canonical path order")
	require.Empty(t, first.counts.FileImports, "retain the exact final ranking, not all candidate paths")
	first.imports[0]["path"] = "changed"
	first.communities["top"].([]map[string]any)[0]["label"] = "changed"
	second, ok := cache.getSummary(key)
	require.True(t, ok)
	require.Equal(t, "repo/098.go", second.imports[0]["path"])
	require.Equal(t, "original", second.communities["top"].([]map[string]any)[0]["label"])
	_, ok = cache.putSummary(outlineResponseKey{scope: "too-large"}, graph.OutlineCounts{ByLanguage: map[string]int{strings.Repeat("x", outlineCachePayloadBytes): 1}}, nil)
	require.False(t, ok)
	// A delayed old epoch cannot republish after either a fresh revision or a
	// scope reset; this is the same guard used by response publication.
	fresh := outlineResponseKey{scope: "fresh", revision: 2, epoch: 2}
	_, ok = cache.putSummary(fresh, graph.OutlineCounts{}, nil)
	require.True(t, ok)
	_, ok = cache.putSummary(key, graph.OutlineCounts{}, nil)
	require.False(t, ok)
	require.Len(t, cache.summaryKeys, 1)
	cache.clear()
	_, ok = cache.putSummary(fresh, graph.OutlineCounts{}, nil)
	require.False(t, ok)
}

func TestOutlineSummaryOwnsSmallSubstringsFromLargeCandidates(t *testing.T) {
	source := strings.Repeat("q", 1<<20)
	path := source[100:120]
	language := source[200:203]
	counts := graph.OutlineCounts{ByLanguage: map[string]int{language: 1}, FileImports: []graph.FileImportCountRow{{FilePath: path, Count: 1}}}
	cache := outlineResponseCache{}
	_, ok := cache.putSummary(outlineResponseKey{}, counts, nil)
	require.True(t, ok)
	retained := cache.summaries[outlineResponseKey{}]
	for key := range retained.counts.ByLanguage {
		require.Equal(t, language, key)
		require.True(t, unsafe.StringData(language) != unsafe.StringData(key))
	}
	require.Equal(t, path, retained.imports[0]["path"])
	require.True(t, unsafe.StringData(path) != unsafe.StringData(retained.imports[0]["path"].(string)))
}
