package daemon

import (
	"context"
	"time"
)

// MCPDispatchTiming is a value copy of one request's original bounded
// admission. It is not a response/transport measurement. Releasing capacity,
// attaching another grant or later slow-lane admission cannot change it.
type MCPDispatchTiming struct {
	Entered time.Time
	Granted time.Time
}

// MCPDispatchTimingForContext returns unavailable unless this context's
// existing lease witnessed both boundaries of its first admission attempt.
// Cache hits, replay followers, unbounded and non-daemon callers have no such
// observation. Reading the snapshot neither samples a clock nor resets it.
func MCPDispatchTimingForContext(ctx context.Context) (MCPDispatchTiming, bool) {
	if ctx == nil || isMCPDispatchUnbounded(ctx) {
		return MCPDispatchTiming{}, false
	}
	lease := mcpDispatchSlotLeaseForContext(ctx)
	if lease == nil {
		return MCPDispatchTiming{}, false
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	value := lease.timing
	return value, !value.Entered.IsZero() && !value.Granted.IsZero()
}

// This stack token fences the first attempt without a new context, allocation
// or registry. Nil-lease primitives retain their clock-free path. Requests
// with an already allocated lease pay two clock samples on successful bounded
// admission, including reads; that overhead requires measurement.
type mcpDispatchTimingAttempt struct {
	lease *mcpDispatchSlotLease
}

func (l *mcpDispatchSlotLease) beginTiming() mcpDispatchTimingAttempt {
	if l == nil {
		return mcpDispatchTimingAttempt{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.timing.Entered.IsZero() {
		return mcpDispatchTimingAttempt{}
	}
	l.timing.Entered = time.Now()
	return mcpDispatchTimingAttempt{lease: l}
}

func (a mcpDispatchTimingAttempt) granted() {
	if a.lease == nil {
		return
	}
	a.lease.mu.Lock()
	if a.lease.timing.Granted.IsZero() {
		a.lease.timing.Granted = time.Now()
	}
	a.lease.mu.Unlock()
}
