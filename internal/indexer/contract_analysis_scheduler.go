package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

// The contract analysis scheduler. A request records its interest in an
// actor's inputs and returns; the work happens on the scheduler's own
// goroutines:
//
//   - An actor is one checkout and the sorted repositories of its cohort.
//     The repository a request selected is not part of it, so requests that
//     reach one cohort through different selected repositories share one
//     actor, and an actor runs at most one job at a time.
//   - Each actor lives on one home ring, the ring of its first cohort
//     repository. One dispatcher goroutine per ring picks its queued actors in
//     the order they were queued; a requeued actor goes to the back.
//   - Jobs share one global job slot, granted in the order the dispatchers
//     asked for it, so a busy repository cannot starve another.
//   - A request's interest lasts contractInterestTTL. While it lasts, an
//     accepted core mutation of a cohort repository (Published) has the
//     dispatcher capture a primary actor's inputs again, so a job follows the
//     accept rather than the next request. Only a request extends interest.
//   - An actor's wanted inputs advance by input epoch, never by arrival: a
//     capture taken at an older epoch cannot replace a newer one.
//
// Nothing of a request waits on the store, a census, the job slot or a
// repository lane: Schedule runs under c.mu and does no I/O.

// contractInterestTTL is how long a request's interest in an actor lasts.
const contractInterestTTL = 60 * time.Second

// Registry bounds: actors with pending or running work, and actors kept.
const (
	contractSchedulerCapacity    = 8
	contractSchedulerMaxCapacity = 32
	contractSchedulerMaxActors   = 64
)

// contractStallEnds is how many consecutive jobs of an actor may end without
// publishing before it reports itself stalled.
const contractStallEnds = 3

// Backoff after a job that could not settle its reads or publish: 1, 2, 4 …
// seconds, at most contractBackoffMax.
const (
	contractBackoffBase = time.Second
	contractBackoffMax  = 30 * time.Second
)

// contractPanicRetry is how long after a panicking job its one retry runs.
const contractPanicRetry = time.Second

// contractRepairWait is where an actor that deferred an unsettled requeue to
// the stuck repair is in the repair's passes.
type contractRepairWait uint8

const (
	contractRepairWaitNone contractRepairWait = iota
	// contractRepairWaitAsked: the actor waits for the next pass.
	contractRepairWaitAsked
	// contractRepairWaitSeen: a pass started after the actor deferred.
	contractRepairWaitSeen
)

// Job triggers: what queued a job.
const (
	contractJobTriggerPublished = "published"
	contractJobTriggerRequeue   = "requeue"
	contractJobTriggerTimer     = "timer"
)

var (
	// ErrContractQueueFull refuses a new actor while the scheduler holds its
	// capacity of actors with pending or running work. It is retryable: the
	// refusal is a *ContractQueueFullError carrying when to ask again.
	ErrContractQueueFull = errors.New("contract analysis: scheduler queue is full")
	// ErrContractUnsupported refuses inputs the worker cannot build: a
	// selected view with no contract input state of its own.
	ErrContractUnsupported = errors.New("contract analysis: the selected inputs cannot be analysed")
	// ErrContractSchedulerClosed refuses requests after Close.
	ErrContractSchedulerClosed = errors.New("contract analysis: scheduler is closed")
)

// ContractQueueFullError is the typed queue-full refusal.
type ContractQueueFullError struct{ RetryAfter time.Duration }

func (e *ContractQueueFullError) Error() string {
	return fmt.Sprintf("%s: retry after %s", ErrContractQueueFull, e.RetryAfter)
}

func (e *ContractQueueFullError) Is(target error) bool { return target == ErrContractQueueFull }

// ContractActor names one actor: a checkout and its sorted cohort.
type ContractActor struct {
	Checkout string
	cohort   string
}

// NewContractActor is the actor of checkout over repos, in any order.
func NewContractActor(checkout string, repos []string) ContractActor {
	sorted := slices.Clone(repos)
	sort.Strings(sorted)
	sorted = slices.Compact(sorted)
	return ContractActor{Checkout: checkout, cohort: strings.Join(sorted, "\x00")}
}

// Repos is the actor's sorted cohort.
func (a ContractActor) Repos() []string {
	if a.cohort == "" {
		return nil
	}
	return strings.Split(a.cohort, "\x00")
}

func (a ContractActor) String() string {
	return a.Checkout + "[" + strings.ReplaceAll(a.cohort, "\x00", ",") + "]"
}

// ContractScheduleRequest is one request's interest. Key names the selected
// repository's attachment; Inputs are the cohort's captured inputs, nil when
// the selected repository has no input state yet (its baseline is asked
// for). Source is the request view's prepared capture source, nil for a
// primary actor; Schedule owns it on every return. Epoch is the scheduler's
// input epoch sampled before Inputs were captured.
type ContractScheduleRequest struct {
	Actor  ContractActor
	Key    graph.ContractAttachmentKey
	Inputs *graphview.SelectedContractInputs
	Source *ContractCaptureSource
	Epoch  uint64
}

// ContractScheduleOutcome is what Schedule did with a request.
type ContractScheduleOutcome string

const (
	// ContractScheduled queued the actor at the request's inputs.
	ContractScheduled ContractScheduleOutcome = "scheduled"
	// ContractCoalesced found the actor already queued or settled at them.
	ContractCoalesced ContractScheduleOutcome = "coalesced"
	// ContractRunning found the actor's job running; a different key reruns
	// it once when the job ends.
	ContractRunning ContractScheduleOutcome = "running"
	// ContractNoop found the inputs already published.
	ContractNoop ContractScheduleOutcome = "noop"
	// ContractDeferred queued inputs that are not accepted yet: the actor
	// waits on their acceptance, or admits their baseline.
	ContractDeferred ContractScheduleOutcome = "deferred"
)

// ContractScheduleResult answers Schedule. ArrivalSeq is the actor's seq for
// the request's inputs: equal inputs answer the same seq.
type ContractScheduleResult struct {
	Result     ContractScheduleOutcome
	Actor      ContractActor
	ArrivalSeq uint64
}

// ContractActorResult is how an actor's promise for a seq resolved.
type ContractActorResult string

const (
	// ContractActorPending: not resolved.
	ContractActorPending ContractActorResult = ""
	// ContractActorPublished: a job published the seq's inputs or newer.
	ContractActorPublished ContractActorResult = "published"
	// ContractActorNoop: the seq's attachments existed; no job ran.
	ContractActorNoop ContractActorResult = "noop"
	// ContractActorFailed: the seq's job failed; it reruns only on a key
	// change.
	ContractActorFailed ContractActorResult = "failed"
	// ContractActorPanic: the seq's job panicked; it is retried once.
	ContractActorPanic ContractActorResult = "panic"
	// ContractActorStalled: consecutive jobs ended without publishing.
	ContractActorStalled ContractActorResult = "stalled"
	// ContractActorInputsStale: the seq's request source can no longer be
	// captured; a new request brings a new one.
	ContractActorInputsStale ContractActorResult = "inputs_stale"
	// ContractActorKeyMoved: newer inputs replaced the seq's.
	ContractActorKeyMoved ContractActorResult = "key_moved"
)

// ContractFillCheckpointStatus distinguishes retained progress from losing its
// continuation authority. A loss never means an attachment was published.
type ContractFillCheckpointStatus string

