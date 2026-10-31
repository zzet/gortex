package indexer

import (
	"time"

	"go.uber.org/zap/zapcore"
)

var canonicalAttemptLapNames = [...]string{
	"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "patch_analysis",
	"attempt_setup", "patch_inspection", "executor_control", "attempt_finalize", "cohort_admission", "batch_admission", "lane_wait",
}

type canonicalAttemptEvidence struct {
	laps                                                             [canonicalPatchLapCount]canonicalLapEvidence
	startNS, endNS                                                   int64
	observed, invalid                                                bool
	nativeMtimeDuplicate                                             bool
	executorStart, executorEnd, derivedStart, derivedEnd             int64
	hasExecutorStart, hasExecutorEnd, hasDerivedStart, hasDerivedEnd bool
}

// New immutable evidence is copied at actual return, never into an already
// exposed early snapshot. Caller holds the collector lock.
func (s *PublicationStamps) freezeAttemptEvidence(origin time.Time) canonicalAttemptEvidence {
	p := s.patchLaps
	out := canonicalAttemptEvidence{observed: p.controlObserved && p.controlFinished, invalid: p.invalid || p.active != 0, nativeMtimeDuplicate: p.nativeMtimeDuplicate}
	if !p.attemptStarted.IsZero() {
		out.startNS = p.attemptStarted.Sub(origin).Nanoseconds()
	}
	if !p.attemptFinished.IsZero() {
		out.endNS = p.attemptFinished.Sub(origin).Nanoseconds()
	}
	for i, v := range p.laps {
		out.laps[i] = canonicalLapEvidence{totalNS: v.total.Nanoseconds(), begun: v.begun, done: v.done, incomplete: v.incomplete}
		if v.begun > 0 {
			out.laps[i].firstNS = v.first.Sub(origin).Nanoseconds()
			out.laps[i].lastNS = v.last.Sub(origin).Nanoseconds()
		}
	}
	for _, mark := range s.marks {
		at := mark.at.Sub(origin).Nanoseconds()
		switch mark.phase {
		case PublicationPatchExecutionStarted:
			if !out.hasExecutorStart {
				out.executorStart, out.hasExecutorStart = at, true
			}
		case PublicationPatchExecutionFinished:
			if !out.hasExecutorEnd {
				out.executorEnd, out.hasExecutorEnd = at, true
			}
		case PublicationPatchDerivedStarted:
			if !out.hasDerivedStart {
				out.derivedStart, out.hasDerivedStart = at, true
			}
		case PublicationPatchDerivedFinished:
			if !out.hasDerivedEnd {
				out.derivedEnd, out.hasDerivedEnd = at, true
			}
		}
	}
	return out
}

func (p *canonicalAttemptEvidence) agreesWithPair(kind canonicalPatchLap, start, end int64, started, finished, invalid bool) bool {
	v := p.laps[kind]
	if invalid || started != (v.begun > 0) || finished != (v.done > 0) || v.begun > 1 || v.done > 1 {
		return false
	}
	if !started {
		return true
	}
	return v.firstNS == start && (!finished || v.lastNS == end && v.totalNS == end-start)
}

func (p *canonicalAttemptEvidence) agreesWithAdmissions(admissions canonicalAdmissionEvidence, lane canonicalLaneWaitEvidence) bool {
	for i, pair := range admissions.pairs {
		if !p.agreesWithPair(patchCohortAdmission+canonicalPatchLap(i), pair.startNS, pair.endNS, pair.started, pair.finished, pair.invalid) {
			return false
		}
	}
	return p.agreesWithPair(patchRepositoryLane, lane.startNS, lane.acquiredNS, lane.started, lane.acquired, lane.invalid)
}

type canonicalAttemptProjection struct {
	states   [canonicalPatchLapCount]string
	values   [canonicalPatchLapCount]float64
	sumNS    int64
	state    string
	complete bool
}

