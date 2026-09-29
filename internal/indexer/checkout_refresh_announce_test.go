package indexer

import (
	"sync/atomic"
	"testing"
)

// TestAWaitingRefreshTicketAnnouncesItsWrite pins that a refresh ticket holds
// a store write announcement from its admission until it completes: while it
// is held, a WAL reclaim holding or about to take the store's writer yields
// at once (store_sqlite: TestWALReclaimYieldsToAnAnnouncedMutation, within
// 100 ms), so an edit is never queued behind the reclaim. The announcement is
// released exactly once, when the ticket finishes.
func TestAWaitingRefreshTicketAnnouncesItsWrite(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	var held, announced atomic.Int32
	c.announceWrite = func() func() {
		held.Add(1)
		announced.Add(1)
		released := false
		return func() {
			if !released {
				released = true
				held.Add(-1)
			}
		}
	}
	ticket := queueCheckoutSourceEdit(t, f, l, "package fixture\nfunc AnnouncedHelper() {}\n")
	if announced.Load() != 1 || held.Load() != 1 {
		t.Fatalf("an admitted ticket announced %d writes, %d held; want 1 held", announced.Load(), held.Load())
	}
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil {
		t.Fatalf("refresh: %+v", result)
	}
	if held.Load() != 0 {
		t.Fatalf("a completed ticket still holds %d write announcements", held.Load())
	}
}
