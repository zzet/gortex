package daemon

import (
	"time"

	"github.com/zzet/gortex/internal/viewmetrics"
)

const (
	mcpWaitAdmitted = iota
	mcpWaitRefused
	mcpWaitCancelled
)

var mcpWaitBoundsMS = [...]int64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000}
var mcpWaitBucketLabels = [...]string{"1ms", "5ms", "10ms", "25ms", "50ms", "100ms", "250ms", "500ms", "1000ms", "2500ms", "5000ms", "10000ms", "30000ms", "inf"}
var mcpWaitOutcomeLabels = [...]string{"admitted", "refused", "cancelled"}

type mcpDispatchWaitStats struct {
	buckets [14]uint64
	count   uint64
	total   time.Duration
}

// MCPDispatchWaitHistogram uses disjoint upper-bound buckets in milliseconds.
// The final bucket is >the last bound. Zero-wait admissions are included.
type MCPDispatchWaitHistogram struct {
	BoundsMS []int64  `json:"bounds_ms"`
	Buckets  []uint64 `json:"buckets"`
	Count    uint64   `json:"count"`
	TotalMS  int64    `json:"total_ms"`
}

type MCPDispatchLaneStatus struct {
	InFlight      int                                 `json:"in_flight"`
	QueueDepth    int                                 `json:"queue_depth"`
	Draining      int                                 `json:"draining"`
	CancelledHeld int                                 `json:"cancelled_held"`
	Refusals      uint64                              `json:"refusals"`
	QueueWait     map[string]MCPDispatchWaitHistogram `json:"queue_wait"`
}

// MCPDispatchStatus is a bounded, identity-free admission snapshot. Protocol
// initialization/notifications share total capacity but are exempt from tool
// reservations and waiter caps; their unbudgeted waiters are reported apart.
type MCPDispatchStatus struct {
	Limit              int                              `json:"limit"`
	SlowLimit          int                              `json:"slow_limit"`
	MutationReserve    int                              `json:"mutation_reserve"`
	ReservedSlotsUsed  int                              `json:"reserved_slots_used"`
	ReservedAdmissions uint64                           `json:"reserved_admissions"`
	DrainingLimit      int                              `json:"draining_limit"`
	DrainingTotal      int                              `json:"draining_total"`
	ProtocolWaiters    int                              `json:"protocol_waiters"`
	Lanes              map[string]MCPDispatchLaneStatus `json:"lanes"`
}

func (s *mcpDispatchScheduler) observeWaitLocked(class MCPDispatchClass, outcome int, waited time.Duration) {
	waited = max(0, waited)
	lane := &s.lanes[class]
	stat := &lane.wait[outcome]
	bucket := len(mcpWaitBoundsMS)
	for i, bound := range mcpWaitBoundsMS {
		if waited <= time.Duration(bound)*time.Millisecond {
			bucket = i
			break
		}
	}
	stat.buckets[bucket]++
	stat.count++
	stat.total += waited
	if outcome == mcpWaitRefused {
		lane.refusals++
		viewmetrics.Count(viewmetrics.MCPDispatchRefusalsTotal, class.String())
	}
	viewmetrics.Count(viewmetrics.MCPDispatchQueueWaitBucketsTotal, class.String(), mcpWaitOutcomeLabels[outcome], mcpWaitBucketLabels[bucket])
	viewmetrics.Observe(viewmetrics.MCPDispatchQueueWaitSeconds, waited, class.String(), mcpWaitOutcomeLabels[outcome])
}
func (s *mcpDispatchScheduler) observeQueuedWaitLocked(waiter *mcpDispatchWaiter, outcome int, now time.Time) {
	if waiter.observed {
		return
	}
	waiter.observed = true
	s.observeWaitLocked(waiter.class, outcome, now.Sub(waiter.queuedAt))
}
func (s *mcpDispatchScheduler) snapshot() MCPDispatchStatus {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	out := MCPDispatchStatus{Limit: cap(s.slots), SlowLimit: s.slowLimit, MutationReserve: s.reserve, ReservedSlotsUsed: min(s.reserve, s.active[MCPDispatchMutating]), ReservedAdmissions: s.reservedAdmissions, DrainingLimit: mcpDispatchDrainLimit, DrainingTotal: s.owner.draining, ProtocolWaiters: s.protocolWaiting, Lanes: make(map[string]MCPDispatchLaneStatus, 3)}
	for _, class := range [...]MCPDispatchClass{MCPDispatchMutating, MCPDispatchFast, MCPDispatchSlow} {
		lane := &s.lanes[class]
		row := MCPDispatchLaneStatus{InFlight: s.active[class], QueueDepth: lane.queued, Draining: s.draining[class], Refusals: lane.refusals, QueueWait: make(map[string]MCPDispatchWaitHistogram, 3)}
		for _, blocked := range s.owner.blocked {
			if blocked.pool == s && blocked.class == class {
				row.CancelledHeld++
			}
		}
		for outcome, label := range mcpWaitOutcomeLabels {
			stat := lane.wait[outcome]
			row.QueueWait[label] = MCPDispatchWaitHistogram{BoundsMS: append([]int64(nil), mcpWaitBoundsMS[:]...), Buckets: append([]uint64(nil), stat.buckets[:]...), Count: stat.count, TotalMS: stat.total.Milliseconds()}
		}
		out.Lanes[class.String()] = row
	}
	return out
}

// MCPDispatchSnapshot is the process pool's live status, independent of index
// readiness and without catalog/store work. It does not expose session IDs.
func MCPDispatchSnapshot() MCPDispatchStatus { return mcpSchedulerFor(mcpDispatchSlots).snapshot() }
