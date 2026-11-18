package indexer

import (
	"context"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Which generation a retirement burst serves.
//
// A generation is fenced (retiring) on its first quantum and removed only on
// its last. The bursts used to rotate the lifecycle cursor to a different
// generation every time, so in a measured run about a hundred generations sat
// fenced and part-swept at once, each getting one burst per lap, and they
// finished in lock-step many minutes later; a parent cannot even be fenced
// until every child's row is gone, so the parents waited on the slowest
// child. The rows deleted per second were the same either way. What changed
// was how many generations those rows ever finished.
//
// A burst now finishes what it started:
//
//  1. the generation the previous burst left fenced with progress, while it
//     has held that priority for less than deferredRetirementActiveCap;
//  2. any other fenced (retiring) generation;
//  3. a leaf — nothing is built on it, so it cannot be refused as "based" —
//     smallest recorded payload first, newest first among equals;
//  4. the lifecycle cursor.
//
// Every deferredRetirementCursorTurn-th choice, and the first choice after a
// generation used up its cap, is the cursor's instead, so neither one large
// generation nor a stream of small new ones can starve the rest.

const (
	deferredRetirementActiveCap  = 30 * time.Second
	deferredRetirementCursorTurn = 16
)

// retirementShapes reads the candidates' shapes for one burst; nil when the
// catalog cannot answer, which leaves the cursor to choose.
func (l *CheckoutLifecycle) retirementShapes(ctx context.Context, ordered []int64) map[int64]store_sqlite.RetirementCandidateShape {
	if l.catalog == nil || len(ordered) == 0 {
		return nil
	}
	shapes, err := l.catalog.RetirementCandidateShapes(ctx, ordered)
	if err != nil {
		return nil
	}
	return shapes
}

// pickDeferredRetirement chooses the generation a burst serves next from the
// newest-first candidates.
func (l *CheckoutLifecycle) pickDeferredRetirement(
	ordered []int64, shapes map[int64]store_sqlite.RetirementCandidateShape, now time.Time,
) int64 {
	if len(ordered) == 0 {
		return 0
	}
	l.coordMu.Lock()
	l.deferredRetirementPicks++
	turn := l.deferredRetirementPicks%deferredRetirementCursorTurn == 0
	active, since := l.deferredRetirementActive, l.deferredRetirementActiveSince
	capped := int64(0)
	if active != 0 && now.Sub(since) >= deferredRetirementActiveCap {
		capped, active = active, 0
		l.deferredRetirementActive = 0
		turn = true
	}
	l.coordMu.Unlock()
	if active != 0 && !turn && containsGeneration(ordered, active) {
		return active
	}
	if turn {
		candidates := ordered
		if capped != 0 && len(ordered) > 1 {
			candidates = withoutGeneration(ordered, capped)
		}
		return l.nextDeferredRetirement(candidates)
	}
	for _, id := range ordered {
		if shape, ok := shapes[id]; ok && shape.State == store_sqlite.ViewGenerationRetiring {
			return id
		}
	}
	leaves := make([]int64, 0, len(ordered))
	for _, id := range ordered {
		if shape, ok := shapes[id]; ok && !shape.HasChildren {
			leaves = append(leaves, id)
		}
	}
	if len(leaves) > 0 {
		sort.SliceStable(leaves, func(i, j int) bool {
			return shapes[leaves[i]].StorageBytes < shapes[leaves[j]].StorageBytes
		})
		return leaves[0]
	}
	return l.nextDeferredRetirement(ordered)
}

// noteDeferredRetirementServed records how the burst left a generation: one
// left fenced with progress keeps the next burst's priority; one removed,
// refused or not advanced gives it up.
func (l *CheckoutLifecycle) noteDeferredRetirementServed(generationID int64, progressed, removed bool, now time.Time) {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	switch {
	case progressed && !removed:
		if l.deferredRetirementActive != generationID {
			l.deferredRetirementActive, l.deferredRetirementActiveSince = generationID, now
		}
	case l.deferredRetirementActive == generationID:
		l.deferredRetirementActive = 0
	}
}

func containsGeneration(ids []int64, id int64) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func withoutGeneration(ids []int64, id int64) []int64 {
	out := make([]int64, 0, len(ids))
	for _, candidate := range ids {
		if candidate != id {
			out = append(out, candidate)
		}
	}
	return out
}
