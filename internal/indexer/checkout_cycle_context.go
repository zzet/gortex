package indexer

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
)

// Per-cycle context the coordinator hands down its own call tree: the instant
// the cycle started (so every step of one cycle can share one working-copy
// sample taken after it), and the refresh tickets the cycle serves (so the
// builder can mark publication phases on the records those tickets opened).

type cycleStartKey struct{}

type publicationTargetKey struct{}

type publicationRecordKey struct{}

// publicationTarget names the refresh tickets one coordinator cycle serves:
// every ticket of checkoutID admitted at or before through.
type publicationTarget struct {
	checkoutID string
	through    uint64
}

// withCycleStart records the instant a coordinator cycle started. Every ticket
// the cycle serves was admitted before it.
func withCycleStart(ctx context.Context, started time.Time) context.Context {
	return context.WithValue(ctx, cycleStartKey{}, started)
}

// cycleStartFrom returns the instant recorded by withCycleStart, zero when the
// context carries none (a caller-driven transition, a test driving reconcile
// directly).
func cycleStartFrom(ctx context.Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	started, _ := ctx.Value(cycleStartKey{}).(time.Time)
	return started
}

// cycleSample is the working-copy sample one cycle's decisions share: taken
// once and reused by the settle check, the reconcile, the working-tree slot
// and the build's change set. Outside a cycle it is an ordinary fresh sample.
//
// What the sample must postdate is every claim the cycle answers: each
// waiting refresh ticket's freshAfter (its request's arrival; an edit's disk
// commit) and every signal the loop consumed to run it. When tickets wait,
// any sample begun after the latest of those will do — an edit ticket's own
// capture sample, taken after its disk commit, typically — so the edit's
// cycle decides without a sample of its own. With no ticket waiting, the
// bound is the cycle's start, as before. Reuse weakens nothing the sample
// decides: a build still re-samples after its payload is complete
// (confirmDirtySnapshotWith), and that is what decides whether it publishes.
func (c *CheckoutCoordinator) cycleSample(ctx context.Context) (gitstate.DirtySnapshot, error) {
	started := cycleStartFrom(ctx)
	if started.IsZero() {
		return c.sampler.Sample(ctx)
	}
	if owed := c.latestTicketFreshAfter(); !owed.IsZero() {
		since := owed
		if signaled := c.lastSignal(); signaled.After(since) {
			since = signaled
		}
		return c.sampler.SampleSince(ctx, since)
	}
	return c.sampler.SampleSince(ctx, started)
}

// latestTicketFreshAfter is the latest freshAfter among the refresh tickets
// still waiting; zero when none waits.
func (c *CheckoutCoordinator) latestTicketFreshAfter() time.Time {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	var latest time.Time
	for _, request := range c.refreshWaiters {
		if request.freshAfter.After(latest) {
			latest = request.freshAfter
		}
	}
	return latest
}

// lastSignal is when Signal last claimed the checkout moved; zero before the
// first.
func (c *CheckoutCoordinator) lastSignal() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.signaledAt
}

// withPublicationTarget records which refresh tickets a cycle serves, so the
// phases the builder reaches can be marked on their publication records.
// through == 0 (a cycle that serves no ticket) records nothing.
func withPublicationTarget(ctx context.Context, checkoutID string, through uint64) context.Context {
	if through == 0 || checkoutID == "" {
		return ctx
	}
	return context.WithValue(ctx, publicationTargetKey{}, publicationTarget{checkoutID: checkoutID, through: through})
}

// markPublicationPhase marks phase on the publication records of the tickets
// the context's cycle serves, and on the record the context carries for work
// no ticket names (withPhaseRecord: an observed filesystem change, a
// background compaction). It is a no-op when the context carries neither.
func markPublicationPhase(ctx context.Context, phase PublicationPhase) {
	if ctx == nil {
		return
	}
	if record := phaseRecordFrom(ctx); record != nil {
		record.Mark(phase)
	}
	target, ok := ctx.Value(publicationTargetKey{}).(publicationTarget)
	if !ok {
		return
	}
	DefaultPublicationPhases().MarkCheckoutThrough(target.checkoutID, target.through, phase)
}

type phaseRecordKey struct{}

// withPhaseRecord attaches the publication record of work no refresh ticket
// names — a filesystem change a cycle observed, a background compaction — so
// the phases the cycle and the builder reach are marked on it too.
func withPhaseRecord(ctx context.Context, record *PublicationPhaseRecord) context.Context {
	if record == nil {
		return ctx
	}
	return context.WithValue(ctx, phaseRecordKey{}, record)
}

// phaseRecordFrom returns the record withPhaseRecord attached, nil if none.
func phaseRecordFrom(ctx context.Context) *PublicationPhaseRecord {
	if ctx == nil {
		return nil
	}
	record, _ := ctx.Value(phaseRecordKey{}).(*PublicationPhaseRecord)
	return record
}

// WithPublicationRecord hands a refresh request's publication record to the
// coordinator. The ticket the request admits binds to the record, and marks
// ticket_enqueued on it, before the coordinator is woken — so the demand-woken
// cycle's first marks (cycle_started, admitted) always find the record bound.
// A record bound only after the admission call returns can miss them.
func WithPublicationRecord(ctx context.Context, record *PublicationPhaseRecord) context.Context {
	if record == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, publicationRecordKey{}, record)
}

// publicationRecordFrom returns the record WithPublicationRecord attached.
func publicationRecordFrom(ctx context.Context) *PublicationPhaseRecord {
	if ctx == nil {
		return nil
	}
	record, _ := ctx.Value(publicationRecordKey{}).(*PublicationPhaseRecord)
	return record
}