const (
	ContractFillCheckpointNone     ContractFillCheckpointStatus = ""
	ContractFillCheckpointRetained ContractFillCheckpointStatus = "retained"
	ContractFillCheckpointMissing  ContractFillCheckpointStatus = "missing"
	ContractFillCheckpointEvicted  ContractFillCheckpointStatus = "evicted"
	ContractFillCheckpointRejected ContractFillCheckpointStatus = "rejected"
)

// ContractActorStatus is an actor's state and, for WaitActor, how the seq
// waited on resolved.
type ContractActorStatus struct {
	FillCheckpoint ContractFillCheckpointStatus
	Result         ContractActorResult
	Seq            uint64
	State          string
	Stalled        bool
	RetryAfter     time.Duration
}

type contractActorState int

const (
	contractActorIdle contractActorState = iota
	contractActorQueued
	contractActorCapturing
	contractActorRunning
	contractActorBackoff
	contractActorDeferred
)

func (s contractActorState) String() string {
	switch s {
	case contractActorQueued:
		return "queued"
	case contractActorCapturing:
		return "capturing"
	case contractActorRunning:
		return "running"
	case contractActorBackoff:
		return "backoff"
	case contractActorDeferred:
		return "deferred"
	default:
		return "idle"
	}
}

// Deferral reasons.
const (
	contractDeferredBaseline        = "baseline"
	contractDeferredInputsAccepting = "inputs_accepting"
)

// contractActorWant is the inputs an actor wants published: their key, the
// seq requests at them were answered, the input epoch they were captured at,
// and what a job needs to capture them.
type contractActorWant struct {
	key            string
	seq            uint64
	epoch          uint64
	inputs         *graphview.SelectedContractInputs
	source         *ContractCaptureSource
	repo, checkout string
}

type contractActorKeySeq struct {
	key string
	seq uint64
}

type contractActor struct {
	id    ContractActor
	repos []string
	ring  *contractRing
	state contractActorState
	// deferred names why a Deferred actor waits, and since when.
	deferred   string
	deferredAt time.Time
	stalled    struct {
		reason string
		since  time.Time
		cycles int
	}
	want    contractActorWant
	running struct {
		contractActorKeySeq
		cancel context.CancelFunc
	}
	// lastPublished is the key and seq the actor last published.
	lastPublished contractActorKeySeq
	// lastCohort is immutable after installation; readers retain its pointer
	// under mu and copy owned values only after releasing mu.
	lastCohort *ContractPublishedCohort
	// Delta state lives in the coordinator LRU; this actor never owns a
	// second pointer that could bypass global eviction.
	// resolved is the latest seq a non-publishing end resolved, and how.
	resolved       uint64
	resolvedResult ContractActorResult
	// failures counts consecutive ends that did not publish; requeues counts
	// requeues of the wanted key; backoff the timed retries since.
	failures, requeues, backoff int
	// failedKey is the key whose job failed: no timer reruns it.
	failedKey string
	// panicked is set once a panicking job's one retry is spent.
	panicked    bool
	nextAttempt time.Time
	// retryOnInputs queues the actor at the next cohort Published even when
	// its key did not move: a capture refused stale, or a job ended
	// unsettled once its requeue was spent.
	retryOnInputs bool
	// repairWait is set while an idle actor waits on a repair pass instead
	// of a timer, and records once a pass of its cohort started since.
	repairWait contractRepairWait
	// dirty asks the dispatcher to capture a primary actor's inputs again,
	// at dirtyEpoch, for a Published at dirtyAt.
	dirty      bool
	dirtyEpoch uint64
	dirtyAt    time.Time
	// trigger and triggeredAt are what queued the actor and when.
	trigger       string
	triggeredAt   time.Time
	interestUntil time.Time
	// interestExpired remembers work suspended when its interest ran out.
	interestExpired bool
	// A lost/rejected partial checkpoint must not restart the same prefix forever.
	fillNeedsCheckpoint bool
	fillCheckpoint      ContractFillCheckpointStatus
	lastUsed            time.Time
	seq                 uint64
	changed             chan struct{}
}

func (a *contractActor) primary() bool { return a.id.Checkout == "" }

// hasWork reports whether the actor holds scheduler capacity.
func (a *contractActor) hasWork() bool { return a.state != contractActorIdle }

func (a *contractActor) inCohort(repo string) bool {
	_, found := slices.BinarySearch(a.repos, repo)
	return found
}

func (a *contractActor) notifyLocked() {
	close(a.changed)
	a.changed = make(chan struct{})
}

// resolveLocked resolves every seq up to seq with result.
func (a *contractActor) resolveLocked(seq uint64, result ContractActorResult) {
	if seq >= a.resolved {
		a.resolved, a.resolvedResult = seq, result
	}
	a.notifyLocked()
}

// contractRing is one repository's dispatcher state.
type contractRing struct {
	repo   string
	actors []*contractActor
	queue  []*contractActor
	wake   chan struct{}
}

func (r *contractRing) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// contractJobSlots is the global job slot: granted in the order asked.
type contractJobSlots struct {
	mu      sync.Mutex
	free    int
	waiters []chan struct{}
}

func (s *contractJobSlots) acquire(ctx context.Context) error {
	s.mu.Lock()
	if s.free > 0 && len(s.waiters) == 0 {
		s.free--
		s.mu.Unlock()
		return nil
	}
	granted := make(chan struct{})
	s.waiters = append(s.waiters, granted)
	s.mu.Unlock()
	select {
	case <-granted:
		return nil
	case <-ctx.Done():
	}
	s.mu.Lock()
	for i, waiter := range s.waiters {
		if waiter == granted {
			s.waiters = slices.Delete(s.waiters, i, i+1)
			s.mu.Unlock()
			return ctx.Err()
		}
	}
	s.mu.Unlock()
	// Granted as the context ended: hand the slot on.
	s.release()
	return ctx.Err()
}

func (s *contractJobSlots) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.waiters) > 0 {
		next := s.waiters[0]
		s.waiters = s.waiters[1:]
		close(next)
		return
	}
	s.free++
}

// contractJobSlotCount is the number of global job slots: one, the CPU
// profile a job is sized for. GORTEX_CONTRACT_JOB_SLOTS overrides it for
// tests.
func contractJobSlotCount(configured int) int {
	if configured > 0 {
		return configured
	}
	if value, err := strconv.Atoi(os.Getenv("GORTEX_CONTRACT_JOB_SLOTS")); err == nil && value > 0 && value <= contractSchedulerMaxCapacity {
		return value
	}
	return 1
}

// contractScheduleKey is the identity of a request's inputs: the cohort's
// physical input witnesses, or the selected repository asking for its
// baseline.
func contractScheduleKey(req ContractScheduleRequest) string {
	if req.Inputs == nil {
		return "baseline\x00" + req.Key.RepoPrefix + "\x00" + req.Key.CheckoutID
	}
	return contractInputsKey(req.Inputs)
}

