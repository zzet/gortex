package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/runtimeactivity"
)

func TestDeferredRetirementWorkerSkipsSweepWhileTrackedActivityAndStops(t *testing.T) {
	checked := make(chan struct{})
	var checkedOnce sync.Once
	var sweeps atomic.Int64
	worker := startDeferredRetirementWorkerWithActivity(
		func(context.Context) (int, bool, error) {
			sweeps.Add(1)
			return 0, false, nil
		},
		func() bool {
			checkedOnce.Do(func() { close(checked) })
			return true
		},
		zap.NewNop(),
	)
	worker.MarkReady()
	awaitRetirementSignal(t, checked)
	worker.Stop()
	if got := sweeps.Load(); got != 0 {
		t.Fatalf("sweep calls while foreground active = %d, want 0", got)
	}
}

func TestDeferredRetirementWorkerDefersForTrackedNonMCPWorkThenResumes(t *testing.T) {
	runtimeactivity.Begin("sparse_generation_build")
	activityEnded := false
	defer func() {
		if !activityEnded {
			runtimeactivity.End("sparse_generation_build")
		}
	}()

	ready := make(chan struct{})
	close(ready)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checked := make(chan struct{})
	var checkedOnce sync.Once
	swept := make(chan struct{})
	var sweeps atomic.Int64
	worker := &deferredRetirementWorker{
		ready:     ready,
		done:      done,
		idlePause: time.Hour,
		trackedActivityActive: func() bool {
			checkedOnce.Do(func() { close(checked) })
			return trackedWorkActive()
		},
	}
	go worker.run(
		ctx,
		func(context.Context) (int, bool, error) {
			sweeps.Add(1)
			close(swept)
			return 0, false, nil
		},
		zap.NewNop(),
		time.Nanosecond,
		time.Nanosecond,
	)
	awaitRetirementSignal(t, checked)
	if got := sweeps.Load(); got != 0 {
		t.Fatalf("sweep calls while sparse generation build active = %d, want 0", got)
	}
	runtimeactivity.End("sparse_generation_build")
	activityEnded = true
	awaitRetirementSignal(t, swept)
	cancel()
	awaitRetirementSignal(t, done)
	if got := sweeps.Load(); got != 1 {
		t.Fatalf("sweep calls after tracked work ended = %d, want 1", got)
	}
}

func TestDeferredRetirementWorkerResumesAfterTrackedActivityClears(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	var activityChecks atomic.Int64
	var sweeps atomic.Int64
	worker := &deferredRetirementWorker{
		ready:     ready,
		done:      done,
		idlePause: time.Hour,
		trackedActivityActive: func() bool {
			return activityChecks.Add(1) == 1
		},
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
		time.Nanosecond,
		time.Nanosecond,
	)
	awaitRetirementSignal(t, swept)
	cancel()
	awaitRetirementSignal(t, done)
	if got := sweeps.Load(); got != 1 {
		t.Fatalf("sweep calls after foreground cleared = %d, want 1", got)
	}
	if got := activityChecks.Load(); got < 2 {
		t.Fatalf("tracked-activity checks = %d, want at least 2", got)
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

func TestDeferredRetirementWorkerNilActivityPreservesSweep(t *testing.T) {
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
		t.Fatalf("sweep calls with nil activity predicate = %d, want 1", got)
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
