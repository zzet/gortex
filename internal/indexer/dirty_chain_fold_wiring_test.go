package indexer

import (
	"context"
	"errors"
	"testing"
	"time"
)

// mcpChainFixture is a running chained coordinator over tree, with the
// lifecycle an MCP edit goes through (BeginCheckoutMutation). Its loop is
// parked (every cycle is driven by an edit or by hand: overrides Debounce and
// debounceDemand); the compactor keeps the daemon's settings. With background
// off (an override: compaction closed), the
// compactions an edit schedules are refused, so a test runs its folds by hand.
func mcpChainFixture(t *testing.T, tree map[string]string, background bool) (*coordinatorFixture, *CheckoutCoordinator, *CheckoutLifecycle) {
	t.Helper()
	f := newCoordinatorFixtureWithTree(t, tree)
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: time.Hour, debounceDemand: true})
	if !background {
		c.compaction.mu.Lock()
		c.compaction.closed = true
		c.compaction.mu.Unlock()
	}
	if out := c.reconcile(context.Background()); out.Err != nil {
		t.Fatalf("initial reconcile: %+v", out)
	}
	l := &CheckoutLifecycle{catalog: f.catalog, store: f.store, coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	return f, c, l
}

// mcpEdit is one edit the way an agent's MCP edit makes it: a checkout
// mutation lease (which withdraws the routed working-tree top), the write,
// and the lease's synchronous republish.
func mcpEdit(t *testing.T, l *CheckoutLifecycle, f *coordinatorFixture, write func()) CheckoutCycle {
	t.Helper()
	ctx := context.Background()
	m, err := l.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatalf("begin the edit: %v", err)
	}
	defer m.Close()
	if err := m.Prepare(ctx); err != nil {
		t.Fatalf("prepare the edit: %v", err)
	}
	write()
	out, err := m.Refresh(ctx)
	if errors.Is(err, ErrCheckoutMutationPending) {
		// A large edit is built in batches: the lease published the first,
		// the checkout's next cycles build the rest.
		c := l.coordinators[f.checkoutID]
		for i := 0; i < 1000 && (out.Rescheduled || out.DirtyBatchRemaining > 0); i++ {
			out = c.reconcile(ctx)
			err = out.Err
		}
	}
	if err != nil {
		t.Fatalf("republish the edit: %v (%+v)", err, out)
	}
	return out
}
