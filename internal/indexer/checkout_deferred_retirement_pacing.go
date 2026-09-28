package indexer

import (
	"context"
	"sort"
	"time"
)

// Pacing the deferred retirement sweep around edits.
//
// A measurement showed what the sweep's gaps cost: resuming between edits right
// after the reclaim had reset the log, it deleted dedicated generations of
// 100k and more nodes and wrote 2.1 GB of WAL in three minutes; every reclaim
// copy after that was cancelled by the next edit, the log never reset, and
// every page lookup of every later edit paid for 575k frames it had to search.
// A retirement is never urgent, an edit always is. So while a checkout is
// being edited the sweep:
//
//   - starts nothing until no foreground work — an edit cycle, a refresh
//     ticket or demand wake, an interactive build or a foreground lane holder
//     — has been seen for deferredRetirementEditIdle, and cancels a chunk in
//     flight the moment one appears;
//   - starts nothing while the log holds more than deferredRetirementWALPause
//     since its last reset (the reclaim resets it between edits; the sweep
//     does not grow it past what one reset clears);
//   - runs slices of deferredRetirementActiveSlice, not the idle slice;
//   - retires the smallest generations first, and leaves a generation larger
//     than deferredRetirementLargeBytes for when the checkout has been idle
//     for deferredRetirementLargeIdle.
//
// "While a checkout is being edited" is foreground work seen within
// deferredRetirementActiveWindow. The starvation guard (no progress for
// deferredRetirementStarvationLimit) relaxes only the idle wait (to
// deferredRetirementStarvedIdle) and the large-generation hold; an edit cycle,
// a pending ticket and the WAL pause always hold.

const (
	deferredRetirementEditIdle     = 20 * time.Second
	deferredRetirementStarvedIdle  = 2 * time.Second
	deferredRetirementActiveWindow = 10 * time.Minute
	deferredRetirementActiveSlice  = 250 * time.Millisecond
	deferredRetirementLargeBytes   = 64 << 20
	deferredRetirementLargeIdle    = 2 * time.Minute
	deferredRetirementWALPause     = 256 << 20
)

// retirementPace is one pass's view of the checkouts' activity.
type retirementPace struct {
	now     time.Time
	busy    string
	last    time.Time
	starved bool
}

// retirementPaceNow reads the foreground activity for one pass.
func (l *CheckoutLifecycle) retirementPaceNow(now time.Time, starved bool) retirementPace {
	busy, last := l.retirementForegroundWork()
	return retirementPace{now: now, busy: busy, last: last, starved: starved}
}

// retirementForegroundWork is the foreground work in flight and the latest
// instant foreground work was seen.
func (l *CheckoutLifecycle) retirementForegroundWork() (string, time.Time) {
	if l.foregroundWork != nil {
		return l.foregroundWork()
	}
	return l.foregroundActivity().ForegroundWork()
}

// active reports that a checkout was edited recently enough for the sweep to
// pace itself.
func (p retirementPace) active() bool {
	return p.busy != "" || (!p.last.IsZero() && p.now.Sub(p.last) < deferredRetirementActiveWindow)
}

// idleFor reports whether no foreground work has been seen for d.
func (p retirementPace) idleFor(d time.Duration) bool {
	return p.busy == "" && (p.last.IsZero() || p.now.Sub(p.last) >= d)
}

// sliceBudget is the elapsed budget of one slice under the pace.
func (p retirementPace) sliceBudget() time.Duration {
	if p.active() {
		return deferredRetirementActiveSlice
	}
	return deferredRetirementSliceBudget
}

// retirementPacedStandDown reports why the pace holds the sweep back now ("" to
// run): foreground work in flight or too recent, or a log that has not been
// reset since the sweep last grew it.
func (l *CheckoutLifecycle) retirementPacedStandDown(starved bool) string {
	pace := l.retirementPaceNow(time.Now(), starved)
	if pace.busy != "" {
		return "foreground_" + pace.busy
	}
	idle := deferredRetirementEditIdle
	if starved {
		idle = deferredRetirementStarvedIdle
	}
	if !pace.idleFor(idle) {
		return "edit_idle"
	}
	if pace.active() && l.walSinceReset() > deferredRetirementWALPause {
		// Refused on the log's size: the reclaim may then run through a busy
		// lane, or nothing between this mark and its pressure mark moves.
		if l.store != nil {
			l.store.RequestWALReclaim()
		}
		return "wal_pause"
	}
	return ""
}

// walSinceReset is the WAL the log holds since its last reset.
func (l *CheckoutLifecycle) walSinceReset() int64 {
	if l.walBytes != nil {
		return l.walBytes()
	}
	if l.store == nil {
		return 0
	}
	mark := l.store.WALWriteMark()
	if !mark.Valid {
		return 0
	}
	return int64(mark.MxFrame) * (int64(mark.PageSize) + 24)
}

// orderForPace orders a pass's candidates under the pace: while a checkout is
// being edited the smallest generation goes first (the count falls without a
// large write) and a generation larger than deferredRetirementLargeBytes waits
// for a longer idle, unless the sweep is starved. Otherwise the order is
// unchanged. size reads a generation's stored payload size (0 unknown).
func orderForPace(pace retirementPace, ordered []int64, size func(int64) int64) []int64 {
	if !pace.active() || len(ordered) == 0 {
		return ordered
	}
	sizes := make(map[int64]int64, len(ordered))
	for _, id := range ordered {
		sizes[id] = size(id)
	}
	out := make([]int64, 0, len(ordered))
	for _, id := range ordered {
		if sizes[id] > deferredRetirementLargeBytes && !pace.starved && !pace.idleFor(deferredRetirementLargeIdle) {
			continue
		}
		out = append(out, id)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if sizes[out[i]] != sizes[out[j]] {
			return sizes[out[i]] < sizes[out[j]]
		}
		return out[i] > out[j]
	})
	return out
}

// generationStorageBytes is a generation's payload size as its publication
// recorded it (0 when unknown or on error).
func (l *CheckoutLifecycle) generationStorageBytes(ctx context.Context) func(int64) int64 {
	return func(id int64) int64 {
		if l.catalog == nil {
			return 0
		}
		row, found, err := l.catalog.GetViewGeneration(ctx, id)
		if err != nil || !found {
			return 0
		}
		return row.StorageBytes
	}
}
