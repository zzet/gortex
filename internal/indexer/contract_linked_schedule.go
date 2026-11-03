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
//   - idle: only once Demand has shown no foreground work for the idle
//     window: none in flight at a sample, and no tool call that started or
//     ended since the one before. The dispatcher keeps a held actor in its
//     queue, skipping it, so a primary actor behind it still starts; a held
//     actor keeps no handoff source, reacquires its inputs when it starts and
//     takes no queue capacity. Demand that returns between the pick and the
//     job body sends the actor back to wait, uncounted as a start.
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
// job slot, leases and reservations. It yields on the shared yield instead,
// as a primary job does, and still stops at its next checkpoint.
const contractLinkedParkMax = 30 * time.Second

// contractDemand is one sample of Demand; sampled is false where the idle
// policy holds nothing, so nothing was asked.
type contractDemand struct {
	sampled, busy bool
	seq           uint64
}

// linkedIdleApplies reports whether the idle policy governs a's jobs: a
// linked actor whose inputs the scheduler can reacquire, with a demand
// signal to wait on.
func (c *ContractAnalysisCoordinator) linkedIdleApplies(a *contractActor) bool {
	return !a.primary() && c.idlePolicyActs()
}

// idlePolicyActs reports whether the idle policy can hold any actor.
func (c *ContractAnalysisCoordinator) idlePolicyActs() bool {
	return c.options.LinkedInputs != nil && c.options.Demand != nil && c.options.LinkedPolicy == ContractLinkedIdle
}

// sampleDemand samples Demand. It runs outside c.mu: the signal reads edit
// and tool state whose owners call into the scheduler under their own locks.
func (c *ContractAnalysisCoordinator) sampleDemand() contractDemand {
	busy, seq := c.options.Demand()
	return contractDemand{sampled: true, busy: busy, seq: seq}
}

// idleDemand samples Demand for a dispatcher step when the idle policy can
// hold anything.
func (c *ContractAnalysisCoordinator) idleDemand() contractDemand {
	if !c.idlePolicyActs() {
		return contractDemand{}
	}
	return c.sampleDemand()
}

// linkedDemand samples Demand for a picked actor the idle policy governs.
func (c *ContractAnalysisCoordinator) linkedDemand(a *contractActor) contractDemand {
	if !c.linkedIdleApplies(a) {
		return contractDemand{}
	}
	return c.sampleDemand()
}