func contractInputsKey(inputs *graphview.SelectedContractInputs) string {
	encoded, err := json.Marshal(inputs.Witnesses)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// NewContractPrimaryInputCapture captures a primary cohort's current inputs
// the way a request with no selected view does: each repository's base input
// state, composed for repo.
func NewContractPrimaryInputCapture(materializer *graphview.Materializer) func(context.Context, []string, string) (*graphview.SelectedContractInputs, error) {
	return func(ctx context.Context, repos []string, repo string) (*graphview.SelectedContractInputs, error) {
		if materializer == nil || len(repos) == 0 {
			return nil, graph.ErrContractInputVector
		}
		captures := make([]*graphview.SelectedContractInputs, 0, len(repos))
		for _, name := range repos {
			input, err := materializer.CaptureContractInputs(ctx, nil, name, "")
			if err != nil {
				return nil, err
			}
			captures = append(captures, input)
		}
		return graphview.ComposeSelectedContractInputs(repo, "", captures...)
	}
}

func (c *ContractAnalysisCoordinator) now() time.Time {
	if c.options.Now != nil {
		return c.options.Now()
	}
	return time.Now()
}

// Schedule records a request's interest in its actor's inputs and returns.
// It runs under c.mu: no store read, no census, no job slot, no lane.
func (c *ContractAnalysisCoordinator) Schedule(ctx context.Context, req ContractScheduleRequest) (result ContractScheduleResult, err error) {
	label := ""
	// The request's source is released unless the actor keeps it; a source
	// the request's inputs replace is released too.
	kept := false
	var dropped *ContractCaptureSource
	defer func() {
		if !kept {
			req.Source.Release()
		}
		dropped.Release()
		if err != nil && label == "" {
			label = viewmetrics.ContractRequestError
		}
		countContractRequest(label)
	}()
	result.Actor = req.Actor
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if req.Inputs == nil && req.Source != nil {
		// A selected view with no input state of its own has no baseline
		// the worker can reconstruct.
		return result, ErrContractUnsupported
	}
	key := contractScheduleKey(req)
	if key == "" || len(req.Actor.Repos()) == 0 {
		return result, graph.ErrContractInputVector
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return result, ErrContractSchedulerClosed
	}
	now := c.now()
	c.expireLocked(now)
	a := c.actors[req.Actor]
	// A request at the inputs the actor last published is answered at once.
	if a != nil && a.state == contractActorIdle && a.lastPublished.key == key && a.want.key == key {
		a.interestUntil, a.lastUsed = now.Add(contractInterestTTL), now
		label = viewmetrics.ContractRequestAttached
		result.Result, result.ArrivalSeq = ContractNoop, a.lastPublished.seq
		return result, nil
	}
	if a == nil {
		if c.activeLocked() >= c.options.QueueCapacity {
			// A known actor is never refused; a new one is, and asks again
			// once a job's time has passed.
			label = viewmetrics.ContractRequestQueueFull
			return result, &ContractQueueFullError{RetryAfter: c.retryAfterLocked()}
		}
		a = c.registerLocked(req.Actor, now)
	}
	a.interestUntil, a.lastUsed = now.Add(contractInterestTTL), now
	defer func() {
		if label == viewmetrics.ContractRequestAdmitted || label == viewmetrics.ContractRequestCoalesced {
			switch {
			case req.Inputs == nil:
				label = viewmetrics.ContractRequestBaselineAdmitted
			case !req.Inputs.State.Accepted:
				label = viewmetrics.ContractRequestInputsPending
			}
		}
	}()
	if a.want.seq > 0 && (key == a.want.key || req.Epoch < a.want.epoch) {
		// The actor already wants these inputs, or newer ones captured at a
		// later epoch: the request waits on that promise.
		result.ArrivalSeq = a.want.seq
		if key == a.want.key && a.state == contractActorIdle && (a.interestExpired || a.resolved >= a.want.seq && a.resolvedResult == ContractActorInputsStale) {
			// Renew work whose interest expired, or whose promise resolved
			// stale, using the inputs and source this request captured.
			dropped, a.want.source, kept = a.want.source, req.Source, true
			a.want.inputs = req.Inputs
			a.want.repo, a.want.checkout = req.Key.RepoPrefix, req.Key.CheckoutID
			a.interestExpired = false
			a.seq++
			a.want.seq, a.want.epoch, a.retryOnInputs = a.seq, max(a.want.epoch, req.Epoch), false
			result.ArrivalSeq = a.seq
			c.enqueueLocked(a, contractJobTriggerInterest, now)
			label, result.Result = viewmetrics.ContractRequestAdmitted, ContractScheduled
			return result, nil
		}
		label, result.Result = viewmetrics.ContractRequestCoalesced, ContractCoalesced
		switch a.state {
		case contractActorRunning, contractActorCapturing:
			result.Result = ContractRunning
		case contractActorDeferred:
			result.Result = ContractDeferred
		}
		return result, nil
	}
	// New inputs: the newest key wins, and the key it replaces drops its
	// source.
	dropped = a.want.source
	a.seq++
	a.want = contractActorWant{key: key, seq: a.seq, epoch: req.Epoch, inputs: req.Inputs, source: req.Source, repo: req.Key.RepoPrefix, checkout: req.Key.CheckoutID}
	a.interestExpired = false
	kept = true
	a.requeues, a.backoff, a.retryOnInputs = 0, 0, false
	result.ArrivalSeq = a.seq
	label, result.Result = viewmetrics.ContractRequestAdmitted, ContractScheduled
	switch a.state {
	case contractActorRunning, contractActorCapturing:
		// The job ends first; the newest key reruns once.
		result.Result = ContractRunning
		a.notifyLocked()
	default:
		c.enqueueLocked(a, contractJobTriggerInterest, now)
	}
	if req.Inputs == nil || !req.Inputs.State.Accepted {
		result.Result = ContractDeferred
	}
	return result, nil
}

// registerLocked adds an actor, evicting the least recently used idle actor
// when the registry is full.
func (c *ContractAnalysisCoordinator) registerLocked(id ContractActor, now time.Time) *contractActor {
	if len(c.actors) >= contractSchedulerMaxActors {
		var victim *contractActor
		for _, candidate := range c.actors {
			if candidate.hasWork() {
				continue
			}
			if victim == nil || candidate.lastUsed.Before(victim.lastUsed) {
				victim = candidate
			}
		}
		if victim != nil {
			c.deltaCaches.put(victim.id, nil)
			c.deltaRegistryEvictions++
			delete(c.actors, victim.id)
			victim.ring.actors = slices.DeleteFunc(victim.ring.actors, func(a *contractActor) bool { return a == victim })
			victim.notifyLocked()
		}
	}
	repos := id.Repos()
	ring := c.rings[repos[0]]
	if ring == nil {
		ring = &contractRing{repo: repos[0], wake: make(chan struct{}, 1)}
		c.rings[repos[0]] = ring
		c.workers.Add(1)
		go c.dispatch(ring)
	}
	a := &contractActor{id: id, repos: repos, ring: ring, lastUsed: now, changed: make(chan struct{})}
	c.actors[id] = a
	ring.actors = append(ring.actors, a)
	return a
}

// activeLocked counts actors with pending or running work.
func (c *ContractAnalysisCoordinator) activeLocked() int {
	active := 0
	for _, a := range c.actors {
		if a.hasWork() {
			active++
		}
	}
	return active
}

// retryAfterLocked is when a refused request should ask again: the mean job
// time, within 1..30 s.
func (c *ContractAnalysisCoordinator) retryAfterLocked() time.Duration {
	retry := c.meanJob
	if retry <= 0 {
		retry = 5 * time.Second
	}
	return min(max(retry, time.Second), contractBackoffMax)
}

// setStateLocked moves an actor to state, stepping the capacity epoch when it
// leaves pending or running work.
func (c *ContractAnalysisCoordinator) setStateLocked(a *contractActor, state contractActorState) {
	had := a.hasWork()
	a.state = state
	if state != contractActorDeferred {
		a.deferred = ""
	}
	if state != contractActorBackoff {
		a.nextAttempt = time.Time{}
	}
	if state != contractActorIdle {
		a.repairWait = contractRepairWaitNone
	}
	if had && !a.hasWork() {
		c.capacityEpoch++
		close(c.capacityChanged)
		c.capacityChanged = make(chan struct{})
	}
	if a.dirty && state != contractActorRunning && state != contractActorCapturing {
		// A Published that arrived during the job is captured now.
		a.ring.signal()
	}
	a.notifyLocked()
}

// enqueueLocked queues an actor at the back of its home ring.
func (c *ContractAnalysisCoordinator) enqueueLocked(a *contractActor, trigger string, at time.Time) {
	if trigger == contractJobTriggerPublished && !a.dirtyAt.IsZero() {
		at = a.dirtyAt
	}
	a.trigger, a.triggeredAt = trigger, at
	a.ring.queue = slices.DeleteFunc(a.ring.queue, func(q *contractActor) bool { return q == a })
	a.ring.queue = append(a.ring.queue, a)
	c.setStateLocked(a, contractActorQueued)
	a.ring.signal()
}

// expireLocked returns actors whose interest ran out while they waited on
// inputs or a timer to idle: nothing starts for them any more.
func (c *ContractAnalysisCoordinator) expireLocked(now time.Time) {
	for _, a := range c.actors {
		if now.Before(a.interestUntil) {
			continue
		}
		switch a.state {
		case contractActorDeferred, contractActorBackoff:
			a.interestExpired = true
			a.want.source.Release()
			a.want.source = nil
			a.retryOnInputs, a.dirty = false, false
			c.setStateLocked(a, contractActorIdle)
		case contractActorIdle:
			// A stale primary capture can wait here with an unresolved
			// promise. Preserve its suspended intent before dropping the
			// Published retry; a fresh RPC may renew it after the TTL.
			if a.retryOnInputs {
				a.interestExpired = true
			}
			// Interest expiry stops scheduling; cache eviction has its own
			// independent global memory budget.
			a.retryOnInputs, a.dirty = false, false
		}
	}
}

// Status is an actor's current state.
func (c *ContractAnalysisCoordinator) Status(actor ContractActor) ContractActorStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.actors[actor]
	if a == nil {
		return ContractActorStatus{State: contractActorIdle.String()}
	}
	return ContractActorStatus{Result: a.resolvedResult, Seq: a.resolved, State: a.state.String(), FillCheckpoint: a.fillCheckpoint, Stalled: a.stalled.cycles > 0, RetryAfter: c.retryAfterLocked()}
}

