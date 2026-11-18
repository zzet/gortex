package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func startDeferredRetirementWorkerForTest(t *testing.T, sweep func(context.Context) (int, bool, error), basePause, maxBackoff time.Duration) *deferredRetirementWorker {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	worker := &deferredRetirementWorker{
		ready:  make(chan struct{}),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go worker.run(ctx, sweep, zap.NewNop(), basePause, maxBackoff)
	t.Cleanup(worker.Stop)
	return worker
}

func TestDeferredRetirementWorkerWaitsForReadyAndStops(t *testing.T) {
	var calls atomic.Int32
	worker := startDeferredRetirementWorkerForTest(t, func(context.Context) (int, bool, error) {
		calls.Add(1)
		return 0, false, nil
	}, time.Millisecond, 10*time.Millisecond)
	time.Sleep(25 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("sweep calls before readiness = %d, want 0", got)
	}
	worker.MarkReady()
	select {
	case <-worker.done:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit after the first successful empty sweep")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("sweep calls = %d, want 1", got)
	}
	worker.Stop()
	worker.Stop()
}

func TestDeferredRetirementWorkerUsesBasePauseForProductivePending(t *testing.T) {
	const basePause = 20 * time.Millisecond
	callTimes := make(chan time.Time, 3)
	var calls atomic.Int32
	worker := startDeferredRetirementWorkerForTest(t, func(context.Context) (int, bool, error) {
		callTimes <- time.Now()
		if calls.Add(1) <= 2 {
			return 1, true, nil
		}
		return 0, false, nil
	}, basePause, 500*time.Millisecond)
	worker.MarkReady()
	var times [3]time.Time
	for i := range times {
		select {
		case times[i] = <-callTimes:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for sweep call %d", i+1)
		}
	}
	for i := 1; i < len(times); i++ {
		if elapsed := times[i].Sub(times[i-1]); elapsed < basePause {
			t.Fatalf("productive pending retry %d elapsed = %s, want at least %s", i, elapsed, basePause)
		}
	}
	select {
	case <-worker.done:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit after draining pending work")
	}
}

func TestDeferredRetirementWorkerBacksOffErrorsAndJoinsCancellation(t *testing.T) {
	const basePause = 15 * time.Millisecond
	callTimes := make(chan time.Time, 3)
	enteredBlockingSweep := make(chan struct{})
	var calls atomic.Int32
	worker := startDeferredRetirementWorkerForTest(t, func(ctx context.Context) (int, bool, error) {
		callTimes <- time.Now()
		switch calls.Add(1) {
		case 1, 2:
			return 0, true, errors.New("blocked")
		default:
			close(enteredBlockingSweep)
			<-ctx.Done()
			return 0, true, ctx.Err()
		}
	}, basePause, 200*time.Millisecond)
	worker.MarkReady()
	first, second, third := <-callTimes, <-callTimes, <-callTimes
	if elapsed := second.Sub(first); elapsed < basePause {
		t.Fatalf("first error retry elapsed = %s, want at least %s", elapsed, basePause)
	}
	if elapsed := third.Sub(second); elapsed < 2*basePause {
		t.Fatalf("second error retry elapsed = %s, want at least %s", elapsed, 2*basePause)
	}
	<-enteredBlockingSweep
	stopped := make(chan struct{})
	go func() { worker.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel and join a sweep blocked on context")
	}
}

func TestDaemonTeardownJoinsDeferredRetirementBeforeStoreClose(t *testing.T) {
	enteredSweep := make(chan struct{})
	worker := startDeferredRetirementWorkerForTest(t, func(ctx context.Context) (int, bool, error) {
		close(enteredSweep)
		<-ctx.Done()
		return 0, true, ctx.Err()
	}, time.Millisecond, 10*time.Millisecond)
	worker.MarkReady()
	<-enteredSweep
	controller := &realController{}
	closed := make(chan struct{})
	runTeardown := installDaemonTeardown(controller, worker.Stop, func() error {
		select {
		case <-worker.done:
		default:
			t.Fatal("store close ran before deferred retirement worker joined")
		}
		close(closed)
		return nil
	})
	if err := controller.Shutdown(context.Background()); err != nil {
		t.Fatalf("controller shutdown: %v", err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("controller shutdown did not run store close")
	}
	runTeardown()
}
