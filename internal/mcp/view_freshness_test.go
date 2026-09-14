package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// W5.9. The shipped agent instructions advertise three request-level knobs —
// require_exact, require_fresh and an absolute RFC3339 wait_deadline. Before
// this item require_fresh and wait_deadline were implemented nowhere, and
// require_exact was refused on every guarded tool (closed schema +
// wrapToolArgGuard), which made the already-shipped knob unusable.
//
// The knobs are accepted on every tool and declared in no tool's schema: three
// more properties on every registered tool would grow every tools/list, and
// the compact / agent / localization surfaces are byte budgeted. The guard
// admits them from one allowlist (requestFreshnessArgKeys) and the contract is
// stated in the shipped instructions. These tests pin both halves and the
// wait's behaviour.

// ----------------------------------------------------------- acceptance ---

// Every guarded tool must accept all three knobs. The guard rejects any key
// that is neither declared nor admitted, so a knob missing from the allowlist
// is an unusable knob — which is exactly what require_exact was.
func TestGuardedToolsAcceptTheFreshnessKnobsWithoutDeclaringThem(t *testing.T) {
	srv, _ := setupTestServer(t)

	srv.facades.mu.RLock()
	names := make([]string, 0, len(srv.facades.captured))
	tools := make(map[string]mcplib.Tool, len(srv.facades.captured))
	for name, captured := range srv.facades.captured {
		names = append(names, name)
		tools[name] = captured.tool
	}
	srv.facades.mu.RUnlock()
	sort.Strings(names)
	require.Greater(t, len(names), 50, "conformance test must cover the registered legacy catalog")

	guarded := 0
	for _, name := range names {
		tool := tools[name]
		if _, closed := toolArgGuardKeys(tool); !closed {
			continue
		}
		guarded++
		t.Run(name, func(t *testing.T) {
			args := map[string]any{}
			for _, knob := range requestFreshnessArgNames {
				// The knobs stay out of every published schema: that is the
				// byte contract tools_list_budget_test.go and
				// facade_tools_test.go hold the whole surface to.
				require.NotContains(t, tool.InputSchema.Properties, knob,
					"%s publishes %q into its input schema, which grows every tools/list carrying it", name, knob)
				args[knob] = knobValue(knob)
			}
			// The guard is the thing that has to admit them instead.
			ran := false
			handler := wrapToolArgGuard(tool, func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				ran = true
				return mcplib.NewToolResultText("ok"), nil
			})
			t.Setenv(toolArgGuardEnv, "reject")
			res, err := handler(context.Background(), freshnessRequestFor(name, args))
			require.NoError(t, err)
			require.False(t, res.IsError, "%s refused a request-level knob: %s", name, guardResultText(res))
			require.True(t, ran, "%s never dispatched", name)
		})
	}
	require.Greater(t, guarded, 50, "the guarded population must be what this test covered")
}

// Nothing publishes the knobs into a schema, so the shipped views guide is the
// discovery path a client actually has. It has to name all three, say where
// they go, and state the limitation the rider reports — an instruction surface
// that over-promises is how a caller ends up trusting a stale committed base.
func TestViewsGuideStatesTheRequestLevelFreshnessContract(t *testing.T) {
	for _, required := range []string{
		requireExactArgName,
		requireFreshArgName,
		waitDeadlineArgName,
		"request-level arguments",             // where they are sent
		"published in no tool's input schema", // why tools/list does not show them
		freshReasonCommittedBaseAdvance,       // the limitation the rider reports
		freshReasonDeadlineExceeded,
		freshReasonInterrupted,
		freshReasonCoordinatorUnavailable,
		freshReasonPublicationFailed,
		freshReasonRouteWithdrawn,
		freshReasonWaitTargetUnavailable,
		freshReasonCheckoutRootChanged,
		freshReasonCheckoutRefreshStopped,
		// The reason a caller must be able to tell apart from
		// deadline_exceeded: extending wait_deadline is the obvious move for
		// one and the useless move for the other, and the rider is all the
		// caller sees.
		freshReasonRefreshAdmissionAbandoned,
		// fresh:true is a claim about which route answered, not only about
		// what the coordinator did. A caller cannot check that itself — it
		// sees one rider — so the condition has to be written down.
		"the same checkout, served exactly, at the same route epoch",
		// require_exact + require_fresh refuses on EVERY unfresh outcome, not
		// only on an expired deadline. A caller that reads require_exact as
		// "never hand me something I did not ask for" must find that written
		// down, because the alternative reading — a stale route with
		// fresh:false and no refusal — is the one that silently loses data.
		"every " + "`fresh:false`" + " outcome refuses",
	} {
		require.Contains(t, guideViews, required,
			"the views guide is the only place these knobs are documented, and it omits %q", required)
	}
	// The guide must not claim a declaration that does not exist: the whole
	// point of the allowlist is that the schemas stay as they were.
	require.NotContains(t, guideViews, "declare them in their input schema",
		"the guide claims a schema declaration no tool carries")
}

// knobValue is a well-formed value for one request-level knob.
func knobValue(knob string) any {
	if knob == waitDeadlineArgName {
		return time.Now().Add(time.Minute).Format(time.RFC3339)
	}
	return true
}

func freshnessRequestFor(tool string, args map[string]any) mcplib.CallToolRequest {
	req := mcplib.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	return req
}

// The production entrypoint: a real tools/call frame through the registered
// MCP server, under the reject opt-in, carrying all three knobs. Before the
// guard admitted them every one was an unknown key and the call was refused
// before the handler ran.
func TestGuardedToolDispatchAcceptsTheFreshnessKnobs(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full") // legacy names are session-gated off the default surface
	srv, _ := setupTestServer(t)
	ctx := guardE2ESession(t, srv, "freshness_knob_e2e")
	t.Setenv(toolArgGuardEnv, "reject")

	res := guardE2ECall(t, srv, ctx, 2, "read_file", map[string]any{
		"path":              "main.go",
		requireExactArgName: true,
		requireFreshArgName: false,
		waitDeadlineArgName: time.Now().Add(time.Minute).Format(time.RFC3339),
	})
	text := guardResultText(res)
	require.False(t, res.IsError, "the guard refused a published request-level knob: %s", text)
	// The handler really ran, so this cannot pass vacuously on a call that
	// was refused or never dispatched.
	require.Contains(t, text, "func helper", "read_file did not answer: %s", text)
	require.NotContains(t, text, "does not accept option", text)
	require.NotContains(t, text, "_ignored_options",
		"a published knob still warns as an ignored option: %s", text)
}

// ----------------------------------------------------------------- parse ---

func freshnessRequest(args map[string]any) *mcplib.CallToolRequest {
	req := &mcplib.CallToolRequest{}
	req.Params.Name = "get_symbol"
	req.Params.Arguments = args
	return req
}

func TestWaitDeadlineMustBeAbsoluteRFC3339InTheFuture(t *testing.T) {
	future := time.Now().Add(time.Minute)

	t.Run("accepts an absolute future timestamp", func(t *testing.T) {
		parsed := takeRequestFreshness(freshnessRequest(map[string]any{
			requireFreshArgName: true,
			waitDeadlineArgName: future.Format(time.RFC3339Nano),
		}))
		require.NoError(t, parsed.err)
		require.True(t, parsed.requireFresh)
		require.True(t, parsed.hasDeadline)
		require.WithinDuration(t, future, parsed.deadline, time.Second)
	})

	t.Run("reads the knobs out of a facade options envelope", func(t *testing.T) {
		parsed := takeRequestFreshness(freshnessRequest(map[string]any{
			"options": map[string]any{
				requireExactArgName: true,
				requireFreshArgName: true,
				waitDeadlineArgName: future.Format(time.RFC3339Nano),
			},
		}))
		require.NoError(t, parsed.err)
		require.True(t, parsed.requireExact)
		require.True(t, parsed.requireFresh)
		require.True(t, parsed.hasDeadline)
	})

	for name, value := range map[string]any{
		"a relative duration":    "30s",
		"a date without a time":  "2031-01-02",
		"an empty string":        "   ",
		"a non-string":           int64(1767225600),
		"a past absolute time":   time.Now().Add(-time.Minute).Format(time.RFC3339Nano),
		"a naive local datetime": "2031-01-02T03:04:05",
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			parsed := takeRequestFreshness(freshnessRequest(map[string]any{
				requireFreshArgName: true,
				waitDeadlineArgName: value,
			}))
			require.Error(t, parsed.err)
			require.Equal(t, graphview.CodeInvalidViewSelector, graphview.CodeOf(parsed.err))
			require.False(t, parsed.hasDeadline)
			require.Contains(t, parsed.err.Error(), waitDeadlineArgName)
		})
	}

	t.Run("an unbounded wait still carries a default ceiling", func(t *testing.T) {
		parsed := takeRequestFreshness(freshnessRequest(map[string]any{requireFreshArgName: true}))
		require.NoError(t, parsed.err)
		require.False(t, parsed.hasDeadline)
		now := time.Now()
		require.WithinDuration(t, now.Add(freshnessDefaultWait), parsed.effectiveDeadline(now, context.Background()), time.Second)
	})

	// The request's own deadline is the hang firewall's (boundToolHandler).
	// Waiting right up to it means the firewall fires at the same instant the
	// wait ends, the firewall usually wins, and the call is abandoned — the
	// caller gets the firewall's diagnosis and no freshness rider, and the
	// call is counted into the process-wide abandoned-call budget. The clamp
	// keeps transportDeadlineMargin of room for the answer to get out.
	t.Run("the clamp leaves the firewall room to answer", func(t *testing.T) {
		parsed := takeRequestFreshness(freshnessRequest(map[string]any{
			requireFreshArgName: true,
			waitDeadlineArgName: time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		}))
		now := time.Now()
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(10*time.Second))
		defer cancel()
		effective := parsed.effectiveDeadline(now, ctx)
		require.WithinDuration(t, now.Add(10*time.Second-transportDeadlineMargin), effective, 100*time.Millisecond)
		require.True(t, effective.Before(now.Add(10*time.Second)),
			"the wait runs to the request's own deadline and races the hang firewall")
	})

	t.Run("a caller's own shorter bound is left alone", func(t *testing.T) {
		parsed := takeRequestFreshness(freshnessRequest(map[string]any{
			requireFreshArgName: true,
			waitDeadlineArgName: time.Now().Add(2 * time.Second).Format(time.RFC3339Nano),
		}))
		now := time.Now()
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(time.Hour))
		defer cancel()
		require.WithinDuration(t, now.Add(2*time.Second), parsed.effectiveDeadline(now, ctx), 100*time.Millisecond)
	})
}

