package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

// This uses real initialize/tools-call frames and the registered wrappers.
// The issue bounds barrier wait, not normalization, graph walk or serialization:
// retain the full call's observed wall time without subtracting a second run.
func TestRegisteredRelationBarrierLiteral250ms(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	for _, surface := range []string{"legacy", "facade"} {
		for _, unrelated := range []int{0, 8} {
			for _, explicitFalse := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/unrelated_%d/explicit_false_%t", surface, unrelated, explicitFalse), func(t *testing.T) {
					s, root := newRelationFixture(t)
					for i := 0; i < unrelated; i++ {
						path := writeUnindexed(t, root, fmt.Sprintf("outside%d.go", i), fmt.Sprintf("package p\nfunc Outside%d() {}\n", i))
						pendingFreshnessReceipt(s, fmt.Sprintf("outside-%d", i), "", path, uint64(i+1))
					}
					pendingFreshnessReceipt(s, "unresolved-target", "", writeUnindexed(t, root, "fresh.go", "package p\nfunc Fresh() {}\n"), uint64(unrelated+1))
					ctx := relationBarrierRegisteredSession(t, s, surface)
					name, args := relationBarrierRegisteredRequest(surface, "fresh.go::Fresh")
					if explicitFalse {
						relationBarrierSetFreshness(surface, args, "require_fresh", false)
					}
					started := time.Now()
					result := guardE2ECall(t, s, ctx, 2, name, args)
					elapsed := time.Since(started)
					require.False(t, result.IsError, resultText(result))
					body := lifecycleResultPayload(t, result)
					require.Equal(t, "pending", body["graph_freshness"])
					require.Equal(t, false, body["complete"])
					require.Equal(t, []any{"fresh.go"}, body["pending_files"])
					require.Equal(t, float64(1), body["pending_file_count"])
					for _, field := range []string{"risk", "summary", "by_depth", "total_affected"} {
						require.Contains(t, body, field, "the walk result must remain present, not only a pending label")
					}
					wait, ok := body["barrier_wait_ms"].(float64)
					require.True(t, ok, "the actual producer wait must be present")
					t.Logf("registered wall=%s, producer barrier_wait_ms=%.9f; wall includes wrapper/walk/serialization", elapsed, wait)
					require.GreaterOrEqual(t, wait, float64(0))
					require.LessOrEqual(t, wait, float64(250), "literal barrier maximum, no clamp or widened allowance")
				})
			}
		}
	}
}

// The timeout path must obey the same maximum when the seed is already
// resolved. A quick successful publication does not exercise this branch.
func TestRegisteredRelationBarrierResolvedPendingLiteral250ms(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	for _, surface := range []string{"legacy", "facade"} {
		for _, unrelated := range []int{0, 8} {
			t.Run(fmt.Sprintf("%s/unrelated_%d", surface, unrelated), func(t *testing.T) {
				s, root := newRelationFixture(t)
				id := targetID(t, s)
				require.NotNil(t, s.graph.GetNode(id), "this seed must be resolved before the request")
				for i := 0; i < unrelated; i++ {
					path := writeUnindexed(t, root, fmt.Sprintf("outside%d.go", i), fmt.Sprintf("package p\nfunc Outside%d() {}\n", i))
					pendingFreshnessReceipt(s, fmt.Sprintf("outside-%d", i), "", path, uint64(i+1))
				}
				receipt := pendingFreshnessReceipt(s, "resolved-pending", "",
					writeUnindexed(t, root, "target.go", "package p\n\nfunc Target() { Other() }\n"), uint64(unrelated+1))
				ctx := relationBarrierRegisteredSession(t, s, surface)
				name, args := relationBarrierRegisteredRequest(surface, id)
				started := time.Now()
				result := guardE2ECall(t, s, ctx, 2, name, args)
				elapsed := time.Since(started)
				require.False(t, result.IsError, resultText(result))
				body := lifecycleResultPayload(t, result)
				require.Equal(t, "pending", body["graph_freshness"])
				require.Equal(t, false, body["complete"])
				require.Equal(t, []any{"target.go"}, body["pending_files"])
				require.True(t, receipt.outcome(true).Pending, "no publisher may turn the timeout fixture into a success")
				wait, ok := body["barrier_wait_ms"].(float64)
				require.True(t, ok)
				t.Logf("resolved/no-publication registered wall=%s, producer barrier_wait_ms=%.9f", elapsed, wait)
				require.GreaterOrEqual(t, wait, float64(0))
				require.LessOrEqual(t, wait, float64(250), "resolved scoped pending receipts obey the literal barrier maximum")
			})
		}
	}
}

// A possible non-require_fresh fast pending answer must not replace a caller's
// explicit freshness contract. The real publisher indexes and completes the
// same receipt after the registered handler has entered, in the original
// caller's allowance. No timer or reply is fabricated.
func TestRegisteredRelationBarrierRequireFreshPublication(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	for _, surface := range []string{"legacy", "facade"} {
		t.Run(surface, func(t *testing.T) { testRegisteredRelationBarrierPublication(t, surface, true) })
	}
}

