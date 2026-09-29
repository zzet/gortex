package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// A background cycle that ran to its end — here one whose reconcile fails, as
// a build of a working tree edited faster than it is sampled does — ends the
// run of consecutive yields. Otherwise a checkout whose builds keep failing
// would stay unpreemptible for good once it had yielded maxViewBuildYields
// times, and every interactive edit would wait out its whole doomed build.
func TestABackgroundCycleThatRanToItsEndResetsTheYieldCount(t *testing.T) {
	gate := NewViewBuildGate()
	gate.Open()
	store := builderOpenStoreAt(t, filepath.Join(t.TempDir(), "catalog.sqlite"))
	outcomes := make(chan CheckoutCycle, 1)
	c := &CheckoutCoordinator{
		checkoutID: "background",
		// A family with no primary dedicated graph: reconcile fails at its
		// first catalog read, after the cycle has held the lane.
		familyID:       "family-without-a-primary",
		catalog:        store.Catalog(),
		gate:           gate,
		logger:         zap.NewNop(),
		signal:         make(chan struct{}, 1),
		done:           make(chan struct{}),
		lifetime:       t.Context(),
		cyclePreflight: func(context.Context) (CheckoutCycle, bool) { return CheckoutCycle{}, false },
		cycleDone:      func(out CheckoutCycle) { outcomes <- out },
		laneYields:     maxViewBuildYields,
	}
	c.cycle(t.Context())
	out := awaitLaneYieldOutcome(t, outcomes)
	if out.YieldedTo != "" || out.Rescheduled {
		t.Fatalf("a cycle nothing asked to yield reported a yield: %+v", out)
	}
	if out.Err == nil {
		t.Fatalf("reconcile was expected to fail, so the test covers a failed cycle: %+v", out)
	}
	if got := c.backgroundLaneYields(); got != 0 {
		t.Fatalf("yield count after a failed background cycle = %d, want 0 (err %v)", got, out.Err)
	}
	// The cycle is armed however many yields it counted (checkout cycles are
	// never unpreemptible), so the gate refused nothing.
	if stats := gate.Stats(); stats.YieldRefusals != 0 || stats.Active {
		t.Fatalf("gate after the cycle: refusals %d active %t, want no refusal and the lane released", stats.YieldRefusals, stats.Active)
	}
}
