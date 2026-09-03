package mcp

import (
	"context"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func putViewWorktreeInAvailabilityGrace(t *testing.T, stack *viewStack) {
	t.Helper()
	stack.setWorktreeState(t, store_sqlite.CheckoutStateAvailabilityGrace)
	if err := stack.store.Catalog().DeleteCheckoutRoute(context.Background(), viewTestWorktree); err != nil {
		t.Fatalf("withdraw worktree route: %v", err)
	}
	if _, found, err := stack.store.Catalog().GetCheckoutRoute(context.Background(), viewTestWorktree); err != nil {
		t.Fatalf("read withdrawn worktree route: %v", err)
	} else if found {
		t.Fatal("availability grace retained the worktree route")
	}
}

func TestAvailabilityGraceRetainsIdentityForEligiblePrimaryFallback(t *testing.T) {
	stack := newViewStack(t)
	putViewWorktreeInAvailabilityGrace(t, stack)

	var reader graph.Reader
	res, err := stack.callWithView(t, stack.repoRoot, "search_symbols", worktreeViewArgs(),
		captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatalf("eligible grace search: %v", err)
	}
	if res.IsError {
		t.Fatalf("eligible grace search was refused: %s", viewResultText(t, res))
	}
	if !hasNode(reader, "repo/edit.go::Old") {
		t.Error("availability grace did not fall back to the primary corpus")
	}
	if hasNode(reader, "repo/added.go::Fresh") || hasNode(reader, "repo/keep.go::Dirty") {
		t.Error("withdrawn worktree layers leaked into the availability-grace fallback")
	}

	rider := resultFreshness(t, res)
	wantActual := "base:" + stack.graphID
	if rider["requested_view"] != "worktree:"+viewTestWorktree || rider["actual_view"] != wantActual {
		t.Errorf("rider = %v, want retained worktree request and %q answer", rider, wantActual)
	}
	if rider["exact"] != false || rider["fallback_reason"] != string(store_sqlite.CheckoutStateAvailabilityGrace) {
		t.Errorf("rider = %v, want labeled availability-grace fallback", rider)
	}
	if rider["graph_id"] != stack.graphID || rider["checkout_id"] != viewTestWorktree {
		t.Errorf("rider identity = %v, want graph %q checkout %q", rider, stack.graphID, viewTestWorktree)
	}
}

func TestAvailabilityGraceKeepsExactFileAndEditPoliciesStrict(t *testing.T) {
	stack := newViewStack(t)
	putViewWorktreeInAvailabilityGrace(t, stack)

	for _, tool := range []string{"get_symbol", "read_file", "edit_file"} {
		t.Run(tool, func(t *testing.T) {
			ran := false
			res, err := stack.callWithView(t, stack.repoRoot, tool, worktreeViewArgs(),
				func(context.Context) (*mcplib.CallToolResult, error) {
					ran = true
					return mcplib.NewToolResultText(`{"ok":true}`), nil
				})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			assertToolError(t, res, graphview.CodeCheckoutInaccessible)
			if ran {
				t.Errorf("%s reached its handler during availability grace", tool)
			}
		})
	}
}

func TestForgottenGraceCheckoutIDDoesNotCaptureBaseRequests(t *testing.T) {
	stack := newViewStack(t)
	putViewWorktreeInAvailabilityGrace(t, stack)
	if err := stack.store.Catalog().DeleteCheckout(context.Background(), viewTestWorktree); err != nil {
		t.Fatalf("forget expired checkout: %v", err)
	}

	ran := false
	res, err := stack.callWithView(t, stack.repoRoot, "search_symbols", worktreeViewArgs(),
		func(context.Context) (*mcplib.CallToolResult, error) {
			ran = true
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("stale worktree selector: %v", err)
	}
	assertToolError(t, res, graphview.CodeCheckoutInaccessible)
	if ran {
		t.Fatal("a forgotten checkout ID reached the search handler")
	}

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{name: "default"},
		{name: "base", args: map[string]any{"view": map[string]any{"kind": "base", "graph_id": stack.graphID}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reader graph.Reader
			res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", tc.args,
				captureReader(stack.srv, &reader))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if res.IsError {
				t.Fatalf("surviving %s selection was refused: %s", tc.name, viewResultText(t, res))
			}
			if !hasNode(reader, "repo/edit.go::Old") {
				t.Errorf("surviving %s selection did not read the primary corpus", tc.name)
			}
			if hasNode(reader, "repo/added.go::Fresh") {
				t.Errorf("surviving %s selection read the forgotten worktree layer", tc.name)
			}
		})
	}
}