// And the same clamp through the production chain: a wait_deadline well past
// the hang firewall's budget must come back as a freshness answer, not as an
// abandoned call. boundToolHandler is the outermost layer of wrapToolHandler,
// so this is the real race the clamp exists to avoid.
func TestRequireFreshAnswersInsideTheHangFirewall(t *testing.T) {
	stack := newViewStack(t)
	stack.srv.ToolCallTimeout = 1200 * time.Millisecond
	stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
		answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			return pendingTicket(checkoutID, root), nil
		},
	}
	args := map[string]any{
		requireFreshArgName: true,
		// Far past the firewall's budget: only the clamp can end this wait
		// early enough for an answer to be rendered.
		waitDeadlineArgName: time.Now().Add(10 * time.Minute).Format(time.RFC3339Nano),
	}
	started := time.Now()
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", args, captureReader(stack.srv, new(graph.Reader)))
	elapsed := time.Since(started)
	require.NoError(t, err)
	require.False(t, res.IsError, "the call was abandoned instead of answered: %s", viewResultText(t, res))

	rider := resultFreshness(t, res)
	require.Equal(t, false, rider["fresh"], "rider = %v", rider)
	require.Equal(t, freshReasonDeadlineExceeded, rider["fresh_reason"], "rider = %v", rider)
	require.GreaterOrEqual(t, elapsed, 100*time.Millisecond, "the request did not wait at all")
	require.Less(t, elapsed, 900*time.Millisecond,
		"the wait ran into the hang firewall's own deadline instead of stopping a margin short of it")
}

// ------------------------------------------------------------------ wait ---

// freshnessCall is one admission the request made against the coordinator.
type freshnessCall struct {
	checkoutID string
	root       string
}

// fakeFreshnessWaiter stands in for the checkout lifecycle so a test can drive
// the settle signal's outcomes without a live coordinator. It records what the
// production path asked for, which is the wiring trace.
type fakeFreshnessWaiter struct {
	mu     sync.Mutex
	calls  []freshnessCall
	answer func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error)
	// answerCtx is answer for a case that has to see the context the wait
	// hands the coordinator. A real coordinator's bounds are derived from it,
	// so a test that means "the CALLER's bound ended" says so by waiting for
	// this context rather than by sleeping past a wall-clock instant.
	answerCtx func(ctx context.Context, call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error)
}

func (f *fakeFreshnessWaiter) RequestCheckoutRefresh(
	ctx context.Context,
	checkoutID, expectedRoot string,
) (*indexer.CheckoutRefreshTicket, error) {
	f.mu.Lock()
	f.calls = append(f.calls, freshnessCall{checkoutID: checkoutID, root: expectedRoot})
	n := len(f.calls)
	answer, answerCtx := f.answer, f.answerCtx
	f.mu.Unlock()
	if answerCtx != nil {
		return answerCtx(ctx, n, checkoutID, expectedRoot)
	}
	if answer == nil {
		return nil, indexer.ErrCheckoutRefreshStopped
	}
	return answer(n, checkoutID, expectedRoot)
}

func (f *fakeFreshnessWaiter) observed() []freshnessCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]freshnessCall(nil), f.calls...)
}

// settledTicket is the coordinator's success: a route that describes the tree
// it sampled.
func settledTicket(checkoutID, root string, generation uint64) *indexer.CheckoutRefreshTicket {
	done := make(chan indexer.MutationResult, 1)
	done <- indexer.MutationResult{RequestedGeneration: generation, AppliedGeneration: generation, Reindexed: true}
	close(done)
	return &indexer.CheckoutRefreshTicket{
		CheckoutID: checkoutID,
		Root:       root,
		Ticket:     &indexer.MutationTicket{Path: root, Generation: generation, Done: done},
	}
}

// pendingTicket never completes: the build outlives the caller's deadline.
func pendingTicket(checkoutID, root string) *indexer.CheckoutRefreshTicket {
	return &indexer.CheckoutRefreshTicket{
		CheckoutID: checkoutID,
		Root:       root,
		Ticket:     &indexer.MutationTicket{Path: root, Generation: 1, Done: make(chan indexer.MutationResult)},
	}
}

// writeCaughtUpDirtyGeneration publishes a second working-tree generation
// carrying a symbol no earlier layer has, so "the request re-read the route
// after the wait" is observable in the answer rather than only in a rider.
func writeCaughtUpDirtyGeneration(t *testing.T, stack *viewStack) int64 {
	t.Helper()
	generationID, handle, err := stack.store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          stack.graphID,
		LayerID:          "layer-view-dirty-caught-up",
		CheckoutID:       viewTestWorktree,
		GenerationKind:   "dirty",
		BaseGenerationID: stack.commit,
		TreeOID:          "tree-view-dirty-caught-up",
		CreatedAt:        5000,
	})
	require.NoError(t, err)
	handle.AddBatch([]*graph.Node{
		viewFileNode("repo/keep.go", 12),
		viewRepoNode("repo/keep.go::Keeper", "Keeper", graph.KindFunction, "repo/keep.go", 3),
		viewRepoNode("repo/keep.go::AfterTheWait", "AfterTheWait", graph.KindFunction, "repo/keep.go", 9),
	}, []*graph.Edge{
		{From: "repo/keep.go", To: "repo/keep.go::AfterTheWait", Kind: graph.EdgeContains, FilePath: "repo/keep.go", Line: 9},
	})
	require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: "repo", FilePath: "repo/keep.go", Mode: store_sqlite.OwnershipReplace},
	}))
	require.NoError(t, stack.store.PublishPayloadGeneration(context.Background(), generationID, 6000))
	return generationID
}

func freshArgs(extra map[string]any, deadline time.Duration) map[string]any {
	args := map[string]any{
		requireFreshArgName: true,
		waitDeadlineArgName: time.Now().Add(deadline).Format(time.RFC3339Nano),
	}
	for name, value := range extra {
		args[name] = value
	}
	return args
}

// The headline behaviour, driven through the whole tool middleware: the route
// is behind the working copy, require_fresh waits on the coordinator's settle
// signal, the coordinator publishes, and the request re-reads the route it
// waited for instead of answering out of the stack it selected first.
func TestRequireFreshReadsTheRouteItWaitedFor(t *testing.T) {
	stack := newViewStack(t)
	caughtUp := writeCaughtUpDirtyGeneration(t, stack)
	waiter := &fakeFreshnessWaiter{
		answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			// The coordinator publishes, then reports. Both halves happen
			// before the request is allowed to look at the route again.
			routeViewCheckout(t, stack.store, stack.graphID, stack.commit, caughtUp, store_sqlite.RouteActive)
			return settledTicket(checkoutID, root, uint64(caughtUp)), nil
		},
	}
	stack.srv.freshnessWaiter = waiter

	var reader graph.Reader
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", freshArgs(nil, time.Minute), captureReader(stack.srv, &reader))
	require.NoError(t, err)
	require.False(t, res.IsError, "a satisfied freshness wait must not refuse: %s", viewResultText(t, res))

	calls := waiter.observed()
	require.Len(t, calls, 1, "the production request path must reach the settle signal exactly once")
	require.Equal(t, viewTestWorktree, calls[0].checkoutID, "the wait must name the routed checkout")
	require.Equal(t, stack.worktreeRoot, calls[0].root, "the wait must name the checkout root it was admitted against")

	require.True(t, hasNode(reader, "repo/keep.go::AfterTheWait"),
		"the request answered out of the generation stack it selected before the wait")
	require.False(t, hasNode(reader, "repo/keep.go::Dirty"),
		"the superseded working-tree generation is still being read")

	rider := resultFreshness(t, res)
	require.Equal(t, true, rider["fresh"], "rider = %v", rider)
	require.Equal(t, true, rider["exact"], "rider = %v", rider)
	require.NotContains(t, rider, "fresh_reason", "a fresh answer needs no excuse: %v", rider)
	require.Contains(t, rider, "waited_ms")
	require.Contains(t, rider, "wait_deadline")
}

