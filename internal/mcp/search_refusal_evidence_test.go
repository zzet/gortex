package mcp

import (
	"context"
	"encoding/json"
	"os"
	"sync"
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
			require.Equal(t, map[string]any{"stage": "bind.refresh/live/superseded_nil", "capture_state": "unsealed"}, evidence)
			encoded, err := json.Marshal(evidence)
			require.NoError(t, err)
			require.LessOrEqual(t, len(encoded), 128, "fixed scalar evidence cannot retain request or source payloads")
		})
	}
}

func TestSearchRefusalEvidenceKeepsDefaultErrorAndBoundedScalars(t *testing.T) {
	var cause error = projectionSuperseded
	for _, flag := range []string{"", "0", "true"} {
		t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", flag)
		require.True(t, annotateSearchRefusal(context.Background(), searchRefusalBindRefresh, cause) == cause)
		// Windows Getenv allocates for its UTF-16 conversion. Annotation
		// must add no allocation beyond the same platform environment lookup.
		lookupAllocs := testing.AllocsPerRun(100, func() {
			if os.Getenv("GORTEX_SEARCH_REFUSAL_EVIDENCE") != flag {
				panic("refusal evidence environment changed")
			}
		})
		annotationAllocs := testing.AllocsPerRun(100, func() {
			if annotateSearchRefusal(context.Background(), searchRefusalBindRefresh, cause) != cause {
				panic("default refusal identity changed")
			}
		})
		require.Equal(t, lookupAllocs, annotationAllocs, "disabled annotation adds no allocation beyond its refusal-only environment lookup (flag=%q)", flag)
		result := searchRefusalToolResult(cause)
		require.Nil(t, result.Meta)
	}
	t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
	require.Nil(t, annotateSearchRefusal(context.Background(), searchRefusalBindRefresh, nil))
	require.True(t, annotateSearchRefusal(context.Background(), searchRefusalBindRefresh, context.Canceled) == context.Canceled)
	require.True(t, annotateSearchRefusal(context.Background(), searchRefusalStage(255), cause) == cause)
	facts := &requestProjectionFacts{}
	for _, state := range []string{"none", "unsealed", "sealed", "disabled", "foreign"} {
		ctx := context.WithValue(context.Background(), requestProjectionFactsKey{}, facts)
		if state != "none" {
			capture := &pendingSymbolCapture{}
			if state == "sealed" || state == "foreign" {
				capture.facts, capture.entries = facts, map[string]*projectionEntry{"fixture": {}}
			}
			capture.disabled = state == "disabled"
			if state == "foreign" {
				capture.facts = &requestProjectionFacts{}
			}
			ctx = context.WithValue(ctx, pendingSymbolCaptureKey{}, capture)
		}
		for stage := searchRefusalBindInherited; stage <= searchRefusalRefreshCancelledView; stage++ {
			marked := annotateSearchRefusal(ctx, stage, cause)
			require.ErrorIs(t, marked, cause)
			require.Equal(t, cause.Error(), marked.Error())
			require.Same(t, marked, annotateSearchRefusal(ctx, searchRefusalResponseValidation, marked), "preserve first refusal site")
			result := searchRefusalToolResult(marked)
			require.True(t, result.IsError)
			require.Equal(t, cause.Error(), resultText(result))
			member := result.Meta.AdditionalFields["gortex_search_refusal"]
			require.Equal(t, map[string]any{"stage": stage.label(), "capture_state": state}, member)
			encoded, err := json.Marshal(result.Meta)
			require.NoError(t, err)
			require.LessOrEqual(t, len(encoded), 128, "complete added member has a caller-independent fixed bound")
		}
	}
}

func TestRegisteredNativeLiteralRefusalEvidenceIdentifiesFinalValidation(t *testing.T) {
	t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
	s, backend, base, path, _ := snapshotFacadeFixture(t)
	s.engine.SetSearchProvider(s.indexer.Search)
	count, err := backend.SymbolFTSCount()
	require.NoError(t, err)
	require.Positive(t, count)
	ack := snapshotRegisteredCall(t, s, base, 12, "edit", snapshotEditArgs(path, "return 2", "return 2 /* SnapshotLiteralOnly */"))
	require.False(t, ack.IsError, resultText(ack))
	require.Equal(t, true, extractTextResult(t, ack)["readback_exact"])
	leaf, ok := s.facades.legacy("search_symbols")
	require.True(t, ok)
	type answer struct {
		ctx    context.Context
		result *mcplib.CallToolResult
		err    error
	}
	held, answered := make(chan answer, 1), make(chan answer, 1)
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	s.facades.capture(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		result, err := leaf.handler(ctx, req)
		held <- answer{ctx, result, err}
		<-released
		return result, err
	})
	args := snapshotSearchArgs()
	args["query"] = "SnapshotLiteralOnly"
	var worker sync.WaitGroup
	worker.Add(1)
	t.Cleanup(func() { release(); worker.Wait() })
	go func() {
		defer worker.Done()
		result, err := externalVersionRegisteredCall(s, base, 20, "search", args)
		answered <- answer{result: result, err: err}
	}()
	var selected answer
	select {
	case selected = <-held:
	case early := <-answered:
		t.Fatalf("no completed leaf: result=%v error=%v", early.result, early.err)
	case <-time.After(3 * time.Second):
		t.Fatal("native literal query did not reach completed leaf")
	}
	require.NoError(t, selected.err)
	require.NotNil(t, selected.result)
	require.False(t, selected.result.IsError, resultText(selected.result))
	fields := extractTextResult(t, selected.result)
	content, ok := fields["content_matches"].(map[string]any)
	require.True(t, ok, "actual physical literal fallback, not a fabricated selected result")
	require.Positive(t, content["count"])
	require.NoError(t, validateRequestProjections(selected.ctx))
	peer := snapshotRegisteredCall(t, s, base, 30, "edit", snapshotEditArgs(path, "NeighborBefore", "Neighbor"))
	require.False(t, peer.IsError, resultText(peer))
	require.Equal(t, true, extractTextResult(t, peer)["readback_exact"])
	require.ErrorIs(t, validateRequestProjections(selected.ctx), projectionSuperseded)
	snapshotPhysicalTarget(t, path)
	release()
	worker.Wait()
	final := <-answered
	require.NoError(t, final.err)
	require.NotNil(t, final.result)
	require.True(t, final.result.IsError, "stage evidence never authorizes mutable literal fallback")
	require.Equal(t, "projection not published: superseded", resultText(final.result))
	require.NotNil(t, final.result.Meta)
	require.Equal(t, map[string]any{"stage": "response.projection_validation", "capture_state": "disabled"}, final.result.Meta.AdditionalFields["gortex_search_refusal"])
	encoded, err := json.Marshal(final.result)
	require.NoError(t, err)
	t.Logf("native_FTS=%d actual_completed_literal_then_peer_ACK_final=%s", count, encoded)
}