// WaitActor waits until the actor's promise for seq resolves: a job published
// its inputs or newer ones, they were found published, newer inputs replaced
// them, its job failed or stalled, or its source went stale. It returns the
// context's error when the context ends first.
func (c *ContractAnalysisCoordinator) WaitActor(ctx context.Context, actor ContractActor, seq uint64) (ContractActorStatus, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return ContractActorStatus{}, ErrContractSchedulerClosed
		}
		a := c.actors[actor]
		if a == nil {
			c.mu.Unlock()
			// An evicted actor had no work: ask again.
			return ContractActorStatus{Result: ContractActorKeyMoved, Seq: seq}, nil
		}
		status := ContractActorStatus{Seq: seq, State: a.state.String(), FillCheckpoint: a.fillCheckpoint, Stalled: a.stalled.cycles > 0, RetryAfter: c.retryAfterLocked()}
		switch {
		case a.lastPublished.seq >= seq:
			status.Result = ContractActorPublished
		case a.resolved >= seq:
			status.Result = a.resolvedResult
		case a.want.seq > seq:
			status.Result = ContractActorKeyMoved
		}
		changed := a.changed
		c.mu.Unlock()
		if status.Result != ContractActorPending {
			return status, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return status, ctx.Err()
		case <-c.ctx.Done():
			return status, ErrContractSchedulerClosed
		}
	}
}

// CapacityEpoch counts the times an actor left pending or running work.
func (c *ContractAnalysisCoordinator) CapacityEpoch() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capacityEpoch
}

// WaitCapacity waits until the capacity epoch passes since.
func (c *ContractAnalysisCoordinator) WaitCapacity(ctx context.Context, since uint64) error {
	for {
		c.mu.Lock()
		epoch, changed, closed := c.capacityEpoch, c.capacityChanged, c.closed
		c.mu.Unlock()
		if closed {
			return ErrContractSchedulerClosed
		}
		if epoch > since {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-c.ctx.Done():
			return ErrContractSchedulerClosed
		}
	}
}

// InputEpoch counts accepted core mutations announced to the scheduler. A
// request samples it before capturing its inputs.
func (c *ContractAnalysisCoordinator) InputEpoch() uint64 { return c.publishedEpoch() }

// WaitInputs waits until the input epoch passes since.
func (c *ContractAnalysisCoordinator) WaitInputs(ctx context.Context, since uint64) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(24 * time.Hour)
	}
	if c.waitPublished(ctx, since, deadline) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.ctx.Err() != nil {
		return ErrContractSchedulerClosed
	}
	return context.DeadlineExceeded
}

// markPublishedLocked marks the actors a Published of repo concerns: a
// primary actor captures its inputs again; a handoff-sourced actor waiting
// on inputs resolves its promise as stale, so its next request brings a
// source at the new inputs. An actor whose interest ran out is left alone.
func (c *ContractAnalysisCoordinator) markPublishedLocked(repo string, now time.Time) {
	for _, a := range c.actors {
		if a.inCohort(repo) {
			c.markActorLocked(a, now)
		}
	}
}

func (c *ContractAnalysisCoordinator) markActorLocked(a *contractActor, now time.Time) {
	if !now.Before(a.interestUntil) {
		return
	}
	if a.primary() && c.options.Inputs != nil {
		if !a.dirty {
			a.dirtyAt = now
		}
		a.dirty, a.dirtyEpoch = true, c.published
		a.ring.signal()
		return
	}
	switch {
	case a.state == contractActorDeferred, a.state == contractActorBackoff, a.state == contractActorIdle && a.retryOnInputs:
		if !a.primary() && c.options.LinkedInputs != nil {
			a.retryOnInputs = false
			c.enqueueLocked(a, contractJobTriggerPublished, now)
			return
		}
		a.retryOnInputs = false
		a.resolveLocked(a.want.seq, ContractActorInputsStale)
		c.setStateLocked(a, contractActorIdle)
	}
}

// catchUpLocked marks an actor for the Published events that arrived after
// its wanted inputs were captured and before it waited on them.
func (c *ContractAnalysisCoordinator) catchUpLocked(a *contractActor, want contractActorWant) {
	if c.published > want.epoch {
		c.markActorLocked(a, c.now())
	}
}

// dispatch is one ring's dispatcher.
func (c *ContractAnalysisCoordinator) dispatch(ring *contractRing) {
	defer c.workers.Done()
	for c.dispatchStep(ring) {
	}
}

