package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A coordinator is tied to one durable graph identity. Once that identity is
// deleted, no amount of polling can make the same coordinator valid again;
// lifecycle reconciliation must build a replacement. The old behavior kept
// polling forever and emitted one warning every interval.
func TestCoordinatorStopsAfterDesignatedGraphIsDeleted(t *testing.T) {
	f := newCoordinatorFixture(t)
	core, logs := observer.New(zap.WarnLevel)
	cycles := make(chan CheckoutCycle, 4)
	c := f.coordinator(t, CheckoutCoordinatorConfig{
		Debounce:     time.Millisecond,
		PollInterval: time.Millisecond,
		Logger:       zap.New(core),
		cycleDone: func(cycle CheckoutCycle) {
			cycles <- cycle
		},
	})

	if err := f.catalog.DeleteDedicatedGraph(context.Background(), f.graphID); err != nil {
		t.Fatalf("delete designated graph: %v", err)
	}
	c.Signal("graph deleted")

	select {
	case cycle := <-cycles:
		if cycle.Err == nil {
			t.Fatal("cycle succeeded after its designated graph was deleted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("coordinator did not reconcile the graph deletion")
	}
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("coordinator kept polling a structurally deleted graph")
	}

	events := logs.FilterMessage("checkout coordinator: reconcile failed").All()
	if len(events) != 1 {
		t.Fatalf("structural graph deletion logged %d reconcile warnings, want one", len(events))
	}
}

// A graph row can exist before publication or while another guarded writer is
// replacing its active generation. That is not structural loss: an already
// running coordinator keeps its route and retries when the same graph becomes
// servable again.
func TestCoordinatorSurvivesTemporarilyUnpublishedDesignatedGraph(t *testing.T) {
	f := newCoordinatorFixture(t)
	cycles := make(chan CheckoutCycle, 4)
	c := f.coordinator(t, CheckoutCoordinatorConfig{
		Debounce:     time.Millisecond,
		PollInterval: -1,
		cycleDone: func(cycle CheckoutCycle) {
			cycles <- cycle
		},
	})

	graph, found, err := f.catalog.GetDedicatedGraph(context.Background(), f.graphID)
	if err != nil || !found {
		t.Fatalf("read designated graph: found=%v err=%v", found, err)
	}
	activeGeneration := graph.ActiveGenerationID
	graph.ActiveGenerationID = 0
	if err := f.catalog.UpsertDedicatedGraph(context.Background(), graph); err != nil {
		t.Fatalf("make graph temporarily unpublished: %v", err)
	}
	c.Signal("graph temporarily unpublished")

	select {
	case cycle := <-cycles:
		var unavailable *primaryBaseUnavailableError
		if cycle.Err == nil || !errors.As(cycle.Err, &unavailable) ||
			!unavailable.Temporary() || unavailable.Terminal() {
			t.Fatalf("temporary cycle = %+v, classification=%+v", cycle, unavailable)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("coordinator did not observe the temporary graph state")
	}
	if !c.Running() {
		t.Fatal("coordinator stopped for a graph that still exists")
	}

	graph.ActiveGenerationID = activeGeneration
	if err := f.catalog.UpsertDedicatedGraph(context.Background(), graph); err != nil {
		t.Fatalf("restore active generation: %v", err)
	}
	c.Signal("graph published")
	select {
	case cycle := <-cycles:
		if cycle.Err != nil {
			t.Fatalf("coordinator did not recover after publication: %v", cycle.Err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("coordinator did not retry after publication")
	}
	if !c.Running() {
		t.Fatal("coordinator stopped after the graph recovered")
	}
}