func TestRegisteredRelationBarrierResolvedDefaultPublication(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	for _, surface := range []string{"legacy", "facade"} {
		t.Run(surface, func(t *testing.T) { testRegisteredRelationBarrierPublication(t, surface, false) })
	}
}

func testRegisteredRelationBarrierPublication(t *testing.T, surface string, requireFresh bool) {
	t.Helper()
	s, root := newRelationFixture(t)
	id, file, source := "fresh.go::Fresh", "fresh.go", "package p\nfunc Fresh() {}\n"
	if !requireFresh {
		id, file, source = targetID(t, s), "target.go", "package p\n\nfunc Target() { Other() }\n"
	}
	path := writeUnindexed(t, root, file, source)
	receipt := pendingFreshnessReceipt(s, "pending-target", "", path, 1)
	base := relationBarrierRegisteredSession(t, s, surface)
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	legacy, ok := s.facades.legacy("explain_change_impact")
	require.True(t, ok)
	entered := make(chan struct{})
	var enteredOnce sync.Once
	var indexed atomic.Bool
	s.addTool(legacy.tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		enteredOnce.Do(func() { close(entered) })
		return legacy.handler(ctx, req)
	})
	published := make(chan error, 1)
	go func() {
		select {
		case <-entered:
		case <-ctx.Done():
			published <- ctx.Err()
			return
		}
		timer := time.NewTimer(75 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			published <- ctx.Err()
			return
		}
		err := s.indexer.IndexFile(filepath.Clean(path))
		if err == nil {
			indexed.Store(true)
			completeFreshnessReceipt(receipt, indexer.MutationResult{RequestedGeneration: 1, AppliedGeneration: 1, Reindexed: true})
		}
		published <- err
	}()
	name, args := relationBarrierRegisteredRequest(surface, id)
	if requireFresh {
		relationBarrierSetFreshness(surface, args, "require_fresh", true)
	}
	relationBarrierSetFreshness(surface, args, "wait_deadline", time.Now().Add(2*time.Second).Format(time.RFC3339Nano))
	result := guardE2ECall(t, s, ctx, 2, name, args)
	indexedBeforeReply := indexed.Load()
	if result.IsError {
		t.Logf("registered publication returned tool error before publisher completion: %s", resultText(result))
		cancel()
	}
	require.NoError(t, <-published)
	require.True(t, indexedBeforeReply, "known seeds and explicit require_fresh must retain the real publication wait")
	require.False(t, result.IsError, resultText(result))
	body := lifecycleResultPayload(t, result)
	state, _ := body["graph_freshness"].(string)
	require.True(t, state == "" || state == "fresh", resultText(result))
	require.NotNil(t, s.graph.GetNode(id))
	t.Logf("real publication require_fresh=%t barrier_wait_ms=%v", requireFresh, body["barrier_wait_ms"])
}

func TestRegisteredRelationBarrierUnrelatedUnresolvedDoesNotWait(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	for _, surface := range []string{"legacy", "facade"} {
		t.Run(surface, func(t *testing.T) {
			s, root := newRelationFixture(t)
			pendingFreshnessReceipt(s, "outside", "", writeUnindexed(t, root, "outside.go", "package p\nfunc Outside() {}\n"), 1)
			ctx := relationBarrierRegisteredSession(t, s, surface)
			name, args := relationBarrierRegisteredRequest(surface, "gone.go::Gone")
			result := guardE2ECall(t, s, ctx, 2, name, args)
			require.False(t, result.IsError, resultText(result))
			body := lifecycleResultPayload(t, result)
			require.NotEqual(t, "pending", body["graph_freshness"])
			require.NotContains(t, body, "pending_files")
			require.Less(t, body["barrier_wait_ms"], float64(250), "an unrelated receipt must not spend the optional grace")
		})
	}
}

func relationBarrierRegisteredSession(t *testing.T, s *Server, surface string) context.Context {
	t.Helper()
	id := "relation-barrier-" + surface
	require.NoError(t, s.ConnectSession(id, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(id) })
	ctx := s.WithClientSession(WithSessionID(t.Context(), id), id)
	if surface == "facade" {
		s.NoteSessionToolPolicy(id, FacadeSurfaceVersion, "hide")
	} else {
		s.NoteSessionToolPolicy(id, "all", "defer")
	}
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"relation-barrier","version":"1"}}}`)))
	return ctx
}

func relationBarrierRegisteredRequest(surface, id string) (string, map[string]any) {
	if surface == "facade" {
		return "change", map[string]any{"operation": "impact", "target": map[string]any{"symbols": []string{id}}, "output": map[string]any{"format": "json"}}
	}
	return "explain_change_impact", map[string]any{"ids": id, "format": "json"}
}

func relationBarrierSetFreshness(surface string, args map[string]any, key string, value any) {
	if surface == "facade" {
		options, _ := args["options"].(map[string]any)
		if options == nil {
			options = make(map[string]any)
			args["options"] = options
		}
		options[key] = value
		return
	}
	args[key] = value
}