// An expired bound is not a failure: the stale route still answers, and the
// rider says exactly what the caller did not get.
func TestRequireFreshDeadlineExpiryAnswersWithATruthfulRider(t *testing.T) {
	stack := newViewStack(t)
	waiter := &fakeFreshnessWaiter{
		answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			return pendingTicket(checkoutID, root), nil
		},
	}
	stack.srv.freshnessWaiter = waiter

	var reader graph.Reader
	deadline := time.Now().Add(200 * time.Millisecond)
	args := map[string]any{
		requireFreshArgName: true,
		waitDeadlineArgName: deadline.Format(time.RFC3339Nano),
	}
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", args, captureReader(stack.srv, &reader))
	require.NoError(t, err)
	require.False(t, res.IsError, "an expired wait without require_exact must still answer: %s", viewResultText(t, res))
	require.Len(t, waiter.observed(), 1)

	rider := resultFreshness(t, res)
	require.Equal(t, false, rider["fresh"], "rider = %v", rider)
	require.Equal(t, freshReasonDeadlineExceeded, rider["fresh_reason"], "rider = %v", rider)
	require.Equal(t, deadline.UTC().Format(time.RFC3339), rider["wait_deadline"], "rider = %v", rider)
	waited, ok := rider["waited_ms"].(float64)
	require.True(t, ok, "rider = %v", rider)
	require.GreaterOrEqual(t, waited, float64(100), "the request reported a wait it did not perform")
	// The route that answered is still the route that was asked for.
	require.True(t, hasNode(reader, "repo/keep.go::Dirty"))
}

// require_exact turns the same expiry into a refusal that names the bound, so
// a caller that must not read a stale route never silently does.
func TestRequireFreshDeadlineExpiryRefusesUnderRequireExact(t *testing.T) {
	stack := newViewStack(t)
	stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
		answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			return pendingTicket(checkoutID, root), nil
		},
	}
	deadline := time.Now().Add(150 * time.Millisecond)
	args := map[string]any{
		requireExactArgName: true,
		requireFreshArgName: true,
		waitDeadlineArgName: deadline.Format(time.RFC3339Nano),
	}
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", args, captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	assertToolError(t, res, graphview.CodeViewBuilding)
	text := viewResultText(t, res)
	require.Contains(t, text, deadline.UTC().Format(time.RFC3339), "the refusal must name the deadline: %s", text)
	require.Contains(t, text, waitDeadlineArgName, "the refusal must name the knob that bounded it: %s", text)
}

// A coordinator that cannot answer is reported as such rather than as a fresh
// route. The refusal is terminal: the wait must not spin on it.
//
// The error here is deliberately NOT one of the coordinator's two checkout
// sentinels. coordinator_unavailable says "nothing on this server can publish
// a checkout generation", which is only true for a refusal the server cannot
// attribute to this checkout;
// TestACheckoutThatCannotRefreshIsNotAnUnavailableCoordinator covers the two
// that it can.
func TestRequireFreshReportsAnUnavailableCoordinator(t *testing.T) {
	stack := newViewStack(t)
	waiter := &fakeFreshnessWaiter{
		answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			return nil, errFreshnessUnattributable
		},
	}
	stack.srv.freshnessWaiter = waiter
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", freshArgs(nil, time.Minute), captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	require.False(t, res.IsError)
	rider := resultFreshness(t, res)
	require.Equal(t, false, rider["fresh"], "rider = %v", rider)
	require.Equal(t, freshReasonCoordinatorUnavailable, rider["fresh_reason"], "rider = %v", rider)
	require.Len(t, waiter.observed(), 1, "a terminal refusal must not be retried until the deadline")
}

// A superseded ticket means the tree moved while the coordinator sampled it.
// The wait re-admits against the newer sample rather than reporting the older
// publication as this request's freshness.
func TestRequireFreshReadmitsASupersededTicket(t *testing.T) {
	stack := newViewStack(t)
	caughtUp := writeCaughtUpDirtyGeneration(t, stack)
	waiter := &fakeFreshnessWaiter{
		answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			if call == 1 {
				return nil, indexer.ErrCheckoutRefreshSuperseded
			}
			routeViewCheckout(t, stack.store, stack.graphID, stack.commit, caughtUp, store_sqlite.RouteActive)
			return settledTicket(checkoutID, root, uint64(caughtUp)), nil
		},
	}
	stack.srv.freshnessWaiter = waiter

	var reader graph.Reader
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", freshArgs(nil, time.Minute), captureReader(stack.srv, &reader))
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.Len(t, waiter.observed(), 2, "the superseded admission was not retried against a newer sample")
	require.Equal(t, true, resultFreshness(t, res)["fresh"])
	require.True(t, hasNode(reader, "repo/keep.go::AfterTheWait"))
}

// A view that reads a committed base has no working-copy route to advance.
// Advancing a committed base on demand is a later item; until then the answer
// says so instead of implying a wait that never happened.
func TestRequireFreshOnACommittedBaseSaysItDidNotWait(t *testing.T) {
	type committedBaseCase struct {
		// build returns the session cwd and the request arguments.
		build func(stack *viewStack) (cwd string, args map[string]any)
		// carrier marks the case that reaches freshnessCarrier: selection
		// produced no view at all, so the rider the answer carries is one this
		// item invented a place for. Its exactness claim is therefore the one
		// that must restate auto's own resolution and nothing more.
		carrier bool
	}
	cases := map[string]committedBaseCase{
		"the shared corpus under a dedicated checkout's cwd": {
			build: func(stack *viewStack) (string, map[string]any) {
				return stack.repoRoot, freshArgs(nil, time.Minute)
			},
			carrier: true,
		},
		"a labelled base graph": {
			build: func(stack *viewStack) (string, map[string]any) {
				return stack.worktreeRoot, freshArgs(map[string]any{
					"view": map[string]any{"kind": "base", "graph_id": stack.graphID},
				}, time.Minute)
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stack := newViewStack(t)
			waiter := &fakeFreshnessWaiter{}
			stack.srv.freshnessWaiter = waiter
			cwd, args := tc.build(stack)
			var reader graph.Reader
			res, err := stack.callWithView(t, cwd, "get_symbol", args, captureReader(stack.srv, &reader))
			require.NoError(t, err)
			require.False(t, res.IsError, viewResultText(t, res))
			require.Empty(t, waiter.observed(), "a committed base must not be waited on")
			// Carrying a rider must not change what answered: the request still
			// reads the indexed corpus it read before require_fresh existed.
			require.True(t, hasNode(reader, "repo/edit.go::Old"),
				"the freshness rider carrier narrowed a base answer")
			rider := resultFreshness(t, res)
			require.NotNil(t, rider, "a require_fresh request was answered with no freshness rider at all")
			require.Equal(t, false, rider["fresh"], "rider = %v", rider)
			require.Equal(t, freshReasonCommittedBaseAdvance, rider["fresh_reason"], "rider = %v", rider)
			if !tc.carrier {
				return
			}
			// The carrier's live shape. Selection produced no view, so every
			// field here was written by freshnessCarrier — and a request that
			// selected nothing has no route to claim. It says what was asked
			// for and what the wait did, and nothing else.
			require.Equal(t, "auto", rider["requested_view"], "rider = %v", rider)
			require.NotContains(t, rider, "actual_view",
				"the carrier named a route the request never selected: %v", rider)
			require.NotContains(t, rider, "exact",
				"the carrier invented an exactness claim for a request that produced no view: %v", rider)
			require.NotContains(t, rider, "fallback_reason",
				"the carrier invented a fallback the request never took: %v", rider)
		})
	}
}

// Everything above is opt-in: a request that asked for nothing waits for
// nothing and its rider gains no freshness fields.
func TestRequestWithoutRequireFreshNeverWaits(t *testing.T) {
	stack := newViewStack(t)
	waiter := &fakeFreshnessWaiter{
		answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			t.Errorf("a request that did not ask for freshness reached the settle signal")
			return nil, indexer.ErrCheckoutRefreshStopped
		},
	}
	stack.srv.freshnessWaiter = waiter
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil, captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	require.Empty(t, waiter.observed())
	rider := resultFreshness(t, res)
	for _, field := range []string{"fresh", "waited_ms", "wait_deadline", "fresh_reason"} {
		require.NotContains(t, rider, field, "an ordinary request gained a freshness field: %v", rider)
	}
}

// A malformed bound is refused before anything is selected or waited on.
func TestMalformedWaitDeadlineRefusesTheRequest(t *testing.T) {
	stack := newViewStack(t)
	waiter := &fakeFreshnessWaiter{}
	stack.srv.freshnessWaiter = waiter
	args := map[string]any{requireFreshArgName: true, waitDeadlineArgName: "in 30 seconds"}
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", args, captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	assertToolError(t, res, graphview.CodeInvalidViewSelector)
	require.Contains(t, strings.ToLower(viewResultText(t, res)), waitDeadlineArgName)
	require.Empty(t, waiter.observed())
}

