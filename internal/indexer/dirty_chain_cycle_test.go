package indexer

import (
	"fmt"
	"testing"
	"time"
)

// The coordinator with working-tree chaining on: which parent a cycle builds
// over, what a reader pinned before a flip keeps seeing, what a torn or
// superseded build publishes, how a chain is retired once the route leaves
// it, and the foreground-cost rules the cycle keeps (a refresh ticket skips
// the quiet window, one cycle shares one working-copy sample, no cycle
// deletes payload).

const chainIslandTwo = "package fixture\n\nfunc Island() {\n}\n\nfunc IslandTwo() {\n}\n"

func chainCoordinator(t *testing.T, cfg CheckoutCoordinatorConfig) (*coordinatorFixture, *CheckoutCoordinator) {
	t.Helper()
	f := newCoordinatorFixture(t)
	return f, f.inertCoordinator(t, cfg)
}

// newRunningRefreshFixture is a coordinator whose loop runs, behind a quiet
// window far longer than any test waits, with a lifecycle that routes refresh
// requests to it.
func newRunningRefreshFixture(t *testing.T) (*coordinatorFixture, *CheckoutCoordinator, *CheckoutLifecycle) {
	t.Helper()
	f := newCoordinatorFixture(t)
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: time.Hour})
	l := &CheckoutLifecycle{
		catalog:      f.catalog,
		store:        f.store,
		coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c},
	}
	return f, c, l
}

func TestRefreshTicketCycleSkipsTheQuietWindow(t *testing.T) {
	f, _, l := newRunningRefreshFixture(t)
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)

	record := DefaultPublicationPhases().Begin(f.checkoutID,
		fmt.Sprintf("test-refresh-%d", time.Now().UnixNano()), PublicationSourceFreshRequest, time.Now())
	ctx := WithPublicationRecord(t.Context(), record)
	started := time.Now()
	ticket, err := l.RequestCheckoutRefresh(ctx, f.checkoutID, f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	// The window is an hour: only a demand wake can complete this ticket.
	result := awaitCheckoutRefresh(t, ticket)
	if result.Err != nil || !result.Reindexed || result.AppliedGeneration == 0 {
		t.Fatalf("the ticket = %+v, want a publication", result)
	}
	t.Logf("ticket published generation %d in %v with an hour-long quiet window",
		result.AppliedGeneration, time.Since(started))
	if got := f.route().DirtyGenerationID; uint64(got) != result.AppliedGeneration {
		t.Fatalf("the route names %d, the ticket reported %d", got, result.AppliedGeneration)
	}

	// The record handed in through the context was bound before the wake, so
	// the demand-woken cycle's first phases reached it.
	snap := record.Snapshot()
	have := map[PublicationPhase]bool{}
	for _, p := range snap.Phases {
		have[p.Phase] = true
	}
	for _, phase := range []PublicationPhase{
		PublicationTicketEnqueued, PublicationCycleStarted, PublicationAdmitted,
		PublicationPlanned, PublicationExtracted, PublicationPayloadFlushed, PublicationPublished, PublicationRouteFlipped,
	} {
		if !have[phase] {
			t.Errorf("the record misses %s: %+v", phase, snap.Phases)
		}
	}
	if snap.Ticket != ticket.Ticket.Generation {
		t.Errorf("the record is bound to ticket %d, want %d", snap.Ticket, ticket.Ticket.Generation)
	}
}
