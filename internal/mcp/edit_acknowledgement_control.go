package mcp

import "time"

// Actual leaf preparation/validation/formatting/receipt/response control work
// between the fixed named operations. These are online monotonic intervals,
// never a residual or retained history. All helpers require a.mu.
func (a *editAcknowledgement) openEditControlLocked(at time.Time) {
	laps := &a.laps
	if !laps.controlObserved {
		return
	}
	data := &laps.data[editAckLeafControl]
	if at.IsZero() || at.Before(a.entered) || laps.active != 0 || !laps.lastEnd.IsZero() && at.Before(laps.lastEnd) {
		data.invalid = true
		laps.overlap = true
		return
	}
	laps.sequence++
	laps.active, laps.activeKind = laps.sequence, editAckLeafControl
	laps.controlStarted = at
	data.started++
	if data.first.IsZero() {
		data.first = at
	}
}

func (a *editAcknowledgement) closeEditControlLocked(at time.Time) {
	laps := &a.laps
	if !laps.controlObserved {
		return
	}
	data := &laps.data[editAckLeafControl]
	if laps.active == 0 || laps.activeKind != editAckLeafControl || laps.controlStarted.IsZero() {
		data.invalid = true
		laps.overlap = true
		return
	}
	data.completed++
	if at.IsZero() || at.Before(laps.controlStarted) {
		data.invalid = true
	} else {
		data.elapsed += at.Sub(laps.controlStarted)
	}
	data.last = at
	laps.lastEnd = at
	laps.controlStarted = time.Time{}
	laps.active = 0
}

func (a *editAcknowledgement) leafControlOpenLocked() bool {
	return a.laps.controlObserved && !a.envelope.leafStarted.IsZero() && a.envelope.leafFinished.IsZero()
}

// The bound handler's actual finish and caller-facing reply finish already
// have genuine monotonic readings. This separate delivery/finalization pair
// starts where reply_prepare ends and excludes transport serialization. A
// detached physical handler finishing later is not clipped into the ACK.
func acknowledgementCallerResult(entered, actual, reply time.Time) editAcknowledgementLapSnapshot {
	out := editAcknowledgementLapSnapshot{State: "unobserved"}
	if actual.IsZero() {
		return out
	}
	out.Started = 1
	if entered.IsZero() || actual.Before(entered) {
		out.State = "invalid_interval"
		return out
	}
	if reply.IsZero() {
		out.State = "pending_reply"
		return out
	}
	out.Completed = 1
	if actual.After(reply) {
		out.State = "outside_reply_window"
		return out
	}
	ms := float64(reply.Sub(actual)) / float64(time.Millisecond)
	ackMS := ms
	out.ObservedMS, out.AckMS, out.State = &ms, &ackMS, "measured"
	out.ackNS, out.hasAckNS = reply.Sub(actual).Nanoseconds(), true
	return out
}
