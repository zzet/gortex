package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Converging the backfill before the writer is taken.
//
// The reclaim's writer step has walReclaimMaxWriterHold for its final
// backfill and the TRUNCATE. On a big log under a continuous write stream the
// frames written while the writer-free PASSIVE ran are more than that slice
// can copy, so the capped backfill ran out of time on every attempt and the
// log grew without bound (the 2026-09-25 crash window: six attempts, all
// `backfill_failed … deadline exceeded`, 549 MB to 2.05 GB). So before the
// writer is taken the reclaim runs writer-free PASSIVE passes back to back,
// measuring the copy rate of each, until the remainder (frames not yet
// backfilled) would copy inside walReclaimConvergeSlice at that rate. Each
// pass only has to copy what the stream wrote during the previous one, so the
// remainder shrinks geometrically as long as the copy outruns the writes. The
// passes are bounded; a pass that copies nothing (a reader's mark holds the
// backfill) ends them, since another would not do better.

// Vars, not consts, only so the in-package cases can change them; production
// never assigns them.
var (
	// walReclaimConvergeMaxPasses bounds the writer-free passes of one
	// attempt.
	walReclaimConvergeMaxPasses = 12
	// walReclaimConvergeSlice is the part of the writer hold the final
	// backfill is planned for. A quarter of the cap: between the last pass and
	// the writer the stream keeps writing (the write-gate wait alone can be a
	// second), the copy rate under the writer is not the pass's, and the
	// reset needs the rest.
	walReclaimConvergeSlice = walReclaimMaxWriterHold / 4
	// walReclaimConvergeSmallFrames: a remainder this small goes to the
	// writer step without a measured rate.
	walReclaimConvergeSmallFrames uint32 = 1024
	// walReclaimSkipConverge disables the passes for the mutation check that
	// proves they carry the continuous-stream case. Never set in production.
	walReclaimSkipConverge = false
)

// walReclaimConvergence is what the passes did, for the attempt's log line.
type walReclaimConvergence struct {
	passes          int
	copiedFrames    int64
	remainderFrames int64
	frameBytes      int64
	rateFramesPerS  float64
	elapsed         time.Duration
	stop            string // fits | small | no_progress | max_passes | cancelled | error
}

func (c walReclaimConvergence) String() string {
	return fmt.Sprintf("converge_passes=%d converge_stop=%s copied_bytes=%d remainder_bytes=%d copy_rate_mb_s=%.1f converge_elapsed=%s",
		c.passes, c.stop, c.copiedFrames*c.frameBytes, c.remainderFrames*c.frameBytes,
		c.rateFramesPerS*float64(c.frameBytes)/(1<<20), c.elapsed.Round(time.Millisecond))
}

// convergeBackfill runs the writer-free passes. It holds nothing but the
// checkpoint connection; ctx is the attempt's (shutdown, a bulk window or an
// edit cycle cancel it).
func (s *Store) convergeBackfill(ctx context.Context, ckptDB *sql.DB) walReclaimConvergence {
	return s.convergeBackfillPaced(ctx, ckptDB, nil)
}

// convergeBackfillPaced is convergeBackfill with the attempt's paced passes.
func (s *Store) convergeBackfillPaced(ctx context.Context, ckptDB *sql.DB, attempt *backgroundCheckpointAttempt) walReclaimConvergence {
	started := time.Now()
	mark := readWALWriteMark(s.dbPath)
	conv := walReclaimConvergence{frameBytes: int64(mark.PageSize) + 24}
	if mark.PageSize == 0 {
		conv.frameBytes = 4096 + 24
	}
	for {
		conv.elapsed = time.Since(started)
		snap, ok := readWALIndexSnapshot(s.dbPath)
		if !ok {
			conv.stop = "error"
			return conv
		}
		remainder := uint32(0)
		if snap.MxFrame > snap.NBackfill {
			remainder = snap.MxFrame - snap.NBackfill
		}
		conv.remainderFrames = int64(remainder)
		switch {
		case remainder <= convergeSmallFor(attempt):
			conv.stop = "small"
			return conv
		case conv.rateFramesPerS > 0 && float64(remainder)/conv.rateFramesPerS <= convergeSliceFor(attempt).Seconds():
			conv.stop = "fits"
			return conv
		case walReclaimSkipConverge || conv.passes >= walReclaimConvergeMaxPasses:
			conv.stop = "max_passes"
			return conv
		case ctx.Err() != nil:
			conv.stop = "cancelled"
			return conv
		}
		passStart := time.Now()
		var waitedBefore time.Duration
		if attempt != nil && attempt.copy != nil {
			waitedBefore = attempt.copy.pauseNs + attempt.copy.budgetNs
		}
		result, err := s.pacedPassive(ctx, ckptDB, attempt)
		took := time.Since(passStart)
		if attempt != nil && attempt.copy != nil {
			// Time paused for an edit or waiting for budget is not copy time.
			took -= attempt.copy.pauseNs + attempt.copy.budgetNs - waitedBefore
		}
		conv.passes++
		if err != nil && !errors.Is(err, errSQLiteCheckpointIncomplete) {
			if ctx.Err() != nil || errors.Is(err, errWALCheckpointYieldedToCycle) {
				conv.stop = "cancelled"
			} else {
				conv.stop = "error"
			}
			return conv
		}
		copied := int64(result.CheckpointedFrames) - int64(snap.NBackfill)
		if int64(result.WALFrames) < int64(snap.MxFrame) {
			// The log was reset under us (another connection's TRUNCATE):
			// whatever is left is new.
			copied = int64(result.CheckpointedFrames)
		}
		if copied <= 0 {
			conv.stop = "no_progress"
			return conv
		}
		conv.copiedFrames += copied
		if took > 0 {
			conv.rateFramesPerS = float64(copied) / took.Seconds()
		}
	}
}

// convergenceSuffix renders the passes for the attempt's log line ("" when
// they did not run).
func (r walReclaimResult) convergenceSuffix() string {
	if r.convergence == nil {
		return ""
	}
	return " " + r.convergence.String()
}

// walReclaimLeaseOverrideSpacing is the least time between two reclaim
// attempts run inside a bulk window. A var only so the in-package cases can
// shorten it.
var walReclaimLeaseOverrideSpacing = 30 * time.Second

// leaseOverrideDue reports whether a bulk-window override may run now.
func (s *Store) leaseOverrideDue(now time.Time) bool {
	last := s.walReclaim.cycle.lastLeaseOverride.Load()
	return last == 0 || now.Sub(time.Unix(0, last)) >= walReclaimLeaseOverrideSpacing
}

// convergeSliceFor is the writer-step time the remainder is planned for: a
// pressure attempt's reset holds the writer at most walReclaimPressureHold, of
// which the copy gets half.
func convergeSliceFor(attempt *backgroundCheckpointAttempt) time.Duration {
	if attempt != nil && attempt.copy != nil && attempt.copy.pressure {
		return walReclaimPressureHold / 2
	}
	return walReclaimConvergeSlice
}

// convergeSmallFor is the remainder handed to the writer step without a
// measured rate: a pressure attempt's short hold takes less.
func convergeSmallFor(attempt *backgroundCheckpointAttempt) uint32 {
	if attempt != nil && attempt.copy != nil && attempt.copy.pressure {
		return walReclaimPressureSmallFrames
	}
	return walReclaimConvergeSmallFrames
}
