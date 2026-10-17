package indexer

import (
	"context"
	"testing"
	"time"
)

func openTestViewBuildGate() *ViewBuildGate {
	g := NewViewBuildGate()
	g.Open()
	return g
}

func yieldFired(yield <-chan struct{}) bool {
	if yield == nil {
		return false
	}
	select {
	case <-yield:
		return true
	default:
		return false
	}
}

// queueInteractive starts an interactive Acquire in the background and waits
// until it is queued. The returned func waits for its grant and releases it.
func queueInteractive(t *testing.T, g *ViewBuildGate) func() {
	t.Helper()
	granted := make(chan func(), 1)
	go func() {
		release, err := g.Acquire(context.Background(), ViewBuildInteractive)
		if err != nil {
			t.Errorf("interactive acquire: %v", err)
			granted <- func() {}
			return
		}
		granted <- release
	}()
	deadline := time.Now().Add(2 * time.Second)
	for g.Stats().InteractiveQueued == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the interactive build never queued")
		}
		time.Sleep(time.Millisecond)
	}
	return func() {
		select {
		case release := <-granted:
			release()
		case <-time.After(2 * time.Second):
			t.Fatal("the interactive build was never granted the lane")
		}
	}
}

// TestAnInteractiveWaiterAsksAYieldableBackgroundHolderToYield: the lane is
// held by background work that armed cooperative preemption; an edit's build
// queueing closes the holder's channel, and once the holder releases, the
// edit's build gets the lane ahead of the holder's re-queued attempt.
func TestAnInteractiveWaiterAsksAYieldableBackgroundHolderToYield(t *testing.T) {
	g := openTestViewBuildGate()
	release, err := g.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	yield, withdraw := g.NoteYieldable(0)
	defer withdraw()
	if yield == nil || yieldFired(yield) {
		t.Fatal("an idle lane asked the holder to yield")
	}
	// Background demand does not preempt background work.
	bgDone := make(chan func(), 1)
	go func() {
		r, err := g.Acquire(context.Background(), ViewBuildBackground)
		if err != nil {
			t.Errorf("background acquire: %v", err)
		}
		bgDone <- r
	}()
	for g.Stats().BackgroundQueued == 0 {
		time.Sleep(time.Millisecond)
	}
	if yieldFired(yield) {
		t.Fatal("a queued background build preempted background work")
	}

	finish := queueInteractive(t, g)
	select {
	case <-yield:
	case <-time.After(2 * time.Second):
		t.Fatal("the holder was not asked to yield to a waiting interactive build")
	}
	if got := g.Stats().YieldRequests; got != 1 {
		t.Fatalf("YieldRequests = %d, want 1", got)
	}
	release() // the holder yields at its safe point
	finish()  // the interactive build is granted before the queued background one
	if r := <-bgDone; r != nil {
		r()
	}
}

func TestNoteYieldableFiresAtOnceWhenAnInteractiveBuildAlreadyWaits(t *testing.T) {
	g := openTestViewBuildGate()
	release, err := g.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	finish := queueInteractive(t, g)
	yield, withdraw := g.NoteYieldable(1)
	defer withdraw()
	if !yieldFired(yield) {
		t.Fatal("arming with an interactive build already queued did not ask for the lane")
	}
	release()
	finish()
}

// TestInteractiveAndExhaustedHoldersAreNotPreempted: an interactive holder
// is never asked to yield, and background work that has already yielded
// maxViewBuildYields times runs to completion.
func TestInteractiveAndExhaustedHoldersAreNotPreempted(t *testing.T) {
	g := openTestViewBuildGate()
	release, err := g.Acquire(context.Background(), ViewBuildInteractive)
	if err != nil {
		t.Fatal(err)
	}
	if yield, _ := g.NoteYieldable(0); yield != nil {
		t.Fatal("an interactive holder was armed for preemption")
	}
	release()

	release, err = g.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	yield, withdraw := g.NoteYieldable(maxViewBuildYields)
	defer withdraw()
	if yield != nil {
		t.Fatal("background work past its yield budget was armed for preemption")
	}
	finish := queueInteractive(t, g)
	if got := g.Stats(); got.YieldRequests != 0 || got.YieldRefusals != 1 {
		t.Fatalf("stats = requests %d refusals %d, want 0 and 1", got.YieldRequests, got.YieldRefusals)
	}
	release()
	finish()
}

// TestAWithdrawnOrReleasedYieldDoesNotFire: a holder past its commit point
// withdraws, and a later holder of the lane never inherits an earlier
// holder's channel.
func TestAWithdrawnOrReleasedYieldDoesNotFire(t *testing.T) {
	g := openTestViewBuildGate()
	release, err := g.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	yield, withdraw := g.NoteYieldable(0)
	withdraw()
	finish := queueInteractive(t, g)
	if yieldFired(yield) {
		t.Fatal("a withdrawn yield fired")
	}
	release()
	finish()

	release, err = g.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := g.NoteYieldable(0)
	release()
	release, err = g.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	finish = queueInteractive(t, g)
	if yieldFired(first) {
		t.Fatal("an earlier holder's channel fired for the next holder's lane")
	}
	release()
	finish()
}
