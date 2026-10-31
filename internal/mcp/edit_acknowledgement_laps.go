package mcp

import (
	"context"
	"time"
)

// Fixed acknowledgement operations, not a general span/event collection. The
// slots accumulate repeated chunks (for example more than one file) without
// retaining per-call arrays, contexts, source strings or parser results.
type editAcknowledgementLapKind uint8

const (
	editAckParseGate editAcknowledgementLapKind = iota
	editAckDiskWrite
	editAckTicketAdmission
	editAckReindexWait
	editAckLeafControl
	editAckLapCount
)

type editAcknowledgementLapData struct {
	started, completed uint64
	notApplicable      uint64
	elapsed            time.Duration
	first, last        time.Time
	invalid            bool
}

type editAcknowledgementLaps struct {
	data            [editAckLapCount]editAcknowledgementLapData
	sequence        uint64
	active          uint64
	activeKind      editAcknowledgementLapKind
	lastEnd         time.Time
	controlStarted  time.Time
	controlObserved bool
	overlap         bool
}

// ObservedMS is actual completed execution, even if it outlived the caller.
// AckMS is present only for complete valid chunks contained in the frozen
// reply wall. They are alternatives, not two durations to add together.
type editAcknowledgementLapSnapshot struct {
	ObservedMS    *float64
	AckMS         *float64
	State         string
	Started       uint64
	Completed     uint64
	NotApplicable uint64
	ackNS         int64 // actual scalar retained until final projection, never interval history
	hasAckNS      bool
}

type editAcknowledgementLap struct {
	owner      *editAcknowledgement
	kind       editAcknowledgementLapKind
	sequence   uint64
	started    time.Time
	applicable bool
}

func (s *Server) beginAcknowledgementLap(ctx context.Context, kind editAcknowledgementLapKind) editAcknowledgementLap {
	return beginEditAcknowledgementLap(editAcknowledgementFrom(ctx, s), kind)
}

func beginEditAcknowledgementLap(a *editAcknowledgement, kind editAcknowledgementLapKind) editAcknowledgementLap {
	if a == nil {
		return editAcknowledgementLap{} // raw/read/native callers do not sample a clock
	}
	return a.beginLapAt(kind, time.Now())
}

// Keep the real old/new parse-gate work inside one interval. Callers retain
// their existing enable/refusal/override policy; unsupported languages remain
// the gate's existing Checked=false verdict, not a new parse implementation.
func (s *Server) checkAcknowledgedParseGate(ctx context.Context, path string, before, after []byte) parseGateResult {
	s.noteAcknowledgementParser(ctx, "old_new_parse_gate")
	lap := s.beginAcknowledgementLap(ctx, editAckParseGate)
	defer lap.finish()
	result := checkParseGate(path, before, after)
	lap.applicable = result.Checked
	return result
}

func (a *editAcknowledgement) beginLapAt(kind editAcknowledgementLapKind, at time.Time) editAcknowledgementLap {
	a.mu.Lock()
	defer a.mu.Unlock()
	laps := &a.laps
	if laps.active != 0 && laps.activeKind == editAckLeafControl {
		a.closeEditControlLocked(at)
	}
	laps.sequence++
	data := &laps.data[kind]
	data.started++
	if data.first.IsZero() {
		data.first = at
	}
	if at.IsZero() || at.Before(a.entered) {
		data.invalid = true
	}
	if laps.active != 0 {
		laps.overlap = true
		data.invalid = true
		laps.data[laps.activeKind].invalid = true
	} else {
		if !laps.lastEnd.IsZero() && at.Before(laps.lastEnd) {
			laps.overlap = true
			data.invalid = true
		}
		laps.active, laps.activeKind = laps.sequence, kind
	}
	return editAcknowledgementLap{owner: a, kind: kind, sequence: laps.sequence, started: at, applicable: true}
}

func (lap *editAcknowledgementLap) finish() {
	if lap.owner == nil {
		return // no clock on an unobserved or already-finished token
	}
	lap.finishAt(time.Now())
}

func (lap *editAcknowledgementLap) finishAt(at time.Time) {
	a := lap.owner
	if a == nil {
		return
	}
	lap.owner = nil // this token belongs to one synchronous caller
	a.mu.Lock()
	defer a.mu.Unlock()
	laps := &a.laps
	data := &laps.data[lap.kind]
	data.completed++
	if !lap.applicable {
		data.notApplicable++
	}
	if at.IsZero() || at.Before(lap.started) || lap.started.IsZero() {
		data.invalid = true
	} else if lap.applicable {
		data.elapsed += at.Sub(lap.started)
	}
	if data.last.IsZero() || at.After(data.last) {
		data.last = at
	}
	matched := laps.active == lap.sequence
	if matched {
		laps.active = 0
	} else {
		laps.overlap = true
		data.invalid = true
	}
	// A checked-but-not-applicable parse gate still performed actual guard
	// dispatch. Its measured call belongs to leaf control, not a fake parse.
	if matched && !lap.applicable && !data.invalid && a.leafControlOpenLocked() {
		a.openEditControlLocked(lap.started)
		a.closeEditControlLocked(at)
	}
	if at.After(laps.lastEnd) {
		laps.lastEnd = at
	}
	if matched && laps.active == 0 && a.leafControlOpenLocked() {
		a.openEditControlLocked(at)
	}
}

func (laps editAcknowledgementLaps) snapshot(kind editAcknowledgementLapKind, entered, reply, actual time.Time) editAcknowledgementLapSnapshot {
	data := laps.data[kind]
	out := editAcknowledgementLapSnapshot{Started: data.started, Completed: data.completed, NotApplicable: data.notApplicable, State: "unobserved"}
	if data.started == 0 {
		return out
	}
	if data.invalid || entered.IsZero() || data.first.Before(entered) {
		out.State = "invalid_interval"
		return out
	}
	if data.completed != data.started {
		out.State = "incomplete"
		return out
	}
	if data.notApplicable == data.started {
		out.State = "not_applicable"
		return out
	}
	if !actual.IsZero() && data.last.After(actual) {
		out.State = "outside_execution_window"
		return out
	}
	ms := float64(data.elapsed) / float64(time.Millisecond)
	out.ObservedMS = &ms
	if reply.IsZero() {
		out.State = "pending_reply"
		return out
	}
	if reply.Before(entered) || data.last.After(reply) {
		out.State = "outside_reply_window"
		return out
	}
	ackMS := ms
	out.AckMS, out.State = &ackMS, "measured"
	out.ackNS, out.hasAckNS = data.elapsed.Nanoseconds(), true
	if data.notApplicable > 0 {
		out.State = "measured_with_not_applicable_chunks"
	}
	return out
}
