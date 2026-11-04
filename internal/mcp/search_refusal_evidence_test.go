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

// The source transition remains a strict refusal: another physical edit is
// not the same-version checked COW exception. The opt-in evidence identifies
// this real registered refusal without changing its outcome or authority.
func TestRegisteredNativeSearchRefusalEvidenceIsOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "default_off"
		if enabled {
			name = "opt_in"
		}
		t.Run(name, func(t *testing.T) {
			flag := ""
			if enabled {
				flag = "1"
			}
			t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", flag)
			s, backend, base, path, original := snapshotFacadeFixture(t)
			s.engine.SetSearchProvider(s.indexer.Search)
			require.Implements(t, (*graph.FoldedNameCandidateReader)(nil), backend)
			count, err := backend.SymbolFTSCount()
			require.NoError(t, err)
			require.Positive(t, count)
			before := snapshotPhysicalTarget(t, path)
			const peerID = "search-refusal-evidence-peer"
			require.NoError(t, s.ConnectSession(peerID, func([]byte) error { return nil }))
			t.Cleanup(func() { s.DisconnectSession(peerID) })
			peer := s.WithClientSession(WithSessionID(t.Context(), peerID), peerID)
			s.NoteSessionToolPolicy(peerID, FacadeSurfaceVersion, "hide")
			require.NotNil(t, s.MCPServer().HandleMessage(peer, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"readback-peer","version":"1"}}}`)))
			s.MCPServer().HandleMessage(peer, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
			var observed *projectionEntry
			var peerReply *mcplib.CallToolResult
			var peerErr, originalErr error
			var remaining time.Duration
			calls := 0
			observer := checkedProjectionReadObserver(func(ctx context.Context, expected *projectionEntry) {
				calls++
				observed = expected
				peerReply, peerErr = externalVersionRegisteredCall(s, peer, 30, "edit", snapshotEditArgs(path, "NeighborBefore", "Neighbor"))
				originalErr = ctx.Err()
				if deadline, ok := ctx.Deadline(); ok {
					remaining = time.Until(deadline)
				}
			})
			leaf, ok := s.facades.legacy("search_symbols")
			require.True(t, ok)
			s.facades.capture(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				return leaf.handler(context.WithValue(ctx, checkedProjectionReadObserverKey{}, observer), req)
			})
			pendingFileProjections.markSuspect(path)
			args := snapshotSearchArgs()
			args["query"] = "name:Target"
			result, callErr := externalVersionRegisteredCall(s, base, 20, "search", args)
			wire, marshalErr := json.Marshal(result)
			t.Logf("opt_in=%v native_FTS=%d observer_calls=%d remaining_original_budget=%s result=%s", enabled, count, calls, remaining, wire)
			require.NoError(t, marshalErr)
			require.Equal(t, 1, calls)
			require.Same(t, original, observed)
			require.NoError(t, peerErr)
			require.NotNil(t, peerReply)
			require.False(t, peerReply.IsError, resultText(peerReply))
			require.Equal(t, "local", extractTextResult(t, peerReply)["ack_mode"])
			require.NoError(t, originalErr)
			require.Positive(t, remaining)
			current := liveEntry(pendingFileProjections, path)
			require.NotNil(t, current)
			require.Greater(t, current.sourceCommitSeq, original.sourceCommitSeq)
			require.NotSame(t, original.layer, current.layer)
			require.NotEqual(t, before, snapshotPhysicalTarget(t, path))
			require.Nil(t, backend.GetNode("main.go::Target"))
			require.NoError(t, callErr)
			require.NotNil(t, result)
			require.True(t, result.IsError, "evidence cannot turn a physical supersession into success")
			require.Equal(t, "projection not published: superseded", resultText(result))
			require.NotNil(t, result.Meta)
			evidence, present := result.Meta.AdditionalFields["gortex_search_refusal"]
			if !enabled {
				require.False(t, present, "default wire metadata remains unchanged")
				return
			}
			require.True(t, present, "an opted-in real refusal must identify its existing failure site")
			require.Equal(t, map[string]any{"stage": "bind.refresh", "capture_state": "unsealed"}, evidence)
			encoded, err := json.Marshal(evidence)
			require.NoError(t, err)
			require.LessOrEqual(t, len(encoded), 128, "fixed scalar evidence cannot retain request or source payloads")
		})
	}
}
