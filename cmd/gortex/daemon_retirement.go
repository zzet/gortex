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

type deferredRetirementSweep func(context.Context) (retired int, pending bool, err error)

type deferredRetirementWorker struct {
	ready     chan struct{}
	readyOnce sync.Once
	cancel    context.CancelFunc
	stopOnce  sync.Once
	done      chan struct{}
	idlePause time.Duration
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
	if sweep == nil {
		return
	}
	if basePause <= 0 {
		basePause = time.Second
	}
	if maxBackoff < basePause {
		maxBackoff = basePause
	}
	idlePause := w.idlePause
	errorBackoff := basePause
	for {
		// The lifecycle discovers and ages debt even during ordinary request
		// traffic. Its shared build permit and Store quantum guard actual work.
		retired, pending, err := sweep(ctx)
		if retired > 0 {
			logger.Info("daemon: deferred startup retirement progress", zap.Int("retired_generations", retired), zap.Bool("pending", pending),
				zap.Int64("interactive_preemptions", indexer.DeferredRetirementPreemptions()),
				zap.Int64("parked_generations", indexer.DeferredRetirementParked()))
		}
		if err != nil && ctx.Err() == nil {
			logger.Warn("daemon: deferred startup retirement will retry", zap.Error(err), zap.Duration("retry_after", errorBackoff))
		}
		pause := basePause
		switch {
		case err != nil:
			pause = errorBackoff
			if errorBackoff < maxBackoff {
				errorBackoff *= 2
				if errorBackoff > maxBackoff {
					errorBackoff = maxBackoff
				}
			}
		case pending:
			errorBackoff = basePause
		default:
			errorBackoff = basePause
			if idlePause <= 0 {
				// Direct test/embedding workers retain the original one-shot
				// behavior. The daemon starter sets a bounded lifetime cadence.
				return
			}
			// Runtime coordinators can enqueue retirement after startup has
			// drained. Stay alive without hot-polling an empty catalog.
			pause = idlePause
		}
		if !waitDeferredRetirementPause(ctx, pause) {
			return
		}
	}
}

func waitDeferredRetirementPause(ctx context.Context, pause time.Duration) bool {
	timer := time.NewTimer(pause)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return false
	case <-timer.C:
		return true
	}
}
