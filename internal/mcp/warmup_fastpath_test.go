package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// ---------------------------------------------------------------------------
// warmupStateFromSnapshot — phase → real-percentage derivation
// ---------------------------------------------------------------------------

// TestWarmupStateFromSnapshot_NilSnapshot — no readiness phase has been
// published (single-process server, or daemon pre-first-phase): the
// state is not-known, so the fast path stays a pure pass-through.
func TestWarmupStateFromSnapshot_NilSnapshot(t *testing.T) {
	w := warmupStateFromSnapshot(nil)
	assert.False(t, w.known, "nil snapshot must yield a not-known state")
	assert.False(t, w.warming(), "not-known is never warming")
	assert.Equal(t, 0, w.percent)
}

// TestWarmupStateFromSnapshot_PhaseProgress — every known warmup phase
// maps to its real, monotonically non-decreasing completion
// percentage, and mid-warmup phases report warming() == true.
func TestWarmupStateFromSnapshot_PhaseProgress(t *testing.T) {
	// The pipeline order — percent must be non-decreasing along it.
	order := []string{
		"snapshot_loaded",
		"parallel_parse",
		"parallel_parse_done",
		"resolve",
		"resolve_done",
		"deferred_passes_all",
		"deferred_passes_all_done",
		"global_resolve",
		"global_resolve_done",
		"end_batch",
		"end_batch_done",
		"watcher_started",
		"checkout_builds_pending",
		"degraded",
	}
	prev := 0
	for _, phase := range order {
		w := warmupStateFromSnapshot(map[string]any{"phase": phase, "ready": false})
		require.True(t, w.known, "phase %q must be known", phase)
		assert.True(t, w.warming(), "phase %q is mid-warmup so warming() must be true", phase)
		assert.Greater(t, w.percent, 0, "phase %q must have a real percent", phase)
		assert.Less(t, w.percent, 100, "mid-warmup phase %q must be < 100%%", phase)
		assert.GreaterOrEqual(t, w.percent, prev,
			"percent must not regress along the pipeline at phase %q", phase)
		prev = w.percent
	}
}

// TestWarmupStateFromSnapshot_Ready — the terminal `ready` phase
// reports 100% and clears the warming flag.
func TestWarmupStateFromSnapshot_Ready(t *testing.T) {
	w := warmupStateFromSnapshot(map[string]any{"phase": "ready", "ready": true})
	assert.True(t, w.known)
	assert.True(t, w.ready)
	assert.False(t, w.warming(), "a ready daemon is not warming")
	assert.Equal(t, 100, w.percent)
}

// TestWarmupStateFromSnapshot_UnknownPhase — a forward-compat phase
// string this build does not recognise still yields a real (non-zero)
// percent rather than a placeholder, and is treated as mid-warmup.
func TestWarmupStateFromSnapshot_UnknownPhase(t *testing.T) {
	w := warmupStateFromSnapshot(map[string]any{"phase": "some_future_phase", "ready": false})
	assert.True(t, w.known)
	assert.True(t, w.warming())
	assert.Equal(t, 1, w.percent, "unknown mid-warmup phase falls back to a non-zero floor")

	// ready flag wins even with an unrecognised phase string.
	wr := warmupStateFromSnapshot(map[string]any{"phase": "some_future_phase", "ready": true})
	assert.False(t, wr.warming())
	assert.Equal(t, 100, wr.percent)
}

// ---------------------------------------------------------------------------
// checkWarmupFastPath — graph-tool vs exempt-tool gating
// ---------------------------------------------------------------------------

// warmingServer builds a minimal Server whose readiness broadcaster
// already published the given phase, so warmupSnapshot reflects it.
func warmingServer(t *testing.T, phase string, ready bool) *Server {
	t.Helper()
	s := &Server{readinessBroadcaster: newReadinessBroadcaster(&fakeSpecificSender{}, zap.NewNop())}
	s.readinessBroadcaster.publish(map[string]any{"phase": phase, "ready": ready})
	return s
}

