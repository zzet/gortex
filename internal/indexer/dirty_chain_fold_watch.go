package indexer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Refusals, retries and starvation of a stepped fold.
//
// A step the store gives back to an edit (ErrChainFoldYielded) or refuses over
// the log's mark (ErrChainFoldWALMark, which also asks the store's reclaim to
// run through a busy lane) is asked for again. Once a fold was refused
// on the log's size for 2.5 minutes with nothing in the log to say so. The
// watch records every refusal by reason, logs the first one and a summary
// every dirtyChainFoldWaitLog while the fold waits, and gives the fold up when
// no step has committed for dirtyChainFoldStarvation: the members it holds go
// back to the sweep, the store's one fold is free for the inline fold at the
// cap, and the next compaction starts over on the chain then routed.

const (
	// dirtyChainFoldStarvation is how long a fold may go without a committed
	// step before it gives up.
	dirtyChainFoldStarvation = 2 * time.Minute
	// dirtyChainFoldWaitLog is how often a waiting fold logs its refusals.
	dirtyChainFoldWaitLog = 10 * time.Second
)

// errChainFoldStarved is a fold given up for want of progress.
var errChainFoldStarved = errors.New("indexer: chain fold starved")

// foldStepWatch observes one stepped fold's steps.
type foldStepWatch struct {
	logger    *zap.Logger
	checkout  string
	to        int64
	starve    time.Duration
	now       func() time.Time
	walBytes  func() int64
	started   time.Time
	progress  time.Time
	logged    time.Time
	yielded   int
	walMark   int
	waiting   bool
	lastCause string
}

// newFoldStepWatch starts a watch over the fold into to.
func (c *CheckoutCoordinator) newFoldStepWatch(to int64) *foldStepWatch {
	starve := dirtyChainFoldStarvation
	c.compaction.mu.Lock()
	if c.compaction.starvation > 0 {
		starve = c.compaction.starvation
	}
	c.compaction.mu.Unlock()
	now := time.Now()
	w := &foldStepWatch{logger: c.logger, checkout: c.checkoutID, to: to, starve: starve, now: time.Now, started: now, progress: now}
	if c.store != nil {
		store := c.store
		w.walBytes = func() int64 {
			mark := store.WALWriteMark()
			if !mark.Valid {
				return 0
			}
			return int64(mark.MxFrame) * (int64(mark.PageSize) + 24)
		}
	}
	return w
}

// stepped records a committed step.
func (w *foldStepWatch) stepped() {
	if w == nil {
		return
	}
	now := w.now()
	if w.waiting && w.logger != nil {
		w.logger.Info("checkout coordinator: chain fold stepping again",
			zap.String("checkout", w.checkout), zap.Int64("folded_generation", w.to),
			zap.Duration("waited", now.Sub(w.progress)), zap.Int("yielded", w.yielded), zap.Int("wal_mark", w.walMark))
	}
	w.progress, w.waiting = now, false
}

// refused records a step given back or refused, and reports errChainFoldStarved
// once no step has committed for the starvation limit.
func (w *foldStepWatch) refused(err error) error {
	if w == nil {
		return nil
	}
	now := w.now()
	cause := "other"
	switch {
	case errors.Is(err, store_sqlite.ErrChainFoldYielded):
		w.yielded++
		cause = "yielded"
	case errors.Is(err, store_sqlite.ErrChainFoldWALMark):
		w.walMark++
		cause = "wal_mark"
	}
	var wal int64
	if w.walBytes != nil {
		wal = w.walBytes()
	}
	fields := func() []zap.Field {
		return []zap.Field{
			zap.String("checkout", w.checkout), zap.Int64("folded_generation", w.to), zap.String("cause", cause),
			zap.Int("yielded", w.yielded), zap.Int("wal_mark", w.walMark), zap.Int64("wal_bytes", wal),
			zap.Duration("waiting", now.Sub(w.progress)), zap.Duration("elapsed", now.Sub(w.started)),
		}
	}
	if w.logger != nil && (!w.waiting || cause != w.lastCause || now.Sub(w.logged) >= dirtyChainFoldWaitLog) {
		w.logger.Info("checkout coordinator: chain fold step refused; retrying", fields()...)
		w.logged = now
	}
	w.waiting, w.lastCause = true, cause
	if now.Sub(w.progress) >= w.starve {
		if w.logger != nil {
			w.logger.Warn("checkout coordinator: chain fold starved; giving it up", fields()...)
		}
		return fmt.Errorf("%w: no step committed for %s (%d given back, %d over the log's mark)",
			errChainFoldStarved, now.Sub(w.progress).Round(time.Second), w.yielded, w.walMark)
	}
	return nil
}

// Import slices yield immediately to arriving writers. Retry soon enough to
// use the gaps between their writes rather than adding a full 50 ms pause to
// each interrupted 10 ms slice. WAL pressure and other folds retain the
// longer backoff; store admission and transaction interruption are unchanged.
func foldStepRetryDelay(ctx context.Context, err error) time.Duration {
	if fold, _ := ctx.Value(importFoldPublicationKey{}).(*importFoldPublication); fold != nil && errors.Is(err, store_sqlite.ErrChainFoldYielded) {
		return dirtyChainCompactionYieldPoll
	}
	return foldStepRetryPoll
}

// runChainFoldStepsWatched is runChainFoldSteps with the watch told of every
// step and every refusal; the watch may give the fold up.
func runChainFoldStepsWatched(ctx context.Context, fold chainFoldSteps, retryable func(error) bool, afterStep func(step int), watch *foldStepWatch) (steps, retries int, err error) {
	for {
		done, err := fold.Step(ctx)
		if err != nil {
			if ctx.Err() == nil && retryable != nil && retryable(err) {
				retries++
				if starved := watch.refused(err); starved != nil {
					return steps, retries, starved
				}
				timer := time.NewTimer(foldStepRetryDelay(ctx, err))
				select {
				case <-ctx.Done():
					timer.Stop()
					return steps, retries, ctx.Err()
				case <-timer.C:
				}
				continue
			}
			return steps, retries, err
		}
		steps++
		watch.stepped()
		if afterStep != nil {
			afterStep(steps)
		}
		if done {
			return steps, retries, nil
		}
	}
}

// Begin can yield too: its empty-target transaction takes the same writer
// gate as a step. A rolled-back begin leaves no reservation or payload behind,
// so retry that same target under the driver's existing cancellation and
// starvation bounds rather than marking a live fold failed on the first edit.
func beginChainFoldWatched(ctx context.Context, backend chainFoldBackend, chain []int64, to int64, owner string, watch *foldStepWatch) (chainFoldSteps, int, error) {
	for retries := 0; ; retries++ {
		if err := ctx.Err(); err != nil {
			return nil, retries, err
		}
		fold, err := backend.BeginChainFold(ctx, chain, to, owner)
		if err == nil {
			return fold, retries, nil
		}
		if ctx.Err() != nil {
			return nil, retries, ctx.Err()
		}
		if !backend.StepRetryable(err) {
			return nil, retries, err
		}
		if err := watch.refused(err); err != nil {
			return nil, retries, err
		}
		timer := time.NewTimer(foldStepRetryDelay(ctx, err))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, retries + 1, ctx.Err()
		case <-timer.C:
		}
	}
}