// …and for EVERY call shape, not only the ones that resolve a view the
// ordinary way. Two shapes used to slip through:
//
//   - a catalog-only checkout control (mutation_status / reindex_repository)
//     never calls resolveRequestView at all, so a refusal that lives there is
//     never reached;
//   - detect_changes with a checkout-scoped control converts any
//     resolveRequestView error into a degraded fallback view, which turned the
//     refusal into a silently degraded answer.
//
// A bound the server cannot parse is a request the server does not understand;
// answering it anyway is how a caller ends up reading a route it explicitly
// bounded. The parse therefore happens in the middleware, ahead of every
// branch.
func TestMalformedWaitDeadlineRefusesEveryCallShape(t *testing.T) {
	type shape struct {
		tool    string
		args    func(stack *viewStack) map[string]any
		prepare func(t *testing.T, stack *viewStack)
	}
	shapes := map[string]shape{
		"an ordinary routed read": {
			tool: "get_symbol",
			args: func(*viewStack) map[string]any { return map[string]any{} },
		},
		"a catalog-only checkout control": {
			tool: "mutation_status",
			args: func(*viewStack) map[string]any { return map[string]any{} },
		},
		"detect_changes over a checkout-scoped control with a pending graph": {
			tool: "detect_changes",
			args: func(*viewStack) map[string]any {
				args := worktreeViewArgs()
				args["scope"] = "unstaged"
				return args
			},
			// Deleting the route is what makes resolveRequestView refuse, and
			// therefore what arms the degrade carve-out this shape used to
			// take instead of refusing.
			prepare: func(t *testing.T, stack *viewStack) {
				require.NoError(t, stack.store.Catalog().DeleteCheckoutRoute(context.Background(), viewTestWorktree))
			},
		},
	}
	for name, tc := range shapes {
		t.Run(name, func(t *testing.T) {
			stack := newViewStack(t)
			if tc.prepare != nil {
				tc.prepare(t, stack)
			}
			waiter := &fakeFreshnessWaiter{}
			stack.srv.freshnessWaiter = waiter
			args := tc.args(stack)
			args[requireFreshArgName] = true
			args[waitDeadlineArgName] = "in 30 seconds"
			ran := false
			res, err := stack.callWithView(t, stack.repoRoot, tc.tool, args,
				func(context.Context) (*mcplib.CallToolResult, error) {
					ran = true
					return mcplib.NewToolResultText(`{"ok":true}`), nil
				})
			require.NoError(t, err)
			assertToolError(t, res, graphview.CodeInvalidViewSelector)
			require.Contains(t, viewResultText(t, res), waitDeadlineArgName)
			require.False(t, ran, "a request with an unparseable bound reached the handler anyway")
			require.Empty(t, waiter.observed(), "a request with an unparseable bound waited")
		})
	}
}

// ------------------------------------------------------- the stale lease ---

// The wait is the one request shape designed to be long, which makes it the
// worst one to hold a stale generation pinned through: the caller has just
// said the selected stack is not the one it wants, and the retirement sweep
// cannot collect a leased generation. The pre-wait view is therefore released
// BEFORE the wait blocks, and the answer comes from a view selected after it.
func TestRequireFreshReleasesTheStaleLeaseBeforeWaiting(t *testing.T) {
	stack := newViewStack(t)
	heldDuringWait := -1
	stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
		answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			// Sampled from inside the wait: this runs while the request is
			// blocked, which is exactly the window the stale pin used to span.
			heldDuringWait = stack.leases.Held()
			return pendingTicket(checkoutID, root), nil
		},
	}
	heldInHandler := -1
	deadline := time.Now().Add(200 * time.Millisecond)
	args := map[string]any{
		requireFreshArgName: true,
		waitDeadlineArgName: deadline.Format(time.RFC3339Nano),
	}
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", args,
		func(context.Context) (*mcplib.CallToolResult, error) {
			heldInHandler = stack.leases.Held()
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))

	require.Equal(t, 0, heldDuringWait,
		"the stale generation stack stayed leased for the whole wait_deadline")
	// The positive control: the observation above is not vacuous. The request
	// really does lease generations — it holds them while the handler reads,
	// which is what makes "zero during the wait" a statement about the wait.
	require.Greater(t, heldInHandler, 0,
		"nothing was ever leased, so the assertion above proves nothing")
}

// -------------------------------------------------- the withdrawn route ---

// A wait that the coordinator satisfied still has to be about the answer that
// comes back. If the route stops serving a view between the publication and
// the re-selection, the carrier is all that is left — and stamping the wait's
// success on it would claim freshness for a shared-corpus answer that the wait
// was never about.
func TestFreshWaitWhoseRouteWithdrewIsNotReportedFresh(t *testing.T) {
	stack := newViewStack(t)
	stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
		answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			return settledTicket(checkoutID, root, uint64(stack.dirty)), nil
		},
	}
	// A pre-wait view that names the routed checkout, so the wait is admitted
	// against it exactly as a routed request's would be…
	rider := graphview.NewViewRider(graphview.Selector{Kind: graphview.SelectorAuto})
	rider.CheckoutID = viewTestWorktree
	pre := &requestView{kind: requestViewKindBase, rider: rider}
	// …and a re-selection that resolves to the shared corpus with no view at
	// all: auto with no session cwd is selectRequestView's (nil, nil) arm.
	policy := requestViewPolicy{freshness: requestFreshness{requireFresh: true}}
	view, err := stack.srv.settleRequestFreshness(
		context.Background(), graphview.Selector{Kind: graphview.SelectorAuto}, policy, pre, nil)
	require.NoError(t, err)
	require.NotNil(t, view, "a require_fresh request must not be answered with silence")

	outcome := view.freshnessOutcome()
	require.NotNil(t, outcome)
	require.False(t, outcome.fresh,
		"the wait's success was claimed for an answer the wait was not about")
	require.Equal(t, freshReasonRouteWithdrawn, outcome.reason)
	require.True(t, view.routeless, "the carrier must make no route claim")
}

// ------------------------------------------- production waiter selection ---

// The seam every test above installs is a test seam. Nothing pinned which
// waiter production picks when no seam is installed, so a checkoutFreshness()
// that stopped returning s.lifecycle would answer every require_fresh request
// with a permanently truthful-looking fresh:false /
// fresh_reason:"coordinator_unavailable" and no test would notice.
//
// newViewStack hands NewServer a MultiIndexer and no CheckoutLifecycle, so
// server.go builds the real one (server.go:1809-1822). This test installs no
// seam: the request must reach that lifecycle, whose coordinator has nothing
// running for the fixture checkout, so the ticket never completes and the
// bound — not the absence of a waiter — is what ends the wait.
func TestRequireFreshWithoutASeamUsesTheCheckoutLifecycle(t *testing.T) {
	stack := newViewStack(t)
	require.Nil(t, stack.srv.freshnessWaiter, "this test must exercise the production selection")
	require.NotNil(t, stack.srv.lifecycle, "the fixture server must carry the checkout lifecycle")

	selected := stack.srv.checkoutFreshness()
	require.NotNil(t, selected, "production require_fresh has no settle signal to wait on")
	lifecycle, ok := selected.(*indexer.CheckoutLifecycle)
	require.True(t, ok, "production picked %T rather than the checkout lifecycle", selected)
	require.Same(t, stack.srv.lifecycle, lifecycle)

	deadline := time.Now().Add(400 * time.Millisecond)
	args := map[string]any{
		requireFreshArgName: true,
		waitDeadlineArgName: deadline.Format(time.RFC3339Nano),
	}
	started := time.Now()
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", args, captureReader(stack.srv, new(graph.Reader)))
	elapsed := time.Since(started)
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))

	rider := resultFreshness(t, res)
	require.Equal(t, false, rider["fresh"], "rider = %v", rider)
	require.Equal(t, freshReasonDeadlineExceeded, rider["fresh_reason"],
		"the request never reached the live coordinator: rider = %v", rider)
	require.NotEqual(t, freshReasonCoordinatorUnavailable, rider["fresh_reason"], "rider = %v", rider)
	require.GreaterOrEqual(t, elapsed, 300*time.Millisecond,
		"the request answered without waiting on the live coordinator at all")
}

// ------------------------------------------------- the undeclared residual ---

// Nothing publishes the knobs into a tool schema, so on EVERY tool — facade and
// legacy alike — they are keys the tool does not declare, and that is exactly
// the population reconcileArgKeys rewrites. The middleware parses them before
// reconciliation runs (overlay.go), so a collision could not silently steal a
// knob from the request; it would still corrupt the handler's own arguments by
// moving a bool or a timestamp onto a real parameter. Today no tool's parameter
// is close enough to collide, and this is what keeps it that way.
func TestNoToolAliasesTheFreshnessKnobs(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full")
	srv, _ := setupTestServer(t)
	names := srv.liveToolNames()
	covered, facades := 0, 0
	for _, name := range names {
		real := srv.toolParamNames(name)
		if len(real) == 0 {
			continue
		}
		covered++
		if isFacadeToolName(name) {
			facades++
		}
		t.Run(name, func(t *testing.T) {
			for _, knob := range requestFreshnessArgNames {
				args := map[string]any{knob: knobValue(knob)}
				rewrites := reconcileArgKeys(args, real)
				require.Empty(t, rewrites,
					"%s aliases the request-level knob %q onto one of its own parameters", name, knob)
				require.Contains(t, args, knob,
					"%s lost the request-level knob %q to parameter reconciliation", name, knob)
			}
		})
	}
	require.Greater(t, covered, 50, "the live tool surface must be what this test covered")

	// Positive control: the assertions above are not vacuous. A tool whose own
	// parameter sits inside the matcher's edit-distance budget of a knob does
	// get the knob rewritten onto it — which is exactly the collision the loop
	// above proves no tool currently has.
	collision := toolParams{"require_fres": "boolean"}
	args := map[string]any{requireFreshArgName: true}
	require.NotEmpty(t, reconcileArgKeys(args, collision),
		"the alias matcher no longer rewrites a near-miss, so this test proves nothing")
	require.NotContains(t, args, requireFreshArgName)
}

// The compact facade surface is the default one, and it declares nothing
// either — yet an undeclared knob there is still honoured by the request
// middleware. This drives a facade name through the same wrapToolHandler chain
// a real call takes — reconcileToolParams included — and requires that
// require_fresh reached the settle signal.
func TestFacadeToolHonoursTheUndeclaredFreshnessKnobs(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1") // a facade name is gated off the legacy surface
	stack := newViewStack(t)
	require.True(t, isFacadeToolName("read"), "this test must exercise a facade name")
	waiter := &fakeFreshnessWaiter{
		answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			return pendingTicket(checkoutID, root), nil
		},
	}
	stack.srv.freshnessWaiter = waiter

	deadline := time.Now().Add(150 * time.Millisecond)
	args := map[string]any{
		requireFreshArgName: true,
		waitDeadlineArgName: deadline.Format(time.RFC3339Nano),
	}
	res, err := stack.callWithView(t, stack.worktreeRoot, "read", args, captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))

	calls := waiter.observed()
	require.Len(t, calls, 1, "a facade tool's require_fresh never reached the settle signal")
	require.Equal(t, viewTestWorktree, calls[0].checkoutID)
	rider := resultFreshness(t, res)
	require.Equal(t, false, rider["fresh"], "rider = %v", rider)
	require.Equal(t, freshReasonDeadlineExceeded, rider["fresh_reason"], "rider = %v", rider)
}

