package indexer

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

// Linked-checkout scheduling. A linked worktree's actor runs the same jobs as
// a primary actor, but its fills compete for CPU with the reads agents make
// in that worktree. Its linked policy decides when its job may start:
//
//   - idle: only once Demand has reported nothing for the idle window. The
//     dispatcher keeps a held actor in its queue, skipping it, so a primary
//     actor behind it still starts; a held actor keeps no handoff source and
//     reacquires its inputs when it starts. Demand that returns between the
//     pick and the job body sends the actor back to wait, uncounted as a
//     start.
//   - throttled: one linked job in flight, and one linked actor's starts at
//     least the throttle interval apart. The idle window does not apply.
//   - eager: as a primary actor.
//
// A strict request, whose caller waits for fresh results, exempts its actor
// from every policy for the rest of the caller's budget.
//
// Requests and Published events still record interest and wanted inputs
// while an actor is held; only the start waits. A linked fill the idle
// policy started yields to demand without the progress slice the shared
// yield grants, and stops at its next fill checkpoint once it met demand:
// the actor is requeued, held until the next idle window, and resumes from
// its retained checkpoint.

// contractDemandPoll is how often a dispatcher with a held actor samples
// Demand again while the idle window is closed.
const contractDemandPoll = 250 * time.Millisecond

// contractLinkedParkMax bounds how long one linked job parks for demand in
// all. Past it the job no longer parks: a long busy spell must not pin its
// job slot, leases and reservations; it still stops at its next checkpoint.
const contractLinkedParkMax = 30 * time.Second

// linkedIdleApplies reports whether the idle policy governs a's jobs: a
// linked actor whose inputs the scheduler can reacquire, with a demand
// signal to wait on.
func (c *ContractAnalysisCoordinator) linkedIdleApplies(a *contractActor) bool {
	return !a.primary() && c.options.LinkedInputs != nil && c.options.Demand != nil && c.options.LinkedPolicy == ContractLinkedIdle
}

// idleDemand samples Demand for a dispatcher step when the idle policy can
// hold anything. It runs outside c.mu: the signal reads edit and tool state
// whose owners call into the scheduler under their own locks.
func (c *ContractAnalysisCoordinator) idleDemand() bool {
	return c.options.LinkedInputs != nil && c.options.Demand != nil && c.options.LinkedPolicy == ContractLinkedIdle && c.options.Demand()
}

// linkedDemand samples Demand for a picked actor the idle policy governs,
// outside c.mu.
func (c *ContractAnalysisCoordinator) linkedDemand(a *contractActor) bool {
	return c.linkedIdleApplies(a) && c.options.Demand()
}

// linkedHoldLocked reports whether a's linked policy holds it at now, and
// when to look again; zero waits for a signal.
func (c *ContractAnalysisCoordinator) linkedHoldLocked(a *contractActor, now time.Time) (bool, time.Duration) {
	if a.primary() || c.options.LinkedInputs == nil || now.Before(a.strictUntil) {
		return false, 0
	}
	switch c.options.LinkedPolicy {
	case ContractLinkedIdle:
		if c.options.Demand == nil {
			return false, 0
		}
		if remaining := c.options.LinkedIdleWindow - now.Sub(c.lastDemand); remaining > 0 {
			return true, min(remaining, contractDemandPoll)
		}
	case ContractLinkedThrottled:
		// One linked job in flight: the one in flight wakes the held when
		// it leaves (wakeHeldLocked).
		for _, other := range c.actors {
			if other != a && !other.primary() && (other.state == contractActorCapturing || other.state == contractActorRunning) {
				return true, 0
			}
		}
		if !a.linkedStartedAt.IsZero() {
			if remaining := c.options.LinkedThrottleInterval - now.Sub(a.linkedStartedAt); remaining > 0 {
				return true, remaining
			}
		}
	}
	return false, 0
}

// wakeHeldLocked signals every ring that holds an actor, when a linked job
// that throttled them leaves its slot.
func (c *ContractAnalysisCoordinator) wakeHeldLocked() {
	for _, ring := range c.rings {
		for _, queued := range ring.queue {
			if queued.held {
				ring.signal()
				break
			}
		}
	}
}

// linkedHeldLocked reports whether a's linked policy holds it at now.
func (c *ContractAnalysisCoordinator) linkedHeldLocked(a *contractActor, now time.Time) bool {
	held, _ := c.linkedHoldLocked(a, now)
	return held
}

// holdLocked keeps a queued actor waiting for its linked policy. The first
// skip of a hold is counted; a held actor never pins its handoff source, so
// the source is returned for release outside c.mu.
func (c *ContractAnalysisCoordinator) holdLocked(a *contractActor) *ContractCaptureSource {
	if !a.held {
		a.held = true
		countContractLinkedJob(c.options.LinkedPolicy, viewmetrics.ContractLinkedDeferred)
	}
	source := a.want.source
	a.want.source = nil
	return source
}

// linkedHeldAgainLocked re-checks a picked actor's idle hold with a demand
// sample taken outside c.mu: true when demand returned since the pick.
func (c *ContractAnalysisCoordinator) linkedHeldAgainLocked(a *contractActor, busy bool) bool {
	if !c.linkedIdleApplies(a) {
		return false
	}
	now := c.now()
	if busy {
		c.lastDemand = now
	}
	return c.linkedHeldLocked(a, now)
}

