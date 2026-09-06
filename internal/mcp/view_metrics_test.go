package mcp

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// What the request seam reports about itself.
//
// A fallback is the one outcome a caller can miss: the answer arrives, it is
// correct about the base corpus, and only the rider says it is not about the
// view that was asked for. The counters make the rate visible without anyone
// reading a rider, and the reason label says which of the two ways a routed
// view can be unavailable it was.

func servedDelta(before, after viewmetrics.Snapshot, kind string) int64 {
	key := viewmetrics.RequestServedTotal + "{kind=" + kind + "}"
	return after.Counters[key] - before.Counters[key]
}

func fallbackDelta(before, after viewmetrics.Snapshot, reason string) int64 {
	key := viewmetrics.RequestFallbackTotal + "{reason=" + reason + "}"
	return after.Counters[key] - before.Counters[key]
}

// TestRoutedRequestCountsAsWorktreeServed pins the exact case: a session
// inside a routed worktree is answered by its own view and nothing is counted
// as a fallback.
func TestRoutedRequestCountsAsWorktreeServed(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader

	before := viewmetrics.Read()
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil, captureReader(stack.srv, &reader)); err != nil {
		t.Fatalf("call: %v", err)
	}
	after := viewmetrics.Read()

	if got := servedDelta(before, after, viewmetrics.ViewWorktree); got != 1 {
		t.Fatalf("worktree views served = %d, want 1", got)
	}
	if got := servedDelta(before, after, viewmetrics.ViewBase); got != 0 {
		t.Fatalf("a routed request was also counted as base (%d)", got)
	}
	for _, reason := range viewmetrics.ViewErrorCodes {
		if got := fallbackDelta(before, after, reason); got != 0 {
			t.Fatalf("an exact answer counted a %s fallback (%d)", reason, got)
		}
	}
}

// TestHalfRoutedRequestCountsABuildingFallback pins the common degradation: a
// checkout whose route names only one slot is answered from the base, and the
// reason is the one a caller can act on by waiting.
func TestHalfRoutedRequestCountsABuildingFallback(t *testing.T) {
	stack := newViewStack(t)
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, 0, store_sqlite.RouteActive)

	var reader graph.Reader
	before := viewmetrics.Read()
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil, captureReader(stack.srv, &reader)); err != nil {
		t.Fatalf("call: %v", err)
	}
	after := viewmetrics.Read()

	if got := fallbackDelta(before, after, graphview.CodeViewBuilding); got != 1 {
		t.Fatalf("view_building fallbacks = %d, want 1", got)
	}
	if got := fallbackDelta(before, after, graphview.CodeCheckoutInaccessible); got != 0 {
		t.Fatalf("a building route was counted as inaccessible (%d)", got)
	}
	if got := servedDelta(before, after, viewmetrics.ViewBase); got != 1 {
		t.Fatalf("base views served = %d, want 1", got)
	}
	if got := servedDelta(before, after, viewmetrics.ViewWorktree); got != 0 {
		t.Fatalf("a fallback was counted as a worktree view (%d)", got)
	}
}

// TestUnreadableBindingCountsAnInaccessibleFallback pins the other reason: a
// catalog that cannot be read is a different operational problem from a view
// that is still being built. Independent canonical ownership permits this
// fallback; an unbound automatic checkout is refused and is not counted served.
func TestUnreadableBindingCountsAnInaccessibleFallback(t *testing.T) {
	stack := newViewStack(t)
	breakCheckoutReads(t, stack.dbPath)

	var reader graph.Reader
	before := viewmetrics.Read()
	res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", nil, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("canonical fallback was refused: %s", viewResultText(t, res))
	}
	after := viewmetrics.Read()

	if got := fallbackDelta(before, after, graphview.CodeCheckoutInaccessible); got != 1 {
		t.Fatalf("checkout_inaccessible fallbacks = %d, want 1", got)
	}
	if got := fallbackDelta(before, after, graphview.CodeViewBuilding); got != 0 {
		t.Fatalf("an unreadable catalog was counted as a building view (%d)", got)
	}
	if got := servedDelta(before, after, viewmetrics.ViewBase); got != 1 {
		t.Fatalf("canonical base views served = %d, want 1", got)
	}
}

func TestUnreadableAutomaticBindingCountsNeitherServedNorFallback(t *testing.T) {
	stack := newViewStack(t)
	breakCheckoutReads(t, stack.dbPath)

	var reader graph.Reader
	before := viewmetrics.Read()
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	after := viewmetrics.Read()
	assertToolError(t, res, graphview.CodeCheckoutInaccessible)
	if reader != nil {
		t.Fatal("refused automatic checkout reached the graph handler")
	}
	for _, kind := range []string{viewmetrics.ViewBase, viewmetrics.ViewWorktree} {
		if got := servedDelta(before, after, kind); got != 0 {
			t.Fatalf("refused checkout counted a %s answer (%d)", kind, got)
		}
	}
	for _, reason := range viewmetrics.ViewErrorCodes {
		if got := fallbackDelta(before, after, reason); got != 0 {
			t.Fatalf("refused checkout counted a %s fallback (%d)", reason, got)
		}
	}
}

// TestPrimaryCWDCountsABaseView pins the case that is not a degradation at
// all: a path served from the indexed corpus is a base view, exactly, and must
// not inflate the fallback rate.
func TestPrimaryCWDCountsABaseView(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader

	before := viewmetrics.Read()
	if _, err := stack.callWithView(t, stack.repoRoot, "get_symbol", nil, captureReader(stack.srv, &reader)); err != nil {
		t.Fatalf("call: %v", err)
	}
	after := viewmetrics.Read()

	if got := servedDelta(before, after, viewmetrics.ViewBase); got != 1 {
		t.Fatalf("base views served = %d, want 1", got)
	}
	for _, reason := range viewmetrics.ViewErrorCodes {
		if got := fallbackDelta(before, after, reason); got != 0 {
			t.Fatalf("the primary's own corpus counted a %s fallback (%d)", reason, got)
		}
	}
}
