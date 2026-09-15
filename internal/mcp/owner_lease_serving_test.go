package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The repository-owner lease on the serving request.
//
// The generation lease a materialized view holds stops a payload generation
// from being RETIRED. Repository lifetime is a different guarantee: owner
// finalization is followed by a physical purge of that repository's payload
// (indexer/repository_cleanup.go), and nothing on the request path held it.
// The unrouted base-corpus request — no selector, no automatic checkout, which
// is the shape most tool calls have — materialized no view at all and so held
// nothing whatsoever. The tests below drive the production middleware, not the
// primitive.

// viewStackOwner is the registered owner of the fixture's indexed repository:
// the dedicated graph the catalog rows name, with the primary checkout's
// identity. It is what CloseRepositoryAdmission is addressed by.
func viewStackOwner(stack *viewStack) graphview.RepositoryOwner {
	return graphview.RepositoryOwner{
		GraphID:     stack.graphID,
		CheckoutID:  viewTestPrimary,
		Incarnation: "inc-primary",
		RepoPrefix:  "repo",
	}
}

func registerViewStackOwner(t *testing.T, stack *viewStack) graphview.RepositoryOwner {
	t.Helper()
	owner := viewStackOwner(stack)
	if err := stack.leases.RegisterRepositoryOwner(owner); err != nil {
		t.Fatalf("RegisterRepositoryOwner: %v", err)
	}
	return owner
}

// assertNotDrained fails when a closed registration drained while something is
// still reading it. The wait is a negative assertion — under the defect the
// drain closes synchronously inside CloseRepositoryAdmission — so a slow
// machine cannot turn it into a false failure.
func assertNotDrained(t *testing.T, drain *graphview.RepositoryDrain, what string) {
	t.Helper()
	select {
	case <-drain.Done():
		t.Fatalf("the repository drained while %s", what)
	case <-time.After(50 * time.Millisecond):
	}
}

func assertDrains(t *testing.T, drain *graphview.RepositoryDrain, what string) {
	t.Helper()
	select {
	case <-drain.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("the repository never drained after %s", what)
	}
}

// callWithViewContext is callWithView with the caller's own context, so a test
// can cancel the request the way a client hanging up does.
func (v *viewStack) callWithViewContext(
	t *testing.T,
	ctx context.Context,
	tool string,
	leaf func(ctx context.Context) (*mcplib.CallToolResult, error),
) (*mcplib.CallToolResult, error) {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = map[string]any{}
	handler := v.srv.wrapToolHandler(func(hctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		return leaf(hctx)
	})
	return handler(ctx, req)
}

// TestUnroutedRequestHoldsTheRepositoryOwnerLease is the core contract: a
// request being served keeps its repositories un-finalizable for as long as it
// is being served, even when it materialized no view at all.
func TestUnroutedRequestHoldsTheRepositoryOwnerLease(t *testing.T) {
	stack := newViewStack(t)
	owner := registerViewStackOwner(t, stack)

	var drain *graphview.RepositoryDrain
	if _, err := stack.callWithView(t, stack.repoRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			// The premise: this request really is the unrouted shape, so what
			// holds the owner cannot be a routed view's base pin.
			if view := requestViewFromContext(hctx); view != nil && view.materialized != nil {
				return nil, errors.New("the fixture routed a view; this arm must exercise the base-corpus path")
			}
			closed, err := stack.leases.CloseRepositoryAdmission(owner)
			if err != nil {
				return nil, err
			}
			drain = closed
			// Inside the handler goroutine, so a failure is returned rather
			// than raised: t.Fatal is only valid on the test's own goroutine.
			select {
			case <-drain.Done():
				return nil, errors.New("the repository drained while its request was still being served")
			case <-time.After(50 * time.Millisecond):
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("call: %v", err)
	}
	assertDrains(t, drain, "the request returned")
}

// otherRepoOwner is a second registered repository with no relationship to the
// fixture's routed stack: it is what a worker that names what it reads must
// NOT be admitted to.
func otherRepoOwner() graphview.RepositoryOwner {
	return graphview.RepositoryOwner{
		GraphID:     "graph-other",
		CheckoutID:  "checkout-other",
		Incarnation: "inc-other",
		RepoPrefix:  "other",
	}
}

// TestDetachedWorkerKeepsTheRepositoryOwnerLease is the handoff half: work the
// handler leaves running behind it holds the repository whose payload it is
// still reading, so finalization and the physical purge wait for the worker,
// not for the response.
func TestDetachedWorkerKeepsTheRepositoryOwnerLease(t *testing.T) {
	viewmetrics.Reset()
	stack := newViewStack(t)
	owner := registerViewStackOwner(t, stack)

	worker := make(chan struct{})
	workerDone := make(chan struct{})
	var pin *requestViewPin
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			pin = handoffRequestView(hctx, viewmetrics.HandoffRepositoryIndex)
			if pin == nil {
				return nil, errors.New("a routed request handed off no payload at all")
			}
			if pin.owner == nil {
				return nil, errors.New("the handoff carried no repository admission for the stack's own repository")
			}
			go func() {
				defer close(workerDone)
				defer pin.release()
				<-worker
			}()
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("call: %v", err)
	}
	// The request has returned and released its own admission; only the
	// detached worker's joined hold is left.
	assertOutstandingHandoffs(t, viewmetrics.HandoffRepositoryIndex, 1)
	if got := pin.owner.Owners(); len(got) != 1 || got[0].RepoPrefix != "repo" {
		t.Fatalf("the detached worker is admitted to %v, want exactly the stack's repository", got)
	}
	drain, err := stack.leases.CloseRepositoryAdmission(owner)
	if err != nil {
		t.Fatalf("CloseRepositoryAdmission: %v", err)
	}
	assertNotDrained(t, drain, "a detached worker was still reading")

	close(worker)
	<-workerDone
	assertDrains(t, drain, "the detached worker finished")
	assertOutstandingHandoffs(t, viewmetrics.HandoffRepositoryIndex, 0)
}

