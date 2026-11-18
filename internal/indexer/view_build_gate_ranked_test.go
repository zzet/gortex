package indexer

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// The shared build lane under contention, in fake time: every granted build
// holds the lane for rankedGateBuild. What an interactive waiter (a cycle
// serving a caller's ticket) waits for is measured against the old policy
// (arrival order within a class, background after a burst of four) and the
// current one (background only once starved, newest demand first).

const rankedGateBuild = 500 * time.Millisecond

// runGateBuild acquires the lane with rank, records when it was granted, holds
// it for one build and releases it.
func runGateBuild(ctx context.Context, gate *ViewBuildGate, priority ViewBuildPriority, rank func() int64, granted chan<- string, name string) {
	release, err := gate.AcquireRanked(ctx, priority, nil, rank)
	if err != nil {
		granted <- name + ":error"
		return
	}
	granted <- name
	time.Sleep(rankedGateBuild)
	release()
}

// interactiveWaitBehindBackground queues a background build while the burst
// of interactive grants is spent, then an interactive one, and returns how
// long the interactive one waited and the grant order.
func interactiveWaitBehindBackground(t *testing.T, oldPolicy bool) (time.Duration, []string) {
	var waited time.Duration
	var order []string
	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		if oldPolicy {
			gate.backgroundStarvation = 0
		}
		gate.Open()
		active, err := gate.Acquire(t.Context(), ViewBuildBackground)
		if err != nil {
			t.Fatal(err)
		}
		// Four interactive builds were granted in a row just before.
		gate.mu.Lock()
		gate.interactiveBurst = maxInteractiveBuildBurst
		gate.mu.Unlock()
		granted := make(chan string, 4)
		go runGateBuild(t.Context(), gate, ViewBuildBackground, nil, granted, "background")
		synctest.Wait()
		queued := time.Now()
		go runGateBuild(t.Context(), gate, ViewBuildInteractive, nil, granted, "interactive")
		synctest.Wait()
		time.Sleep(rankedGateBuild) // the in-flight build
		active()
		for range 2 {
			name := <-granted
			order = append(order, name)
			if name == "interactive" {
				waited = time.Since(queued)
			}
		}
		time.Sleep(4 * rankedGateBuild) // let the builds still holding the lane finish
		synctest.Wait()
	})
	return waited, order
}

func TestViewBuildGateInteractiveTicketNeverWaitsBehindQueuedBackground(t *testing.T) {
	before, beforeOrder := interactiveWaitBehindBackground(t, true)
	after, afterOrder := interactiveWaitBehindBackground(t, false)
	t.Logf("interactive wait with a background build queued and the burst spent: old policy %v (order %v), current %v (order %v); one in-flight build = %v",
		before, beforeOrder, after, afterOrder, rankedGateBuild)
	if after > rankedGateBuild {
		t.Fatalf("the interactive build waited %v, want at most the one in-flight build (%v)", after, rankedGateBuild)
	}
	if afterOrder[0] != "interactive" {
		t.Fatalf("grant order %v, want the interactive build before the queued background one", afterOrder)
	}
}

func TestViewBuildGateStarvedBackgroundStillRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		gate.Open()
		active, err := gate.Acquire(t.Context(), ViewBuildBackground)
		if err != nil {
			t.Fatal(err)
		}
		granted := make(chan string, 16)
		go runGateBuild(t.Context(), gate, ViewBuildBackground, nil, granted, "background")
		synctest.Wait()
		// A continuous interactive stream: one queued behind the active build
		// whenever the lane changes hands.
		stream := func(i int) {
			go runGateBuild(t.Context(), gate, ViewBuildInteractive, nil, granted, "interactive")
			synctest.Wait()
		}
		stream(0)
		active()
		var order []string
		for i := 1; len(order) < 16; i++ {
			name := <-granted
			order = append(order, name)
			if name == "background" {
				break
			}
			stream(i)
		}
		time.Sleep(4 * rankedGateBuild) // let the builds still holding the lane finish
		synctest.Wait()
		if order[len(order)-1] != "background" {
			t.Fatalf("the background build never ran under a continuous interactive stream: %v", order)
		}
		// It ran only once it had waited past the starvation bound and a
		// burst of interactive grants had passed.
		if interactive := len(order) - 1; interactive < maxInteractiveBuildBurst ||
			time.Duration(interactive)*rankedGateBuild+rankedGateBuild < viewBuildBackgroundStarvation {
			t.Fatalf("background ran after %d interactive builds, want a starvation-bound wait (%v) and a full burst", interactive, viewBuildBackgroundStarvation)
		}
		t.Logf("under a continuous interactive stream the background build ran after %d interactive builds", len(order)-1)
	})
}