// TestCheckWarmupFastPath_GraphToolDuringWarmup — a graph-querying
// tool invoked mid-warmup is flagged for decoration with a real
// percent + phase envelope.
func TestCheckWarmupFastPath_GraphToolDuringWarmup(t *testing.T) {
	s := warmingServer(t, "parallel_parse", false)
	env, warming := s.checkWarmupFastPath("search_symbols")
	require.True(t, warming, "graph tool during warmup must be flagged")
	assert.True(t, env.Warming)
	assert.Equal(t, "parallel_parse", env.Phase)
	assert.Equal(t, warmupPhaseProgress["parallel_parse"], env.Percent)
	assert.True(t, env.Retriable)
	assert.True(t, env.PartialResults, "decoration envelope marks the result partial")
	assert.NotEmpty(t, env.Message)
}

// TestCheckWarmupFastPath_GraphToolWhenReady — once the daemon is
// ready, the same tool is a transparent pass-through (no envelope).
func TestCheckWarmupFastPath_GraphToolWhenReady(t *testing.T) {
	s := warmingServer(t, "ready", true)
	_, warming := s.checkWarmupFastPath("search_symbols")
	assert.False(t, warming, "a ready daemon must not flag graph tools")
}

// TestCheckWarmupFastPath_NoPhasePublished — with no readiness phase
// ever published (single-process server), the fast path is inert.
func TestCheckWarmupFastPath_NoPhasePublished(t *testing.T) {
	s := &Server{readinessBroadcaster: newReadinessBroadcaster(&fakeSpecificSender{}, zap.NewNop())}
	_, warming := s.checkWarmupFastPath("find_usages")
	assert.False(t, warming, "no published phase means not-warming")

	// And a Server with no broadcaster at all is equally inert.
	bare := &Server{}
	_, warming = bare.checkWarmupFastPath("find_usages")
	assert.False(t, warming)
}

// TestCheckWarmupFastPath_ExemptToolUnaffected — graph-independent
// tools (subscription, session-control, discovery/indexing) are never
// flagged, even mid-warmup.
func TestCheckWarmupFastPath_ExemptToolUnaffected(t *testing.T) {
	s := warmingServer(t, "global_resolve", false)
	for _, tool := range []string{
		"subscribe_workspace_readiness",
		"unsubscribe_workspace_readiness",
		"set_planning_mode",
		"tools_search",
		"index_repository",
		"track_repository",
	} {
		_, warming := s.checkWarmupFastPath(tool)
		assert.False(t, warming, "exempt tool %q must never be flagged during warmup", tool)
	}
}

// ---------------------------------------------------------------------------
// decorateResultWithWarming — the three result shapes
// ---------------------------------------------------------------------------

func warmingEnv() warmupEnvelope {
	return newWarmupEnvelope(warmupStateFromSnapshot(
		map[string]any{"phase": "parallel_parse", "ready": false}), true)
}

// decodeWarmingResult extracts the JSON object from a decorated tool
// result and returns its `warming` sub-object.
func decodeWarmingResult(t *testing.T, res *mcp.CallToolResult) (map[string]any, map[string]any) {
	t.Helper()
	require.NotNil(t, res)
	require.NotEmpty(t, res.Content)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent, got %T", res.Content[0])
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &obj))
	warming, ok := obj["warming"].(map[string]any)
	require.True(t, ok, "decorated result must carry a `warming` object, got: %s", tc.Text)
	return obj, warming
}

// TestDecorateResultWithWarming_JSONObject — a JSON-object handler
// result keeps every original key and gains the `warming` block.
func TestDecorateResultWithWarming_JSONObject(t *testing.T) {
	orig, err := mcp.NewToolResultJSON(map[string]any{
		"results": []string{"Bar", "Baz"},
		"total":   2,
	})
	require.NoError(t, err)

	decorated := decorateResultWithWarming(orig, warmingEnv())
	obj, warming := decodeWarmingResult(t, decorated)

	// Original partial data is preserved.
	assert.Equal(t, float64(2), obj["total"])
	assert.Len(t, obj["results"], 2)
	// Warming block is real and marked partial.
	assert.Equal(t, true, warming["warming"])
	assert.Equal(t, "parallel_parse", warming["phase"])
	assert.Equal(t, float64(warmupPhaseProgress["parallel_parse"]), warming["percent"])
	assert.Equal(t, true, warming["partial_results"])
	assert.Equal(t, true, warming["retriable"])
	assert.NotEmpty(t, warming["message"])
}

