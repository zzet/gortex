package indexer

import (
	"path/filepath"
	"sync/atomic"
	"time"
	"weak"
)

// mutationSetPublicationMember is callback-local admission metadata. It neither
// creates a publication record nor grants an older waiter the span of a newer
// executor. The frontier remains the sole graph-completion authority.
type mutationSetPublicationMember struct {
 invocation *sharedSetInvocation
	path               string
	generation         uint64
	record             *PublicationPhaseRecord
	schedulingEntered  time.Time
	backgroundEligible bool
}

// mutationSetPublicationMembers selects only the original exact serving
// generation. Revalidate at attachment: this snapshot does not reserve a record
// against supersession, eviction, cancellation, or a later physical owner.
func (w *Watcher) mutationSetPublicationMembers(frontier map[string]uint64) []mutationSetPublicationMember {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopping {
		return nil
	}
	var members []mutationSetPublicationMember
	for path, generation := range frontier {
		if generation == 0 || w.pendingGeneration[path] != generation || w.observedMutationGeneration[path] != generation {
			continue
		}
		waiter := w.mutationWaiters[path][generation]
		record := waiter.publication
		if record == nil || waiter.ticket == nil || waiter.ticket.PublicationRecord != record || waiter.ticket.Generation != generation || waiter.ticket.Path != path {
			continue
		}
		if record.ownsMutationSetGeneration(generation) {
			members = append(members, mutationSetPublicationMember{invocation:waiter.sharedInvocation,path: path, generation: generation, record: record, schedulingEntered: waiter.schedulingEntered, backgroundEligible: waiter.backgroundEligible})
		}
	}
	return members
}