// TestDetachedWorkerIsNotAdmittedToRepositoriesItDoesNotRead is the bound on
// the hold above, and the reason a detached worker gets a NAMED scope rather
// than the request's own.
//
// A serving request cannot know which repositories its handler will read, so
// it is admitted to every open one. That is safe for a request lifetime and
// unsafe for anything longer: handing the same scope to work of unbounded
// duration — `track`'s cold first index of repo A, which outlives its request
// by minutes — would block the cleanup drain, and therefore the physical
// payload purge behind an untrack, of every unrelated repo B for as long as it
// ran. Both arms are asserted: the unrouted request (track's own shape) hands
// off nothing at all, and a routed one hands off only its own repository.
func TestDetachedWorkerIsNotAdmittedToRepositoriesItDoesNotRead(t *testing.T) {
	viewmetrics.Reset()
	stack := newViewStack(t)
	registerViewStackOwner(t, stack)
	other := otherRepoOwner()
	if err := stack.leases.RegisterRepositoryOwner(other); err != nil {
		t.Fatalf("RegisterRepositoryOwner(other): %v", err)
	}

	// Arm 1 — the unrouted shape a `track` call has. It materialized no
	// payload, so there is nothing for a worker to keep alive and no scope to
	// extend past the request.
	if _, err := stack.callWithView(t, stack.repoRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			if view := requestViewFromContext(hctx); view != nil && view.materialized != nil {
				return nil, errors.New("the fixture routed a view; this arm must exercise the base-corpus path")
			}
			if scope := requestRepositoryScopeFromContext(hctx); len(scope.Owners()) != 2 {
				return nil, errors.New("the request itself was not admitted to both open repositories")
			}
			if pin := handoffRequestView(hctx, viewmetrics.HandoffRepositoryIndex); pin != nil {
				pin.release()
				return nil, errors.New("an unrouted request handed a detached worker a repository scope it does not read")
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("unrouted call: %v", err)
	}

	// Arm 2 — a routed request whose worker outlives it. The unrelated
	// repository's drain must not wait behind it.
	worker := make(chan struct{})
	workerDone := make(chan struct{})
	var pin *requestViewPin
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			pin = handoffRequestView(hctx, viewmetrics.HandoffRepositoryIndex)
			if pin == nil {
				return nil, errors.New("a routed request handed off nothing")
			}
			go func() {
				defer close(workerDone)
				defer pin.release()
				<-worker
			}()
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("routed call: %v", err)
	}
	drain, err := stack.leases.CloseRepositoryAdmission(other)
	if err != nil {
		t.Fatalf("CloseRepositoryAdmission(other): %v", err)
	}
	select {
	case <-drain.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("untracking an unrelated repository waited behind a detached worker that never reads it")
	}
	close(worker)
	<-workerDone
}

