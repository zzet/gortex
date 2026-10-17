package indexer

import (
	"fmt"
	"testing"
	"time"
)

func phaseOffsets(snapshot PublicationPhaseSnapshot) map[PublicationPhase]int64 {
	out := make(map[PublicationPhase]int64, len(snapshot.Phases))
	for _, phase := range snapshot.Phases {
		out[phase.Phase] = phase.OffsetNS
	}
	return out
}

func TestPublicationPhaseMarksAreFirstWinsAndMonotonic(t *testing.T) {
	r := NewPublicationPhaseRecorder(0, 0)
	origin := time.Now()
	record := r.Begin("checkout-a", "mutation-1", PublicationSourceMCPEdit, origin)
	if again := r.Begin("checkout-a", "mutation-1", PublicationSourceMCPEdit, origin.Add(time.Hour)); again != record {
		t.Fatal("reopening a key minted a second record")
	}

	record.MarkAt(PublicationReceiptCommitted, origin.Add(3*time.Millisecond))
	record.MarkAt(PublicationReceiptCommitted, origin.Add(9*time.Millisecond)) // later: loses
	record.MarkAt(PublicationTicketEnqueued, origin.Add(5*time.Millisecond))
	record.MarkAt(PublicationAdmitted, origin.Add(-time.Millisecond)) // before the origin: refused
	record.MarkAt(PublicationPlanned, time.Time{})                    // no instant: refused

	snapshot := record.Snapshot()
	offsets := phaseOffsets(snapshot)
	if got := offsets[PublicationReceived]; got != 0 {
		t.Fatalf("received is the origin, got offset %d", got)
	}
	if got := offsets[PublicationReceiptCommitted]; got != (3 * time.Millisecond).Nanoseconds() {
		t.Fatalf("receipt_committed is not first-wins: %d", got)
	}
	if _, ok := offsets[PublicationAdmitted]; ok {
		t.Fatal("a mark before the origin was recorded")
	}
	if _, ok := offsets[PublicationPlanned]; ok {
		t.Fatal("a zero instant was recorded")
	}
	for i := 1; i < len(snapshot.Phases); i++ {
		if snapshot.Phases[i].OffsetNS < snapshot.Phases[i-1].OffsetNS {
			t.Fatalf("phases are not ordered by offset: %+v", snapshot.Phases)
		}
	}
	if snapshot.Clock != "monotonic" || snapshot.Terminal || snapshot.Source != PublicationSourceMCPEdit || snapshot.CheckoutID != "checkout-a" {
		t.Fatalf("snapshot header: %+v", snapshot)
	}

	record.SetGeneration(42)
	record.MarkAt(PublicationTicketCompleted, origin.Add(20*time.Millisecond))
	record.MarkAt(PublicationTicketFailed, origin.Add(21*time.Millisecond))
	record.MarkAt(PublicationRouteFlipped, origin.Add(22*time.Millisecond))
	final := record.Snapshot()
	offsets = phaseOffsets(final)
	if !final.Terminal || final.DirtyGenerationID != 42 {
		t.Fatalf("completion did not close the record: %+v", final)
	}
	if _, ok := offsets[PublicationTicketFailed]; ok {
		t.Fatal("a terminal record took a second terminal mark")
	}
	if _, ok := offsets[PublicationRouteFlipped]; ok {
		t.Fatal("a terminal record took a mark after it closed")
	}
}

// A live mark lands at its elapsed offset from a time.Now() origin, both
// carrying monotonic readings, so time.Time.Sub measures elapsed time rather
// than a wall-clock difference.
func TestPublicationPhaseOffsetsUseTheMonotonicClock(t *testing.T) {
	r := NewPublicationPhaseRecorder(0, 0)
	origin := time.Now()
	record := r.Begin("checkout-a", "mutation-1", PublicationSourceMCPEdit, origin)
	time.Sleep(2 * time.Millisecond)
	record.Mark(PublicationTicketEnqueued)
	offset := phaseOffsets(record.Snapshot())[PublicationTicketEnqueued]
	if offset < (2 * time.Millisecond).Nanoseconds() {
		t.Fatalf("a mark taken 2ms after the origin sits at %dns", offset)
	}
	if offset > time.Minute.Nanoseconds() {
		t.Fatalf("offset %dns is not a monotonic reading", offset)
	}
}