// noteDemandLocked records a sample taken outside c.mu: work in flight, or a
// tool call that started or ended since an earlier sample, closes the idle
// window at now. Samples may arrive out of order; one older than the highest
// sequence seen adds nothing.
func (c *ContractAnalysisCoordinator) noteDemandLocked(now time.Time, demand contractDemand) {
	if !demand.sampled {
		return
	}
	if demand.busy || demand.seq > c.demandSeq {
		c.lastDemand = now
	}
	c.demandSeq = max(c.demandSeq, demand.seq)
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

// idleHeldLocked reports whether the idle policy holds a at now. Only such a
// hold may outlast a's interest: it lasts as long as demand does, while a
// throttled actor only waits for its turn, as a queued actor waits for the
// slot.
func (c *ContractAnalysisCoordinator) idleHeldLocked(a *contractActor, now time.Time) bool {
	return c.options.LinkedPolicy == ContractLinkedIdle && c.linkedHeldLocked(a, now)
}

// holdLocked keeps a queued actor waiting for its linked policy. The first
// skip of a hold is counted; an idle hold takes no queue capacity, so it
// wakes requests waiting for capacity. A held actor never pins its handoff
// source, so the source is returned for release outside c.mu.
func (c *ContractAnalysisCoordinator) holdLocked(a *contractActor) *ContractCaptureSource {
	if !a.held {
		a.held = true
		countContractLinkedJob(c.options.LinkedPolicy, viewmetrics.ContractLinkedDeferred)
		if c.options.LinkedPolicy == ContractLinkedIdle {
			c.capacityFreedLocked()
		}
	}
	source := a.want.source
	a.want.source = nil
	return source
}

// linkedHeldAgainLocked re-checks a picked actor's idle hold with a demand
// sample taken outside c.mu: true when demand returned since the pick.
func (c *ContractAnalysisCoordinator) linkedHeldAgainLocked(a *contractActor, demand contractDemand) bool {
	if !demand.sampled {
		return false
	}
	now := c.now()
	c.noteDemandLocked(now, demand)
	return c.linkedHeldLocked(a, now)
}

// endLinkedDeferred puts a picked linked actor back in its queue, held until
// its next idle window. No job started: its trigger is kept and the hold is
// counted as a deferral of its own.
func (c *ContractAnalysisCoordinator) endLinkedDeferred(a *contractActor, want contractActorWant, trigger string, triggeredAt time.Time) {
	c.mu.Lock()
	if a.running.seq == want.seq {
		a.running.contractActorKeySeq = contractActorKeySeq{}
		a.running.cancel = nil
	}
	if c.supersededLocked(a, want) {
		c.mu.Unlock()
		return
	}
	c.enqueueLocked(a, trigger, triggeredAt)
	unpinned := c.holdLocked(a)
	c.mu.Unlock()
	unpinned.Release()
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
// demand since its last look it returns at once. A tool call that came and
// went since then marks the job, which stops at its next checkpoint. Work in
// flight marks it too, and parks it with no progress slice until the work
// ends, another actor waits for the slot or the context ends. A job that has
// parked for contractLinkedParkMax in all yields on the shared yield.
func (c *ContractAnalysisCoordinator) linkedYield(a *contractActor, job *contractAnalysisJob) func(context.Context) error {
	var parked atomic.Int64
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.strict(a) {
			return c.options.Yield(ctx)
		}
		demand := c.sampleDemand()
		if !job.noteDemand(demand) {
			return nil
		}
		job.demandSeen.Store(true)
		start := c.now()
		defer func() { parked.Add(int64(c.now().Sub(start))) }()
		poll := time.NewTicker(5 * time.Millisecond)
		defer poll.Stop()
		for {
			if c.linkedParkEnds(a, demand) || !demand.busy {
				return ctx.Err()
			}
			if time.Duration(parked.Load())+c.now().Sub(start) >= contractLinkedParkMax {
				return c.options.Yield(ctx)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-poll.C:
			}
			demand = c.sampleDemand()
			job.noteDemand(demand)
		}
	}
}

// noteDemand records a demand sample of the job and reports whether it shows
// demand the job has not seen: work in flight, or a sequence past the
// highest it saw.
func (job *contractAnalysisJob) noteDemand(demand contractDemand) bool {
	for {
		seen := job.demandSeq.Load()
		if demand.seq <= seen {
			return demand.busy
		}
		if job.demandSeq.CompareAndSwap(seen, demand.seq) {
			return true
		}
	}
}

// linkedParkEnds records a demand sample of a linked job and reports whether
// another actor waits for its slot or a strict request arrived.
func (c *ContractAnalysisCoordinator) linkedParkEnds(a *contractActor, demand contractDemand) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.noteDemandLocked(now, demand)
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

// linkedStartFields are a linked job's start log fields. since_gate_ms is
// the time since its linked policy let it start: the payload reservations
// written in between delay the line, and a log audit checks foreground work
// at the gate, not at the line.
func (job *contractAnalysisJob) linkedStartFields() []zap.Field {
	if job.linkedPolicy == "" {
		return nil
	}
	return append(job.linkedFields(), zap.Float64("since_gate_ms", contractMillis(time.Since(job.gatedAt))))
}

// linkedEndFields are a linked job's completion log fields: demand_stop is
// set when the fill stopped at a checkpoint for demand.
func (job *contractAnalysisJob) linkedEndFields(outcome string) []zap.Field {
	if job.linkedPolicy == "" {
		return nil
	}
	return append(job.linkedFields(), zap.Bool("demand_stop", job.stoppedForDemand(outcome)))
}

// linkedFields name a linked job's policy and whether a strict request
// started it.
func (job *contractAnalysisJob) linkedFields() []zap.Field {
	return []zap.Field{zap.String("policy", string(job.linkedPolicy)), zap.Bool("strict", job.strict)}
}

// stoppedForDemand reports whether a job that ended with outcome stopped at
// a fill checkpoint because it met demand.
func (job *contractAnalysisJob) stoppedForDemand(outcome string) bool {
	return outcome == viewmetrics.ContractJobFillContinued && job.demandStop.Load()
}

// countLinkedStartLocked counts a linked actor's job body about to start,
// apart when a strict request started it, and dates the gate it passed.
func (c *ContractAnalysisCoordinator) countLinkedStartLocked(a *contractActor, job *contractAnalysisJob) {
	if a.primary() {
		return
	}
	job.gatedAt = time.Now()
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
