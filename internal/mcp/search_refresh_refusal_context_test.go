package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// An owner change and an exhausted child allowance can produce the same
// retained bind.refresh error. Preserve the refusal and identify only the
// context/outcome actually observed at that existing return site.
func TestRegisteredSearchRefreshRefusalIdentifiesExpiredChildContext(t *testing.T) {
	t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
	s, backend, base, path, original := snapshotFacadeFixture(t)
	s.engine.SetSearchProvider(s.indexer.Search)
	nativeCount, err := backend.SymbolFTSCount()
	require.NoError(t, err)
	require.Positive(t, nativeCount)
	physical := snapshotPhysicalTarget(t, path)
	oldFacts, err := s.bindPendingSearch(base)
	require.NoError(t, err)
	const peerID = "refresh-context-peer"
	require.NoError(t, s.ConnectSession(peerID, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(peerID) })
	peer := s.WithClientSession(WithSessionID(t.Context(), peerID), peerID)
	s.NoteSessionToolPolicy(peerID, FacadeSurfaceVersion, "hide")
	require.NotNil(t, s.MCPServer().HandleMessage(peer, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"refresh-context-peer","version":"1"}}}`)))
	s.MCPServer().HandleMessage(peer, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	var peerResult *mcplib.CallToolResult
	var peerErr error
	var checked context.Context
	var remaining time.Duration
	var release func()
	var worker sync.WaitGroup
	callbacks := 0
	t.Cleanup(func() {
		if release != nil {
			release()
		}
		worker.Wait()
	})
	observe := checkedProjectionReadObserver(func(ctx context.Context, expected *projectionEntry) {
		callbacks++
		checked = ctx
		peerResult, peerErr = externalVersionRegisteredCall(s, peer, 30, "read", map[string]any{
			"operation": "source", "target": map[string]any{"symbol": "main.go::Target"},
			"context": map[string]any{"context_lines": 0}, "output": map[string]any{"format": "json"},
		})
		if deadline, ok := ctx.Deadline(); ok {
			remaining = time.Until(deadline)
		}
		// The real replacement is already published. The existing successor
		// check waits on this registry mutex until its ORIGINAL child expires;
		// there is no fabricated error or extended/new budget.
		pendingFileProjections.mu.Lock()
		release = sync.OnceFunc(pendingFileProjections.mu.Unlock)
		worker.Add(1)
		go func() {
			defer worker.Done()
			<-ctx.Done()
			release()
		}()
	})
	leaf, ok := s.facades.legacy("search_symbols")
	require.True(t, ok)
	s.facades.capture(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		return leaf.handler(context.WithValue(ctx, checkedProjectionReadObserverKey{}, observe), req)
	})
	pendingFileProjections.markSuspect(path)
	args := snapshotSearchArgs()
	args["query"] = "name:Target"
	result, callErr := externalVersionRegisteredCall(s, base, 20, "search", args)
	worker.Wait()
	current := liveEntry(pendingFileProjections, path)
	wire, marshalErr := json.Marshal(result)
	require.NotNil(t, checked)
	t.Logf("native_FTS=%d observer_calls=%d original_remaining_before_mutex=%s actual_child_error=%v response=%s error=%v", nativeCount, callbacks, remaining, checked.Err(), wire, callErr)
	require.NoError(t, marshalErr)
	require.Equal(t, 1, callbacks)
	require.NoError(t, peerErr)
	require.NotNil(t, peerResult)
	require.False(t, peerResult.IsError, resultText(peerResult))
	require.Positive(t, remaining)
	require.ErrorIs(t, checked.Err(), context.DeadlineExceeded)
	require.NotNil(t, current)
	require.NotSame(t, original, current)
	require.Same(t, original.layer, current.layer)
	require.Equal(t, original.rawHash, current.rawHash)
	require.Equal(t, original.sourceCommitSeq, current.sourceCommitSeq)
	require.True(t, current.readVersionChecked)
	require.Zero(t, current.commitSeq)
	require.True(t, liveProjectionForAcknowledgement(current))
	require.Equal(t, physical, snapshotPhysicalTarget(t, path))
	require.ErrorIs(t, validateRequestProjections(oldFacts), projectionSuperseded)
	require.NoError(t, callErr)
	require.NotNil(t, result)
	require.True(t, result.IsError)
	require.Equal(t, "projection not published: superseded", resultText(result))
	require.NotNil(t, result.Meta)
	t.Log("REFRESH_CONTEXT_PREMISES_VERIFIED: actual registered COW, original child deadline expired, typed refusal unchanged")
	require.Equal(t, map[string]any{"stage": "bind.refresh/deadline/superseded_nil", "capture_state": "unsealed"}, result.Meta.AdditionalFields["gortex_search_refusal"])
}

type searchRefreshUnobservedContext struct{ context.Context }

func (searchRefreshUnobservedContext) Err() error {
	panic("disabled or non-refusal annotation must not inspect child context")
}

func TestSearchRefreshRefusalEvidenceKeepsDefaultAndClosedOutcomes(t *testing.T) {
	unobserved := searchRefreshUnobservedContext{context.Background()}
	t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "")
	require.True(t, annotateSearchRefreshRefusal(t.Context(), unobserved, nil, projectionSuperseded) == projectionSuperseded)
	t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
	require.Nil(t, annotateSearchRefreshRefusal(t.Context(), unobserved, nil, nil), "canonical nil view without error stays successful")
	require.Nil(t, annotateSearchRefreshRefusal(t.Context(), unobserved, &projectionEntry{}, nil))
	require.ErrorIs(t, annotateSearchRefreshRefusal(t.Context(), unobserved, nil, context.Canceled), context.Canceled)
	unknown := searchRefusalToolResult(annotateSearchRefreshRefusal(t.Context(), nil, nil, projectionSuperseded))
	require.Equal(t, map[string]any{"stage": "bind.refresh", "capture_state": "none"}, unknown.Meta.AdditionalFields["gortex_search_refusal"])
	expired, finishExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer finishExpired()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name  string
		child context.Context
	}{
		{name: "live", child: t.Context()},
		{name: "deadline", child: expired},
		{name: "cancelled", child: cancelled},
	} {
		for _, hasView := range []bool{false, true} {
			var view *projectionEntry
			outcome := "nil"
			if hasView {
				view = &projectionEntry{}
				outcome = "view"
			}
			marked := annotateSearchRefreshRefusal(t.Context(), tc.child, view, projectionSuperseded)
			require.ErrorIs(t, marked, projectionSuperseded)
			require.Equal(t, projectionSuperseded.Error(), marked.Error())
			result := searchRefusalToolResult(marked)
			require.Equal(t, map[string]any{"stage": "bind.refresh/" + tc.name + "/superseded_" + outcome, "capture_state": "none"}, result.Meta.AdditionalFields["gortex_search_refusal"])
			encoded, err := json.Marshal(result.Meta)
			require.NoError(t, err)
			require.LessOrEqual(t, len(encoded), 128)
		}
	}
}