func TestPublicationPhaseCheckoutMarksReachOnlyServedTickets(t *testing.T) {
	r := NewPublicationPhaseRecorder(0, 0)
	served := r.Begin("checkout-a", "mutation-1", PublicationSourceMCPEdit, time.Time{})
	served.BindTicket(7)
	served.BindTicket(9) // first binding wins: a record follows one ticket
	later := r.Begin("checkout-a", "mutation-2", PublicationSourceMCPEdit, time.Time{})
	later.BindTicket(11)
	unbound := r.Begin("checkout-a", "fresh-1", PublicationSourceFreshRequest, time.Time{})
	other := r.Begin("checkout-b", "mutation-3", PublicationSourceMCPEdit, time.Time{})
	other.BindTicket(3)

	if n := r.MarkCheckoutThrough("checkout-a", 0, PublicationCycleStarted); n != 0 {
		t.Fatalf("a cycle that captured no ticket marked %d records", n)
	}
	if n := r.MarkCheckoutThrough("checkout-a", 8, PublicationCycleStarted); n != 1 {
		t.Fatalf("a cycle through ticket 8 marked %d records, want 1", n)
	}
	if n := r.MarkCheckoutThrough("checkout-a", 8, PublicationCycleStarted); n != 0 {
		t.Fatalf("re-marking a served phase marked %d records", n)
	}
	if _, ok := phaseOffsets(served.Snapshot())[PublicationCycleStarted]; !ok {
		t.Fatal("the served record was not marked")
	}
	for _, record := range []*PublicationPhaseRecord{later, unbound, other} {
		if _, ok := phaseOffsets(record.Snapshot())[PublicationCycleStarted]; ok {
			t.Fatalf("record %s was marked by a cycle that did not serve it", record.Snapshot().Key)
		}
	}
	if got := served.Snapshot().Ticket; got != 7 {
		t.Fatalf("the record follows ticket %d, want the first binding 7", got)
	}
	if n := r.MarkCheckoutThrough("checkout-a", 11, PublicationAdmitted); n != 2 {
		t.Fatalf("a cycle through ticket 11 marked %d records, want 2", n)
	}
}

func TestPublicationPhaseRecorderIsBounded(t *testing.T) {
	r := NewPublicationPhaseRecorder(3, 2)
	for i := 1; i <= 5; i++ {
		r.Begin("checkout-a", fmt.Sprintf("a-%d", i), PublicationSourceMCPEdit, time.Time{})
	}
	snapshots := r.Snapshot("checkout-a")
	if len(snapshots) != 3 || snapshots[0].Key != "a-3" || snapshots[2].Key != "a-5" {
		t.Fatalf("per-checkout bound kept %+v, want a-3..a-5", snapshots)
	}
	if _, ok := r.Lookup("a-1"); ok {
		t.Fatal("an evicted record is still reachable by key")
	}

	r.Begin("checkout-b", "b-1", PublicationSourceMCPEdit, time.Time{})
	time.Sleep(time.Millisecond)
	r.Begin("checkout-a", "a-6", PublicationSourceMCPEdit, time.Time{}) // a is now the most recent
	r.Begin("checkout-c", "c-1", PublicationSourceMCPEdit, time.Time{})
	if got := r.Checkouts(); len(got) != 2 || got[0] != "checkout-a" || got[1] != "checkout-c" {
		t.Fatalf("checkout bound kept %v, want the two most recently touched", got)
	}
	if _, ok := r.Lookup("b-1"); ok {
		t.Fatal("the evicted checkout's record is still reachable by key")
	}

	r.mu.Lock()
	keys := len(r.byKey)
	r.mu.Unlock()
	if keys != 4 {
		t.Fatalf("the key index holds %d records, want exactly the 4 retained", keys)
	}

	// A burst far past both bounds stays within them.
	for i := range 1000 {
		r.Begin(fmt.Sprintf("burst-%d", i%7), fmt.Sprintf("burst-key-%d", i), PublicationSourceFreshRequest, time.Time{})
	}
	r.mu.Lock()
	keys, checkouts := len(r.byKey), len(r.byCheckout)
	r.mu.Unlock()
	if checkouts > 2 || keys > 2*3 {
		t.Fatalf("a burst grew the recorder to %d checkouts / %d records", checkouts, keys)
	}
}

