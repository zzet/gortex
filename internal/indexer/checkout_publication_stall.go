package indexer

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// checkoutPublicationStallThreshold is how many cycles in a row must publish
// nothing before a require_fresh answer reports the checkout as stalled.
const checkoutPublicationStallThreshold = 3

// Stall reasons. A failed cycle's reason is stallReasonFailedPrefix followed
// by checkoutCycleFailureClass of its error. The others name what stopped a
// rescheduled cycle (CheckoutCycle.rescheduledBy): the working tree moving
// under its builds, a lost route flip, the primary base advancing, or the
// checkout committing under the cycle.
const (
	stallReasonTornByMotion = "torn_by_motion"
	stallReasonRouteMoved   = "route_moved"
	stallReasonBaseMoved    = "base_moved"
	stallReasonHeadMoved    = "head_moved"
	stallReasonFailedPrefix = "failed:"
)

// CheckoutPublicationStall is one checkout's run of cycles that published
// nothing: a working tree edited faster than it can be built (every attempt
// torn), or builds that keep failing. It is what turns "the route is behind"
// into a reason a person, or a require_fresh caller, can act on.
type CheckoutPublicationStall struct {
	// CheckoutID names the working copy the run belongs to.
	CheckoutID string `json:"checkout_id"`
	// ConsecutiveNonpublishingCycles counts the cycles in a row, since the
	// last one that published, that failed or were rescheduled without
	// routing anything. Held, deferred and yielded cycles neither count nor
	// end the run: they attempted nothing.
	ConsecutiveNonpublishingCycles int `json:"consecutive_nonpublishing_cycles"`
	// Since is when the first cycle of the run ended (Unix seconds).
	Since int64 `json:"since"`
	// LastPublicationAgeSeconds is how long ago a cycle last published: it
	// left the route describing the working tree (a build, a reuse or a
	// settled no-op) or routed one batch of a large working tree. -1 when
	// none has since the coordinator started.
	LastPublicationAgeSeconds int64 `json:"last_publication_age_s"`
	// StallReason is the latest cycle's outcome: torn_by_motion (the working
	// tree moved under its builds), route_moved (a lost route flip),
	// base_moved (the primary base advanced under it), head_moved (the
	// checkout committed under it), or failed:<class> with the bounded class
	// of checkoutCycleFailureClass.
	StallReason string `json:"stall_reason"`
	// ChangeSetSize is how many paths the latest working-tree build planned
	// over the parent it stood on (the routed parent for a chained delta), 0
	// when that cycle reached no build.
	ChangeSetSize int `json:"change_set_size"`
}

// checkoutPublicationStallRun is the coordinator's side of CheckoutPublicationStall.
type checkoutPublicationStallRun struct {
	mu            sync.Mutex
	consecutive   int
	since         time.Time
	lastPublished time.Time
	reason        string
	changeSet     int
}

// notePublicationOutcome folds one finished cycle into the run.
func (c *CheckoutCoordinator) notePublicationOutcome(out CheckoutCycle, at time.Time) {
	if c.lifetimeContext().Err() != nil {
		return
	}
	reason := ""
	switch {
	case out.Held || out.Deferred:
		return
	case out.Err != nil:
		reason = stallReasonFailedPrefix + checkoutCycleFailureClass(out.Err)
	case out.YieldedTo == treeMovedReason:
		reason = stallReasonTornByMotion
	case out.DirtyBuilt && out.DirtyBatchRemaining > 0:
		// One batch of a large working tree: routed, and the next batch
		// stands on it. Progress, not a stall.
	case out.Rescheduled && out.YieldedTo == "" && out.rescheduledBy != "":
		reason = out.rescheduledBy
	case out.Rescheduled:
		// Stepped aside for a refresh ticket or an interactive build.
		return
	}
	r := &c.publication
	r.mu.Lock()
	defer r.mu.Unlock()
	if reason == "" {
		r.consecutive, r.since, r.reason, r.changeSet = 0, time.Time{}, "", 0
		r.lastPublished = at
		return
	}
	if r.consecutive == 0 {
		r.since = at
	}
	r.consecutive++
	r.reason = reason
	r.changeSet = 0
	if w := out.DirtyWork; w != nil {
		r.changeSet = w.PlanChanged + w.PlanAdded + w.PlanDeleted
	}
}

// publicationStall reports the coordinator's current run, false when the
// latest cycle that attempted anything left the route describing the tree.
func (c *CheckoutCoordinator) publicationStall(now time.Time) (CheckoutPublicationStall, bool) {
	r := &c.publication
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.consecutive == 0 {
		return CheckoutPublicationStall{}, false
	}
	age := int64(-1)
	if !r.lastPublished.IsZero() {
		age = int64(now.Sub(r.lastPublished) / time.Second)
	}
	return CheckoutPublicationStall{
		CheckoutID:                     c.checkoutID,
		ConsecutiveNonpublishingCycles: r.consecutive,
		Since:                          r.since.Unix(),
		LastPublicationAgeSeconds:      age,
		StallReason:                    r.reason,
		ChangeSetSize:                  r.changeSet,
	}, true
}

// checkoutCycleFailureClass is the bounded class of a failed cycle's error,
// for the reconcile-failed log's cause and a stall's reason. The full error
// stays in the log beside it.
func checkoutCycleFailureClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, store_sqlite.ErrCatalogStaleGuard):
		return "catalog_guard"
	case errors.Is(err, ErrDirtySnapshotChanged), errors.Is(err, errCheckoutUnsettled):
		return "working_tree_moved"
	case errors.Is(err, errRouteMoved):
		return "route_moved"
	case errors.Is(err, errBaseMoved):
		return "base_moved"
	default:
		return "other"
	}
}

// PublicationStalls reports every live coordinator whose latest cycles
// published nothing, longest run first. The ordinary answer is empty.
func (l *CheckoutLifecycle) PublicationStalls() []CheckoutPublicationStall {
	if l == nil {
		return nil
	}
	l.coordMu.Lock()
	coordinators := make([]*CheckoutCoordinator, 0, len(l.coordinators))
	for _, c := range l.coordinators {
		coordinators = append(coordinators, c)
	}
	l.coordMu.Unlock()
	now := time.Now()
	var out []CheckoutPublicationStall
	for _, c := range coordinators {
		if stall, ok := c.publicationStall(now); ok {
			out = append(out, stall)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ConsecutiveNonpublishingCycles != out[j].ConsecutiveNonpublishingCycles {
			return out[i].ConsecutiveNonpublishingCycles > out[j].ConsecutiveNonpublishingCycles
		}
		return out[i].CheckoutID < out[j].CheckoutID
	})
	return out
}

// CheckoutPublicationStalled reports one checkout's run once it reaches
// checkoutPublicationStallThreshold cycles: the require_fresh rider's
// publication_stalled.
func (l *CheckoutLifecycle) CheckoutPublicationStalled(checkoutID string) (CheckoutPublicationStall, bool) {
	if l == nil || checkoutID == "" {
		return CheckoutPublicationStall{}, false
	}
	l.coordMu.Lock()
	c := l.coordinators[checkoutID]
	l.coordMu.Unlock()
	if c == nil {
		return CheckoutPublicationStall{}, false
	}
	stall, ok := c.publicationStall(time.Now())
	if !ok || stall.ConsecutiveNonpublishingCycles < checkoutPublicationStallThreshold {
		return CheckoutPublicationStall{}, false
	}
	return stall, true
}