// TestRoutedRequestHandsOffTheBasePin closes a gap the base-corpus pin left:
// requestView.close released the base pin unconditionally while the handoff
// joined only the
// derived generation lease, so a detached worker kept the stack and lost both
// halves of the corpus underneath it — generation zero and the owner that
// speaks for it.
func TestRoutedRequestHandsOffTheBasePin(t *testing.T) {
	viewmetrics.Reset()
	stack := newViewStack(t)
	registerViewStackOwner(t, stack)

	worker := make(chan struct{})
	workerDone := make(chan struct{})
	var pin *requestViewPin
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			view := requestViewFromContext(hctx)
			if view == nil || view.materialized == nil || view.basePin == nil {
				return nil, errors.New("the routed request took no base pin to hand off")
			}
			pin = handoffRequestView(hctx, viewmetrics.HandoffFileMutation)
			if pin == nil {
				return nil, errors.New("a routed request handed off nothing")
			}
			go func() {
				defer close(workerDone)
				defer pin.release()
				<-worker
			}()
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if pin.base == nil {
		t.Fatal("the handoff dropped the request's base pin")
	}
	if got := pin.base.Generations(); len(got) != 1 || got[0] != graphview.BaseCorpusGeneration {
		t.Fatalf("the handed-off base pin holds %v, want [%d]", got, graphview.BaseCorpusGeneration)
	}
	if !pin.base.OwnerPinned() {
		t.Fatal("the handed-off base pin lost the registered owner half")
	}
	if !stack.leases.InUse(graphview.BaseCorpusGeneration) {
		t.Fatal("the detached worker is reading an unpinned base corpus")
	}
	close(worker)
	<-workerDone
	assertOutstandingHandoffs(t, viewmetrics.HandoffFileMutation, 0)
	if stack.leases.InUse(graphview.BaseCorpusGeneration) {
		t.Fatal("the base corpus stayed pinned after every holder released")
	}
}

// TestClientCancellationRetainsWhatTheHandlerStillReads is the fifth
// request-outliving path. A cancelled client frees the transport, but the
// handler goroutine keeps running and keeps reading the view and the
// repositories it was admitted to — and that arm held nothing and counted
// nothing at all, unlike the deadline arm beside it.
func TestClientCancellationRetainsWhatTheHandlerStillReads(t *testing.T) {
	viewmetrics.Reset()
	stack := newViewStack(t)
	registerViewStackOwner(t, stack)
	stack.srv.ToolCallTimeout = 30 * time.Second

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	handlerReturned := make(chan struct{})
	ctx, cancel := context.WithCancel(
		WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot))
	defer cancel()
	go func() {
		<-entered
		cancel()
	}()

	_, err := stack.callWithViewContext(t, ctx, "get_symbol",
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			entered <- struct{}{}
			<-release
			close(handlerReturned)
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call = %v, want %v", err, context.Canceled)
	}
	// The frame answered; the handler did not. What it is still reading is
	// held on its behalf and accounted for in the same series the deadline arm
	// reports under.
	waitOutstandingHandoffs(t, viewmetrics.HandoffAbandonedHandler, 1)
	if got := handoffCounter(t, viewmetrics.HandoffTotal, viewmetrics.HandoffAbandonedHandler, viewmetrics.HandoffJoined); got != 1 {
		t.Fatalf("joined handoffs after a client cancellation = %d, want 1", got)
	}
	if !stack.leases.InUse(stack.dirty) {
		t.Fatal("the cancelled handler is reading an unpinned generation")
	}
	if !stack.leases.InUse(graphview.BaseCorpusGeneration) {
		t.Fatal("the cancelled handler is reading an unpinned base corpus")
	}

	close(release)
	<-handlerReturned
	// The hold is released by the waiter that observes the handler exit, not
	// by the frame that answered, so the level comes back down on its own.
	waitOutstandingHandoffs(t, viewmetrics.HandoffAbandonedHandler, 0)
	if got := handoffCounter(t, viewmetrics.HandoffTotal, viewmetrics.HandoffAbandonedHandler, viewmetrics.HandoffRefused); got != 0 {
		t.Fatalf("refused handoffs = %d, want 0", got)
	}
}

// TestCancelledHandlerKeepsTheRepositoryOwnerLease is the same arm stated in
// repository lifetime rather than counters, on the unrouted shape that has no
// payload to pin: an untrack that starts while a cancelled handler is still
// running waits for the handler, because the middleware frame holding the
// request's admission is inside the goroutine that has not returned.
func TestCancelledHandlerKeepsTheRepositoryOwnerLease(t *testing.T) {
	viewmetrics.Reset()
	stack := newViewStack(t)
	owner := registerViewStackOwner(t, stack)
	stack.srv.ToolCallTimeout = 30 * time.Second

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	handlerReturned := make(chan struct{})
	ctx, cancel := context.WithCancel(
		WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.repoRoot))
	defer cancel()
	go func() {
		<-entered
		cancel()
	}()

	if _, err := stack.callWithViewContext(t, ctx, "get_symbol",
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			entered <- struct{}{}
			<-release
			close(handlerReturned)
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call = %v, want %v", err, context.Canceled)
	}

	drain, err := stack.leases.CloseRepositoryAdmission(owner)
	if err != nil {
		t.Fatalf("CloseRepositoryAdmission: %v", err)
	}
	assertNotDrained(t, drain, "a cancelled handler was still running")
	close(release)
	<-handlerReturned
	assertDrains(t, drain, "the cancelled handler exited")
}