// dispatchStep runs one step of a ring's dispatcher: a due timer, one
// recapture, or one queued actor; else it waits for work. It reports false
// once the coordinator closed. A panic is logged and the dispatcher goes on.
func (c *ContractAnalysisCoordinator) dispatchStep(ring *contractRing) (open bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			c.options.Logger.Error("contract analysis dispatcher recovered", zap.String("repo", ring.repo), zap.Any("panic", recovered))
			open = c.ctx.Err() == nil
		}
	}()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	now := c.now()
	c.expireLocked(now)
	wait := time.Duration(-1)
	for _, a := range ring.actors {
		if a.state != contractActorBackoff || a.nextAttempt.IsZero() {
			continue
		}
		if !now.Before(a.nextAttempt) {
			c.enqueueLocked(a, contractJobTriggerTimer, now)
		} else if d := a.nextAttempt.Sub(now); wait < 0 || d < wait {
			wait = d
		}
	}
	for _, a := range ring.actors {
		if a.dirty && a.state != contractActorRunning && a.state != contractActorCapturing {
			epoch, repos, repo := c.published, a.repos, a.want.repo
			if repo == "" {
				repo = repos[0]
			}
			c.mu.Unlock()
			c.recapture(a, epoch, repos, repo)
			return true
		}
	}
	var next *contractActor
	for len(ring.queue) > 0 && next == nil {
		if candidate := ring.queue[0]; candidate.state == contractActorQueued {
			next = candidate
		}
		ring.queue = ring.queue[1:]
	}
	if next == nil {
		wake := ring.wake
		c.mu.Unlock()
		var timeout <-chan time.Time
		if wait >= 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			timeout = timer.C
		}
		select {
		case <-wake:
		case <-timeout:
		case <-c.ctx.Done():
			return false
		}
		return true
	}
	want := next.want
	next.want.source = nil
	next.running.contractActorKeySeq = contractActorKeySeq{want.key, want.seq}
	trigger, triggeredAt := next.trigger, next.triggeredAt
	c.setStateLocked(next, contractActorCapturing)
	c.mu.Unlock()
	c.startActor(next, want, trigger, triggeredAt)
	return true
}

// recapture captures a primary actor's inputs again after a Published and
// queues it when they moved past its wanted key.
func (c *ContractAnalysisCoordinator) recapture(a *contractActor, epoch uint64, repos []string, repo string) {
	inputs, err := c.options.Inputs(c.ctx, repos, repo)
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.dirtyEpoch <= epoch {
		a.dirty = false
	}
	if err != nil || inputs == nil || c.closed || a.state == contractActorRunning || a.state == contractActorCapturing {
		if err != nil && graphview.CodeOf(err) != graphview.CodeRequiredCapabilityIncomplete && c.ctx.Err() == nil {
			c.options.Logger.Debug("contract analysis recapture refused", zap.String("actor", a.id.String()), zap.Error(err))
		}
		return
	}
	now := c.now()
	key := contractInputsKey(inputs)
	if key == a.want.key || epoch < a.want.epoch {
		if key == a.want.key && a.retryOnInputs && (a.state == contractActorIdle || a.state == contractActorBackoff) {
			// The inputs are as wanted, captured again at this epoch: a
			// refusal of them waits for the next Published.
			a.retryOnInputs = false
			a.want.epoch = max(a.want.epoch, epoch)
			c.enqueueLocked(a, contractJobTriggerPublished, now)
		}
		return
	}
	// A Published-driven capture does not extend the actor's interest.
	a.want.source.Release()
	a.seq++
	a.want = contractActorWant{key: key, seq: a.seq, epoch: epoch, inputs: inputs, repo: repo}
	a.requeues, a.backoff, a.retryOnInputs = 0, 0, false
	if key == a.lastPublished.key {
		a.lastPublished.seq = a.seq
		c.setStateLocked(a, contractActorIdle)
		return
	}
	c.enqueueLocked(a, contractJobTriggerPublished, now)
}

// startActor takes a picked actor from capturing to a job, on the dispatcher:
// a request for a baseline admits it, inputs not accepted yet defer, inputs
// already published are a noop. Only a job takes the job slot.
func (c *ContractAnalysisCoordinator) startActor(a *contractActor, want contractActorWant, trigger string, triggeredAt time.Time) {
	owned := true
	started := time.Now()
	defer func() {
		recovered := recover()
		if owned {
			want.source.Release()
		}
		if recovered != nil {
			elapsed := time.Since(started)
			countContractJob(viewmetrics.ContractModeCold, viewmetrics.ContractJobPanic, elapsed)
			c.options.Logger.Error("contract analysis admission panicked", zap.String("actor", a.id.String()), zap.Any("panic", recovered))
			// No job slot was acquired: resolve the actor and arrange its
			// retry here, leaving slot ownership to the job goroutine.
			c.endJob(a, want, viewmetrics.ContractJobPanic, elapsed)
		}
	}()
	reacquire := !a.primary() && want.source == nil && c.options.LinkedInputs != nil
	if !reacquire {
		switch {
		case !a.primary() && want.source == nil:
			// A requeued handoff actor has no source left to capture.
			c.endCapture(a, want, ContractActorInputsStale)
			return
		case want.inputs == nil:
			c.park(a, want, c.admitBaselines(want, []string{want.repo}))
			return
		case !want.inputs.State.Accepted:
			if err := want.inputs.Validate(c.ctx); err != nil {
				c.endInputs(a, want, err)
				return
			}
			baseline := c.admitPendingBaselines(want)
			if !baseline {
				// A repair may have accepted the inputs between the validation
				// and the admission without announcing it: capture them again
				// rather than wait on a producer that is gone.
				if err := want.inputs.Validate(c.ctx); err != nil {
					c.endInputs(a, want, err)
					return
				}
			}
			c.park(a, want, baseline)
			return
		}
		if c.attached(want) {
			c.endNoop(a, want)
			return
		}
	}
	if err := c.slots.acquire(c.ctx); err != nil {
		c.endCapture(a, want, ContractActorPending)
		return
	}
	owned = false
	c.workers.Add(1)
	go c.runActor(a, want, trigger, triggeredAt)
}

// admitBaselines asks for the baselines of namespaces and reports whether
// one was admitted.
func (c *ContractAnalysisCoordinator) admitBaselines(want contractActorWant, namespaces []string) bool {
	if c.options.ReconcileBaseline == nil {
		return false
	}
	admitted := false
	for _, namespace := range namespaces {
		started, err := c.options.ReconcileBaseline(c.ctx, nil, namespace, "")
		if err != nil {
			if c.ctx.Err() == nil {
				c.options.Logger.Warn("contract baseline admission refused", zap.String("repo", namespace), zap.Error(err))
			}
			continue
		}
		admitted = admitted || started
	}
	return admitted
}

// admitPendingBaselines asks for the baseline of every pending primary
// namespace of the wanted inputs. Immutable positive inputs are their core
// producer's to accept.
func (c *ContractAnalysisCoordinator) admitPendingBaselines(want contractActorWant) bool {
	pending := make(map[string]bool)
	for _, witness := range want.inputs.Witnesses {
		if witness.Found && !witness.State.Accepted && witness.GenerationID == 0 && witness.State.CheckoutID == "" {
			pending[witness.State.RepoPrefix] = true
		}
	}
	namespaces := make([]string, 0, len(pending))
	for namespace := range pending {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)
	return c.admitBaselines(want, namespaces)
}

// attached reports whether every cohort attachment of the wanted inputs
// exists already.
func (c *ContractAnalysisCoordinator) attached(want contractActorWant) bool {
	var receiver interface {
		GetContractAttachmentContext(context.Context, graph.ContractAttachmentKey) (*graph.ContractAttachment, error)
	} = c.options.Store.AtGeneration(0)
	if want.source != nil {
		if len(want.source.sources) == 0 {
			return false
		}
		receiver = want.source.sources[len(want.source.sources)-1].Handle
	}
	names := make(map[string]bool)
	for _, witness := range want.inputs.Witnesses {
		names[witness.State.RepoPrefix] = true
	}
	for name := range names {
		state, err := graph.ComposeContractInputState(name, want.checkout, want.inputs.Witnesses)
		if err != nil || !state.Accepted {
			return false
		}
		header, err := receiver.GetContractAttachmentContext(c.ctx, graph.ContractAttachmentKey{RepoPrefix: name, CheckoutID: want.checkout, InputVersion: state.InputVersion, InputFingerprint: state.InputFingerprint})
		if err != nil || header == nil {
			return false
		}
	}
	return len(names) > 0
}

