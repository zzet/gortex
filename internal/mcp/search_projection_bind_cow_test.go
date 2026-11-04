package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// A second registered reader can replace a physically checked same-byte owner
// before the first search binds it. This is deliberately before capture seals,
// unlike the completed-leaf interleaving parent. No stage is inferred for a
// retained native-run refusal from this controlled lifecycle witness.
func TestRegisteredNativeNameSearchBindsCheckedCOWDuringSelection(t *testing.T) {
	s, backend, base, path, original := snapshotFacadeFixture(t)
	s.engine.SetSearchProvider(s.indexer.Search)
	require.Implements(t, (*graph.FoldedNameCandidateReader)(nil), backend)
	nativeCount, err := backend.SymbolFTSCount()
	require.NoError(t, err)
	require.Positive(t, nativeCount)
	before := snapshotPhysicalTarget(t, path)
	oldFacts, err := s.bindPendingSearch(base)
	require.NoError(t, err)
	require.NoError(t, validateRequestProjections(oldFacts))

	const peerSession = "native-name-bind-peer"
	require.NoError(t, s.ConnectSession(peerSession, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(peerSession) })
	peer := s.WithClientSession(WithSessionID(t.Context(), peerSession), peerSession)
	s.NoteSessionToolPolicy(peerSession, FacadeSurfaceVersion, "hide")
	require.NotNil(t, s.MCPServer().HandleMessage(peer, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"gortex-load-peer","version":"1"}}}`)))
	s.MCPServer().HandleMessage(peer, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))

	var observed *projectionEntry
	var peerResult *mcplib.CallToolResult
	var peerErr, originalContextErr error
	var callbackCount int
	var remaining time.Duration
	observe := checkedProjectionReadObserver(func(ctx context.Context, expected *projectionEntry) {
		callbackCount++
		if callbackCount != 1 {
			return
		}
		observed = expected
		peerResult, peerErr = externalVersionRegisteredCall(s, peer, 30, "read", map[string]any{
			"operation": "source", "target": map[string]any{"symbol": "main.go::Target"},
			"context": map[string]any{"context_lines": 0}, "output": map[string]any{"format": "json"},
		})
		originalContextErr = ctx.Err()
		if deadline, ok := ctx.Deadline(); ok {
			remaining = time.Until(deadline)
		}
	})
	leaf, ok := s.facades.legacy("search_symbols")
	require.True(t, ok)
	s.facades.capture(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		return leaf.handler(context.WithValue(ctx, checkedProjectionReadObserverKey{}, observe), req)
	})
	pendingFileProjections.markSuspect(path)
	require.ErrorIs(t, validateRequestProjections(oldFacts), projectionSuperseded)
	args := snapshotSearchArgs()
	args["query"] = "name:Target"
	result, callErr := externalVersionRegisteredCall(s, base, 20, "search", args)
	leafJSON, marshalErr := json.Marshal(result)
	peerJSON, peerMarshalErr := json.Marshal(peerResult)
	current := liveEntry(pendingFileProjections, path)
	after := snapshotPhysicalTarget(t, path)
	t.Logf("FTS=%d observer_calls=%d original_context_error=%v remaining_original_budget=%s peer_json=%s search_json=%s search_error=%v", nativeCount, callbackCount, originalContextErr, remaining, peerJSON, leafJSON, callErr)
	// Validate the actual cause/premise before the intended completion oracle.
	require.NoError(t, marshalErr)
	require.NoError(t, peerMarshalErr)
	require.Positive(t, callbackCount)
	require.Same(t, original, observed)
	require.NoError(t, peerErr)
	require.NotNil(t, peerResult)
	require.False(t, peerResult.IsError, resultText(peerResult))
	require.NoError(t, originalContextErr, "an expired allowance is a different refusal")
	require.Positive(t, remaining, "the original150ms budget must not be reset")
	require.NotNil(t, current)
	require.NotSame(t, original, current)
	require.Same(t, original.layer, current.layer)
	require.Equal(t, original.rawHash, current.rawHash)
	require.Equal(t, original.contentSHA256, current.contentSHA256)
	require.Equal(t, original.sourceCommitSeq, current.sourceCommitSeq)
	require.Zero(t, current.commitSeq)
	require.True(t, current.readVersionChecked)
	require.True(t, original.suspect.Load())
	require.Equal(t, before, after)
	require.Nil(t, backend.GetNode("main.go::Target"), "canonical publication remains held")
	require.ErrorIs(t, validateRequestProjections(oldFacts), projectionSuperseded)
	require.NoError(t, callErr)
	snapshotTargetRows(t, result)
}