// TestResourceReadHoldsTheRepositoryOwnerLease is the resources/prompts half
// of the serving-scope acquisition.
//
// boundResourceHandler and boundPromptHandler wrap requestScoped, not the tool
// middleware, so the admission had to be taken there too — and a resource read
// takes the base-corpus path (SelectorAuto, no selector to refuse) far more
// often than a tool call does, which is exactly the shape that used to hold no
// repository lifetime at all. Both surfaces are driven through their real
// production wrappers.
func TestResourceReadHoldsTheRepositoryOwnerLease(t *testing.T) {
	stack := newViewStack(t)
	owner := registerViewStackOwner(t, stack)
	stack.srv.ToolCallTimeout = 30 * time.Second
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.repoRoot)

	var drain *graphview.RepositoryDrain
	resource := stack.srv.boundResourceHandler("gortex://probe",
		func(hctx context.Context, _ mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
			if view := requestViewFromContext(hctx); view != nil && view.materialized != nil {
				return nil, errors.New("the fixture routed a view; this arm must exercise the base-corpus path")
			}
			closed, err := stack.leases.CloseRepositoryAdmission(owner)
			if err != nil {
				return nil, err
			}
			drain = closed
			select {
			case <-drain.Done():
				return nil, errors.New("the repository drained while a resource read was still being served")
			case <-time.After(50 * time.Millisecond):
			}
			return nil, nil
		})
	if _, err := resource(ctx, mcplib.ReadResourceRequest{}); err != nil {
		t.Fatalf("resource read: %v", err)
	}
	assertDrains(t, drain, "the resource read returned")
}

// TestPromptFetchHoldsTheRepositoryOwnerLease is the prompt surface of the
// same seam: boundPromptHandler wraps the identical requestScoped middleware,
// and gortex's prompt handlers run whole-graph passes over the corpus they
// must therefore keep registered.
func TestPromptFetchHoldsTheRepositoryOwnerLease(t *testing.T) {
	stack := newViewStack(t)
	owner := registerViewStackOwner(t, stack)
	stack.srv.ToolCallTimeout = 30 * time.Second
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.repoRoot)

	var drain *graphview.RepositoryDrain
	prompt := stack.srv.boundPromptHandler("probe",
		func(hctx context.Context, _ mcplib.GetPromptRequest) (*mcplib.GetPromptResult, error) {
			if scope := requestRepositoryScopeFromContext(hctx); len(scope.Owners()) != 1 {
				return nil, errors.New("a prompt fetch was admitted to no repository")
			}
			closed, err := stack.leases.CloseRepositoryAdmission(owner)
			if err != nil {
				return nil, err
			}
			drain = closed
			select {
			case <-drain.Done():
				return nil, errors.New("the repository drained while a prompt fetch was still being served")
			case <-time.After(50 * time.Millisecond):
			}
			return &mcplib.GetPromptResult{}, nil
		})
	if _, err := prompt(ctx, mcplib.GetPromptRequest{}); err != nil {
		t.Fatalf("prompt fetch: %v", err)
	}
	assertDrains(t, drain, "the prompt fetch returned")
}

// TestServingScopeOmitsAClosingRepository keeps the availability half honest:
// untracking one repository must not deny service to requests, and must not be
// starved by them either. A request that arrives after the close is admitted
// to the repositories that are still open and to nothing else, so the closing
// registration's drain is reachable within one request lifetime.
func TestServingScopeOmitsAClosingRepository(t *testing.T) {
	stack := newViewStack(t)
	owner := registerViewStackOwner(t, stack)
	drain, err := stack.leases.CloseRepositoryAdmission(owner)
	if err != nil {
		t.Fatalf("CloseRepositoryAdmission: %v", err)
	}
	select {
	case <-drain.Done():
	default:
		t.Fatal("an idle registration did not drain on close")
	}
	var scoped []graphview.RepositoryOwner
	if _, err := stack.callWithView(t, stack.repoRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			scoped = requestRepositoryScopeFromContext(hctx).Owners()
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("a request during an untrack was refused: %v", err)
	}
	if len(scoped) != 0 {
		t.Fatalf("a request was admitted to a closing repository: %v", scoped)
	}
}
