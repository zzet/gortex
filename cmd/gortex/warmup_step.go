package main

import (
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// startWarmupStep logs before work begins, including on no-op paths. The returned
// function records elapsed wall time and result counts without retaining graph
// objects or starting a timer goroutine. Nested steps must not be added to their
// parent's duration when accounting for total warmup time.
func startWarmupStep(logger *zap.Logger, step string, fields ...zap.Field) func(...zap.Field) time.Duration {
	if logger == nil {
		logger = zap.NewNop()
	}
	started := time.Now()
	base := append([]zap.Field{zap.String("step", step)}, fields...)
	logger.Info("daemon: warmup step start", base...)
	return func(result ...zap.Field) time.Duration {
		elapsed := time.Since(started)
		done := make([]zap.Field, 0, len(base)+len(result)+1)
		done = append(done, base...)
		done = append(done, zap.Duration("elapsed", elapsed))
		done = append(done, result...)
		logger.Info("daemon: warmup step complete", done...)
		return elapsed
	}
}

// warmupResolveTimer splits synchronous resolution at its readiness callback.
// Atomics protect the observation even if a resolver calls back on another
// goroutine. Callback delivery itself is preserved, including repeated calls.
type warmupResolveTimer struct {
	started      time.Time
	computeNanos atomic.Int64
	logger       *zap.Logger
}

func newWarmupResolveTimer(logger *zap.Logger) *warmupResolveTimer {
	if logger == nil {
		logger = zap.NewNop()
	}
	r := &warmupResolveTimer{started: time.Now(), logger: logger}
	logger.Info("daemon: warmup step start", zap.String("step", "resolve_compute"))
	return r
}

func (r *warmupResolveTimer) ready(markReady func()) func() {
	return func() {
		compute := max(time.Since(r.started), time.Nanosecond)
		if r.computeNanos.CompareAndSwap(0, int64(compute)) {
			r.logger.Info("daemon: warmup step complete", zap.String("step", "resolve_compute"), zap.Duration("elapsed", compute), zap.Bool("queryable_callback", true))
			r.logger.Info("daemon: warmup step start", zap.String("step", "resolve_tail"))
		}
		if markReady != nil {
			markReady()
		}
	}
}

func (r *warmupResolveTimer) finish(err error) (time.Duration, time.Duration) {
	total := time.Since(r.started)
	compute := time.Duration(r.computeNanos.Load())
	if compute == 0 {
		r.logger.Info("daemon: warmup step complete", zap.String("step", "resolve_compute"), zap.Duration("elapsed", total), zap.Bool("queryable_callback", false), zap.Error(err))
		return total, 0
	}
	tail := max(total-compute, 0)
	r.logger.Info("daemon: warmup step complete", zap.String("step", "resolve_tail"), zap.Duration("elapsed", tail), zap.Error(err))
	return compute, tail
}