// TestDecorateResultWithWarming_JSONArray — an array payload is
// wrapped under a stable `results` key so the partial answer survives
// verbatim alongside the warming flag.
func TestDecorateResultWithWarming_JSONArray(t *testing.T) {
	orig, err := mcp.NewToolResultJSON([]string{"a", "b", "c"})
	require.NoError(t, err)

	decorated := decorateResultWithWarming(orig, warmingEnv())
	obj, warming := decodeWarmingResult(t, decorated)

	results, ok := obj["results"].([]any)
	require.True(t, ok, "array payload must be preserved under `results`")
	assert.Len(t, results, 3)
	assert.Equal(t, true, warming["warming"])
	assert.Equal(t, true, warming["partial_results"])
}

// TestDecorateResultWithWarming_EmptyResult — a handler that produced
// nothing usable mid-warmup is replaced with a standalone warming-only
// envelope so the caller still gets flag + percent + phase + message.
func TestDecorateResultWithWarming_EmptyResult(t *testing.T) {
	empty := &mcp.CallToolResult{}
	decorated := decorateResultWithWarming(empty, warmingEnv())
	_, warming := decodeWarmingResult(t, decorated)

	assert.Equal(t, true, warming["warming"])
	assert.Equal(t, "parallel_parse", warming["phase"])
	assert.Equal(t, float64(warmupPhaseProgress["parallel_parse"]), warming["percent"])
	// No partial data was available — the flag says so.
	assert.Equal(t, false, warming["partial_results"])
	assert.Equal(t, true, warming["retriable"])
	assert.NotEmpty(t, warming["message"])
}

// TestDecorateResultWithWarming_PreservesErrorFlag — an error result
// raised mid-warmup keeps IsError but still carries the retry signal.
func TestDecorateResultWithWarming_PreservesErrorFlag(t *testing.T) {
	errRes := mcp.NewToolResultError("graph not ready")
	require.True(t, errRes.IsError)

	decorated := decorateResultWithWarming(errRes, warmingEnv())
	assert.True(t, decorated.IsError, "error flag must survive decoration")
	_, warming := decodeWarmingResult(t, decorated)
	assert.Equal(t, true, warming["warming"])
	assert.Equal(t, true, warming["retriable"])
}

// TestDecorateResultWithWarming_DoesNotClobberExistingKey — a handler
// that already set its own `warming` key keeps it untouched.
func TestDecorateResultWithWarming_DoesNotClobberExistingKey(t *testing.T) {
	orig, err := mcp.NewToolResultJSON(map[string]any{"warming": "handler-set"})
	require.NoError(t, err)
	decorated := decorateResultWithWarming(orig, warmingEnv())
	require.NotEmpty(t, decorated.Content)
	tc := decorated.Content[0].(mcp.TextContent)
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &obj))
	assert.Equal(t, "handler-set", obj["warming"], "decoration must not overwrite a handler's own key")
}

// ---------------------------------------------------------------------------
// End-to-end — the fast path through wrapToolHandler
// ---------------------------------------------------------------------------

// fastPathTestServer builds a Server (struct literal, same shape as
// notes_test.go's newTestServer) wired with a readiness broadcaster so
// wrapToolHandler's warmup hook is live.
func fastPathTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/foo.go::Bar", Name: "Bar", Kind: graph.KindFunction, FilePath: "pkg/foo.go"})
	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	s.readinessBroadcaster = newReadinessBroadcaster(&fakeSpecificSender{}, zap.NewNop())
	return s
}

// echoHandler is a stand-in graph-querying tool: it returns a JSON
// object of best-effort "partial" data, exactly what a real tool would
// return against a half-built graph.
func echoHandler(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultJSON(map[string]any{"results": []string{"Bar"}, "total": 1})
}

