package indexer

import (
	"context"
	"time"
)

// pointPublicationAttempt is local to one runnable point attempt. Failed,
// retried, stopped and superseded attempts never publish this collector into
// a receipt. The guarded existing waiter completion accepts only its exact
// winning record, outside the watcher lock and before Done delivery.
type pointPublicationAttempt struct {
	record      *PublicationPhaseRecord
	stamps      *PublicationStamps
	independent bool
}

func (w *Watcher) beginPointPublication(path string, generation uint64) *pointPublicationAttempt {
	return w.beginPointPublicationObserved(path, generation, 0, nil)
}

func (w *Watcher) beginPointPublicationObserved(path string, generation uint64, retry int, observation *canonicalPointTimer) *pointPublicationAttempt {
	w.mu.Lock()
	var record *PublicationPhaseRecord
	var predecessor *PublicationPhaseRecord
	if !w.stopping && w.pendingGeneration[path] == generation {
		record = w.mutationWaiters[path][generation].publication
		if record == nil && observation != nil {
			if requested := w.observedMutationGeneration[path]; requested != 0 && requested < generation {
				predecessor = w.mutationWaiters[path][requested].publication
			}
		}
	}
	w.mu.Unlock()
	independent := false
	if record == nil && predecessor != nil {
		origin := observation.entered
		if retry == 0 && !observation.debounce.started.IsZero() && !observation.debounce.started.After(observation.entered) {
			origin = observation.debounce.started
		}
		record = predecessor.beginCoalescedAttempt(generation, retry, origin)
		independent = record != nil
		if independent && origin.Equal(observation.debounce.started) {
			record.MarkAt(PublicationTimerArmStarted, origin)
		}
	}
	if record == nil {
		return nil // no tracing allocations on native/uninstrumented paths
	}
	stamps := record.newCanonicalPatchStamps()
	stamps.markAt(PublicationCycleStarted, time.Now())
	return &pointPublicationAttempt{record: record, stamps: stamps, independent: independent}
}

func (p *pointPublicationAttempt) withContext(ctx context.Context) context.Context {
	if p == nil {
		return ctx
	}
	// Keep the existing lane cancellation and deadline. Only the attempt's
	// collector travels; attaching a live record would publish rejected spans.
	return context.WithValue(ctx, publicationStampsKey{}, p.stamps)
}

func markPointPublication(ctx context.Context, phase PublicationPhase) time.Time {
	if stamps := PublicationStampsFrom(ctx); stamps != nil {
		at := time.Now()
		stamps.markAt(phase, at)
		return at
	}
	return time.Time{}
}
