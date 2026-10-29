package main

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// daemonWarmup owns the one startup producer. Cancellation is deliberately
// separate from joining: shutdown must revoke work before waiting on watchers,
// coordinators or the backend. No producer or callback runs under mu.
type daemonWarmup struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	done   chan struct{}
}

func newDaemonWarmup(parent context.Context) *daemonWarmup {
	ctx, cancel := context.WithCancel(parent)
	return &daemonWarmup{ctx: ctx, cancel: cancel}
}

func (w *daemonWarmup) start(run func(context.Context)) bool {
	w.mu.Lock()
	if w.done != nil || w.ctx.Err() != nil {
		w.mu.Unlock()
		return false
	}
	done := make(chan struct{})
	w.done = done
	w.mu.Unlock()
	go func() {
		defer close(done)
		if w.ctx.Err() == nil {
			run(w.ctx)
		}
	}()
	return true
}

func (w *daemonWarmup) wait() {
	// This is an ownership boundary, not a timeout guarantee. A legacy stage
	// without a context-aware read still has to return before backend close;
	// shutdown phase logs retain that wait rather than declaring early success.
	w.mu.Lock()
	done := w.done
	w.mu.Unlock()
	if done != nil {
		<-done
	}
}

func stopDaemonWarmup(w *daemonWarmup, controller *realController, logger *zap.Logger) {
	// Cancellation precedes every wait, including the watcher handoff barrier.
	w.cancel()
	started := time.Now()
	logger.Info("daemon: shutdown phase start", zap.String("phase", "warmup_join"))
	w.wait()
	logger.Info("daemon: shutdown phase complete", zap.String("phase", "warmup_join"), zap.Duration("elapsed", time.Since(started)))
	started = time.Now()
	logger.Info("daemon: shutdown phase start", zap.String("phase", "watcher_stop"))
	controller.StopWatcher()
	logger.Info("daemon: shutdown phase complete", zap.String("phase", "watcher_stop"), zap.Duration("elapsed", time.Since(started)))
}
