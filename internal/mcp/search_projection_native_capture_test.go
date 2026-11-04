package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// The existing capture interleaving fixture has a reader-only engine. Bind
// its real native provider before the query, then hold the completed leaf
// across another session's ACK. This is controlled coverage, not attribution
// of any retained native-run refusal.
func TestRegisteredNativeNameSearchKeepsCapturedVersionAfterPeerACK(t *testing.T) {
	for _, kind := range []string{"new_positive", "complete_old_negative"} {
		t.Run(kind, func(t *testing.T) {
			s, backend, base, path, original := snapshotFacadeFixture(t)
			s.engine.SetSearchProvider(s.indexer.Search)
			require.IsType(t, (*search.Swappable)(nil), s.indexer.Search())
			nativeCount, err := backend.SymbolFTSCount()
			require.NoError(t, err)
			require.Positive(t, nativeCount, "a reader-only engine cannot prove the native interleaving")
			require.NotNil(t, backend.GetNode("main.go::TargetBefore"))
			require.Nil(t, backend.GetNode("main.go::Target"), "own ACK has not been canonically published")

			const peerSession = "native-name-capture-peer"
			require.NoError(t, s.ConnectSession(peerSession, func([]byte) error { return nil }))
			t.Cleanup(func() { s.DisconnectSession(peerSession) })
			peer := s.WithClientSession(WithSessionID(t.Context(), peerSession), peerSession)
			s.NoteSessionToolPolicy(peerSession, FacadeSurfaceVersion, "hide")
			require.NotNil(t, s.MCPServer().HandleMessage(peer, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"gortex-load-peer","version":"1"}}}`)))
			s.MCPServer().HandleMessage(peer, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))

			args := snapshotSearchArgs()
			args["query"] = "name:Target"
			if kind == "complete_old_negative" {
				args["query"] = "name:TargetBefore"
				args["options"].(map[string]any)["path"] = "main.go"
			}
			check := func(result *mcplib.CallToolResult) map[string]any {
				require.NotNil(t, result)
				require.False(t, result.IsError, resultText(result))
				body := extractTextResult(t, result)
				value, marshalErr := json.Marshal(body)
				require.NoError(t, marshalErr)
				name := "Target"
				if kind == "complete_old_negative" {
					name = "TargetBefore"
				}
				channel := negativeQuerySummarize(value, "main.go::"+name, name)
				require.True(t, channel.Valid)
				require.True(t, channel.Complete, "a partial result cannot prove old-name absence")
				if kind == "new_positive" {
					snapshotTargetRows(t, result)
					require.True(t, channel.FoundExact)
					require.Equal(t, 1, channel.Rows)
				} else {
					require.False(t, channel.HasOldIdentity)
					require.Zero(t, channel.Rows)
				}
				if result.StructuredContent != nil {
					structured, marshalErr := json.Marshal(result.StructuredContent)
					require.NoError(t, marshalErr)
					other := negativeQuerySummarize(structured, "main.go::"+name, name)
					require.True(t, other.Valid)
					require.True(t, other.Complete)
					require.Equal(t, channel.body, other.body, "declared channels must agree when both exist")
				}
				return body
			}
			type selected struct {
				ctx    context.Context
				result *mcplib.CallToolResult
				err    error
			}
			leaf, ok := s.facades.legacy("search_symbols")
			require.True(t, ok)
			held := make(chan selected, 1)
			released := make(chan struct{})
			release := sync.OnceFunc(func() { close(released) })
			leafDone := make(chan struct{})
			s.facades.capture(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				defer close(leafDone)
				result, callErr := leaf.handler(ctx, req)
				held <- selected{ctx, result, callErr}
				<-released
				return result, callErr
			})
			answered := make(chan selected, 1)
			var worker sync.WaitGroup
			worker.Add(1)
			t.Cleanup(func() { release(); worker.Wait() })
			go func() {
				defer worker.Done()
				result, callErr := externalVersionRegisteredCall(s, base, 20, "search", args)
				answered <- selected{base, result, callErr}
			}()
			var captured selected
			select {
			case captured = <-held:
			case early := <-answered:
				t.Fatalf("registered call returned before its leaf: result=%v err=%v", early.result, early.err)
			case <-time.After(3 * time.Second):
				t.Fatal("native registered search did not reach the completed leaf")
			}
			require.NoError(t, captured.err)
			require.Implements(t, (*graph.FoldedNameCandidateReader)(nil), s.readerFor(captured.ctx), "the selected request base must expose coherent name candidates; the exact noncanonical Target row separately proves pending-layer composition")
			leafJSON, err := json.Marshal(captured.result)
			require.NoError(t, err)
			t.Logf("kind=%s backend=%T FTS=%d captured_leaf_json=%s", kind, s.indexer.Search(), nativeCount, leafJSON)
			leafFields := check(captured.result)
			require.NoError(t, validateRequestProjections(captured.ctx))
			capture, _ := captured.ctx.Value(pendingSymbolCaptureKey{}).(*pendingSymbolCapture)
			require.NotNil(t, capture)
			require.NotEmpty(t, capture.entries)
			before := snapshotPhysicalTarget(t, path)

			ack := snapshotRegisteredCall(t, s, peer, 30, "edit", snapshotEditArgs(path, "NeighborBefore", "Neighbor"))
			require.False(t, ack.IsError, resultText(ack))
			ackFields := extractTextResult(t, ack)
			require.Equal(t, "local", ackFields["ack_mode"])
			require.Equal(t, true, ackFields["readback_exact"])
			after := snapshotPhysicalTarget(t, path)
			require.NotEqual(t, before, after, "actual peer edit changes bytes after the completed leaf")
			require.Equal(t, fmt.Sprintf("%x", after), ackFields["content_sha256"])
			physical, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, after, sha256.Sum256(physical))
			require.NotContains(t, string(physical), "func TargetBefore(")
			require.True(t, strings.Contains(string(physical), "func Target("))
			require.NotSame(t, original, liveEntry(pendingFileProjections, path))
			require.Nil(t, backend.GetNode("main.go::Target"), "no canonical publication may substitute for the captured projection")
			require.ErrorIs(t, validateRequestProjections(captured.ctx), projectionSuperseded, "old shared facts must remain invalid")
			require.NoError(t, validateSymbolResponseProjections(captured.ctx, "search_symbols"), "only the selected immutable symbol response may finish")

			release()
			worker.Wait()
			<-leafDone
			response := <-answered
			require.NoError(t, response.err)
			finalJSON, err := json.Marshal(response.result)
			require.NoError(t, err)
			t.Logf("kind=%s peer_ACK=%v before_sha=%x after_sha=%x final_json=%s", kind, ackFields, before, after, finalJSON)
			fields := check(response.result)
			if kind == "new_positive" {
				freshness, ok := fields["freshness"].(map[string]any)
				require.True(t, ok, "retain the existing canonical drift rider")
				require.Equal(t, []any{map[string]any{"file": "main.go"}}, freshness["stale_files"])
				require.NotEmpty(t, freshness["stale_hint"])
				require.Len(t, freshness, 2)
				for _, key := range []string{"state", "file_view", "readback_exact", "not_modified", "current", "latest"} {
					require.NotContains(t, freshness, key)
				}
				delete(fields, "freshness")
			} else {
				require.NotContains(t, fields, "freshness", "an empty result has no returned file for the existing per-file rider")
			}
			for _, key := range []string{"state", "file_view", "readback_exact", "not_modified", "current", "latest"} {
				require.NotContains(t, fields, key)
			}
			require.Equal(t, leafFields, fields, "peer ACK cannot replace the already captured answer")
			require.NotNil(t, response.result.Meta)
			for key, value := range captured.result.Meta.AdditionalFields {
				require.Equal(t, value, response.result.Meta.AdditionalFields[key])
			}
		})
	}
}
