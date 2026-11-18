package indexer

import (
	"context"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// The deferred retirement worker's cadence.
//
// The worker ran one bounded burst per pass and then slept a second even when
// the burst had just committed work and more was waiting: about 50 ms of
// deleting per 1.06 s, a duty of under five percent. With nothing pending it
// slept thirty seconds, and work that arrived in that sleep — a torn build's
// generation, a parent whose last child was just retired — waited it out.
//
// A pending pass that committed work now re-polls at once; one that committed
// nothing (it stood down, waited on a lease, or found only parked work) keeps
// the base pause, so a held-back sweep never spins; and the idle pause ends
// early when new work is owed or a reference is released.

// DeferredRetirementSweep is one deferred retirement pass.
type DeferredRetirementSweep func(context.Context) (retired int, pending bool, err error)

// DeferredRetirementLoop is the worker's cadence around a sweep.
type DeferredRetirementLoop struct {
	Sweep  DeferredRetirementSweep
	Logger *zap.Logger
	// BasePause follows a pending pass that committed nothing, and starts the
	// error backoff, which doubles up to MaxBackoff.
	BasePause, MaxBackoff time.Duration
	// IdlePause follows a pass with nothing pending. Zero or less returns
	// instead (a one-shot drain).
	IdlePause time.Duration
	// Progress, when set, is a monotone count of committed retirement work: a
	// pending pass that advanced it re-polls without pausing.
	Progress func() int64
	// Wake, when set, returns a channel closed when work arrives; it ends the
	// idle pause early.
	Wake func() <-chan struct{}
}

// deferredRetirementCommitted counts committed retirement work in this
// process: quanta and slices that deleted or advanced something.
var deferredRetirementCommitted atomic.Int64

// DeferredRetirementProgress is the monotone count of committed deferred
// retirement work (DeferredRetirementLoop.Progress).
func DeferredRetirementProgress() int64 { return deferredRetirementCommitted.Load() }

// deferredRetirementProgressLog is how often the loop's progress line may be
// written while a drain keeps re-polling.
const deferredRetirementProgressLog = time.Second

// Run sweeps until ctx ends (or, with no IdlePause, until nothing is pending).
func (cfg DeferredRetirementLoop) Run(ctx context.Context) {
	if cfg.Sweep == nil {
		return
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	basePause := cfg.BasePause
	if basePause <= 0 {
		basePause = time.Second
	}
	maxBackoff := max(cfg.MaxBackoff, basePause)
	errorBackoff := basePause
	var retiredSinceLog int
	var lastLog time.Time
	for {
		var wake <-chan struct{}
		if cfg.Wake != nil {
			wake = cfg.Wake()
		}
		var before int64
		if cfg.Progress != nil {
			before = cfg.Progress()
		}
		// The lifecycle discovers and ages debt even during ordinary request
		// traffic. Its shared build permit and Store quantum guard actual work.
		retired, pending, err := cfg.Sweep(ctx)
		progressed := cfg.Progress != nil && cfg.Progress() != before
		retiredSinceLog += retired
		if retiredSinceLog > 0 && (!pending || time.Since(lastLog) >= deferredRetirementProgressLog) {
			logger.Info("daemon: deferred startup retirement progress", zap.Int("retired_generations", retiredSinceLog), zap.Bool("pending", pending),
				zap.Int64("interactive_preemptions", DeferredRetirementPreemptions()),
				zap.Int64("parked_generations", DeferredRetirementParked()))
			retiredSinceLog, lastLog = 0, time.Now()
		}
		if err != nil && ctx.Err() == nil {
			logger.Warn("daemon: deferred startup retirement will retry", zap.Error(err), zap.Duration("retry_after", errorBackoff))
		}
		pause := basePause
		var interrupt <-chan struct{}
		switch {
		case err != nil:
			pause = errorBackoff
			errorBackoff = min(2*errorBackoff, maxBackoff)
		case pending && progressed:
			errorBackoff = basePause
			pause = 0
		case pending:
			errorBackoff = basePause
		default:
			errorBackoff = basePause
			if cfg.IdlePause <= 0 {
				// Direct test/embedding workers retain the original one-shot
				// behavior. The daemon starter sets a bounded lifetime cadence.
				return
			}
			// Runtime coordinators can enqueue retirement after startup has
			// drained. Stay alive without hot-polling an empty catalog.
			pause, interrupt = cfg.IdlePause, wake
		}
		if !waitDeferredRetirementPause(ctx, pause, interrupt) {
			return
		}
	}
}

// waitDeferredRetirementPause waits pause, or until wake closes; false when
// ctx ended.
func waitDeferredRetirementPause(ctx context.Context, pause time.Duration, wake <-chan struct{}) bool {
	if pause <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return ctx.Err() == nil
	case <-timer.C:
		return true
	}
}