// captureRefusal classifies a capture error.
func (c *ContractAnalysisCoordinator) captureRefusal(err error) ContractActorResult {
	if errors.Is(err, graph.ErrContractProjectionStale) {
		return ContractActorInputsStale
	}
	return ContractActorFailed
}

// park defers an actor whose inputs are not accepted: it waits on the
// baseline admitted for them, or on their acceptance.
func (c *ContractAnalysisCoordinator) park(a *contractActor, want contractActorWant, baseline bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.running.seq == want.seq {
		a.running.contractActorKeySeq = contractActorKeySeq{}
		a.running.cancel = nil
	}
	if c.supersededLocked(a, want) {
		return
	}
	reason := contractDeferredInputsAccepting
	if baseline {
		reason = contractDeferredBaseline
	}
	c.setStateLocked(a, contractActorDeferred)
	a.deferred, a.deferredAt = reason, c.now()
	c.catchUpLocked(a, want)
}

// supersededLocked requeues an actor whose wanted inputs moved while it was
// captured, reporting whether it did.
func (c *ContractAnalysisCoordinator) supersededLocked(a *contractActor, want contractActorWant) bool {
	if c.closed {
		c.setStateLocked(a, contractActorIdle)
		return true
	}
	if a.want.seq > want.seq {
		c.enqueueLocked(a, contractJobTriggerInterest, c.now())
		return true
	}
	return false
}

// endNoop resolves an actor whose wanted inputs were published already.
func (c *ContractAnalysisCoordinator) endNoop(a *contractActor, want contractActorWant) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.running.seq == want.seq {
		a.running.contractActorKeySeq = contractActorKeySeq{}
		a.running.cancel = nil
	}
	a.lastPublished = contractActorKeySeq{want.key, want.seq}
	a.resolveLocked(want.seq, ContractActorNoop)
	if c.supersededLocked(a, want) {
		return
	}
	c.setStateLocked(a, contractActorIdle)
}

// endCapture ends a capture that started no job: a stale capture waits for
// the next input epoch (a handoff actor's promise resolves stale, so its next
// request brings a new source); a failed one is resolved and reruns only on
// a key change; a canceled one is dropped.
func (c *ContractAnalysisCoordinator) endCapture(a *contractActor, want contractActorWant, result ContractActorResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.running.seq == want.seq {
		a.running.contractActorKeySeq = contractActorKeySeq{}
		a.running.cancel = nil
	}
	if c.supersededLocked(a, want) {
		return
	}
	switch result {
	case ContractActorInputsStale:
		a.retryOnInputs = true
		if !a.primary() || c.options.Inputs == nil {
			a.resolveLocked(want.seq, ContractActorInputsStale)
		}
	case ContractActorFailed:
		a.failedKey = want.key
		c.endFailureLocked(a, want, ContractActorFailed, viewmetrics.ContractJobFailed)
	}
	if a.state == contractActorCapturing {
		c.setStateLocked(a, contractActorIdle)
	}
	if result == ContractActorInputsStale {
		c.catchUpLocked(a, want)
	}
}