func callWrapped(t *testing.T, s *Server, h mcpserver.ToolHandlerFunc, toolName string) *mcp.CallToolResult {
	t.Helper()
	wrapped := s.wrapToolHandler(h)
	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = map[string]any{}
	res, err := wrapped(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	return res
}

// TestWrapToolHandler_GraphToolDuringWarmup — a graph-querying tool
// dispatched while the daemon warms up returns its best-effort partial
// data AND a real warming envelope.
func TestWrapToolHandler_GraphToolDuringWarmup(t *testing.T) {
	s := fastPathTestServer(t)
	s.readinessBroadcaster.publish(map[string]any{"phase": "deferred_passes_all", "ready": false})

	res := callWrapped(t, s, echoHandler, "search_symbols")
	obj, warming := decodeWarmingResult(t, res)

	// Best-effort partial answer survived.
	assert.Equal(t, float64(1), obj["total"])
	assert.Len(t, obj["results"], 1)
	// Warming envelope carries a real percent + phase.
	assert.Equal(t, true, warming["warming"])
	assert.Equal(t, "deferred_passes_all", warming["phase"])
	assert.Equal(t, float64(warmupPhaseProgress["deferred_passes_all"]), warming["percent"])
	assert.Equal(t, true, warming["partial_results"])
}

// TestWrapToolHandler_GraphToolWhenReady — once warmup completes, the
// same tool returns its plain result with no warming block.
func TestWrapToolHandler_GraphToolWhenReady(t *testing.T) {
	s := fastPathTestServer(t)
	s.readinessBroadcaster.publish(map[string]any{"phase": "ready", "ready": true})

	res := callWrapped(t, s, echoHandler, "search_symbols")
	require.NotEmpty(t, res.Content)
	tc := res.Content[0].(mcp.TextContent)
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &obj))

	assert.Equal(t, float64(1), obj["total"])
	_, hasWarming := obj["warming"]
	assert.False(t, hasWarming, "a ready daemon must not decorate results")
}

// TestWrapToolHandler_GraphToolNoWarmupPipeline — with no readiness
// phase ever published (single-process server), graph tools pass
// through cleanly.
func TestWrapToolHandler_GraphToolNoWarmupPipeline(t *testing.T) {
	s := fastPathTestServer(t) // broadcaster wired but nothing published
	res := callWrapped(t, s, echoHandler, "search_symbols")
	require.NotEmpty(t, res.Content)
	tc := res.Content[0].(mcp.TextContent)
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &obj))
	_, hasWarming := obj["warming"]
	assert.False(t, hasWarming, "no warmup pipeline means no decoration")
}

// TestWrapToolHandler_ExemptToolUnaffectedDuringWarmup — a
// graph-independent tool dispatched mid-warmup is NOT decorated; its
// result passes through byte-for-byte.
func TestWrapToolHandler_ExemptToolUnaffectedDuringWarmup(t *testing.T) {
	s := fastPathTestServer(t)
	s.readinessBroadcaster.publish(map[string]any{"phase": "parallel_parse", "ready": false})

	res := callWrapped(t, s, echoHandler, "subscribe_workspace_readiness")
	require.NotEmpty(t, res.Content)
	tc := res.Content[0].(mcp.TextContent)
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &obj))

	assert.Equal(t, float64(1), obj["total"])
	_, hasWarming := obj["warming"]
	assert.False(t, hasWarming, "exempt tool must not be decorated even during warmup")
}

// TestWrapToolHandler_HandlerErrorNotDecorated — a handler that
// returns a transport error (non-nil err) is left untouched: the fast
// path only decorates a returned result, never a Go error.
func TestWrapToolHandler_HandlerErrorNotDecorated(t *testing.T) {
	s := fastPathTestServer(t)
	s.readinessBroadcaster.publish(map[string]any{"phase": "parallel_parse", "ready": false})

	wantErr := assert.AnError
	failing := func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, wantErr
	}
	wrapped := s.wrapToolHandler(failing)
	req := mcp.CallToolRequest{}
	req.Params.Name = "search_symbols"
	req.Params.Arguments = map[string]any{}
	res, err := wrapped(context.Background(), req)
	require.ErrorIs(t, err, wantErr, "transport error must propagate unchanged")
	assert.Nil(t, res)
}
