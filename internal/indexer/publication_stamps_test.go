package indexer

import (
	"context"
	"testing"
	"time"
)

// Phases a request reaches before its record exists are collected on the
// context and folded into the record, at their own instants, when it opens.
func TestPublicationStampsFoldIntoTheRecordAtTheirOwnInstants(t *testing.T) {
	recorder := NewPublicationPhaseRecorder(0, 0)
	origin := time.Now()
	ctx := WithPublicationStamps(context.Background())
	StampPublicationPhase(ctx, PublicationViewResolved)
	time.Sleep(2 * time.Millisecond)
	StampPublicationPhase(ctx, PublicationParseGated)
	StampPublicationPhase(ctx, PublicationViewResolved) // first stamp wins
	time.Sleep(2 * time.Millisecond)

	record := recorder.Begin("checkout-stamps", "edit-1", PublicationSourceMCPEdit, origin)
	record.Mark(PublicationReceiptCommitted)
	record.Absorb(PublicationStampsFrom(ctx))

	offsets := map[PublicationPhase]int64{}
	for _, phase := range record.Snapshot().Phases {
		offsets[phase.Phase] = phase.OffsetNS
	}
	for _, want := range []PublicationPhase{PublicationViewResolved, PublicationParseGated, PublicationReceiptCommitted} {
		if _, ok := offsets[want]; !ok {
			t.Fatalf("record lacks %s: %+v", want, record.Snapshot().Phases)
		}
	}
	if offsets[PublicationViewResolved] >= offsets[PublicationParseGated] || offsets[PublicationParseGated] >= offsets[PublicationReceiptCommitted] {
		t.Fatalf("stamps were not kept at their own instants: %+v", record.Snapshot().Phases)
	}
	if offsets[PublicationParseGated]-offsets[PublicationViewResolved] < int64(time.Millisecond) {
		t.Fatalf("the repeated stamp replaced the first: %+v", record.Snapshot().Phases)
	}
}

// Once the record is bound to the context, a stamp lands on it directly; a
// context carrying neither records nothing.
func TestPublicationStampLandsOnABoundRecord(t *testing.T) {
	recorder := NewPublicationPhaseRecorder(0, 0)
	record := recorder.Begin("checkout-stamps", "edit-2", PublicationSourceMCPEdit, time.Now())
	ctx := WithPublicationRecord(WithPublicationStamps(context.Background()), record)
	StampPublicationPhase(ctx, PublicationTicketCaptured)
	found := false
	for _, phase := range record.Snapshot().Phases {
		found = found || phase.Phase == PublicationTicketCaptured
	}
	if !found {
		t.Fatalf("the stamp did not land on the bound record: %+v", record.Snapshot().Phases)
	}
	if n := len(PublicationStampsFrom(ctx).marks); n != 0 {
		t.Fatalf("a bound record's stamp was also collected: %d", n)
	}
	StampPublicationPhase(context.Background(), PublicationTicketCaptured) // no collector, no record: no panic
}
