package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func shortenMutationRouteWait(t *testing.T, poll time.Duration) {
	t.Helper()
	previous := mutationRouteWaitPoll
	mutationRouteWaitPoll = poll
	t.Cleanup(func() { mutationRouteWaitPoll = previous })
}

func explicitWorktreeArgs() map[string]any {
	return map[string]any{"view": map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree}}
}

func okLeaf(ran *bool) func(context.Context) (*mcplib.CallToolResult, error) {
	return func(context.Context) (*mcplib.CallToolResult, error) {
		*ran = true
		return mcplib.NewToolResultText(`{"ok":true}`), nil
	}
}

// TestSourceMutationWaitsForARebuildingRoute pins the admission policy: a
// source mutation on an explicit worktree whose route is being rebuilt waits
// for the route instead of being refused.
func TestSourceMutationWaitsForARebuildingRoute(t *testing.T) {
	shortenMutationRouteWait(t, 20*time.Millisecond)
	stack := newViewStack(t)
	if stack.srv.facades == nil || !stack.srv.facades.mutatesSource("edit_file") {
		t.Fatal("fixture: the edit facade is not classified as a source mutation")
	}
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
	routed := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		routed <- stack.store.Catalog().UpsertCheckoutRoute(context.Background(), store_sqlite.CheckoutRoute{
			CheckoutID: viewTestWorktree, GraphID: stack.graphID,
			CommitGenerationID: stack.commit, DirtyGenerationID: stack.dirty, State: store_sqlite.RouteActive,
		})
	}()
	defer func() {
		if err := <-routed; err != nil {
			t.Errorf("re-route: %v", err)
		}
	}()
	var ran bool
	started := time.Now()
	res, err := stack.callWithView(t, stack.repoRoot, "edit_file", explicitWorktreeArgs(), okLeaf(&ran))
	if err != nil {
		t.Fatal(err)
	}
	// Past the route wait the fixture's lifecycle may still refuse the
	// checkout admission itself (it carries no live coordinator); what this
	// pins is that the route wait no longer refuses.
	if text := viewResultText(t, res); res.IsError && strings.Contains(text, "not fully routed") {
		t.Fatalf("the mutation was refused instead of waiting for the route: %s", text)
	}
	if waited := time.Since(started); waited < 150*time.Millisecond {
		t.Fatalf("the route was ready after %v; the fixture did not exercise the wait", waited)
	}
}

// TestSourceMutationRefusalAfterTheWaitIsAnErrorWithText keeps the refusal
// honest when the route does not come back within the request's deadline.
func TestSourceMutationRefusalAfterTheWaitIsAnErrorWithText(t *testing.T) {
	shortenMutationRouteWait(t, 20*time.Millisecond)
	stack := newViewStack(t)
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
	ctx, cancel := context.WithTimeout(WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.repoRoot),
		transportDeadlineMargin+mutationRouteWaitMargin+400*time.Millisecond)
	defer cancel()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = explicitWorktreeArgs()
	var ran bool
	started := time.Now()
	res, err := stack.srv.wrapToolHandler(func(hctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		return okLeaf(&ran)(hctx)
	})(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if ran || res == nil || !res.IsError {
		t.Fatalf("a mutation on an unrouted worktree was admitted: ran=%v res=%+v", ran, res)
	}
	if text := viewResultText(t, res); !strings.Contains(text, "not fully routed") {
		t.Fatalf("the refusal carries no reason: %q", text)
	}
	if waited := time.Since(started); waited < 200*time.Millisecond {
		t.Fatalf("the mutation was refused after %v without waiting for the route: %s", waited, viewResultText(t, res))
	}
}

// TestReadOnAnUnroutedWorktreeIsStillRefusedAtOnce keeps reads on the old
// posture: only a source mutation waits.
func TestReadOnAnUnroutedWorktreeIsStillRefusedAtOnce(t *testing.T) {
	shortenMutationRouteWait(t, 5*time.Second)
	stack := newViewStack(t)
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
	var ran bool
	started := time.Now()
	res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", explicitWorktreeArgs(), okLeaf(&ran))
	if err != nil {
		t.Fatal(err)
	}
	if ran || !res.IsError {
		t.Fatal("a read on an unrouted worktree was served")
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("a read waited %v for the route", waited)
	}
}

// An edit waiting out a rebuilding route loses up to one poll interval after
// the route comes back, on top of the rebuild itself. The shipped interval is
// a small fraction of the edit budget, and of the wait's own cap.
func TestMutationRouteWaitPollIsAFractionOfTheEditBudget(t *testing.T) {
	if mutationRouteWaitPoll > 25*time.Millisecond {
		t.Fatalf("mutation route wait polls every %v; an admitted edit then loses up to that much after its route returns", mutationRouteWaitPoll)
	}
	if mutationRouteWaitPoll <= 0 || mutationRouteWaitCap/mutationRouteWaitPoll < 8 {
		t.Fatalf("poll %v against cap %v: the wait re-selects too rarely to notice the route inside its cap", mutationRouteWaitPoll, mutationRouteWaitCap)
	}
}

func TestAutomaticMutationWaitsBeyondLexicalReadForItsOwnRoute(t *testing.T) {
	stack := newViewStack(t)
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
	ctx, cancel := context.WithTimeout(WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot), 5*time.Second)
	defer cancel()
	published := make(chan error, 1)
	go func() {
		time.Sleep(350 * time.Millisecond)
		published <- stack.store.Catalog().UpsertCheckoutRoute(context.Background(), store_sqlite.CheckoutRoute{CheckoutID: viewTestWorktree, GraphID: stack.graphID, CommitGenerationID: stack.commit, DirtyGenerationID: stack.dirty, State: store_sqlite.RouteActive})
	}()
	started := time.Now()
	view, err := stack.srv.resolveRequestView(ctx, graphview.Selector{Kind: graphview.SelectorAuto}, requestViewPolicy{awaitRoute: true})
	require.NoError(t, <-published)
	require.NoError(t, err)
	defer view.close()
	require.GreaterOrEqual(t, time.Since(started), 300*time.Millisecond)
	require.True(t, view.routed())
	require.True(t, view.rider.Exact)
	require.Equal(t, viewTestWorktree, view.rider.CheckoutID)
}
