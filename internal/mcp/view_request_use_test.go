package mcp

import (
	"context"
	"testing"
	"time"
)

// TestAPlainQueryOfARoutedWorktreeIsAUseForTheLazyRebase pins the MCP half of
// the lazy primary-to-worktree propagation: a primary base advance is only
// noted on a worktree's coordinator, and the next request that
// reads the routed worktree — a plain query, no edit and no fresh-read ticket —
// is what asks for it to be applied.
func TestAPlainQueryOfARoutedWorktreeIsAUseForTheLazyRebase(t *testing.T) {
	stack := newWorktreeSearchStackWithConfig(t, "workspace: main-ws\n")
	stats, found := stack.lifecycle.CheckoutPropagationStats(stack.checkoutID)
	if !found {
		t.Fatal("the routed worktree has no live coordinator")
	}
	if stats.Pending || stats.Wanted {
		t.Fatalf("a propagation is pending before any advance: %+v", stats)
	}

	// A query with nothing pending marks nothing.
	stack.search(t, stack.worktree, "Keeper")
	if stats, _ = stack.lifecycle.CheckoutPropagationStats(stack.checkoutID); stats.Wanted {
		t.Fatalf("a query with no advance pending marked a rebase wanted: %+v", stats)
	}

	checkout, found, err := stack.store.Catalog().GetCheckout(context.Background(), stack.checkoutID)
	if err != nil || !found {
		t.Fatalf("read the worktree checkout: found=%v err=%v", found, err)
	}
	stack.lifecycle.PropagateBaseAdvance(checkout.FamilyID, "", 1<<40, "advanced-tree", "test advance")
	stats, _ = stack.lifecycle.CheckoutPropagationStats(stack.checkoutID)
	if !stats.Pending || stats.Wanted || stats.Noted != 1 {
		t.Fatalf("the advance was not noted, or was marked wanted without a use: %+v", stats)
	}

	// A query of the primary checkout is not a use of the worktree.
	stack.search(t, stack.primary, "Keeper")
	if stats, _ = stack.lifecycle.CheckoutPropagationStats(stack.checkoutID); stats.Wanted {
		t.Fatalf("a query of the primary counted as a use of the worktree: %+v", stats)
	}

	// A plain query of the worktree is.
	stack.search(t, stack.worktree, "Keeper")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if stats, _ = stack.lifecycle.CheckoutPropagationStats(stack.checkoutID); stats.Wanted || stats.Applied > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a plain query of the routed worktree did not count as a use: %+v", stats)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
