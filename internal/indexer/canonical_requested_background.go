package indexer

import (
	"context"
	"math"
	"sync"
	"time"
	"weak"
)

type canonicalSchedulingKey struct{}

// Preserve the outer scheduler's actual entry across MultiWatcher routing.
// Native/unobserved admission performs no clock read or context allocation.
func canonicalSchedulingEntry(ctx context.Context) (context.Context, time.Time) {
	if entered, ok := ctx.Value(canonicalSchedulingKey{}).(time.Time); ok {
		return ctx, entered
	}
	if PublicationRecordFromContext(ctx) == nil {
		return ctx, time.Time{}
	}
	entered := time.Now()
	return context.WithValue(ctx, canonicalSchedulingKey{}, entered), entered
}

type canonicalBackgroundStage uint8

const (
	backgroundAdmission canonicalBackgroundStage = iota
	backgroundInitialArm
	backgroundInitialQuiet
	backgroundHandoff
	backgroundNativeArm
	backgroundNativeQuiet
	backgroundExecuting
	backgroundReturned
)

// One ordered scalar per named lap, not an interval/timer history. Native
// handoffs are counted only after actual successful Stop and actual next arm.
type canonicalBackgroundOwner struct {
	mu                              sync.Mutex
	record                          weak.Pointer[PublicationPhaseRecord]
	watcher                         weak.Pointer[Watcher]
	path, key                       string
	requested, current, handoffs    uint64
	origin, last, armStart, entered time.Time
	phases                          [7]int64
	stage                           canonicalBackgroundStage
	invalid                         bool
	reason                          string
}

func newCanonicalBackground(record *PublicationPhaseRecord, w *Watcher, path string, requested uint64, entered time.Time) *canonicalBackgroundOwner {
	if record == nil || w == nil || path == "" || requested == 0 || entered.IsZero() {
		return nil
	}
	return &canonicalBackgroundOwner{record: weak.Make(record), watcher: weak.Make(w), path: path, key: record.key, requested: requested, current: requested, origin: entered, last: entered}
}
func (b *canonicalBackgroundOwner) failLocked(reason string) {
	b.invalid = true
	if b.reason == "" {
		b.reason = reason
	}
}
func (b *canonicalBackgroundOwner) invalidate(reason string) {
	b.mu.Lock()
	b.failLocked(reason)
	b.mu.Unlock()
}
func (b *canonicalBackgroundOwner) matchesLocked(w *Watcher, path string, generation uint64) bool {
	if b.watcher.Value() != w || b.path != path || b.current != generation {
		b.failLocked("foreign_timer_identity")
		return false
	}
	return !b.invalid
}
func (b *canonicalBackgroundOwner) addLocked(lap int, start, end time.Time) bool {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		b.failLocked("invalid_timing_order")
		return false
	}
	delta := end.Sub(start).Nanoseconds()
	if delta < 0 || b.phases[lap] > math.MaxInt64-delta {
		b.failLocked("timing_overflow")
		return false
	}
	b.phases[lap] += delta
	return true
}
func (b *canonicalBackgroundOwner) armStarted(w *Watcher, path string, generation uint64, successor bool, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.invalid {
		return
	}
	if successor {
		if b.watcher.Value() != w || b.path != path || generation <= b.current {
			b.failLocked("foreign_timer_identity")
			return
		}
		if b.stage != backgroundHandoff {
			b.failLocked("unobserved_timer_handoff")
			return
		}
		if b.handoffs == math.MaxUint64 {
			b.failLocked("handoff_count_overflow")
			return
		}
		if !b.addLocked(4, b.last, at) {
			return
		}
		b.current = generation
		b.handoffs++
		b.armStart = at
		b.stage = backgroundNativeArm
	} else {
		if !b.matchesLocked(w, path, generation) {
			return
		}
		if b.stage != backgroundAdmission {
			b.failLocked("repeated_timer_arm")
			return
		}
		if !b.addLocked(0, b.last, at) {
			return
		}
		b.armStart = at
		b.stage = backgroundInitialArm
	}
	b.last = at
}
func (b *canonicalBackgroundOwner) armFinished(w *Watcher, path string, generation uint64, successor bool, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.matchesLocked(w, path, generation) {
		return
	}
	lap, expected, next := 1, backgroundInitialArm, backgroundInitialQuiet
	if successor {
		lap, expected, next = 5, backgroundNativeArm, backgroundNativeQuiet
	}
	if b.stage != expected {
		b.failLocked("missing_timer_arm")
		return
	}
	if b.addLocked(lap, b.last, at) {
		b.last = at
		b.stage = next
	}
}
func (b *canonicalBackgroundOwner) stopped(w *Watcher, path string, generation uint64, successor bool, start, end time.Time, stopped, native bool) *canonicalBackgroundOwner {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.matchesLocked(w, path, generation) {
		return nil
	}
	if !native {
		b.failLocked("observed_successor")
		return nil
	}
	if !stopped {
		b.failLocked("timer_stop_not_successful")
		return nil
	}
	if b.stage == backgroundExecuting || b.stage == backgroundReturned {
		b.failLocked("timer_callback_already_started")
		return nil
	}
	lap, expected := 2, backgroundInitialQuiet
	if successor {
		lap, expected = 6, backgroundNativeQuiet
	}
	if b.stage != expected {
		b.failLocked("unobserved_timer_handoff")
		return nil
	}
	if !b.addLocked(lap, b.last, start) || !b.addLocked(3, start, end) {
		return nil
	}
	b.last = end
	b.stage = backgroundHandoff
	return b
}
func (b *canonicalBackgroundOwner) callback(w *Watcher, path string, generation uint64, successor bool, at time.Time) {
	b.mu.Lock()
	if b.matchesLocked(w, path, generation) {
		if !successor {
			b.failLocked("requested_callback_started")
		} else if b.stage != backgroundNativeQuiet {
			b.failLocked("timer_callback_already_started")
		} else if b.addLocked(6, b.last, at) {
			b.last = at
			b.entered = at
			b.stage = backgroundExecuting
		}
	}
	b.mu.Unlock()
	b.publishUnavailable() // Callback has not acquired watcher/coordinator locks.
}