// Match the existing recorder's live membership and exact ticket. A retained
// pointer alone cannot revive an evicted or terminal physical publication.
func (p *PublicationPhaseRecord) ownsMutationSetGeneration(generation uint64) bool {
	owner := p.owner.Value()
	if owner == nil {
		return false
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	return owner.byKey[p.key] == p && p.canonicalBound && !p.terminal && p.ticket == generation
}

// The shared executor owns one accumulator, not one fabricated executor per
// member. No member record or unrelated request clock supplies its origin.
type mutationSetPublicationAttempt struct {
 sharedInvocation *sharedSetInvocation
 proofFailure string
 executionReturned time.Time
 sharedWindow canonicalAttemptWindow
	members       []mutationSetPublicationMember
	stamps        *PublicationStamps
	entered       time.Time
	retry         int
	executorFiles int
}

type sharedMutationSetExecution struct {
 ownerProof *sharedOwnerFrozen
 ownerProofState string
 ownerProofReason string
	generation    uint64
	members       int
	executorFiles int
	window        canonicalAttemptWindow
	background    *SharedSetBackground
}

func (w *Watcher) beginMutationSetPublication(frontier map[string]uint64, entered time.Time, retry int) *mutationSetPublicationAttempt {
	members := w.mutationSetPublicationMembers(frontier)
	if len(members) == 0 {
		return nil
	}
	stamps := &PublicationStamps{patchLaps: &canonicalPatchLaps{origin: entered}}
	stamps.markAt(PublicationCycleStarted, entered)
	stamps.startAttemptControl(entered)
	a := &mutationSetPublicationAttempt{members: members, stamps: stamps, entered: entered, retry: retry, executorFiles: len(frontier)}
 w.prepareSharedOwnerAttempt(a)
 return a
}

// Reject failed, stale, stopped and evicted members without changing graph
// completion semantics. Older receipts may still be graph-covered by this
// generation; that does not give them this executor's observation.
func (w *Watcher) finishMutationSetPublication(attempt *mutationSetPublicationAttempt, result *IndexResult, err error, returned time.Time) {
	if attempt == nil || err != nil || result == nil || returned.Before(attempt.entered) {
		return
	}
	failed := make(map[string]bool, len(result.FailedFiles))
	for _, path := range result.FailedFiles {
		failed[filepath.Clean(path)] = true
	}
	attempt.stamps.mu.Lock()
	attempt.stamps.patchLaps.finishAttemptControlLocked(returned)
	accounting := attempt.stamps.freezeAttemptEvidence(attempt.entered)
	attempt.stamps.mu.Unlock()
	window := canonicalAttemptWindow{endNS: returned.Sub(attempt.entered).Nanoseconds(), retry: attempt.retry, accounting: accounting}
	attempt.executionReturned=returned
 attempt.sharedWindow=window
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopping {
		return
	}
	for _, member := range attempt.members {
		if failed[filepath.Clean(member.path)] || w.pendingGeneration[member.path] != member.generation || w.observedMutationGeneration[member.path] != member.generation {
			continue
		}
		waiter := w.mutationWaiters[member.path][member.generation]
		if waiter.publication != member.record || waiter.ticket == nil || waiter.ticket.PublicationRecord != member.record || waiter.ticket.Generation != member.generation || waiter.ticket.Path != member.path {
			continue
		}
		member.record.acceptSharedMutationSetExecution(member.generation, len(attempt.members), attempt.executorFiles, window)
	}
}

func (p *PublicationPhaseRecord) acceptSharedMutationSetExecution(generation uint64, members, executorFiles int, window canonicalAttemptWindow) {
	owner := p.owner.Value()
	if owner == nil {
		return
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if owner.byKey[p.key] != p || !p.canonicalBound || p.terminal || p.ticket != generation || p.sharedSetExecution != nil {
		return
	}
	p.sharedSetExecution = &sharedMutationSetExecution{generation: generation, members: members, executorFiles: executorFiles, window: window}
}

// SharedSetExecution describes one actual multi-file executor. Its timings are
// explicitly nonadditive across members; it does not populate independent P/B
// or claim complete scheduling/background coverage.
type SharedSetExecution struct {
	Scope                    string               `json:"scope"`
	NonadditiveAcrossMembers bool                 `json:"nonadditive_across_members"`
	MemberGeneration         uint64               `json:"member_generation"`
	ObservedMembers          int                  `json:"observed_members"`
	ExecutorFiles            int                  `json:"executor_files"`
	Background               *SharedSetBackground `json:"background,omitempty"`
	Accounting               *EditPatchAccounting `json:"accounting"`
}

func (s PublicationPhaseSnapshot) SharedSetExecution() *SharedSetExecution {
	shared := s.sharedSetExecution
	if shared == nil {
		return nil
	}
	var background *SharedSetBackground
	if shared.background != nil {
		copy := *shared.background
		background = &copy
	}
	accounting := shared.window.projectAccounting()
	accounting.Scope = "shared_mutation_set_executor"
	accounting.AttemptTicket = 0 // a set executor is not a member's point attempt
	return &SharedSetExecution{Scope: "shared_mutation_set_executor", NonadditiveAcrossMembers: true, MemberGeneration: shared.generation, ObservedMembers: shared.members, ExecutorFiles: shared.executorFiles, Background: background, Accounting: accounting}
}

// One observation belongs to the existing live timer closure. No record or
// timer history is retained. armFinished is read only after stormMu admission,
// synchronizing with the actual arm caller's unlock; invalidation is atomic
// because a later failed Stop can occur while the callback is executing.
type mutationSetTimerObservation struct {
 invocation *sharedSetInvocation
	watcher                 weak.Pointer[Watcher]
	armStarted, armFinished time.Time
	frontier                uint64
	invalid                 atomic.Bool
}

// SharedSetBackground is the exact original member's scheduling-to-return
// interval. The timer and callback executor are shared, so these costs cannot
// be added across members or substituted for independent point P/B.
type SharedSetBackground struct {
	Scope                           string  `json:"scope"`
	NonadditiveAcrossMembers        bool    `json:"nonadditive_across_members"`
	CallbackIncludesSharedExecution bool    `json:"callback_includes_shared_execution"`
	RequestedWallIncludesCallback   bool    `json:"requested_wall_includes_callback"`
	RequestedWallMS                 float64 `json:"requested_wall_ms"`
	SchedulingMS                    float64 `json:"scheduling_ms"`
	TimerArmMS                      float64 `json:"timer_arm_ms"`
	QuietMS                         float64 `json:"quiet_ms"`
	CallbackMS                      float64 `json:"callback_ms"`
}

func (w *Watcher) finishMutationSetBackground(attempt *mutationSetPublicationAttempt, timer *mutationSetTimerObservation, returned time.Time) {
 if w.sharedOwnerLegacyFinalizing!=nil {w.sharedOwnerLegacyFinalizing(returned)}
	if attempt == nil || timer == nil || timer.watcher.Value() != w || timer.invalid.Load() || attempt.retry != 0 || timer.armStarted.IsZero() || timer.armFinished.Before(timer.armStarted) || attempt.entered.Before(timer.armFinished) || returned.Before(attempt.entered) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopping {
		return
	}
	for _, member := range attempt.members {
		if !member.backgroundEligible || member.schedulingEntered.IsZero() || timer.armStarted.Before(member.schedulingEntered) || member.generation > timer.frontier || w.pendingGeneration[member.path] != 0 {
			continue
		}
		background := &SharedSetBackground{Scope: "original_member_shared_set_background", NonadditiveAcrossMembers: true, CallbackIncludesSharedExecution: true, RequestedWallIncludesCallback: true,
			RequestedWallMS: float64(returned.Sub(member.schedulingEntered)) / float64(time.Millisecond),
			SchedulingMS:    float64(timer.armStarted.Sub(member.schedulingEntered)) / float64(time.Millisecond),
			TimerArmMS:      float64(timer.armFinished.Sub(timer.armStarted)) / float64(time.Millisecond),
			QuietMS:         float64(attempt.entered.Sub(timer.armFinished)) / float64(time.Millisecond),
			CallbackMS:      float64(returned.Sub(attempt.entered)) / float64(time.Millisecond)}
		member.record.acceptSharedMutationSetBackground(member.generation, background)
	}
}

func (p *PublicationPhaseRecord) acceptSharedMutationSetBackground(generation uint64, background *SharedSetBackground) {
	owner := p.owner.Value()
	if owner == nil {
		return
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	prior := p.sharedSetExecution
	if owner.byKey[p.key] != p || p.ticket != generation || prior == nil || prior.generation != generation || prior.background != nil {
		return
	}
	for _, mark := range p.marks {
		if mark.phase == PublicationTicketFailed {
			return
		}
	}
	next := *prior
	next.background = background
	p.sharedSetExecution = &next // old snapshots remain immutable
}