// endInputs ends a pick whose wanted inputs no longer validate. Moved inputs
// are captured again at once (a primary actor recaptures, a handoff actor's
// promise resolves stale); any other refusal ends as a failed capture.
func (c *ContractAnalysisCoordinator) endInputs(a *contractActor, want contractActorWant, err error) {
	if !errors.Is(err, graph.ErrContractProjectionStale) {
		if c.ctx.Err() != nil {
			c.endCapture(a, want, ContractActorPending)
			return
		}
		c.endCapture(a, want, ContractActorFailed)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.running.seq == want.seq {
		a.running.contractActorKeySeq = contractActorKeySeq{}
		a.running.cancel = nil
	}
	if c.supersededLocked(a, want) {
		return
	}
	c.setStateLocked(a, contractActorIdle)
	if a.primary() && c.options.Inputs != nil {
		if !a.dirty {
			a.dirtyAt = c.now()
		}
		a.dirty, a.dirtyEpoch = true, c.published
		a.ring.signal()
		return
	}
	if !a.primary() && c.options.LinkedInputs != nil {
		c.enqueueLocked(a, contractJobTriggerPublished, c.now())
		return
	}
	a.resolveLocked(want.seq, ContractActorInputsStale)
}

// runActor captures the wanted inputs and runs their job, holding the job
// slot. A panic in either is recovered: the snapshot is released once, the
// reservations are retired, the waiters are told and the actor stays
// schedulable.
func (c *ContractAnalysisCoordinator) runActor(a *contractActor, want contractActorWant, trigger string, triggeredAt time.Time) {
	defer c.workers.Done()
	defer c.slots.release()
	started := time.Now()
	source := want.source
	defer func() { source.Release() }()
	var release func()
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		source.Release()
		if release != nil {
			release()
		}
		countContractJob(viewmetrics.ContractModeCold, viewmetrics.ContractJobPanic, time.Since(started))
		c.options.Logger.Error("contract analysis job panicked", zap.String("actor", a.id.String()), zap.Any("panic", recovered))
		c.endJob(a, want, viewmetrics.ContractJobPanic, time.Since(started))
	}()
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	c.mu.Lock()
	a.running.cancel = cancel
	c.mu.Unlock()
	captureStart := time.Now()
	if !a.primary() && source == nil {
		c.mu.Lock()
		captureEpoch := c.published
		c.mu.Unlock()
		inputs, fresh, err := c.options.LinkedInputs(ctx, a.id, want.repo)
		source = fresh
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && (source == nil || !contractLinkedInputsMatch(a.id, want.repo, inputs)) {
			err = graph.ErrContractInputVector
		}
		if err != nil {
			source.Release()
			result := c.captureRefusal(err)
			if graphview.CodeOf(err) == graphview.CodeViewBuilding || graphview.CodeOf(err) == graphview.CodeRequiredCapabilityIncomplete || graphview.CodeOf(err) == graphview.CodeCapabilityUnavailable {
				result = ContractActorInputsStale
			}
			if ctx.Err() != nil {
				result = ContractActorPending
			}
			c.endCapture(a, want, result)
			return
		}
		c.mu.Lock()
		if c.closed || a.want.seq > want.seq {
			c.mu.Unlock()
			source.Release()
			c.endCapture(a, want, ContractActorPending)
			return
		}
		key := contractInputsKey(inputs)
		if key != want.key {
			a.seq++
			want.key, want.seq = key, a.seq
			a.requeues, a.backoff, a.retryOnInputs = 0, 0, false
		}
		want.inputs, want.epoch = inputs, captureEpoch
		want.source = nil
		a.want = want
		a.running.contractActorKeySeq = contractActorKeySeq{want.key, want.seq}
		a.notifyLocked()
		c.mu.Unlock()
		if !inputs.State.Accepted {
			// Capture owns accepted inputs only. A valid in-flight capture is
			// pending authority, not a failed analysis job or a reusable handle.
			if err := inputs.Validate(ctx); err != nil {
				source.Release()
				c.endInputs(a, want, err)
				return
			}
			baseline := c.admitPendingBaselines(want)
			if !baseline {
				if err := inputs.Validate(ctx); err != nil {
					source.Release()
					c.endInputs(a, want, err)
					return
				}
			}
			source.Release()
			c.park(a, want, baseline)
			return
		}
		freshWant := want
		freshWant.source = source
		if c.attached(freshWant) {
			source.Release()
			c.endNoop(a, want)
			return
		}
	}
	snapshot, targets, err := c.options.Capture(ctx, source, want.inputs, want.repo, want.checkout)
	captureDuration := time.Since(captureStart)
	inner := snapshot.Release
	var once sync.Once
	release = func() {
		once.Do(func() {
			if inner != nil {
				inner()
			}
		})
	}
	snapshot.Release = release
	if err == nil && len(targets) > 0 && (snapshot.Core == nil || inner == nil) {
		err = fmt.Errorf("contract analysis capture: missing owned handoff")
	}
	if err == nil && c.options.RequireRepoConfigs {
		for _, file := range snapshot.Files {
			if _, found := snapshot.RepoConfigs[file.RepoPrefix]; !found {
				err = fmt.Errorf("contract capture: missing accepted configuration for %q", file.RepoPrefix)
				break
			}
		}
	}
	if err == nil {
		seen := make(map[graph.ContractAttachmentKey]bool, len(targets))
		for _, target := range targets {
			if target.Catalog == nil || target.Payload != nil || target.Key.InputVersion == "" || target.Key.InputFingerprint == "" || seen[target.Key] {
				err = fmt.Errorf("contract analysis capture: invalid target")
				break
			}
			seen[target.Key] = true
		}
	}
	var id string
	if err == nil && len(targets) > 0 {
		id, err = contractAnalysisCohortID(snapshot, c.options.Config)
	}
	switch {
	case errors.Is(err, graph.ErrContractProjectionStale):
		countContractCapture(viewmetrics.ContractCaptureStale)
	case err != nil:
		countContractCapture(viewmetrics.ContractCaptureError)
		if ctx.Err() == nil {
			countContractRequest(viewmetrics.ContractRequestError)
			c.options.Logger.Warn("contract analysis capture failed", zap.String("actor", a.id.String()), zap.Error(err))
		}
	default:
		countContractCapture(viewmetrics.ContractCaptureOK)
	}
	if err != nil {
		release()
		source.Release()
		if ctx.Err() != nil {
			c.endCapture(a, want, ContractActorPending)
			return
		}
		c.endCapture(a, want, c.captureRefusal(err))
		return
	}
	if len(targets) == 0 {
		release()
		source.Release()
		c.endNoop(a, want)
		return
	}
	job := &contractAnalysisJob{id: id, snapshot: snapshot, targets: targets, trigger: trigger, queuedAt: triggeredAt, capture: captureDuration}
	c.mu.Lock()
	cached := c.deltaCaches.borrow(a.id)
	canContinueFill := !a.fillNeedsCheckpoint || (cached != nil && cached.continuation != nil)
	if a.fillNeedsCheckpoint && !canContinueFill {
		switch a.fillCheckpoint {
		case ContractFillCheckpointRetained:
			a.fillCheckpoint = ContractFillCheckpointEvicted
		case ContractFillCheckpointRejected:
		default:
			a.fillCheckpoint = ContractFillCheckpointMissing
		}
	}
	retainedBytes := c.deltaCaches.bytes
	cacheComplete, cacheBytes := false, int64(0)
	if cached != nil {
		cacheComplete, cacheBytes = cached.complete, cached.retainedBytes
	}
	registryEvictions := c.deltaRegistryEvictions
	interestExpired := a.interestExpired
	c.mu.Unlock()

	session, sessionErr := newContractDeltaSession(cached, snapshot, c.options.Config)
	if sessionErr != nil {
		c.mu.Lock()
		c.deltaCaches.put(a.id, cached)
		c.mu.Unlock()
		release()
		source.Release()
		c.endCapture(a, want, ContractActorPending)
		return
	}
	job.delta = session
	if (a.primary() || c.options.LinkedInputs != nil) && canContinueFill {
		job.otherActorWaiting = func() bool { return c.otherActorWaiting(a) }
	}
	// Registered after capture succeeds and before the body starts. Returning
	// cache custody precedes panic recovery; a panic discards its unproven state.
	returnedCache := false
	returnCache := func(p *contractDeltaCache) {
		if !returnedCache {
			c.mu.Lock()
			if p != nil && (ctx.Err() != nil) {
				p.continuation = nil
			}
			admission := c.deltaCaches.put(a.id, p)
			if session.continued {
				switch {
				case p == nil || p.continuation == nil:
					a.fillCheckpoint = ContractFillCheckpointMissing
				case admission.reason != "admitted":
					a.fillCheckpoint = ContractFillCheckpointRejected
				default:
					a.fillCheckpoint = ContractFillCheckpointRetained
				}
			}
			c.mu.Unlock()
			returnedCache = true
			c.options.Logger.Info("contract cache admission", zap.String("actor", a.id.String()), zap.String("reason", admission.reason), zap.Int64("requested_bytes", admission.requested), zap.Int64("budget_bytes", admission.budget), zap.Int64("retained_bytes", admission.retained), zap.Int("evicted", admission.evicted), zap.Bool("checkpoint_present", p != nil))
		}
	}
	defer func() {
		recovered := recover()
		returned := session.cache
		if recovered != nil {
			returned = nil
		}
		returnCache(returned)
		if recovered != nil {
			panic(recovered)
		}
	}()

	c.options.Logger.Info("contract cache borrow", zap.String("actor", a.id.String()), zap.Bool("cache_present", cached != nil), zap.Bool("complete", cacheComplete), zap.Int64("checkpoint_bytes", cacheBytes), zap.Int64("retained_bytes", retainedBytes), zap.Uint64("registry_evictions", registryEvictions), zap.Bool("interest_expired", interestExpired))

	c.mu.Lock()
	c.setStateLocked(a, contractActorRunning)
	c.mu.Unlock()
	run := c.execute
	if c.run != nil {
		run = c.run
	}
	body := time.Now()
	outcome := run(job)
	returnCache(session.cache)
	release()
	source.Release()
	c.endJob(a, want, outcome, time.Since(body), job.publication)
}

// otherActorWaiting includes dispatchers already waiting for a global slot.
func (c *ContractAnalysisCoordinator) otherActorWaiting(current *contractActor) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, actor := range c.actors {
		if actor != current && (actor.state == contractActorQueued || actor.state == contractActorCapturing) {
			return true
		}
	}
	return false
}

