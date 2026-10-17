package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/runtimeactivity"
)

// Request activity must not prevent the lifecycle from discovering and aging
// debt. Actual edit/build exclusion is enforced by its shared permit, not this
// outer worker's broad process activity counter.
func TestDeferredRetirementWorkerDiscoversDebtDuringTrackedRequests(t *testing.T) {
	runtimeactivity.Begin("mcp_request")
	defer runtimeactivity.End("mcp_request")
	called := make(chan struct{})
	worker := startDeferredRetirementWorker(func(ctx context.Context) (int, bool, error) {
		if runtimeactivity.Current().Active == 0 {
			return 0, false, errors.New("tracked request ended before debt discovery")
		}
		close(called)
		<-ctx.Done()
		return 0, true, ctx.Err()
	}, zap.NewNop())
	t.Cleanup(worker.Stop)
	worker.MarkReady()
	awaitRetirementSignal(t, called)
	worker.Stop()
	select {
	case <-worker.done:
	default:
		t.Fatal("worker did not join while traffic remained active")
	}
}

func TestDeferredRetirementWorkerServicesWorkAfterEmptyPass(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstEmpty := make(chan struct{})
	workArrived := make(chan struct{})
	serviced := make(chan struct{})
	var calls atomic.Int64
	worker := &deferredRetirementWorker{
		ready:     ready,
		done:      done,
		idlePause: time.Nanosecond,
	}
	go worker.run(
		ctx,
		func(sweepCtx context.Context) (int, bool, error) {
			if calls.Add(1) == 1 {
				close(firstEmpty)
				return 0, false, nil
			}
			<-workArrived
			close(serviced)
			<-sweepCtx.Done()
			return 1, false, sweepCtx.Err()
		},
		zap.NewNop(),
		time.Second,
		time.Minute,
	)
	awaitRetirementSignal(t, firstEmpty)
	close(workArrived)
	awaitRetirementSignal(t, serviced)
	cancel()
	awaitRetirementSignal(t, done)
	if got := calls.Load(); got < 2 {
		t.Fatalf("sweep calls = %d, want at least 2", got)
	}
}

func TestDeferredRetirementWorkerServicesWithoutAnActivityGuard(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	var sweeps atomic.Int64
	worker := &deferredRetirementWorker{
		ready:     ready,
		done:      done,
		idlePause: time.Hour,
	}
	swept := make(chan struct{})
	go worker.run(
		ctx,
		func(context.Context) (int, bool, error) {
			sweeps.Add(1)
			close(swept)
			return 0, false, nil
		},
		zap.NewNop(),
		time.Second,
		time.Minute,
	)
	awaitRetirementSignal(t, swept)
	cancel()
	awaitRetirementSignal(t, done)
	if got := sweeps.Load(); got != 1 {
		t.Fatalf("sweep calls without an activity guard = %d, want 1", got)
	}
}

func awaitRetirementSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for deferred-retirement test signal")
	}
}
