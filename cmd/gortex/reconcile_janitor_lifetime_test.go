package main

import (
	"sync"
	"testing"
	"time"
)

func TestReconcileJanitorStopCancelsAndJoins(t *testing.T) {
	ctx, done, stop := newReconcileJanitorLifetime()
	exited := make(chan struct{})
	joined := make(chan struct{})
	var doneOnce sync.Once
	finishJanitor := func() { doneOnce.Do(func() { close(done) }) }
	// Always release the fake janitor and join the stop actor, even when a
	// pre-release assertion fails. Register before the actor is spawned.
	t.Cleanup(func() {
		finishJanitor()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("janitor stop actor did not join during cleanup")
		}
	})
	go func() {
		defer close(joined)
		stop()
		close(exited)
		stop() // The same joined actor also checks repeated-stop completion.
	}()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not cancel janitor context")
	}
	select {
	case <-exited:
		t.Fatal("stop returned before janitor join")
	default:
	}
	finishJanitor()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("stop or repeated stop did not finish after janitor exit")
	}
}