// newestDemandWait queues another checkout's interactive cycle, then the
// edited checkout's (whose ticket arrived later), and returns how long the
// edited checkout's cycle waited and the grant order.
func newestDemandWait(t *testing.T, ranked bool) (time.Duration, []string) {
	var waited time.Duration
	var order []string
	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		gate.Open()
		active, err := gate.Acquire(t.Context(), ViewBuildInteractive)
		if err != nil {
			t.Fatal(err)
		}
		granted := make(chan string, 4)
		otherDemand := time.Now().UnixNano()
		var otherRank, editedRank func() int64
		if ranked {
			otherRank = func() int64 { return otherDemand }
		}
		go runGateBuild(t.Context(), gate, ViewBuildInteractive, otherRank, granted, "other_checkout")
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		editedDemand := time.Now().UnixNano()
		if ranked {
			editedRank = func() int64 { return editedDemand }
		}
		queued := time.Now()
		go runGateBuild(t.Context(), gate, ViewBuildInteractive, editedRank, granted, "edited_checkout")
		synctest.Wait()
		time.Sleep(rankedGateBuild - 100*time.Millisecond)
		active()
		for range 2 {
			name := <-granted
			order = append(order, name)
			if name == "edited_checkout" {
				waited = time.Since(queued)
			}
		}
		time.Sleep(4 * rankedGateBuild) // let the builds still holding the lane finish
		synctest.Wait()
	})
	return waited, order
}

func TestViewBuildGateNewestDemandFirstAmongInteractiveCheckouts(t *testing.T) {
	before, beforeOrder := newestDemandWait(t, false)
	after, afterOrder := newestDemandWait(t, true)
	t.Logf("edited checkout's cycle behind another checkout's earlier cycle: arrival order %v (order %v), newest demand first %v (order %v)",
		before, beforeOrder, after, afterOrder)
	if afterOrder[0] != "edited_checkout" || after > rankedGateBuild {
		t.Fatalf("the edited checkout waited %v (order %v), want it granted first, within the in-flight build", after, afterOrder)
	}
}

func TestViewBuildGatePassedOverInteractiveWaiterIsNotStarved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		gate.Open()
		active, err := gate.Acquire(t.Context(), ViewBuildInteractive)
		if err != nil {
			t.Fatal(err)
		}
		granted := make(chan string, 32)
		oldDemand := time.Now().UnixNano()
		go runGateBuild(t.Context(), gate, ViewBuildInteractive, func() int64 { return oldDemand }, granted, "early")
		synctest.Wait()
		// A newer demand keeps arriving while the early waiter waits.
		newer := func() {
			time.Sleep(time.Millisecond)
			demand := time.Now().UnixNano()
			go runGateBuild(t.Context(), gate, ViewBuildInteractive, func() int64 { return demand }, granted, "newer")
			synctest.Wait()
		}
		newer()
		active()
		var order []string
		start := time.Now()
		var waited time.Duration
		for len(order) < 32 {
			name := <-granted
			order = append(order, name)
			if name == "early" {
				waited = time.Since(start)
				break
			}
			newer()
		}
		time.Sleep(4 * rankedGateBuild) // let the builds still holding the lane finish
		synctest.Wait()
		if order[len(order)-1] != "early" {
			t.Fatalf("the early waiter was starved: %v", order)
		}
		if waited > viewBuildInteractiveStarvation+rankedGateBuild {
			t.Fatalf("the early waiter waited %v, want at most the starvation bound plus one build", waited)
		}
		t.Logf("an interactive waiter passed over by newer demand was granted after %v (%d newer builds)", waited, len(order)-1)
	})
}
