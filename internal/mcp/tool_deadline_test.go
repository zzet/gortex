package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/runtimeactivity"
)

// registerBlockingTool wires a tool that parks until release is closed, using
// the same addTool path every real tool takes so the test exercises the
// production middleware chain rather than a bypass.
func registerBlockingTool(srv *Server, name string, release <-chan struct{}, entered chan<- struct{}) {
	srv.addTool(
		mcplib.NewTool(name, mcplib.WithDescription("test-only blocking tool")),
		func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-time.After(30 * time.Second):
			}
			return mcplib.NewToolResultText("finally"), nil
		},
	)
}

// TestBlockedToolCallReturnsTerminalResult is the #300 contract: a handler that
// will not return must not become an open-ended silence. This is where the
// embedded stdio server used to lose the session — mcp-go hands every handler
// the server-lifetime context, so nothing downstream could ever cut the call
// off.
func TestBlockedToolCallReturnsTerminalResult(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.ToolCallTimeout = 100 * time.Millisecond

	release := make(chan struct{})
	defer close(release)
	registerBlockingTool(srv, "test_blocks_forever", release, make(chan struct{}, 1))

	start := time.Now()
	res := callToolByName(t, srv, context.Background(), "test_blocks_forever", nil)
	elapsed := time.Since(start)

	require.NotNil(t, res)
	assert.True(t, res.IsError, "an abandoned call must be reported as an error, not a silent empty success")
	text := toolText(res)
	assert.Contains(t, text, "deadline")
	assert.Contains(t, text, "side effect as unknown",
		"the agent must be told the abandoned work may still land")
	assert.Less(t, elapsed, 5*time.Second, "the bound must fire on its own budget, got %s", elapsed)
}

// TestSessionStaysResponsiveAfterBlockedToolCall is the property that actually
// rescues a session. One wedged handler used to occupy an mcp-go stdio worker
// permanently; once the pool and its queue were exhausted, tool calls ran
// inline on the stdin reader and the whole session stopped answering. The
// firewall returns the transport's slot immediately, so later calls still work.
func TestSessionStaysResponsiveAfterBlockedToolCall(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.ToolCallTimeout = 100 * time.Millisecond

	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 4)
	registerBlockingTool(srv, "test_blocks_forever", release, entered)

	for i := 0; i < 3; i++ {
		res := callToolByName(t, srv, context.Background(), "test_blocks_forever", nil)
		require.True(t, res.IsError)
	}
	<-entered // the handler really did run; this is not a registration no-op

	start := time.Now()
	res := callToolByName(t, srv, context.Background(), "index_health", map[string]any{"compact": true})
	require.NotNil(t, res)
	assert.False(t, res.IsError, "an unrelated tool must still answer: %s", toolText(res))
	assert.Less(t, time.Since(start), 5*time.Second)
}

// TestAbandonedToolCallsDrainWhenHandlerFinishes pins the leak accounting: a
// handler abandoned at its deadline is still running, and the counter that
// bounds how many of those may pile up has to come back down when it finishes.
// If it did not, a workspace that was merely slow once would eventually trip
// the fail-fast cap forever.
func TestAbandonedToolCallsDrainWhenHandlerFinishes(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.ToolCallTimeout = 50 * time.Millisecond

	release := make(chan struct{})
	registerBlockingTool(srv, "test_blocks_forever", release, make(chan struct{}, 1))

	before := AbandonedToolCalls()
	res := callToolByName(t, srv, context.Background(), "test_blocks_forever", nil)
	require.True(t, res.IsError)
	require.Eventually(t, func() bool { return AbandonedToolCalls() > before },
		2*time.Second, 5*time.Millisecond, "an abandoned handler must be accounted for")

	close(release)
	require.Eventually(t, func() bool { return AbandonedToolCalls() <= before },
		5*time.Second, 5*time.Millisecond, "the count must drain once the handler completes")
}

// TestCallerCancellationIsReportedAsCancellation keeps the two failure modes
// distinct. A client that hangs up is not the same event as a server-side
// deadline, and conflating them would make the logs useless for telling "the
// graph is wedged" from "the agent moved on".
func TestCallerCancellationIsReportedAsCancellation(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.ToolCallTimeout = 10 * time.Second

	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	registerBlockingTool(srv, "test_blocks_forever", release, entered)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-entered
		cancel()
	}()

	tool := srv.MCPServer().GetTool("test_blocks_forever")
	require.NotNil(t, tool)
	_, err := tool.Handler(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{Name: "test_blocks_forever"},
	})
	require.ErrorIs(t, err, context.Canceled)
}

