package main

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/indexer"
)

const (
	deferredRetirementRetryBase = time.Second
	deferredRetirementRetryMax  = time.Minute
	deferredRetirementIdlePause = 30 * time.Second
)

type deferredRetirementSweep = indexer.DeferredRetirementSweep

type deferredRetirementWorker struct {
	ready     chan struct{}
	readyOnce sync.Once
	cancel    context.CancelFunc
	stopOnce  sync.Once
	done      chan struct{}
	idlePause time.Duration
	// progress and wake are the lifecycle's committed-work count and its
	// new-work signal (indexer.DeferredRetirementLoop); nil keeps the plain
	// pause cadence.
	progress func() int64
	wake     func() <-chan struct{}
	// afterReady runs once, on the worker's goroutine, when the daemon is
	// ready and before the first sweep: the one-time correction of
	// generations an older derivation wrote (SetAfterReady).
	afterReady func(context.Context)
}

// SetAfterReady installs work the worker runs once after MarkReady, before
// its first sweep. It must be called before MarkReady.
func (w *deferredRetirementWorker) SetAfterReady(fn func(context.Context)) {
	if w == nil {
		return
	}
	w.afterReady = fn
}

func startDeferredRetirementWorker(sweep deferredRetirementSweep, logger *zap.Logger) *deferredRetirementWorker {
	if logger == nil {
		logger = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := &deferredRetirementWorker{
		ready:     make(chan struct{}),
		cancel:    cancel,
		done:      make(chan struct{}),
		idlePause: deferredRetirementIdlePause,
		progress:  indexer.DeferredRetirementProgress,
		wake:      indexer.DeferredRetirementWake,
	}
	go worker.run(ctx, sweep, logger, deferredRetirementRetryBase, deferredRetirementRetryMax)
	return worker
}

func (w *deferredRetirementWorker) MarkReady() {
	if w == nil {
		return
	}
	w.readyOnce.Do(func() { close(w.ready) })
}

func (w *deferredRetirementWorker) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { w.cancel(); <-w.done })
}

func (w *deferredRetirementWorker) run(ctx context.Context, sweep deferredRetirementSweep, logger *zap.Logger, basePause, maxBackoff time.Duration) {
	defer close(w.done)
	select {
	case <-ctx.Done():
		return
	case <-w.ready:
	}
	if w.afterReady != nil {
		w.afterReady(ctx)
	}
	indexer.DeferredRetirementLoop{
		Sweep:      sweep,
		Logger:     logger,
		BasePause:  basePause,
		MaxBackoff: maxBackoff,
		IdlePause:  w.idlePause,
		Progress:   w.progress,
		Wake:       w.wake,
	}.Run(ctx)
}