// ------------------------------------------------ the rider carrier claim ---

// The carrier exists so a require_fresh request that resolved to the shared
// corpus has somewhere to say what the wait did. It must not become a place
// where a route claim is invented: a request that produced no view has no route
// to be exact or inexact about. Only SelectorAuto gets a carrier at all — it is
// the only selector selectRequestView answers (nil, nil) for — and even auto's
// carrier names no actual view and claims no exactness.
func TestFreshnessCarrierNeverInventsARouteClaim(t *testing.T) {
	stack := newViewStack(t)

	t.Run("auto is carried without a route claim", func(t *testing.T) {
		carrier := stack.srv.freshnessCarrier(graphview.Selector{Kind: graphview.SelectorAuto}, nil)
		require.NotNil(t, carrier, "a require_fresh auto request must not be answered with silence")
		require.NotNil(t, carrier.rider)
		require.True(t, carrier.routeless, "a carrier with no selected view must be marked routeless")
		require.Empty(t, carrier.rider.ActualView, "the carrier named a route nothing selected")
		require.Empty(t, carrier.rider.FallbackReason)
		// And the rendering drops the claim rather than relying on the zero
		// value of a bool that defaults to true.
		fields := viewRiderFields(carrier)
		require.NotContains(t, fields, "exact", "fields = %v", fields)
		require.NotContains(t, fields, "actual_view", "fields = %v", fields)
		require.Equal(t, "auto", fields["requested_view"], "fields = %v", fields)
	})

	for _, selector := range []graphview.Selector{
		{Kind: graphview.SelectorWorktree, CheckoutID: viewTestWorktree},
		{Kind: graphview.SelectorBase, GraphID: stack.graphID},
		{Kind: graphview.SelectorGitRef, Value: "refs/heads/main", GraphID: stack.graphID},
		{Kind: graphview.SelectorCommit, Value: "deadbeef", GraphID: stack.graphID},
	} {
		t.Run(string(selector.Kind), func(t *testing.T) {
			require.Nil(t, stack.srv.freshnessCarrier(selector, nil),
				"a %s selector that produced no view was given an invented exact route claim", selector.Kind)
		})
	}

	t.Run("an existing view is never replaced", func(t *testing.T) {
		view := &requestView{kind: requestViewKindBase, rider: graphview.NewViewRider(graphview.Selector{Kind: graphview.SelectorAuto})}
		require.Same(t, view, stack.srv.freshnessCarrier(graphview.Selector{Kind: graphview.SelectorAuto}, view))
	})
}

// The carrier's live shape is asserted on the deterministic route:
// TestRequireFreshOnACommittedBaseSaysItDidNotWait's "shared corpus under a
// dedicated checkout's cwd" case, whose selection provably produces no view.
// A cwd outside every tracked repository reaches the same code, but by way of
// automatic checkout discovery, which under -race can answer view_building
// ("selected checkout discovery is pending") instead — a timing-dependent
// route, not a second contract.

// ------------------------------------ the committed-base gate, reached ---

// The gate inside awaitCheckoutFreshness (!graphview.ServesAutomaticView) is
// the one that keeps require_fresh from admitting a refresh against a checkout
// that has no automatic route to advance. Nothing above reaches it: the shared
// corpus produces no view and a labelled base carries no rider checkout id, so
// both are refused earlier, by freshnessWaitTarget.
//
// An explicitly selected dedicated/primary checkout DOES reach it.
// viewForWorktreeSelector gates on checkout state, never on mode, so
// view:{kind:"worktree",checkout_id:<dedicated>} is routed like any other, and
// both the materialized and the not-yet-routed shapes name that checkout as
// the wait target. Without the gate each of them issues a RequestCheckoutRefresh
// admission against a checkout the coordinator has no automatic route for.
func TestRequireFreshOnADedicatedCheckoutNeverAdmitsARefresh(t *testing.T) {
	// The fixture's primary is Ready and Dedicated: exactly the population the
	// gate exists for, and the one graphview.ServesAutomaticView excludes.
	t.Run("the fixture primary is a ready dedicated checkout", func(t *testing.T) {
		stack := newViewStack(t)
		checkout, found, err := stack.store.Catalog().GetCheckout(context.Background(), viewTestPrimary)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, store_sqlite.CheckoutStateReady, checkout.State)
		require.Equal(t, store_sqlite.CheckoutModeDedicated, checkout.EffectiveMode)
		require.False(t, graphview.ServesAutomaticView(checkout),
			"the gate under test does not consider this checkout non-automatic, so nothing below proves anything")
	})

	t.Run("routed, with a ready route of its own", func(t *testing.T) {
		stack := newViewStack(t)
		routeDedicatedCheckout(t, stack)
		waiter := &fakeFreshnessWaiter{}
		stack.srv.freshnessWaiter = waiter

		args := freshArgs(map[string]any{
			"view": map[string]any{"kind": "worktree", "checkout_id": viewTestPrimary},
		}, time.Minute)
		res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", args, captureReader(stack.srv, new(graph.Reader)))
		require.NoError(t, err)
		require.False(t, res.IsError, viewResultText(t, res))

		require.Empty(t, waiter.observed(),
			"require_fresh admitted a checkout refresh against a dedicated checkout with no automatic route")
		rider := resultFreshness(t, res)
		require.Equal(t, false, rider["fresh"], "rider = %v", rider)
		require.Equal(t, freshReasonCommittedBaseAdvance, rider["fresh_reason"], "rider = %v", rider)
		require.Equal(t, viewTestPrimary, rider["checkout_id"], "rider = %v", rider)
		// The route really materialized: this is the gate refusing to admit a
		// refresh for a served dedicated view, not a fallback that never had a
		// wait target in the first place.
		require.Equal(t, true, rider["exact"], "rider = %v", rider)
		require.NotContains(t, rider, "fallback_reason", "rider = %v", rider)
	})

	t.Run("named but not routed yet", func(t *testing.T) {
		// No route row for the primary: selection refuses with view_building
		// and freshnessWaitTarget takes its selector arm, which resolves the
		// same dedicated checkout. The gate is the only thing between that and
		// an admission.
		stack := newViewStack(t)
		waiter := &fakeFreshnessWaiter{}
		stack.srv.freshnessWaiter = waiter

		args := freshArgs(map[string]any{
			"view": map[string]any{"kind": "worktree", "checkout_id": viewTestPrimary},
		}, time.Minute)
		res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", args, captureReader(stack.srv, new(graph.Reader)))
		require.NoError(t, err)
		assertToolError(t, res, graphview.CodeViewBuilding)
		require.Empty(t, waiter.observed(),
			"require_fresh admitted a checkout refresh for an unrouted dedicated checkout")
	})
}

// routeDedicatedCheckout gives the fixture's dedicated primary a ready route
// over the same two published generations the automatic worktree route names,
// so an explicit selector for it materializes instead of refusing. It is the
// only way to reach the gate with a live rider: an unrouted checkout answers
// through the error path instead.
func routeDedicatedCheckout(t *testing.T, stack *viewStack) {
	t.Helper()
	require.NoError(t, stack.store.Catalog().UpsertCheckoutRoute(context.Background(), store_sqlite.CheckoutRoute{
		CheckoutID:         viewTestPrimary,
		GraphID:            stack.graphID,
		CommitGenerationID: stack.commit,
		DirtyGenerationID:  stack.dirty,
		State:              store_sqlite.RouteActive,
	}))
}

// ------------------------------------------- wait-target vocabulary ---

// freshReasonCommittedBaseAdvance is a claim about the VIEW: "what answered
// reads a committed base". Every non-waitable outcome used to carry it,
// including the ones that are facts about a lookup — no catalog wired, a
// catalog read that failed, a checkout row that is gone. Those rode out as a
// false statement about the view on the one item whose thesis is a truthful
// rider.
func TestAnUnresolvableWaitTargetIsNotReportedAsACommittedBase(t *testing.T) {
	stack := newViewStack(t)
	ctx := context.Background()
	auto := graphview.Selector{Kind: graphview.SelectorAuto}

	routedTo := func(checkoutID string) *requestView {
		rider := graphview.NewViewRider(auto)
		rider.CheckoutID = checkoutID
		return &requestView{kind: requestViewKindWorktree, rider: rider}
	}

	t.Run("a routed view whose checkout is gone", func(t *testing.T) {
		_, reason, waitable := stack.srv.freshnessWaitTarget(ctx, auto, routedTo("co-vanished"), nil)
		require.False(t, waitable)
		require.Equal(t, freshReasonWaitTargetUnavailable, reason,
			"a checkout that is not in the catalog was reported as a committed base")
	})

	t.Run("no catalog to resolve the route against", func(t *testing.T) {
		bare := &Server{}
		_, reason, waitable := bare.freshnessWaitTarget(ctx, auto, routedTo(viewTestWorktree), nil)
		require.False(t, waitable)
		require.Equal(t, freshReasonWaitTargetUnavailable, reason)
	})

	t.Run("a worktree selector that no longer registers", func(t *testing.T) {
		selector := graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: "co-vanished"}
		building := graphview.NewViewError(graphview.CodeViewBuilding, "checkout is not fully routed yet")
		_, reason, waitable := stack.srv.freshnessWaitTarget(ctx, selector, nil, building)
		require.False(t, waitable)
		require.Equal(t, freshReasonWaitTargetUnavailable, reason)
	})

	// The control: a request that really did resolve to a committed base keeps
	// the committed-base reason, so the split above is a split and not a
	// rename.
	t.Run("a view that names no checkout route at all", func(t *testing.T) {
		_, reason, waitable := stack.srv.freshnessWaitTarget(ctx, auto, nil, nil)
		require.False(t, waitable)
		require.Equal(t, freshReasonCommittedBaseAdvance, reason)
	})

	t.Run("the routed checkout resolves", func(t *testing.T) {
		checkout, reason, waitable := stack.srv.freshnessWaitTarget(ctx, auto, routedTo(viewTestWorktree), nil)
		require.True(t, waitable)
		require.Empty(t, reason, "a waitable target must not carry a not-waitable reason")
		require.Equal(t, viewTestWorktree, checkout.CheckoutID)
	})
}

