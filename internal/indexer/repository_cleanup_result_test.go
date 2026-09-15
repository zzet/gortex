package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestRepositoryCleanupPendingPreservesDurableWriteFailure(t *testing.T) {
	writeFailure := errors.New("journal write failure")
	for _, test := range []struct {
		name      string
		err       error
		pending   bool
		wantError error
	}{
		{"direct", ErrRepositoryCleanupPending, true, nil},
		{"wrapped", fmt.Errorf("graph: %w", ErrRepositoryCleanupPending), true, nil},
		{"nested pending", errors.Join(ErrRepositoryCleanupPending, errors.Join(fmt.Errorf("nested: %w", ErrRepositoryCleanupPending))), true, nil},
		{"joined write failure", errors.Join(ErrRepositoryCleanupPending, writeFailure), true, writeFailure},
		{"ordinary write failure", writeFailure, false, writeFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			out, err := repositoryCleanupUntrackResult(UntrackResult{Prefix: "repo"}, test.err)
			if out.Prefix != "repo" || out.Pending != test.pending {
				t.Fatalf("result=%+v", out)
			}
			if test.wantError == nil && err != nil {
				t.Fatalf("pure deferral became error: %v", err)
			}
			if test.wantError != nil && !errors.Is(err, test.wantError) {
				t.Fatalf("durable failure suppressed: %v", err)
			}
		})
	}
}

func TestRepositoryCleanupRetryWaitsForOuterAttemptAndOwnedDrain(t *testing.T) {
	state := &repositoryCleanupState{graphID: "graph", initialized: true, attemptDone: make(chan struct{}), waitReady: make(chan struct{})}
	runtime := &repositoryCleanupRuntime{states: map[string]*repositoryCleanupState{"graph": state}, wake: make(chan struct{}, 1)}
	lifecycle := &CheckoutLifecycle{repositoryCleanup: runtime}
	hooks := cleanupHooks{l: lifecycle}
	finish, err := hooks.BeginGraphCleanupAttempt(context.Background(), "graph")
	if err != nil || finish == nil {
		t.Fatalf("capture completion: finish_nil=%v err=%v", finish == nil, err)
	}
	close(state.waitReady)
	if runtime.actionable() {
		t.Fatal("drain completion raced outer journal persistence")
	}
	finish()
	if !runtime.actionable() {
		t.Fatal("completed outer attempt did not become retryable")
	}
	select {
	case <-runtime.wake:
	default:
		t.Fatal("missing targeted first wake")
	}
	for n := 0; n < 100; n++ {
		finish()
	}
	select {
	case <-runtime.wake:
		t.Fatal("retry completion created a self-wake loop")
	default:
	}
}

func TestRepositoryCleanupPartialInitializationRemainsRetryable(t *testing.T) {
	state := &repositoryCleanupState{graphID: "graph", attemptDone: make(chan struct{}), waitReady: make(chan struct{})}
	runtime := &repositoryCleanupRuntime{states: map[string]*repositoryCleanupState{"graph": state}}
	if runtime.actionable() {
		t.Fatal("partial close retried before original journal write")
	}
	close(state.attemptDone)
	if !runtime.actionable() {
		t.Fatal("partial close failure has no retry owner")
	}
}