func TestPublicationPhaseNilRecordIsInert(t *testing.T) {
	var record *PublicationPhaseRecord
	record.Mark(PublicationTicketCompleted)
	record.BindTicket(1)
	record.SetGeneration(1)
	if snapshot := record.Snapshot(); snapshot.Key != "" || len(snapshot.Phases) != 0 {
		t.Fatalf("a nil record rendered %+v", snapshot)
	}
	var recorder *PublicationPhaseRecorder
	if recorder.Begin("c", "k", "", time.Time{}) != nil || recorder.MarkCheckoutThrough("c", 1, PublicationAdmitted) != 0 {
		t.Fatal("a nil recorder recorded something")
	}
}

// A record bound to a refresh ticket receives the coordinator's and the
// builder's phase marks from the cycle that serves the ticket, in order.
func TestPublicationPhaseCoordinatorMarksReachABoundRecord(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	record := DefaultPublicationPhases().Begin(f.checkoutID, fmt.Sprintf("coordinator-marks-%d", time.Now().UnixNano()), PublicationSourceMCPEdit, time.Now())
	ticket := queueCheckoutSourceEdit(t, f, l, "package fixture\n\nfunc PhaseHelper() {}\n")
	record.BindTicket(ticket.Ticket.Generation)
	record.Mark(PublicationTicketEnqueued)
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("the ticket did not publish: %+v", result)
	}
	snapshot := record.Snapshot()
	offsets := phaseOffsets(snapshot)
	t.Logf("coordinator phases: %+v", snapshot.Phases)
	order := []PublicationPhase{
		PublicationReceived, PublicationTicketEnqueued, PublicationCycleStarted, PublicationAdmitted,
		PublicationPlanned, PublicationExtracted, PublicationPayloadFlushed, PublicationPublished, PublicationRouteFlipped,
	}
	previous := int64(-1)
	for _, phase := range order {
		offset, ok := offsets[phase]
		if !ok {
			t.Errorf("the served record lacks %s: %+v", phase, snapshot.Phases)
			continue
		}
		if offset < previous {
			t.Errorf("%s at %dns precedes the phase before it (%dns): %+v", phase, offset, previous, snapshot.Phases)
		}
		previous = offset
	}
}

// A record opened under an interim key is found under the key a reader will
// use once it exists, keeps its marks and binding, and a rekey never steals
// another record's key.
func TestPublicationPhaseRekeyFilesARecordUnderItsFinalKey(t *testing.T) {
	r := NewPublicationPhaseRecorder(4, 4)
	origin := time.Now()
	record := r.Begin("c1", "edit-pending-1", PublicationSourceMCPEdit, origin)
	record.BindTicket(3)
	record.Mark(PublicationTicketEnqueued)
	other := r.Begin("c1", "mutation-9", PublicationSourceMCPEdit, origin)
	if r.Rekey("edit-pending-1", "mutation-9") {
		t.Fatal("rekey took a key another record holds")
	}
	if got, _ := r.Lookup("mutation-9"); got != other {
		t.Fatal("a refused rekey disturbed the key's owner")
	}
	if !r.Rekey("edit-pending-1", "mutation-10") {
		t.Fatal("rekey of an open record was refused")
	}
	if _, ok := r.Lookup("edit-pending-1"); ok {
		t.Fatal("the interim key still resolves")
	}
	got, ok := r.Lookup("mutation-10")
	if !ok || got != record {
		t.Fatal("the record is not found under its final key")
	}
	snapshot := got.Snapshot()
	if snapshot.Key != "mutation-10" || snapshot.Ticket != 3 {
		t.Fatalf("rekeyed snapshot = %+v", snapshot)
	}
	if _, ok := phaseOffsets(snapshot)[PublicationTicketEnqueued]; !ok {
		t.Fatalf("rekey dropped a mark: %+v", snapshot.Phases)
	}
	if r.MarkCheckoutThrough("c1", 3, PublicationCycleStarted) != 1 {
		t.Fatal("the rekeyed record no longer takes the checkout's marks")
	}
	if r.Rekey("missing", "x") || r.Rekey("mutation-10", "") {
		t.Fatal("rekey of an unknown key or to an empty key succeeded")
	}
	if PublicationRecordFromContext(WithPublicationRecord(t.Context(), record)) != record || PublicationRecordFromContext(t.Context()) != nil {
		t.Fatal("the context accessor does not return the handed record")
	}
}