// TestToolDeadlineFiresInsideTheTransportBudget: when the transport already
// imposed a deadline (the daemon socket's per-request lifetime), the handler
// bound must land first, so the client gets the tool-shaped diagnosis instead
// of an opaque JSON-RPC transport error.
func TestToolDeadlineFiresInsideTheTransportBudget(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.ToolCallTimeout = time.Hour // deliberately useless; the transport must win

	release := make(chan struct{})
	defer close(release)
	registerBlockingTool(srv, "test_blocks_forever", release, make(chan struct{}, 1))

	// A transport budget generous enough that subtracting the margin still
	// leaves a positive window.
	ctx, cancel := context.WithTimeout(context.Background(), transportDeadlineMargin+300*time.Millisecond)
	defer cancel()

	res := callToolByName(t, srv, ctx, "test_blocks_forever", nil)
	require.NotNil(t, res, "the handler bound must answer before the transport deadline")
	assert.True(t, res.IsError)
	assert.Contains(t, toolText(res), "deadline")
}

// TestToolDeadlineDisabled keeps the escape hatch honest for operators running
// legitimately long analyze / ask calls.
func TestToolDeadlineDisabled(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.ToolCallTimeout = -1

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	registerBlockingTool(srv, "test_blocks_forever", release, entered)

	done := make(chan *mcplib.CallToolResult, 1)
	go func() {
		done <- callToolByName(t, srv, context.Background(), "test_blocks_forever", nil)
	}()
	<-entered
	select {
	case <-done:
		t.Fatal("with the bound disabled the call must not be cut off")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	res := <-done
	assert.Contains(t, toolText(res), "finally")
}

func TestToolCallTimeoutFromEnv(t *testing.T) {
	t.Setenv(ToolCallTimeoutEnv, "")
	assert.Equal(t, DefaultToolCallTimeout, toolCallTimeoutFromEnv())
	t.Setenv(ToolCallTimeoutEnv, "5s")
	assert.Equal(t, 5*time.Second, toolCallTimeoutFromEnv())
	t.Setenv(ToolCallTimeoutEnv, "off")
	assert.Zero(t, toolCallTimeoutFromEnv())
	t.Setenv(ToolCallTimeoutEnv, "nonsense")
	assert.Equal(t, DefaultToolCallTimeout, toolCallTimeoutFromEnv(),
		"an unparseable value must fall back to the default, never to unbounded")
}

// TestIndexHealthPayloadHonoursCancellation covers the specific handler both
// issue reports named. Its cost is proportional to workspace size — one stat
// per tracked file plus a graph scan — and before this it checked nothing, so a
// client that had already given up still paid for the whole sweep.
func TestIndexHealthPayloadHonoursCancellation(t *testing.T) {
	srv, _ := setupTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	payload, err := srv.buildIndexHealthPayloadCtx(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, payload)

	// The uncancelled path is unchanged.
	payload, err = srv.buildIndexHealthPayloadCtx(context.Background())
	require.NoError(t, err)
	require.NotNil(t, payload)
	assert.Contains(t, payload, "health_score")
}

// TestIndexHealthToolReportsCancellation: the tool must explain itself rather
// than return a half-built payload that reads as a healthy index.
func TestIndexHealthToolReportsCancellation(t *testing.T) {
	srv, _ := setupTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := srv.handleIndexHealth(ctx, mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{Name: "index_health"},
	})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.True(t, strings.Contains(toolText(res), "cancelled"), "got %q", toolText(res))
}

// registerBlockingResource wires a resource through the production registrar so
// the test exercises the wrapper the server actually installs.
func registerBlockingResource(srv *Server, uri string, release <-chan struct{}) {
	srv.addResource(
		mcplib.NewResource(uri, "test-only blocking resource"),
		func(_ context.Context, req mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
			select {
			case <-release:
			case <-time.After(30 * time.Second):
			}
			return jsonResource(req.Params.URI, map[string]any{"ok": true})
		},
	)
}

// TestBlockedResourceReadReturnsTerminalError covers the half of #300 that a
// tool-only firewall would miss. mcp-go's stdio transport queues tool calls onto
// a worker pool but runs resources/read inline on the goroutine that reads
// stdin — so a blocked resource does not hang one call, it stops the session
// reading its own input.
func TestBlockedResourceReadReturnsTerminalError(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.ToolCallTimeout = 100 * time.Millisecond

	release := make(chan struct{})
	defer close(release)
	registerBlockingResource(srv, "gortex://test-blocking", release)

	start := time.Now()
	rpcErr := readResourceExpectingError(t, srv, "gortex://test-blocking")
	assert.Contains(t, rpcErr, "deadline")
	assert.Less(t, time.Since(start), 5*time.Second)
}

// readResourceExpectingError drives a real resources/read frame through the
// server's dispatch — the path the transports use — and returns the JSON-RPC
// error message. Invoking the handler function directly would bypass the
// registrar wrapper this is meant to exercise.
func readResourceExpectingError(t *testing.T, srv *Server, uri string) string {
	t.Helper()
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "resources/read",
		"params":  map[string]any{"uri": uri},
	})
	require.NoError(t, err)

	reply := srv.MCPServer().HandleMessage(context.Background(), frame)
	require.NotNil(t, reply, "resources/read must produce a reply")
	encoded, err := json.Marshal(reply)
	require.NoError(t, err)

	var envelope struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(encoded, &envelope))
	require.NotNilf(t, envelope.Error, "a blocked/panicking read must fail, not succeed: %s", encoded)
	return envelope.Error.Message
}