func requiredAttemptLap(kind canonicalPatchLap) bool {
	// Missing requested deep evidence is not made applicable/complete by a broad
	// control interval. Analysis and a nonexistent batch lock/hook are optional.
	return kind < patchBookkeeping || isAttemptControl(kind) || kind == patchCohortAdmission || kind == patchRepositoryLane
}

func (w canonicalAttemptWindow) evaluateAccounting() canonicalAttemptProjection {
	return w.evaluateAccountingFor(false)
}

// The two existing owners share ordered scalar validation, not their scope.
// Read repair begins after admission/lane acquisition and cannot borrow those
// P intervals. Its one actual control interval surrounds the named operations.
func (w canonicalAttemptWindow) evaluateAccountingFor(readRepair bool) canonicalAttemptProjection {
	p := w.accounting
	out := canonicalAttemptProjection{state: "not_observed"}
	for i := range out.states {
		out.states[i] = "not_observed"
	}
	if !p.observed {
		return out
	}
	if w.startNS < 0 || w.endNS < w.startNS {
		out.state = "invalid_order"
		for i := range out.states {
			out.states[i] = "invalid_order"
		}
		return out
	}
	invalid := p.invalid || p.startNS != w.startNS || p.endNS != w.endNS || w.startNS < 0 || w.endNS < w.startNS
	invalid = invalid || readRepair && p.nativeMtimeDuplicate
	wall := w.endNS - w.startNS
	missing := !p.hasExecutorStart || !p.hasExecutorEnd || !p.hasDerivedStart || !p.hasDerivedEnd
	if p.nativeMtimeDuplicate {
		missing = false
		// A native duplicate never entered the executor. Any execution evidence
		// contradicts that disposition; it cannot be reclassified as zero work.
		invalid = invalid || p.hasExecutorStart || p.hasExecutorEnd || p.hasDerivedStart || p.hasDerivedEnd
	}
	if p.hasExecutorStart && (p.executorStart < w.startNS || p.executorStart > w.endNS) || p.hasExecutorEnd && (p.executorEnd < p.executorStart || p.executorEnd > w.endNS) {
		invalid = true
	}
	for i, v := range p.laps {
		kind := canonicalPatchLap(i)
		if readRepair && kind >= canonicalDeepLapCount && kind != patchExecutorControl {
			if v != (canonicalLapEvidence{}) {
				invalid = true
			}
			continue
		}
		if p.nativeMtimeDuplicate && (kind < canonicalDeepLapCount || kind == patchExecutorControl) {
			if v != (canonicalLapEvidence{}) {
				invalid = true
			}
			out.states[i] = "not_applicable"
			continue
		}
		required := requiredAttemptLap(kind)
		if readRepair {
			required = kind < patchBookkeeping || kind == patchExecutorControl
		}
		if required && (v.begun == 0 || v.begun != v.done || v.incomplete) {
			missing = true
		}
		if v.incomplete || v.begun != v.done {
			missing = true
		}
		if v.begun == 0 {
			continue
		}
		if v.totalNS < 0 || v.firstNS < w.startNS || v.lastNS < v.firstNS || v.lastNS > w.endNS || v.totalNS > v.lastNS-v.firstNS || v.done > v.begun {
			invalid = true
		}
		if kind < canonicalDeepLapCount && (p.hasExecutorStart && v.firstNS < p.executorStart || p.hasExecutorEnd && v.lastNS > p.executorEnd) {
			invalid = true
		}
		if v.begun != v.done || v.incomplete {
			continue
		}
		if v.totalNS < 0 || v.totalNS > wall || out.sumNS > wall-v.totalNS {
			invalid = true
			continue
		}
		out.sumNS += v.totalNS
		out.states[i] = "measured"
	}
	derived := p.laps[patchDerived]
	if p.hasDerivedStart || p.hasDerivedEnd {
		if !p.hasDerivedStart || !p.hasDerivedEnd || derived.begun != 1 || derived.done != 1 || derived.firstNS != p.derivedStart || derived.lastNS != p.derivedEnd || derived.totalNS != p.derivedEnd-p.derivedStart {
			invalid = true
		}
	}
	if invalid {
		out.state = "invalid_order"
		for i := range out.states {
			out.states[i] = "invalid_order"
		}
		return out
	}
	for i := range out.values {
		if out.states[i] == "measured" {
			out.values[i] = float64(p.laps[i].totalNS) / 1e6
		}
	}
	switch {
	case missing:
		out.state = "missing_laps"
	case out.sumNS != wall:
		out.state = "unclassified_gap"
	default:
		out.state, out.complete = "partial", true
	}
	return out
}