// Bounded diagnostic only: it provides neither a measured window nor coverage.
// The first actual invalidation stays immutable for this retained incarnation.
type RequestedBackgroundUnavailable struct {
	PublicationKey         string `json:"publication_key"`
	Scope                  string `json:"scope"`
	Reason                 string `json:"reason"`
	RequestedGeneration    uint64 `json:"requested_generation"`
	LastObservedGeneration uint64 `json:"last_observed_generation"`
	ObservedNativeHandoffs uint64 `json:"observed_native_handoffs"`
}

func (b *canonicalBackgroundOwner) unavailableLocked(reason string) RequestedBackgroundUnavailable {
	return RequestedBackgroundUnavailable{Scope: "requested_background_observation", Reason: reason, PublicationKey: b.key, RequestedGeneration: b.requested, LastObservedGeneration: b.current, ObservedNativeHandoffs: b.handoffs}
}

// Caller must not hold watcher/coordinator locks. Recorder membership fencing
// follows the existing recorder→record order; no expired owner is retained.
func (b *canonicalBackgroundOwner) publishUnavailable() {
	b.mu.Lock()
	if b.reason == "" {
		b.mu.Unlock()
		return
	}
	evidence := b.unavailableLocked(b.reason)
	b.mu.Unlock()
	record := b.record.Value()
	if record == nil {
		return
	}
	owner := record.owner.Value()
	if owner == nil {
		return
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	record.mu.Lock()
	defer record.mu.Unlock()
	if owner.byKey[record.key] == record && record.ticket == b.requested && record.background == nil && record.backgroundUnavailable == nil {
		record.backgroundUnavailable = &evidence
	}
}

type canonicalBackgroundEvidence struct {
	origin                         time.Time
	phases                         [7]int64
	startNS, endNS, lastArmStartNS int64
	requested, applied, handoffs   uint64
	physical                       canonicalAttemptWindow
	debounce                       canonicalDebounceEvidence
}

func (b *canonicalBackgroundOwner) finish(attempt *pointPublicationAttempt, entered, end time.Time, retry int) {
	if retry != 0 {
		b.invalidate("retry_or_backoff")
		b.publishUnavailable()
		return
	}
	if attempt == nil || !attempt.independent {
		return
	}
	b.mu.Lock()
	if b.invalid {
		b.mu.Unlock()
		b.publishUnavailable()
		return
	}
	if b.stage != backgroundExecuting || b.entered != entered || end.Before(entered) {
		b.failLocked("serving_attempt_binding_mismatch")
		b.mu.Unlock()
		b.publishUnavailable()
		return
	}
	b.stage = backgroundReturned
	evidence := canonicalBackgroundEvidence{origin: b.origin, phases: b.phases, requested: b.requested, applied: b.current, handoffs: b.handoffs, startNS: entered.Sub(b.origin).Nanoseconds(), endNS: end.Sub(b.origin).Nanoseconds(), lastArmStartNS: b.armStart.Sub(b.origin).Nanoseconds()}
	diagnostic := b.unavailableLocked("serving_attempt_binding_mismatch")
	b.mu.Unlock()
	record := b.record.Value()
	if record == nil {
		return
	}
	owner := record.owner.Value()
	if owner == nil {
		return
	}
	if attempt.record == nil || attempt.record == record || attempt.record.owner.Value() != owner {
		b.invalidate("serving_attempt_binding_mismatch")
		b.publishUnavailable()
		return
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	record.mu.Lock()
	defer record.mu.Unlock()
	winner := attempt.record
	winner.mu.Lock()
	defer winner.mu.Unlock()
	if owner.byKey[record.key] != record || owner.byKey[winner.key] != winner || record.ticket != b.requested || record.background != nil || record.backgroundUnavailable != nil {
		return
	}
	if record.coalescedWinner.Value() != winner || winner.attemptWindow == nil || winner.debounce == nil || winner.ticket != evidence.applied || winner.ticket <= b.requested {
		record.backgroundUnavailable = &diagnostic
		return
	}
	for _, mark := range record.marks {
		if mark.phase == PublicationTicketFailed {
			return
		}
	}
	evidence.applied = winner.ticket
	evidence.physical = *winner.attemptWindow
	evidence.debounce = *winner.debounce
	if !winner.origin.Add(time.Duration(evidence.physical.startNS)).Equal(entered) || !winner.origin.Add(time.Duration(evidence.physical.endNS)).Equal(end) || !evidence.valid() {
		diagnostic.Reason = "physical_evidence_incomplete"
		record.backgroundUnavailable = &diagnostic
		return
	}
	record.background = &evidence // Fresh late scalar; no earlier snapshot mutation.
}
func (b canonicalBackgroundEvidence) valid() bool {
	if b.origin.IsZero() || b.requested == 0 || b.applied <= b.requested || b.handoffs == 0 || b.lastArmStartNS < 0 || b.startNS < b.lastArmStartNS || b.endNS < b.startNS {
		return false
	}
	p := b.physical.evaluateAccounting()
	if !p.complete || b.physical.retry != 0 || b.physical.ticket != b.applied || b.physical.endNS-b.physical.startNS != b.endNS-b.startNS {
		return false
	}
	if !b.debounce.entered || b.debounce.startedNS < 0 || b.debounce.enteredNS != b.physical.startNS || b.debounce.enteredNS-b.debounce.startedNS != b.startNS-b.lastArmStartNS {
		return false
	}
	sum := int64(0)
	for _, delta := range b.phases {
		if delta < 0 || sum > math.MaxInt64-delta {
			return false
		}
		sum += delta
	}
	return sum == b.startNS && p.sumNS >= 0 && sum <= math.MaxInt64-p.sumNS && sum+p.sumNS == b.endNS
}

var requestedBackgroundLapNames = [...]string{"scheduling_admission_setup", "initial_timer_arm", "canceled_quiet_wait", "timer_stop", "coalescing_control", "successor_timer_arm", "successor_quiet_wait"}

// RequestedBackgroundTiming is one measured scheduler-entry→serving-return
// window. Repeated native arms/quiet/stops are actual disjoint scalar totals;
// every canceled timer must have an observed successful pre-callback Stop.
type RequestedBackgroundTiming struct {
	Scope               string             `json:"scope"`
	Coverage            string             `json:"coverage"`
	SumState            string             `json:"sum_state"`
	OriginWall          string             `json:"origin_wall"`
	RequestedGeneration uint64             `json:"requested_generation"`
	AppliedGeneration   uint64             `json:"applied_generation"`
	NativeHandoffs      uint64             `json:"observed_native_handoffs"`
	WallMS              float64            `json:"background_wall_ms"`
	PhasesMS            map[string]float64 `json:"phases_ms"`
	PartitionComplete   bool               `json:"observed_partition_complete"`
	UnattributedMS      float64            `json:"unattributed_ms"`
}

func (b canonicalBackgroundEvidence) project(physical *EditPatchAccounting) *RequestedBackgroundTiming {
	if physical == nil || !physical.PartitionComplete {
		return nil
	}
	coverage := "one_canceled_timer_one_native_successor"
	if b.handoffs > 1 {
		coverage = "successful_pre_callback_native_handoffs"
	}
	out := &RequestedBackgroundTiming{Scope: "accepted_scheduling_call_entry_to_serving_attempt_return", Coverage: coverage, SumState: "partial", OriginWall: b.origin.UTC().Format(time.RFC3339Nano), RequestedGeneration: b.requested, AppliedGeneration: b.applied, NativeHandoffs: b.handoffs, WallMS: float64(b.endNS) / 1e6, PhasesMS: make(map[string]float64, 7+canonicalPatchLapCount), PartitionComplete: true}
	for i, name := range requestedBackgroundLapNames {
		out.PhasesMS[name] = float64(b.phases[i]) / 1e6
	}
	for name, value := range physical.PhasesMS {
		if value != nil {
			out.PhasesMS[name] = *value
		}
	}
	return out
}
func (p *PublicationPhaseRecord) RequestedBackground() *RequestedBackgroundTiming {
	if related := p.CoalescedPublication(); related != nil {
		return related.Background
	}
	return nil
}