// TestPanickingResourceReadIsContained: moving handlers onto their own goroutine
// takes them out from under whatever recover the transport supplied, so the
// firewall has to bring its own — otherwise one panicking read kills the daemon.
func TestPanickingResourceReadIsContained(t *testing.T) {
	srv, _ := setupTestServer(t)

	srv.addResource(
		mcplib.NewResource("gortex://test-panic", "test-only panicking resource"),
		func(_ context.Context, _ mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
			panic("boom")
		},
	)

	assert.Contains(t, readResourceExpectingError(t, srv, "gortex://test-panic"), "internal error")
}

// TestFailsFastWhenTooManyHandlersAreStuck pins the ceiling on detached work.
// Abandoning a stuck handler frees the transport but leaves a goroutine behind;
// past the cap the honest answer is to refuse, not to keep piling them up.
func TestFailsFastWhenTooManyHandlersAreStuck(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.ToolCallTimeout = 5 * time.Second // long enough that only the cap can trip

	abandonedToolCalls.Add(maxAbandonedToolCalls)
	defer abandonedToolCalls.Add(-maxAbandonedToolCalls)

	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	registerBlockingTool(srv, "test_blocks_forever", release, entered)

	start := time.Now()
	res := callToolByName(t, srv, context.Background(), "test_blocks_forever", nil)
	require.NotNil(t, res)
	assert.True(t, res.IsError)
	assert.Contains(t, toolText(res), "not serving tool calls")
	assert.Less(t, time.Since(start), time.Second, "the cap must refuse immediately, not wait out the budget")

	select {
	case <-entered:
		t.Fatal("a refused call must not reach the handler")
	default:
	}
}

// TestResourceAndPromptHandlersJoinTheActivityBracket is the regression for
// the gap issue #278's reproducer named: resources and prompts run the same
// whole-graph passes tools do (gortex://report materialises AllEdges; the
// orientation prompt runs scopedNodes twice), but carried only the hang
// firewall. Without the bracket the process reads as idle mid-scan, so a
// scheduled FreeOSMemory can land on top of the scan and nothing schedules
// a release when it finishes.
func TestResourceAndPromptHandlersJoinTheActivityBracket(t *testing.T) {
	const childEnv = "GORTEX_TEST_ACTIVITY_BRACKET_CHILD"
	if os.Getenv(childEnv) != "1" {
		// The tracker is process-wide: other tests can still own background
		// work. Reuse the bounded test-process pattern for exact ownership.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0],
			"-test.run=^TestResourceAndPromptHandlersJoinTheActivityBracket$", "-test.count=1")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		output, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "activity-bracket subprocess failed (context=%v):\n%s", ctx.Err(), output)
		return
	}
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	// A legitimate non-MCP owner reproduces the old assertion's false leak:
	// global activity must remain nonzero after our own bracket has ended.
	runtimeactivity.Begin("analysis")
	defer runtimeactivity.End("analysis")
	require.EqualValues(t, 1, runtimeactivity.Current().Active)
	s := &Server{logger: zap.NewNop()}

	var duringResource runtimeactivity.Snapshot
	resource := s.boundResourceHandler("gortex://probe",
		func(context.Context, mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
			duringResource = runtimeactivity.Current()
			return nil, nil
		})
	if _, err := resource(context.Background(), mcplib.ReadResourceRequest{}); err != nil {
		t.Fatalf("resource handler: %v", err)
	}
	assert.EqualValues(t, 1, duringResource.ByKind["mcp"], "resource owns its bracket independently of background activity")
	assert.EqualValues(t, 2, duringResource.Active)
	afterResource := runtimeactivity.Current()
	assert.Zero(t, afterResource.ByKind["mcp"], "resource bracket ended before return")
	assert.EqualValues(t, 1, afterResource.ByKind["analysis"], "unrelated owner stays active")
	assert.EqualValues(t, 1, afterResource.Active, "nonzero process activity is not an owned resource leak")

	var duringPrompt runtimeactivity.Snapshot
	prompt := s.boundPromptHandler("probe",
		func(context.Context, mcplib.GetPromptRequest) (*mcplib.GetPromptResult, error) {
			duringPrompt = runtimeactivity.Current()
			return &mcplib.GetPromptResult{}, nil
		})
	if _, err := prompt(context.Background(), mcplib.GetPromptRequest{}); err != nil {
		t.Fatalf("prompt handler: %v", err)
	}
	assert.EqualValues(t, 1, duringPrompt.ByKind["mcp"], "prompt owns its bracket independently of background activity")
	assert.EqualValues(t, 2, duringPrompt.Active)
	afterPrompt := runtimeactivity.Current()
	assert.Zero(t, afterPrompt.ByKind["mcp"], "prompt bracket ended before return")
	assert.EqualValues(t, 1, afterPrompt.ByKind["analysis"], "unrelated owner stays active")
	assert.EqualValues(t, 1, afterPrompt.Active, "nonzero process activity is not an owned prompt leak")
}