// endLinkedDeferred puts a picked linked actor back in its queue, held until
// its next idle window. No job started: its trigger is kept and the hold is
// counted as a deferral of its own.
func (c *ContractAnalysisCoordinator) endLinkedDeferred(a *contractActor, want contractActorWant, trigger string, triggeredAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.running.seq == want.seq {
		a.running.contractActorKeySeq = contractActorKeySeq{}
		a.running.cancel = nil
	}
	if c.supersededLocked(a, want) {
		return
	}
	c.enqueueLocked(a, trigger, triggeredAt)
	a.held = true
	countContractLinkedJob(c.options.LinkedPolicy, viewmetrics.ContractLinkedDeferred)
}

// linkJob prepares a linked actor's job for its policy before the body
// starts: an idle-governed job yields without a progress slice and stops at
// its next fill checkpoint once it met demand.
func (c *ContractAnalysisCoordinator) linkJob(a *contractActor, job *contractAnalysisJob) {
	if a.primary() {
		return
	}
	job.linkedPolicy = c.options.LinkedPolicy
	if !c.linkedIdleApplies(a) {
		return
	}
	job.yield = c.linkedYield(a, job)
	if other := job.otherActorWaiting; other != nil {
		job.otherActorWaiting = func() bool {
			if other() {
				job.demandStop.Store(false)
				return true
			}
			stop := job.demandSeen.Load() && !c.strict(a)
			job.demandStop.Store(stop)
			return stop
		}
	}
}

// linkedYield is the yield of a linked job the idle policy governs. With no
// demand it returns at once. With demand it marks the job and parks, with
// no progress slice, until demand ends, another actor waits for the slot,
// the context ends, or the job has parked for contractLinkedParkMax in all.
func (c *ContractAnalysisCoordinator) linkedYield(a *contractActor, job *contractAnalysisJob) func(context.Context) error {
	var parked atomic.Int64
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.strict(a) {
			return c.options.Yield(ctx)
		}
		if !c.options.Demand() {
			return nil
		}
		job.demandSeen.Store(true)
		start := c.now()
		defer func() { parked.Add(int64(c.now().Sub(start))) }()
		poll := time.NewTicker(5 * time.Millisecond)
		defer poll.Stop()
		for busy := true; busy; busy = c.options.Demand() {
			if c.linkedParkEnds(a, busy) || time.Duration(parked.Load())+c.now().Sub(start) >= contractLinkedParkMax {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-poll.C:
			}
		}
		return ctx.Err()
	}
}

// linkedParkEnds records a demand sample of a parked linked job and reports
// whether another actor waits for its slot or a strict request arrived.
func (c *ContractAnalysisCoordinator) linkedParkEnds(a *contractActor, busy bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if busy {
		c.lastDemand = now
	}
	return now.Before(a.strictUntil) || c.otherActorWaitingLocked(a, now)
}

// strict reports whether a strict request's budget exempts a's jobs now.
func (c *ContractAnalysisCoordinator) strict(a *contractActor) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now().Before(a.strictUntil)
}

// markStrictLocked exempts a's jobs from its linked policy for the rest of a
// strict request's budget, at most the interest TTL, and wakes its ring so
// a held actor starts at once. The budget is a duration, so it is added to
// the coordinator's own clock.
func (c *ContractAnalysisCoordinator) markStrictLocked(ctx context.Context, a *contractActor, now time.Time) {
	budget := contractInterestTTL
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline))
	}
	if until := now.Add(budget); until.After(a.strictUntil) {
		a.strictUntil = until
	}
	if a.state == contractActorQueued {
		a.ring.signal()
	}
}

// linkedStartFields are a linked job's start log fields.
func (job *contractAnalysisJob) linkedStartFields() []zap.Field {
	if job.linkedPolicy == "" {
		return nil
	}
	return []zap.Field{zap.String("policy", string(job.linkedPolicy)), zap.Bool("strict", job.strict)}
}

// linkedEndFields are a linked job's completion log fields: demand_stop is
// set when the fill stopped at a checkpoint for demand.
func (job *contractAnalysisJob) linkedEndFields(outcome string) []zap.Field {
	if job.linkedPolicy == "" {
		return nil
	}
	return append(job.linkedStartFields(), zap.Bool("demand_stop", job.stoppedForDemand(outcome)))
}

// stoppedForDemand reports whether a job that ended with outcome stopped at
// a fill checkpoint because it met demand.
func (job *contractAnalysisJob) stoppedForDemand(outcome string) bool {
	return outcome == viewmetrics.ContractJobFillContinued && job.demandStop.Load()
}

// countLinkedStartLocked counts a linked actor's job body about to start,
// apart when a strict request started it.
func (c *ContractAnalysisCoordinator) countLinkedStartLocked(a *contractActor, job *contractAnalysisJob) {
	if a.primary() {
		return
	}
	event := viewmetrics.ContractLinkedStarted
	if job.strict = c.now().Before(a.strictUntil); job.strict {
		event = viewmetrics.ContractLinkedStrictStarted
	}
	countContractLinkedJob(c.options.LinkedPolicy, event)
}

// releaseContractSources releases handoff sources taken under c.mu.
func releaseContractSources(sources []*ContractCaptureSource) {
	for _, source := range sources {
		source.Release()
	}
}