// ------------------------------------------ require_exact, everywhere ---

// require_exact means "do not hand me something I did not ask for". Paired with
// require_fresh it used to enforce that for exactly one of the six ways a wait
// can fail — an expired deadline — and answer out of the stale route for the
// other five. A caller that set both knobs and got coordinator_unavailable read
// a stale route it had explicitly refused, with no error to branch on.
func TestRequireExactRefusesEveryUnfreshOutcome(t *testing.T) {
	type unfreshCase struct {
		// arrange installs the failure and returns the request arguments.
		arrange func(stack *viewStack) map[string]any
		reason  string
	}
	cases := map[string]unfreshCase{
		"an unavailable coordinator": {
			arrange: func(stack *viewStack) map[string]any {
				stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
					answer: func(int, string, string) (*indexer.CheckoutRefreshTicket, error) {
						return nil, errFreshnessUnattributable
					},
				}
				return freshArgs(nil, time.Minute)
			},
			reason: freshReasonCoordinatorUnavailable,
		},
		"a checkout whose root changed": {
			arrange: func(stack *viewStack) map[string]any {
				stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
					answer: func(int, string, string) (*indexer.CheckoutRefreshTicket, error) {
						return nil, fmt.Errorf("%w: checkout root changed", indexer.ErrCheckoutMutationStale)
					},
				}
				return freshArgs(nil, time.Minute)
			},
			reason: freshReasonCheckoutRootChanged,
		},
		"a coordinator that stopped for this checkout": {
			arrange: func(stack *viewStack) map[string]any {
				stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
					answer: func(int, string, string) (*indexer.CheckoutRefreshTicket, error) {
						return nil, indexer.ErrCheckoutRefreshStopped
					},
				}
				return freshArgs(nil, time.Minute)
			},
			reason: freshReasonCheckoutRefreshStopped,
		},
		"a publication that cannot be read back": {
			arrange: func(stack *viewStack) map[string]any {
				stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
					answer: func(_ int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
						// The coordinator publishes, and the route row is gone
						// before the request can read it back: the settle
						// signal succeeded and cannot be checked against the
						// answer.
						require.NoError(t, stack.store.Catalog().DeleteCheckoutRoute(context.Background(), checkoutID))
						return settledTicket(checkoutID, root, uint64(stack.dirty)), nil
					},
				}
				return freshArgs(nil, time.Minute)
			},
			reason: freshReasonWaitTargetUnavailable,
		},
		"a publication another view answered": {
			arrange: func(stack *viewStack) map[string]any {
				stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
					answer: func(_ int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
						routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
						return settledTicket(checkoutID, root, uint64(stack.dirty)), nil
					},
				}
				return freshArgs(nil, time.Minute)
			},
			reason: freshReasonRouteWithdrawn,
		},
		"a failed publication": {
			arrange: func(stack *viewStack) map[string]any {
				stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
					answer: func(_ int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
						return failedTicket(checkoutID, root), nil
					},
				}
				return freshArgs(nil, time.Minute)
			},
			reason: freshReasonPublicationFailed,
		},
		"a committed base with nothing to advance": {
			arrange: func(stack *viewStack) map[string]any {
				stack.srv.freshnessWaiter = &fakeFreshnessWaiter{}
				return freshArgs(map[string]any{
					"view": map[string]any{"kind": "base", "graph_id": stack.graphID},
				}, time.Minute)
			},
			reason: freshReasonCommittedBaseAdvance,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Control: without require_exact the same failure answers, out of
			// the read-only route, carrying the reason. The refusal below is
			// therefore require_exact's doing and not the failure's.
			lenient := newViewStack(t)
			res, err := lenient.callWithView(t, lenient.worktreeRoot, "get_symbol",
				tc.arrange(lenient), captureReader(lenient.srv, new(graph.Reader)))
			require.NoError(t, err)
			require.False(t, res.IsError, "without require_exact this outcome must still answer: %s", viewResultText(t, res))
			rider := resultFreshness(t, res)
			require.Equal(t, false, rider["fresh"], "rider = %v", rider)
			require.Equal(t, tc.reason, rider["fresh_reason"], "rider = %v", rider)

			strict := newViewStack(t)
			args := tc.arrange(strict)
			args[requireExactArgName] = true
			res, err = strict.callWithView(t, strict.worktreeRoot, "get_symbol", args, captureReader(strict.srv, new(graph.Reader)))
			require.NoError(t, err)
			assertToolError(t, res, graphview.CodeViewBuilding)
			text := viewResultText(t, res)
			require.Contains(t, text, tc.reason,
				"the refusal must name the outcome the caller has to act on: %s", text)
			require.Contains(t, text, requireExactArgName, "refusal = %s", text)
		})
	}
}

// failedTicket is the coordinator answering, and what it answered is a
// failure: terminal, and not a reason to retry.
func failedTicket(checkoutID, root string) *indexer.CheckoutRefreshTicket {
	done := make(chan indexer.MutationResult, 1)
	done <- indexer.MutationResult{Err: errFreshnessPublication}
	close(done)
	return &indexer.CheckoutRefreshTicket{
		CheckoutID: checkoutID,
		Root:       root,
		Ticket:     &indexer.MutationTicket{Path: root, Generation: 1, Done: done},
	}
}

var errFreshnessPublication = errors.New("publishing the checkout generation failed")

// errFreshnessUnattributable is a refusal the server cannot attribute to this
// checkout: not one of the coordinator's sentinels, not a context expiry. It
// is the only shape coordinator_unavailable is truthful for.
var errFreshnessUnattributable = errors.New("no checkout coordinator is reachable")

// ------------------------------------------------- the parse ordering ---

// overlay.go parses the knobs BEFORE s.reconcileToolParams. That ordering is
// load-bearing and, against the shipped tool surface, invisible: no tool
// declares a parameter inside the alias matcher's edit-distance budget of a
// knob name (TestNoToolAliasesTheFreshnessKnobs), so moving the parse below
// reconciliation changes nothing today and everything the day a tool grows such
// a parameter.
//
// This registers exactly that collision — a tool whose own boolean parameter is
// one edit from require_fresh — and drives a real request through the same
// middleware chain. With the parse where it is, the wait happens. With the
// parse below reconcileToolParams, the matcher has already renamed the knob
// onto the tool's parameter and the request never learns it was asked to wait.
func TestFreshnessKnobsAreReadBeforeParameterReconciliation(t *testing.T) {
	stack := newViewStack(t)
	const probe = "freshness_alias_probe"
	const collidingParam = "require_frsh" // one edit from require_fresh
	stack.srv.mcpServer.AddTool(
		mcplib.NewTool(probe,
			mcplib.WithDescription("test-only: declares a parameter one edit from require_fresh"),
			mcplib.WithBoolean(collidingParam, mcplib.Description("the colliding parameter")),
		),
		func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		},
	)

	// The collision is live: reconciliation really would take require_fresh
	// away from a request for this tool. Without this the test below could pass
	// for the trivial reason that nothing ever rewrites anything.
	real := stack.srv.toolParamNames(probe)
	require.Contains(t, real, collidingParam, "the probe tool's schema did not register")
	probeArgs := map[string]any{requireFreshArgName: true}
	require.NotEmpty(t, reconcileArgKeys(probeArgs, real),
		"the alias matcher does not rewrite this collision, so the ordering it protects is untestable here")
	require.NotContains(t, probeArgs, requireFreshArgName,
		"reconciliation left the knob in place; this test proves nothing")

	waiter := &fakeFreshnessWaiter{
		answer: func(_ int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			return pendingTicket(checkoutID, root), nil
		},
	}
	stack.srv.freshnessWaiter = waiter

	deadline := time.Now().Add(150 * time.Millisecond)
	args := map[string]any{
		requireFreshArgName: true,
		waitDeadlineArgName: deadline.Format(time.RFC3339Nano),
	}
	res, err := stack.callWithView(t, stack.worktreeRoot, probe, args, captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))

	calls := waiter.observed()
	require.Len(t, calls, 1,
		"require_fresh was lost to parameter reconciliation: the request middleware must read the knobs before reconcileToolParams runs")
	require.Equal(t, viewTestWorktree, calls[0].checkoutID)
	rider := resultFreshness(t, res)
	require.Equal(t, false, rider["fresh"], "rider = %v", rider)
	require.Equal(t, freshReasonDeadlineExceeded, rider["fresh_reason"], "rider = %v", rider)
}