// EditPatchAccounting describes only one accepted winning P, not quiet waits,
// backoff/discarded attempts, receipt lifetime, ACK or whole-edit background.
// Even an observed complete partition is not the actual100-edit/5% acceptance.
type EditPatchAccounting struct {
	ExecutionDisposition string              `json:"execution_disposition"`
	AttemptTicket        uint64              `json:"attempt_ticket"`
	AttemptRetry         int                 `json:"attempt_retry"`
	Scope                string              `json:"scope"`
	BackgroundCoverage   string              `json:"background_coverage"`
	SumState             string              `json:"sum_state"`
	PartitionComplete    bool                `json:"observed_partition_complete"`
	PhasesMS             map[string]*float64 `json:"phases_ms"`
	PhaseStates          map[string]string   `json:"phase_states"`
	SubtotalMS           *float64            `json:"observed_subtotal_ms"`
	UnattributedMS       *float64            `json:"unattributed_ms"`
}

func (w canonicalAttemptWindow) projectAccounting() *EditPatchAccounting {
	p := w.evaluateAccounting()
	out := &EditPatchAccounting{AttemptTicket: w.ticket, AttemptRetry: w.retry, Scope: "accepted_winning_physical_attempt", BackgroundCoverage: "not_established", SumState: p.state, PartitionComplete: p.complete,
		PhasesMS: make(map[string]*float64, canonicalPatchLapCount), PhaseStates: make(map[string]string, canonicalPatchLapCount)}
	out.ExecutionDisposition = w.executionDisposition()
	for i, name := range canonicalAttemptLapNames {
		out.PhasesMS[name], out.PhaseStates[name] = nil, p.states[i]
		if p.states[i] == "measured" {
			ms := p.values[i]
			out.PhasesMS[name] = &ms
		}
	}
	if p.state != "not_observed" && p.state != "invalid_order" {
		subtotal := float64(p.sumNS) / 1e6
		residual := float64(w.endNS-w.startNS-p.sumNS) / 1e6
		out.SubtotalMS, out.UnattributedMS = &subtotal, &residual
	}
	return out
}

func (w canonicalAttemptWindow) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	p := w.evaluateAccounting()
	enc.AddString("scope", "accepted_winning_physical_attempt")
	enc.AddString("execution_disposition", w.executionDisposition())
	enc.AddUint64("attempt_ticket", w.ticket)
	enc.AddInt("attempt_retry", w.retry)
	enc.AddString("background_coverage", "not_established")
	enc.AddString("sum_state", p.state)
	enc.AddBool("observed_partition_complete", p.complete)
	for i, name := range canonicalAttemptLapNames {
		enc.AddString(name+"_state", p.states[i])
		if p.states[i] == "measured" {
			enc.AddFloat64(name+"_ms", p.values[i])
		}
	}
	if p.state != "not_observed" && p.state != "invalid_order" {
		enc.AddFloat64("observed_subtotal_ms", float64(p.sumNS)/1e6)
		enc.AddFloat64("unattributed_ms", float64(w.endNS-w.startNS-p.sumNS)/1e6)
	}
	return nil
}

func (w canonicalAttemptWindow) executionDisposition() string {
	if w.accounting.nativeMtimeDuplicate {
		return "native_mtime_duplicate"
	}
	return "not_observed"
}
