package indexer

import (
	"path/filepath"
	"time"

	"github.com/zzet/gortex/internal/semantic"
)

// lifecycleForegroundActivity is the daemon's foreground-activity view the
// background compiler warm-ups yield to (semantic.ForegroundActivity). A
// warm-up is a whole-module `go list -export`: on a single core it competes
// with the very build and fresh query an edit waits for, so the warm-ups
// start only once the daemon is idle and stop the moment this view reports
// foreground work.
//
// Foreground work is an interactive build queued on the shared lane, a lane
// held by a checkout's own cycle, mutation or transition (a cycle of a
// filesystem edit is queued at background priority but is still an edit in
// flight), or a refresh ticket or demand wake waiting on any coordinator.
// The initial committed-base publication and chain compactions are
// background work and do not hold warm-ups back (the warm-up's tools run at
// the lowest priority, so they do not slow those down much either).
type lifecycleForegroundActivity struct {
	l *CheckoutLifecycle
}

var _ semantic.ForegroundActivity = lifecycleForegroundActivity{}

// foregroundActivity returns the lifecycle's foreground-activity view.
func (l *CheckoutLifecycle) foregroundActivity() semantic.ForegroundActivity {
	return lifecycleForegroundActivity{l: l}
}

// foregroundLaneHolder reports whether a lane holder of kind is foreground
// work.
func foregroundLaneHolder(kind string) bool {
	switch kind {
	case "checkout_cycle", "checkout_mutation", "checkout_transition":
		return true
	}
	return false
}

// registeredCoordinators snapshots the registered coordinators.
func (l *CheckoutLifecycle) registeredCoordinators() []*CheckoutCoordinator {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	out := make([]*CheckoutCoordinator, 0, len(l.coordinators))
	for _, c := range l.coordinators {
		if c != nil {
			out = append(out, c)
		}
	}
	return out
}

// ForegroundWork names the foreground work in flight ("" when none) and the
// latest instant foreground work was seen: the newest refresh ticket's
// admission or the end of the newest foreground cycle of any checkout.
func (a lifecycleForegroundActivity) ForegroundWork() (string, time.Time) {
	l := a.l
	if l == nil {
		return "", time.Time{}
	}
	busy := ""
	if gate := l.buildGate(); gate != nil {
		stats := gate.Stats()
		switch {
		case stats.InteractiveQueued > 0:
			busy = "interactive_build"
		case stats.Active && stats.Holder != nil && foregroundLaneHolder(stats.Holder.Kind):
			busy = stats.Holder.Kind
		}
	}
	var last time.Time
	for _, c := range l.registeredCoordinators() {
		if nanos := c.ticketDemand.Load(); nanos != 0 {
			if at := time.Unix(0, nanos); at.After(last) {
				last = at
			}
		}
		c.compaction.mu.Lock()
		ended := c.compaction.lastForeground
		c.compaction.mu.Unlock()
		if ended.After(last) {
			last = ended
		}
		if busy != "" {
			continue
		}
		if c.checkoutRefreshHighWater() != 0 {
			busy = "refresh_ticket"
		} else if len(c.demand) > 0 {
			busy = "demand"
		}
	}
	return busy, last
}

// CheckoutTouched reports whether a checkout rooted at root has admitted a
// refresh ticket (an MCP edit, or a query that asked for a fresh answer)
// since its coordinator started. A filesystem edit nobody queried does not
// count: that checkout is warmed only once the daemon is idle for longer.
func (a lifecycleForegroundActivity) CheckoutTouched(root string) bool {
	if a.l == nil || root == "" {
		return false
	}
	want := canonicalCheckoutRoot(root)
	for _, c := range a.l.registeredCoordinators() {
		if c.ticketDemand.Load() == 0 {
			continue
		}
		if canonicalCheckoutRoot(c.root) == want {
			return true
		}
	}
	return false
}

// canonicalCheckoutRoot is root absolute, cleaned and with symlinks resolved
// where possible, so /tmp and /private/tmp spellings compare equal.
func canonicalCheckoutRoot(root string) string {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return filepath.Clean(root)
}