// --------------------------------------- the bound is not the coordinator ---

// The wait hands the coordinator exactly two bounds — the request's own context
// and this wait's deadline — and nothing else. So a context error coming back
// out of RequestCheckoutRefresh, or riding a ticket's MutationResult, is one of
// those two ending. Reporting it as coordinator_unavailable / publication_failed
// states something false about the SERVER ("nothing here can publish a
// generation" / "the publication failed") for what is a fact about the caller's
// own bound, and it sends the caller looking in the wrong place.
//
// This is not hypothetical: under -race, TestRequireFreshWithoutASeamUsesThe-
// CheckoutLifecycle reached the live lifecycle with ~4ms left on a 400ms bound,
// got context.DeadlineExceeded back from the admission, and rode out as
// fresh_reason:"coordinator_unavailable" after waiting 396ms.
// freshnessTestBound is the wait_deadline a case that means to REACH its bound
// runs under. It is short because the case ends by waiting the bound out, and
// absolute because every knob is: the fake waits for the context the wait hands
// it rather than for a wall clock.
const freshnessTestBound = 400 * time.Millisecond

func TestAnExpiredBoundIsNotReportedAsAnUnavailableCoordinator(t *testing.T) {
	type expiryCase struct {
		// answer is what the coordinator hands back. It is given the context
		// the wait admitted under, and every case here waits for THAT context
		// to end before failing — which is what a coordinator whose only bound
		// is the caller's does, and the only way a test can mean "the caller's
		// bound expired" without racing a wall clock.
		//
		// A context error that is NOT one of the caller's bounds is a
		// different outcome entirely (the coordinator's own capture timeout);
		// it used to land here, and it is pinned in checkout_binding_test.go.
		answer func(ctx context.Context, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error)
		// wrong is the reason the request used to carry.
		wrong string
	}
	cases := map[string]expiryCase{
		"the admission is cut off by the bound": {
			answer: func(ctx context.Context, _, _ string) (*indexer.CheckoutRefreshTicket, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
			wrong: freshReasonCoordinatorUnavailable,
		},
		"the admission is cut off by a wrapped bound": {
			answer: func(ctx context.Context, _, _ string) (*indexer.CheckoutRefreshTicket, error) {
				<-ctx.Done()
				// The live shape: the git sampler wraps the context error it
				// was cut off by (internal/gitstate/dirty.go).
				return nil, fmt.Errorf("sample checkout: %w", ctx.Err())
			},
			wrong: freshReasonCoordinatorUnavailable,
		},
		"the ticket completes with the bound's own error": {
			answer: func(ctx context.Context, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
				<-ctx.Done()
				done := make(chan indexer.MutationResult, 1)
				done <- indexer.MutationResult{Err: ctx.Err()}
				close(done)
				return &indexer.CheckoutRefreshTicket{
					CheckoutID: checkoutID,
					Root:       root,
					Ticket:     &indexer.MutationTicket{Path: root, Generation: 1, Done: done},
				}, nil
			},
			wrong: freshReasonPublicationFailed,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stack := newViewStack(t)
			stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
				answerCtx: func(ctx context.Context, _ int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
					return tc.answer(ctx, checkoutID, root)
				},
			}
			res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol",
				freshArgs(nil, freshnessTestBound), captureReader(stack.srv, new(graph.Reader)))
			require.NoError(t, err)
			require.False(t, res.IsError, viewResultText(t, res))
			rider := resultFreshness(t, res)
			require.Equal(t, false, rider["fresh"], "rider = %v", rider)
			require.Equal(t, freshReasonDeadlineExceeded, rider["fresh_reason"],
				"the request's own bound ending was reported as a fact about the server: rider = %v", rider)
			require.NotEqual(t, tc.wrong, rider["fresh_reason"], "rider = %v", rider)
			require.NotEqual(t, freshReasonRefreshAdmissionAbandoned, rider["fresh_reason"],
				"the caller's OWN bound expiring was blamed on the coordinator: rider = %v", rider)
		})
	}

	// A retryable refusal that merely WRAPS a context error keeps its own
	// meaning: the retryable check runs first, so a busy coordinator is still
	// re-admitted rather than being read as an expired bound. (This is the live
	// shape: "checkout mutation lane is busy; retry: … : context deadline
	// exceeded".)
	t.Run("a busy refusal wrapping a context error is still retried", func(t *testing.T) {
		stack := newViewStack(t)
		caughtUp := writeCaughtUpDirtyGeneration(t, stack)
		waiter := &fakeFreshnessWaiter{
			answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
				if call == 1 {
					return nil, fmt.Errorf("%w: selected checkout discovery is pending: %w",
						indexer.ErrCheckoutMutationBusy, context.DeadlineExceeded)
				}
				routeViewCheckout(t, stack.store, stack.graphID, stack.commit, caughtUp, store_sqlite.RouteActive)
				return settledTicket(checkoutID, root, uint64(caughtUp)), nil
			},
		}
		stack.srv.freshnessWaiter = waiter
		res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol",
			freshArgs(nil, time.Minute), captureReader(stack.srv, new(graph.Reader)))
		require.NoError(t, err)
		require.False(t, res.IsError, viewResultText(t, res))
		require.Len(t, waiter.observed(), 2, "a busy refusal was read as an expired bound instead of being retried")
		require.Equal(t, true, resultFreshness(t, res)["fresh"])
	})
}

// ------------------------------------- the answer the wait is about ---

// The wait's success is a statement about ONE route: the coordinator completes
// a ticket only once the active route names a servable dirty generation whose
// fingerprint equals the tree it just sampled. That claim belongs to the view
// that route materializes, and to nothing else.
//
// The defect this pins: the re-selection after the wait can produce a labelled
// BASE fallback — the route stops being ready between the settle and the
// re-read, viewForSessionCWD falls back with exact:false and
// fallback_reason:"view_building" — and the old check only downgraded when the
// re-selection produced NO view at all. A non-nil fallback kept the wait's
// fresh:true, so the rider shipped
//
//	fresh:true, actual_view:"base", exact:false, fallback_reason:"view_building"
//
// which tells a caller that the shared corpus it is reading is the working
// copy it asked to wait for. This drives the whole middleware, not the helper.
func TestAFreshWaitAnsweredByABaseFallbackIsNotReportedFresh(t *testing.T) {
	stack := newViewStack(t)
	stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
		answer: func(_ int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			// The coordinator publishes and reports success; the route then
			// stops being ready, exactly as ensureRoute's flip or a dirty-slot
			// clear leaves it. Both happen before the request looks again.
			routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
			return settledTicket(checkoutID, root, uint64(stack.dirty)), nil
		},
	}

	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol",
		freshArgs(nil, time.Minute), captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	require.False(t, res.IsError, "without require_exact the fallback must still answer: %s", viewResultText(t, res))

	rider := resultFreshness(t, res)
	// The fallback half of the rider is unchanged and still truthful…
	require.Equal(t, false, rider["exact"], "rider = %v", rider)
	require.Equal(t, string(graphview.SelectorBase), rider["actual_view"], "rider = %v", rider)
	require.Equal(t, string(graphview.CodeViewBuilding), rider["fallback_reason"], "rider = %v", rider)
	// …and the freshness half must not contradict it.
	require.Equal(t, false, rider["fresh"],
		"a base fallback was stamped with the wait's success: rider = %v", rider)
	require.Equal(t, freshReasonRouteWithdrawn, rider["fresh_reason"], "rider = %v", rider)
}

// The same shape under require_exact: the pair means "a route that reflects
// the working copy, or nothing", so the substitution is refused outright and
// the refusal names what happened.
func TestAFreshWaitAnsweredByABaseFallbackRefusesUnderRequireExact(t *testing.T) {
	stack := newViewStack(t)
	stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
		answer: func(_ int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
			return settledTicket(checkoutID, root, uint64(stack.dirty)), nil
		},
	}
	args := freshArgs(map[string]any{requireExactArgName: true}, time.Minute)
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", args, captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	assertToolError(t, res, graphview.CodeViewBuilding)
	require.Contains(t, viewResultText(t, res), freshReasonRouteWithdrawn)
}

// The positive control for both tests above, and the thing that keeps them
// from being satisfiable by "never report fresh": the untouched route, which
// the coordinator published and which then answers the request, still reports
// fresh:true. servesPublishedRoute is one-sided by design; this is the side it
// must not take.
func TestAFreshWaitAnsweredByItsOwnRouteIsStillReportedFresh(t *testing.T) {
	stack := newViewStack(t)
	caughtUp := writeCaughtUpDirtyGeneration(t, stack)
	stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
		answer: func(_ int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			routeViewCheckout(t, stack.store, stack.graphID, stack.commit, caughtUp, store_sqlite.RouteActive)
			return settledTicket(checkoutID, root, uint64(caughtUp)), nil
		},
	}
	var reader graph.Reader
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", freshArgs(nil, time.Minute), captureReader(stack.srv, &reader))
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	rider := resultFreshness(t, res)
	require.Equal(t, true, rider["fresh"], "rider = %v", rider)
	require.Equal(t, true, rider["exact"], "rider = %v", rider)
	require.NotContains(t, rider, "fresh_reason", "rider = %v", rider)
	require.True(t, hasNode(reader, "repo/keep.go::AfterTheWait"),
		"fresh:true was reported for a view that is not the published route")
}

