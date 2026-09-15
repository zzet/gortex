package indexer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRepositoryCleanupLaneCachesDrainAcrossCanceledRetries(t *testing.T) {
	coordinator := newRepositoryMutationCoordinator(nil)
	entered, release := make(chan struct{}), make(chan struct{})
	mutationDone := make(chan error, 1)
	actorExited := make(chan struct{})
	mutationCtx, cancelMutation := context.WithCancel(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	// Registered before starting the actor or making any assertion that can
	// fail while its intentionally paused mutation still holds the lane.
	t.Cleanup(func() {
		cancel()
		cancelMutation()
		unblock()
		select {
		case <-actorExited:
		case <-time.After(5 * time.Second):
			t.Error("captured mutation did not join during cleanup")
		}
	})
	go func() {
		defer close(actorExited)
		mutationDone <- coordinator.runExclusive(mutationCtx, func() error { close(entered); <-release; return nil })
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("mutation never entered the captured lane")
	}
	drain := coordinator.closeAndDrain()
	cancel()
	for n := 0; n < 100; n++ {
		if err := coordinator.closeAndWait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("retry %d: %v", n, err)
		}
		if coordinator.closeAndDrain() != drain {
			t.Fatal("retry created another waiter channel")
		}
	}
	select {
	case <-drain:
		t.Fatal("drain passed captured tail")
	default:
	}
	if err := coordinator.runExclusive(context.Background(), func() error { t.Error("late mutation ran"); return nil }); !errors.Is(err, errRepositoryMutationCoordinatorClosed) {
		t.Fatalf("late mutation admitted: %v", err)
	}
	unblock()
	select {
	case <-actorExited:
	case <-time.After(5 * time.Second):
		t.Fatal("captured mutation did not exit after release")
	}
	if err := <-mutationDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-drain:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not complete after the captured mutation")
	}
	finalCtx, cancelFinal := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFinal()
	if err := coordinator.closeAndWait(finalCtx); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryCleanupLaneOldFinalizerCannotDetachReplacement(t *testing.T) {
	coordinator := newRepositoryMutationCoordinator(nil)
	mi := &MultiIndexer{}
	lane := &repositoryCleanupLane{owner: mi, prefix: "repo", coordinator: coordinator, finalized: true}
	if err := mi.finalizeRepositoryCleanupLane(lane); err != nil {
		t.Fatal(err)
	}
	other := &MultiIndexer{}
	if err := other.finalizeRepositoryCleanupLane(lane); err == nil {
		t.Fatal("foreign finalized handle accepted")
	}
}
