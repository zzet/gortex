package semantic

import (
	"testing"
	"testing/synctest"

	"go.uber.org/zap"
)

// A committed tree's pass takes only a free slot, and an admission for an
// edit takes that slot while the pass holds it, ending the pass instead of
// stopping a server.
func TestCommittedWorkspaceEntryYieldsToAnEdit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stopper := &recordingStopper{}
		w := NewCheckoutWorkspaces(2, zap.NewNop())
		w.SetStopper(stopper)

		holdOne := acquire(t, w, "go", famRoot("first"))
		defer holdOne()
		preempted := 0
		releaseCommitted, ok := w.AcquireCommitted("go", famRoot("first"), func() { preempted++ })
		if !ok {
			t.Fatal("a committed pass was refused a free slot")
		}
		// The committed entry is its own: the checkout's pair is still held
		// once, by the edit.
		if got := len(w.Live()); got != 2 {
			t.Fatalf("live entries %d, want 2 (the checkout's pair and the committed entry)", got)
		}
		// No slot is free now: a second committed pass is refused rather than
		// evicting anything.
		if _, ok := w.AcquireCommitted("go", famRoot("second"), func() {}); ok {
			t.Fatal("a committed pass was admitted with no free slot")
		}

		release := acquire(t, w, "go", famRoot("second"))
		defer release()
		synctest.Wait()
		if preempted != 1 {
			t.Errorf("the committed pass was preempted %d times, want 1", preempted)
		}
		if got := stopper.calls(); len(got) != 0 {
			t.Errorf("a server was stopped for a committed entry: %v", got)
		}
		// The released committed hold of an evicted entry changes nothing.
		releaseCommitted()
		if got := len(w.Live()); got != 2 {
			t.Errorf("live entries %d after the release, want 2", got)
		}
	})
}

// A committed entry gives its slot back as soon as no pass holds it.
func TestCommittedWorkspaceEntryLeavesWithItsLastHold(t *testing.T) {
	w := NewCheckoutWorkspaces(1, zap.NewNop())
	release, ok := w.AcquireCommitted("go", famRoot("first"), func() {})
	if !ok {
		t.Fatal("refused a free slot")
	}
	release()
	if got := len(w.Live()); got != 0 {
		t.Fatalf("live entries %d after the last hold, want 0", got)
	}
	release2 := acquire(t, w, "go", famRoot("first"))
	release2()
}