// servesPublishedRoute's remaining arms. The two a request shape can reach
// deterministically are covered end to end above; the route-epoch and
// generation arms need the route to move in the microseconds between the
// publication read and the re-selection, which no request shape can schedule.
// The function has exactly one production caller (settleRequestFreshness), so
// driving it directly with a REAL materialized view and the fixture's REAL
// route is the whole contract.
func TestServesPublishedRouteAcceptsOnlyThePublishedRoute(t *testing.T) {
	stack := newViewStack(t)
	ctx := context.Background()
	materialized, err := stack.srv.materializer.MaterializeCheckout(ctx, viewTestWorktree)
	require.NoError(t, err)
	defer materialized.Close()
	published, found, err := stack.store.Catalog().GetCheckoutRoute(ctx, viewTestWorktree)
	require.NoError(t, err)
	require.True(t, found)
	require.Greater(t, published.DirtyGenerationID, int64(0), "the fixture route names no dirty generation")

	routed := func(mutate func(v *requestView)) *requestView {
		rider := graphview.NewViewRider(graphview.Selector{
			Kind: graphview.SelectorWorktree, CheckoutID: viewTestWorktree,
		})
		rider.MarkExact("worktree:" + viewTestWorktree)
		rider.CheckoutID = viewTestWorktree
		view := &requestView{kind: requestViewKindWorktree, rider: rider, materialized: materialized}
		if mutate != nil {
			mutate(view)
		}
		return view
	}
	require.True(t, servesPublishedRoute(routed(nil), viewTestWorktree, published),
		"the published route itself must be accepted, or nothing is ever fresh")

	moved := published
	moved.RouteEpoch++
	unread := published
	unread.DirtyGenerationID = published.DirtyGenerationID + 1000

	type rejection struct {
		view  *requestView
		route store_sqlite.CheckoutRoute
	}
	cases := map[string]rejection{
		"no view at all": {view: nil, route: published},
		"a labelled base fallback": {view: func() *requestView {
			view := routed(nil)
			view.kind, view.materialized = requestViewKindBase, nil
			require.NoError(t, view.rider.MarkFallback(string(graphview.SelectorBase), string(graphview.CodeViewBuilding)))
			return view
		}(), route: published},
		"an inexact rider over a materialized stack": {view: routed(func(v *requestView) {
			require.NoError(t, v.rider.MarkFallback(string(graphview.SelectorBase), string(graphview.CodeViewBuilding)))
		}), route: published},
		"a routeless freshness carrier":     {view: routed(func(v *requestView) { v.routeless = true }), route: published},
		"another checkout":                  {view: routed(func(v *requestView) { v.rider.CheckoutID = "co-other" }), route: published},
		"a route that moved again":          {view: routed(nil), route: moved},
		"a generation the stack never read": {view: routed(nil), route: unread},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.False(t, servesPublishedRoute(tc.view, viewTestWorktree, tc.route),
				"the wait's success would have been claimed for %s", name)
		})
	}
}

// ------------------------------- a checkout that cannot be refreshed ---

// coordinator_unavailable's own doc says "nothing on this server can publish a
// checkout generation". The coordinator's two checkout sentinels say something
// much narrower — this checkout's root changed, or this checkout's coordinator
// stopped — and every other checkout on the server still refreshes. Collapsing
// them into coordinator_unavailable is the same class of false claim as
// labelling a lookup failure "committed base": it tells the caller to stop
// asking when the right move is to re-select.
func TestACheckoutThatCannotRefreshIsNotAnUnavailableCoordinator(t *testing.T) {
	type sentinelCase struct {
		admission error
		completed error
		reason    string
	}
	cases := map[string]sentinelCase{
		"a root that changed": {
			admission: fmt.Errorf("%w: checkout root changed", indexer.ErrCheckoutMutationStale),
			completed: fmt.Errorf("%w: checkout root is unavailable", indexer.ErrCheckoutMutationStale),
			reason:    freshReasonCheckoutRootChanged,
		},
		"a coordinator that stopped": {
			admission: indexer.ErrCheckoutRefreshStopped,
			completed: indexer.ErrCheckoutRefreshStopped,
			reason:    freshReasonCheckoutRefreshStopped,
		},
	}
	for name, tc := range cases {
		t.Run(name+" (refused at admission)", func(t *testing.T) {
			stack := newViewStack(t)
			waiter := &fakeFreshnessWaiter{
				answer: func(int, string, string) (*indexer.CheckoutRefreshTicket, error) {
					return nil, tc.admission
				},
			}
			stack.srv.freshnessWaiter = waiter
			res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol",
				freshArgs(nil, time.Minute), captureReader(stack.srv, new(graph.Reader)))
			require.NoError(t, err)
			require.False(t, res.IsError, viewResultText(t, res))
			rider := resultFreshness(t, res)
			require.Equal(t, false, rider["fresh"], "rider = %v", rider)
			require.Equal(t, tc.reason, rider["fresh_reason"], "rider = %v", rider)
			require.NotEqual(t, freshReasonCoordinatorUnavailable, rider["fresh_reason"],
				"a fact about one checkout was reported as a fact about the server")
			require.Len(t, waiter.observed(), 1, "a terminal refusal must not be retried until the deadline")
		})
		t.Run(name+" (the ticket completes with it)", func(t *testing.T) {
			stack := newViewStack(t)
			stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
				answer: func(_ int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
					done := make(chan indexer.MutationResult, 1)
					done <- indexer.MutationResult{Err: tc.completed}
					close(done)
					return &indexer.CheckoutRefreshTicket{
						CheckoutID: checkoutID,
						Root:       root,
						Ticket:     &indexer.MutationTicket{Path: root, Generation: 1, Done: done},
					}, nil
				},
			}
			res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol",
				freshArgs(nil, time.Minute), captureReader(stack.srv, new(graph.Reader)))
			require.NoError(t, err)
			require.False(t, res.IsError, viewResultText(t, res))
			rider := resultFreshness(t, res)
			require.Equal(t, false, rider["fresh"], "rider = %v", rider)
			require.Equal(t, tc.reason, rider["fresh_reason"], "rider = %v", rider)
			require.NotEqual(t, freshReasonPublicationFailed, rider["fresh_reason"],
				"a checkout that stopped being refreshable was reported as a build that broke")
		})
	}
}

// A wait target that names no checkout or no root is a lookup that did not
// answer, not a server with no coordinator. It is the last
// coordinator_unavailable this item takes away from a fact about one checkout.
func TestAnEmptyWaitTargetIsNotAnUnavailableCoordinator(t *testing.T) {
	stack := newViewStack(t)
	stack.srv.freshnessWaiter = &fakeFreshnessWaiter{}
	for name, checkout := range map[string]store_sqlite.Checkout{
		"no checkout id": {RootPath: stack.worktreeRoot},
		"no root path":   {CheckoutID: viewTestWorktree},
	} {
		t.Run(name, func(t *testing.T) {
			fresh, reason := stack.srv.awaitCheckoutFreshness(
				context.Background(), checkout, time.Now().Add(time.Minute))
			require.False(t, fresh)
			require.Equal(t, freshReasonWaitTargetUnavailable, reason)
		})
	}
}

// The one unfresh outcome TestRequireExactRefusesEveryUnfreshOutcome cannot
// arrange through the middleware: interrupted. It needs the request's own
// context to end while the ticket is being waited on, and by then the
// middleware has no seam left.
//
// What matters is the contract, not which of the two refusals fires: a
// require_exact + require_fresh caller whose request was interrupted must
// never be handed the stale route. Either the post-wait exactness refusal
// answers (freshnessExactRefusal) or the re-selection itself fails on the dead
// context — both return no view and an error, and neither is a stale answer.
func TestAnInterruptedFreshWaitUnderRequireExactNeverAnswers(t *testing.T) {
	stack := newViewStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stack.srv.freshnessWaiter = &fakeFreshnessWaiter{
		answer: func(_ int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			// The ticket never completes; the request ends underneath it.
			go cancel()
			return pendingTicket(checkoutID, root), nil
		},
	}
	selector := graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: viewTestWorktree}
	policy := requestViewPolicy{freshness: requestFreshness{
		requireFresh: true,
		requireExact: true,
		deadline:     time.Now().Add(time.Minute),
		hasDeadline:  true,
	}}
	pre, selectErr := stack.srv.selectRequestView(context.Background(), selector, policy)
	require.NoError(t, selectErr)
	require.NotNil(t, pre, "the fixture must select a routed view for the wait to be admitted against")

	view, err := stack.srv.settleRequestFreshness(ctx, selector, policy, pre, nil)
	require.Error(t, err, "an interrupted wait under require_exact answered out of the stale route")
	require.Nil(t, view, "a refusal must not also hand back a view")
}

// The deterministic half of the same outcome: a wait whose request context is
// already dead reports interrupted, and nothing else. The test above cannot
// observe that rider — the re-selection after the wait dies on the same dead
// context — so the reason itself is pinned here, at the one function that
// produces it.
func TestAWaitWhoseRequestEndedReportsInterrupted(t *testing.T) {
	stack := newViewStack(t)
	waiter := &fakeFreshnessWaiter{
		answer: func(_ int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			return pendingTicket(checkoutID, root), nil
		},
	}
	stack.srv.freshnessWaiter = waiter
	checkout, found, err := stack.store.Catalog().GetCheckout(context.Background(), viewTestWorktree)
	require.NoError(t, err)
	require.True(t, found)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fresh, reason := stack.srv.awaitCheckoutFreshness(ctx, checkout, time.Now().Add(time.Minute))
	require.False(t, fresh)
	require.Equal(t, freshReasonInterrupted, reason,
		"a request that ended was reported as something the server or the checkout did")
	require.Empty(t, waiter.observed(), "a dead request must not be admitted to the coordinator")
}
