package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Done is reached only when WaitPayloadBuildFlights evaluates the active
// flight's waiting select. Err is inherited and cannot signal this barrier.
type payloadCleanupQueuedContext struct {
	context.Context
	queued chan struct{}
	once   sync.Once
}

func (c *payloadCleanupQueuedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.queued) })
	return c.Context.Done()
}

func TestWaitPayloadBuildFlightsJoinsExistingOwnerWithoutRecovery(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "flights.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	id, err := store.Catalog().CreateViewGeneration(ctx, ViewGeneration{OwnerKind: "dedicated_graph", GenerationKind: "dedicated_base", State: ViewGenerationBuilding})
	if err != nil {
		t.Fatal(err)
	}
	// Missing flights remain missing; waiting must not elect a recovery leader.
	if err := store.WaitPayloadBuildFlights(ctx, id); err != nil {
		t.Fatal(err)
	}
	if store.PayloadBuildFlightActive(id) {
		t.Fatal("wait created a physical flight")
	}
	flight, leader, ready, err := store.JoinPayloadBuildFlight(ctx, id, false)
	if err != nil || !leader || ready {
		t.Fatalf("claim leader=%v ready=%v err=%v", leader, ready, err)
	}
	waitCtx, cancelWait := context.WithCancel(ctx)
	physicalFailure := errors.New("physical build failed")
	joined := make(chan error, 1)
	done := make(chan struct{})
	started := false
	// Registered before cancellation assertions or goroutine creation. LIFO
	// runs this before Store.Close; failure cleanup never consumes joined.
	t.Cleanup(func() {
		cancelWait()
		flight.Complete(physicalFailure)
		if started {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("flight waiter did not join after cancellation and owner completion")
			}
		}
	})
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.WaitPayloadBuildFlights(cancelled, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	if !store.PayloadBuildFlightActive(id) {
		t.Fatal("canceled wait completed another actor's flight")
	}
	waiting := &payloadCleanupQueuedContext{Context: waitCtx, queued: make(chan struct{})}
	started = true
	go func() {
		defer close(done)
		joined <- store.WaitPayloadBuildFlights(waiting, id)
	}()
	select {
	case <-waiting.queued:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never reached the active-flight select")
	}
	select {
	case err := <-joined:
		t.Fatalf("wait escaped active owner after queued barrier: %v", err)
	default:
	}
	flight.Complete(physicalFailure)
	// Cleanup waits for lifetime only: failure of the completed build must not
	// keep that flight alive or cause an unrelated cleanup failure.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not finish after physical owner completion")
	}
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	if err := store.WaitPayloadBuildFlights(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestWaitPayloadBuildFlightsRejectsGenerationZero(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "invalid-flight.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.WaitPayloadBuildFlights(context.Background(), 0); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Fatalf("generation zero is an owner lease, not a physical flight: %v", err)
	}
}

func BenchmarkWaitPayloadBuildFlightsAlreadyDrained(b *testing.B) {
	store, err := Open(filepath.Join(b.TempDir(), "drained.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	for _, test := range []struct {
		name string
		ids  []int64
	}{
		{"empty-control", nil}, {"one-owner", []int64{1}}, {"four-owners", []int64{1, 2, 3, 4}},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := store.WaitPayloadBuildFlights(ctx, test.ids...); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