// endJob moves an actor out of its job by how the job ended.
func (c *ContractAnalysisCoordinator) endJob(a *contractActor, want contractActorWant, outcome string, elapsed time.Duration, publication ...*ContractPublishedCohort) {
	// Job-owned proof is immutable once installed. Clone corpus-sized values
	// before taking the scheduler mutex; Schedule/status never wait on that copy.
	var owned *ContractPublishedCohort
	if outcome == viewmetrics.ContractJobPublished && len(publication) > 0 && contractPublishedCohortComplete(publication[0]) {
		owned = cloneContractPublishedCohort(publication[0])
		owned.Seq = want.seq
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a.running = struct {
		contractActorKeySeq
		cancel context.CancelFunc
	}{}
	if outcome != "" && outcome != viewmetrics.ContractJobPanic {
		if c.meanJob == 0 {
			c.meanJob = elapsed
		} else {
			c.meanJob = (c.meanJob*3 + elapsed) / 4
		}
	}
	now := c.now()
	rerun := a.want.seq > want.seq
	switch outcome {
	case "":
		// Every target was attached when the job looked: no job ran.
		a.lastPublished = contractActorKeySeq{want.key, want.seq}
		a.resolveLocked(want.seq, ContractActorNoop)
		c.setStateLocked(a, contractActorIdle)
	case viewmetrics.ContractJobFillContinued:
		a.fillNeedsCheckpoint = true
		// Cache custody and all job resources returned before this transition.
		// This is scheduling progress, not an attachment result or retry failure.
		c.enqueueLocked(a, contractJobTriggerRequeue, now)
	case viewmetrics.ContractJobPublished:
		a.fillNeedsCheckpoint = false
		a.fillCheckpoint = ContractFillCheckpointNone
		if owned != nil {
			a.lastCohort = owned
		}
		a.lastPublished = contractActorKeySeq{want.key, want.seq}
		a.failures, a.requeues, a.backoff, a.panicked, a.failedKey = 0, 0, 0, false, ""
		a.stalled.cycles, a.stalled.reason = 0, ""
		c.setStateLocked(a, contractActorIdle)
	case viewmetrics.ContractJobSupersededKeyMoved:
		a.failures++
		if !c.stallLocked(a, want.seq, outcome, now) {
			// The key moved: capture the new one and run it now.
			a.dirty = a.primary() && c.options.Inputs != nil
			c.enqueueLocked(a, contractJobTriggerRequeue, now)
		}
	case viewmetrics.ContractJobSupersededInputsMoving:
		a.failures++
		if !rerun && !c.stallLocked(a, want.seq, outcome, now) {
			if a.requeues == 0 {
				a.requeues++
				countContractRequeue()
				c.enqueueLocked(a, contractJobTriggerRequeue, now)
			} else {
				a.retryOnInputs = true
				c.setStateLocked(a, contractActorIdle)
			}
		}
	case viewmetrics.ContractJobSupersededReadUnsettled, viewmetrics.ContractJobSupersededPublishRefused:
		a.failures++
		// A repair already due for the cohort is asked for before this end
		// asks for its own scan.
		due := !rerun && a.requeues > 0 && c.repairPending != nil && c.repairPending(a.repos)
		if outcome == viewmetrics.ContractJobSupersededReadUnsettled && c.unsettled != nil {
			// An apply that never completes leaves reads unsettled: the
			// repair looks for rows an earlier process left in flight.
			c.unsettled(a.repos)
		}
		if !rerun && !c.stallLocked(a, want.seq, outcome, now) {
			switch {
			case a.requeues == 0:
				a.requeues++
				countContractRequeue()
				c.enqueueLocked(a, contractJobTriggerRequeue, now)
			case due:
				// A repair of what the job waited on is due: its accept
				// announces itself, and the actor runs again then. A pass
				// that repairs nothing backs the actor off instead.
				a.retryOnInputs = true
				c.setStateLocked(a, contractActorIdle)
				a.repairWait = contractRepairWaitAsked
			default:
				c.backoffLocked(a, now)
			}
		}
	case viewmetrics.ContractJobPreempted:
		// Close canceled the job.
		c.setStateLocked(a, contractActorIdle)
	case viewmetrics.ContractJobPanic:
		if a.panicked {
			c.endFailureLocked(a, want, ContractActorFailed, outcome)
			c.setStateLocked(a, contractActorIdle)
		} else {
			a.panicked = true
			a.failures++
			a.resolveLocked(want.seq, ContractActorPanic)
			c.setStateLocked(a, contractActorBackoff)
			a.nextAttempt = now.Add(contractPanicRetry)
			a.ring.signal()
		}
	default:
		c.endFailureLocked(a, want, ContractActorFailed, outcome)
		c.setStateLocked(a, contractActorIdle)
	}
	if rerun && a.state != contractActorQueued {
		c.enqueueLocked(a, contractJobTriggerInterest, now)
	}
	a.notifyLocked()
}

// endFailureLocked resolves a failed job: no timer reruns it, a key change
// does.
func (c *ContractAnalysisCoordinator) endFailureLocked(a *contractActor, want contractActorWant, result ContractActorResult, outcome string) {
	a.failures++
	a.failedKey = want.key
	a.resolveLocked(want.seq, result)
	c.stallLocked(a, want.seq, outcome, c.now())
}

// stallLocked flags an actor whose consecutive jobs ended without publishing
// contractStallEnds times, resolving the promise of the job that ended at
// seq; it schedules again on a key change or a Published. A newer seq asked
// for while the job ran is not resolved: its job has not run yet. It reports
// whether it stalled the actor.
func (c *ContractAnalysisCoordinator) stallLocked(a *contractActor, seq uint64, reason string, now time.Time) bool {
	if a.failures < contractStallEnds {
		return false
	}
	if a.stalled.cycles == 0 {
		a.stalled.since = now
	}
	a.stalled.reason = reason
	a.stalled.cycles++
	a.resolveLocked(seq, ContractActorStalled)
	// A failed job reruns only on a key change; the others again on the
	// next Published of the cohort.
	a.retryOnInputs = reason != viewmetrics.ContractJobFailed && reason != viewmetrics.ContractJobPanic
	if a.state == contractActorRunning || a.state == contractActorCapturing || a.state == contractActorQueued {
		c.setStateLocked(a, contractActorIdle)
	}
	c.options.Logger.Info("contract analysis actor stalled", zap.String("actor", a.id.String()), zap.String("reason", reason), zap.Int("ends", a.failures))
	return true
}

// backoffLocked retries an actor on the 1, 2, 4 … 30 s timer, or at the next
// cohort Published, whichever comes first.
func (c *ContractAnalysisCoordinator) backoffLocked(a *contractActor, now time.Time) {
	a.retryOnInputs = true
	c.setStateLocked(a, contractActorBackoff)
	a.nextAttempt = now.Add(min(contractBackoffBase<<a.backoff, contractBackoffMax))
	a.backoff++
	a.ring.signal()
}

// attachRepair wires a stuck-input repair to the scheduler: an unsettled
// job's actor waits for a due repair instead of a timer, and an unsettled
// end asks the repair to scan the cohort's unaccepted rows.
func (c *ContractAnalysisCoordinator) attachRepair(r *ContractStuckRepair) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repairPending = r.Pending
	c.unsettled = r.Scan
}

// repairPassStarted marks the actors of repo's cohort waiting on a repair
// pass as seen by the pass starting now.
func (c *ContractAnalysisCoordinator) repairPassStarted(repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.actors {
		if a.repairWait == contractRepairWaitAsked && a.inCohort(repo) {
			a.repairWait = contractRepairWaitSeen
		}
	}
}

// repairPassEnded backs off the actors of repo's cohort a pass saw waiting
// on it and that are waiting still: a pass that repaired announced a
// Published, which queued them, so one still waiting was waiting on a pass
// that found nothing to repair, or found the lane busy, and announces
// nothing.
func (c *ContractAnalysisCoordinator) repairPassEnded(repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, a := range c.actors {
		if a.repairWait != contractRepairWaitSeen || !a.inCohort(repo) {
			continue
		}
		a.repairWait = contractRepairWaitNone
		if a.state == contractActorIdle && a.retryOnInputs {
			c.backoffLocked(a, now)
		}
	}
}

// deferredOnInputsSince is when the earliest actor of repo's cohort began
// waiting on inputs not yet accepted with a current baseline, zero when
// none waits.
func (c *ContractAnalysisCoordinator) deferredOnInputsSince(repo string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var since time.Time
	for _, a := range c.actors {
		if a.state != contractActorDeferred || a.deferred != contractDeferredInputsAccepting || !a.inCohort(repo) {
			continue
		}
		if since.IsZero() || a.deferredAt.Before(since) {
			since = a.deferredAt
		}
	}
	return since
}
