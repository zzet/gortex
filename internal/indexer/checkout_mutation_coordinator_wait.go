package indexer

import (
	"context"
	"time"
)

// checkoutMutationCoordinatorWait bounds how long a mutation that arrives
// before its checkout's coordinator is registered (a daemon still activating
// its checkouts, say) waits for it, when the caller's context carries no
// earlier deadline.
const checkoutMutationCoordinatorWait = 30 * time.Second

// checkoutMutationCoordinatorPoll is how often that wait looks again.
const checkoutMutationCoordinatorPoll = 25 * time.Millisecond

// activateCheckoutForMutation is how a mutation that finds no coordinator
// asks for one; a variable so a test can observe the request without
// bringing a real coordinator up.
var activateCheckoutForMutation = (*CheckoutLifecycle).ActivateCheckout

// awaitMutationCoordinator returns the checkout's registered coordinator and
// whether the lifecycle is closing its coordinators. A checkout the catalog
// knows whose coordinator is not registered yet is waited for, bounded by the
// caller's context and checkoutMutationCoordinatorWait: an edit sent while
// the daemon is still activating is admitted once the coordinator exists
// instead of being refused as read-only, and a dormant one is activated
// first. A closing lifecycle answers at once.
func (l *CheckoutLifecycle) awaitMutationCoordinator(ctx context.Context, checkoutID string) (*CheckoutCoordinator, bool) {
	lookup := func() (*CheckoutCoordinator, bool) {
		l.coordMu.Lock()
		defer l.coordMu.Unlock()
		return l.coordinators[checkoutID], l.coordinatorClosing
	}
	c, closing := lookup()
	if c != nil || closing {
		return c, closing
	}
	// Waiting alone admits nothing when no one is bringing the coordinator
	// up: a checkout left dormant at startup, whose route still serves exact
	// reads from the catalog, stays unregistered until something activates
	// it, and the edit used to be refused only after the whole wait (the MCP
	// layer activated it then, for the retry). An edit is a use: ask for the
	// activation now and wait for it. Activation is idempotent and never
	// signals a live or activating coordinator.
	activateCheckoutForMutation(l, checkoutID, "source mutation needs its checkout coordinator")
	waitCtx, cancel := context.WithTimeout(ctx, checkoutMutationCoordinatorWait)
	defer cancel()
	ticker := time.NewTicker(checkoutMutationCoordinatorPoll)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return lookup()
		case <-ticker.C:
			if c, closing := lookup(); c != nil || closing {
				return c, closing
			}
		}
	}
}
