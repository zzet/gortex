package indexer

import (
	"slices"
	"testing"
	"time"
)

// What a cycle reports about the time before it builds: which stage it waited
// in and what held the build lane, and — for a change no ticket named — the
// publication record it opens so a filesystem edit is timed like an MCP edit.

// newRecordKeys returns the records of checkoutID opened since before.
func newRecordKeys(checkoutID string, before []PublicationPhaseSnapshot) []PublicationPhaseSnapshot {
	seen := map[string]bool{}
	for _, r := range before {
		seen[r.Key] = true
	}
	var out []PublicationPhaseSnapshot
	for _, r := range DefaultPublicationPhases().Snapshot(checkoutID) {
		if !seen[r.Key] {
			out = append(out, r)
		}
	}
	return out
}

func phaseNames(r PublicationPhaseSnapshot) []PublicationPhase {
	var out []PublicationPhase
	for _, p := range r.Phases {
		out = append(out, p.Phase)
	}
	return out
}

func TestObservedChangeCycleOpensAPublicationRecord(t *testing.T) {
	f := newCoordinatorFixture(t)
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: 10 * time.Millisecond})
	cycles := make(chan CheckoutCycle, 8)
	c.cycleDone = func(out CheckoutCycle) { cycles <- out }
	awaitBuild := func() CheckoutCycle {
		t.Helper()
		for {
			select {
			case out := <-cycles:
				if out.DirtyBuilt || out.DirtyReused {
					return out
				}
			case <-time.After(30 * time.Second):
				t.Fatal("no cycle published the change")
			}
		}
	}
	// Bring the route up (the first cycle builds both slots).
	c.Signal("first")
	awaitBuild()

	before := DefaultPublicationPhases().Snapshot(f.checkoutID)
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	c.Signal("filesystem edit")
	out := awaitBuild()

	var observed *PublicationPhaseSnapshot
	for _, r := range newRecordKeys(f.checkoutID, before) {
		if r.Source == publicationSourceObservedChange {
			observed = &r
		}
	}
	if observed == nil {
		t.Fatalf("the ticketless cycle opened no observed-change record: %+v", newRecordKeys(f.checkoutID, before))
	}
	have := phaseNames(*observed)
	for _, phase := range []PublicationPhase{
		PublicationChangeObserved, PublicationAdmitted, PublicationPlanned, PublicationExtracted,
		PublicationPayloadFlushed, PublicationPublished, PublicationRouteFlipped, PublicationTicketCompleted,
	} {
		if !slices.Contains(have, phase) {
			t.Errorf("the observed-change record misses %s: %v", phase, have)
		}
	}
	if !observed.Terminal || observed.DirtyGenerationID != out.DirtyGenerationID {
		t.Fatalf("the observed-change record = %+v, want terminal on generation %d", observed, out.DirtyGenerationID)
	}

	// A cycle that finds nothing changed opens none.
	before = DefaultPublicationPhases().Snapshot(f.checkoutID)
	c.Signal("nothing changed")
	select {
	case <-cycles:
	case <-time.After(10 * time.Second):
		t.Fatal("the settle cycle never ran")
	}
	for _, r := range newRecordKeys(f.checkoutID, before) {
		if r.Source == publicationSourceObservedChange {
			t.Fatalf("a settled cycle opened an observed-change record: %+v", r)
		}
	}
}
