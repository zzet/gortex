package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestOutlineCacheProofUsesMutationScopeAndAnalysisIdentity(t *testing.T) {
	s, store := outlineFixture(t)
	scope := graph.NodeScope{WorkspaceID: "ws", ProjectID: "p", RepoAllow: map[string]bool{"a": true, "b": false}}
	first, ok := s.outlineCacheKey(store, scope, true)
	require.True(t, ok)
	reorder := scope
	reorder.RepoAllow = map[string]bool{"b": false, "a": true}
	same, ok := s.outlineCacheKey(store, reorder, true)
	require.True(t, ok)
	require.Equal(t, first, same)
	for _, other := range []graph.NodeScope{
		{WorkspaceID: "other", ProjectID: "p", RepoAllow: scope.RepoAllow},
		{WorkspaceID: "ws", ProjectID: "other", RepoAllow: scope.RepoAllow},
		{WorkspaceID: "ws", ProjectID: "p", RepoAllow: map[string]bool{"b": true}},
		{WorkspaceID: "ws", ProjectID: "p", RepoAllow: map[string]bool{"a": false}},
	} {
		key, ok := s.outlineCacheKey(store, other, true)
		require.True(t, ok)
		require.NotEqual(t, first, key)
	}
	// An unchanged row count and unchanged edge count do not prove freshness.
	n := store.GetNode("a-main")
	n.Name = "renamed"
	store.AddNode(n)
	metadata, ok := s.outlineCacheKey(store, scope, true)
	require.True(t, ok)
	require.NotEqual(t, first, metadata)
	s.analysisMu.Lock()
	s.analysisEpoch++
	s.analysisMu.Unlock()
	epoch, ok := s.outlineCacheKey(store, scope, true)
	require.True(t, ok)
	require.NotEqual(t, metadata, epoch)
	s.InvalidateSessionScopes()
	retrack, ok := s.outlineCacheKey(store, scope, true)
	require.True(t, ok)
	require.NotEqual(t, epoch, retrack)
	// Unknown revision domains and composed visibility never inherit a base key.
	for _, r := range []graph.Reader{legacyOutlineReader{store}, graph.New(), graph.NewOverlaidView(store, graph.NewOverlayLayer())} {
		_, ok := s.outlineCacheKey(r, scope, true)
		require.False(t, ok)
	}
	_, ok = s.outlineCacheKey(store, scope, false)
	require.False(t, ok)
	for _, ids := range []map[string]bool{nil, {}, {"a-main": true}} {
		key, ok := s.outlineCacheKey(newContractCoreEdges(store, t.Context(), ids), scope, true)
		require.True(t, ok)
		require.NotEqual(t, retrack, key)
	}
	// Retargeting even to another generation of the same physical store cannot
	// reuse a result from the old payload handle.
	view, ok := s.outlineCacheKey(store.AtGeneration(7), scope, true)
	require.True(t, ok)
	require.NotEqual(t, retrack, view)
}

func TestOutlineCacheOwnsAndBoundsPayloads(t *testing.T) {
	c := outlineResponseCache{}
	key := outlineResponseKey{scope: "a"}
	payload := map[string]any{
		"summary":  map[string]any{"languages": []outlineLanguage{{Name: "go", Nodes: 3}}},
		"hotspots": []map[string]any{{"name": "before"}},
	}
	c.put(key, payload)
	payload["hotspots"].([]map[string]any)[0]["name"] = "outside mutation"
	first, ok := c.get(key)
	require.True(t, ok)
	require.Equal(t, "before", first["hotspots"].([]map[string]any)[0]["name"])
	first["summary"].(map[string]any)["languages"].([]outlineLanguage)[0].Name = "changed"
	first["hotspots"].([]map[string]any)[0]["name"] = "response decoration"
	second, ok := c.get(key)
	require.True(t, ok)
	require.Equal(t, "before", second["hotspots"].([]map[string]any)[0]["name"])
	require.Equal(t, "go", second["summary"].(map[string]any)["languages"].([]outlineLanguage)[0].Name)
	for i := 0; i < outlineCacheEntries+3; i++ {
		c.put(outlineResponseKey{scope: fmt.Sprint(i)}, payload)
	}
	require.Len(t, c.keys, outlineCacheEntries)
	require.Len(t, c.values, outlineCacheEntries)
	c.put(outlineResponseKey{scope: "oversized"}, map[string]any{"name": strings.Repeat("x", outlineCachePayloadBytes)})
	_, ok = c.get(outlineResponseKey{scope: "oversized"})
	require.False(t, ok)
	c.put(outlineResponseKey{revision: 1, scope: "next"}, payload)
	require.Len(t, c.keys, 1, "old revision history must be dropped")
	c.clear()
	c.put(key, payload)
	require.Empty(t, c.keys, "a pre-untrack producer cannot republish after scope invalidation")
}

func TestOutlineCacheReformatsAndInvalidatesRealReplies(t *testing.T) {
	s, store := outlineFixture(t)
	ctx := outlineBoundContext(s, "cache", "ws", map[string]bool{"a": true})
	initial := outlineJSON(t, s, ctx, store)
	require.Len(t, s.outlineResponses.keys, 1)
	require.Equal(t, initial, outlineJSON(t, s, ctx, store))
	// Request-specific formatting is applied after fetching an owned payload.
	result, err := s.handleGetRepoOutline(ctx, makeReq("get_repo_outline", map[string]any{"format": "json", "fields": "summary"}))
	require.NoError(t, err)
	filtered := resultJSON(t, result)
	require.Contains(t, filtered, "summary")
	require.Empty(t, filtered["hotspots"].([]any)[0], "fields selects list-record fields while preserving the outline sections")
	require.Equal(t, initial, outlineJSON(t, s, ctx, store), "field selection must not mutate a cached payload")
	n := store.GetNode("a-main")
	n.Name = "noLongerMain"
	store.AddNode(n)
	updated := outlineJSON(t, s, ctx, store)
	require.NotEqual(t, initial, updated)
	require.Len(t, s.outlineResponses.keys, 1)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.handleGetRepoOutline(cancelled, makeReq("get_repo_outline", map[string]any{"format": "json"}))
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, s.outlineResponses.keys, 1)
	s.InvalidateSessionScopes()
	require.Empty(t, s.outlineResponses.keys)
}
