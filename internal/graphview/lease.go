package graphview

import (
	"context"
	"slices"
	"sync"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// LeaseManager refcounts the graph generations that in-flight requests are
// reading. A generation with a live lease must stay materialized: dropping it
// under a reader would hand back results stitched from two different states of
// the world.
//
// The zero value is ready to use; NewLeaseManager exists for call sites that
// prefer an explicit constructor. All methods are safe for concurrent use.
type LeaseManager struct {
	mu     sync.Mutex
	cond   *sync.Cond
	counts map[int64]int
}

// NewLeaseManager returns an empty lease manager.
func NewLeaseManager() *LeaseManager {
	m := &LeaseManager{}
	m.mu.Lock()
	m.initLocked()
	m.mu.Unlock()
	return m
}

// initLocked lazily builds the map and the condition variable so the zero
// value works. The caller holds m.mu.
func (m *LeaseManager) initLocked() {
	if m.counts == nil {
		m.counts = make(map[int64]int)
	}
	if m.cond == nil {
		m.cond = sync.NewCond(&m.mu)
	}
}

// Lease is a pin on a set of generations, held until Release.
type Lease struct {
	mgr  *LeaseManager
	ids  []int64
	once sync.Once
}

// Acquire pins every id and returns the lease that holds them. The returned
// lease is never nil; acquiring no ids yields a lease whose Release is a no-op.
// Repeating an id in one call pins it that many times, and Release drops each
// of those pins — the refcount stays balanced either way.
func (m *LeaseManager) Acquire(ids ...int64) *Lease {
	l := &Lease{mgr: m, ids: slices.Clone(ids)}
	if len(l.ids) == 0 {
		return l
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	pinned := 0
	for _, id := range l.ids {
		if m.counts[id] == 0 {
			// The gauge counts generations under a lease, not lease holders:
			// a second reader of the same generation adds no new thing that
			// retirement has to refuse.
			pinned++
		}
		m.counts[id]++
	}
	viewmetrics.AddGauge(viewmetrics.LeasesHeld, int64(pinned))
	return l
}

// Held reports how many payload generations currently have a live lease. It
// is the level the LeasesHeld gauge tracks, read directly for a status
// payload that wants the number rather than the counter's history.
func (m *LeaseManager) Held() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.counts)
}

// InUse reports whether any live lease pins id.
func (m *LeaseManager) InUse(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[id] > 0
}

// WaitDrain blocks until no live lease pins any of ids, or ctx is done — in
// which case it returns ctx.Err(). Waiting on an already-drained set returns
// nil immediately, even from a cancelled context: there was nothing to wait
// for. It never polls; releases wake it through the condition variable, and
// cancellation wakes it through a context.AfterFunc broadcast.
func (m *LeaseManager) WaitDrain(ctx context.Context, ids ...int64) error {
	if len(ids) == 0 {
		return nil
	}
	m.mu.Lock()
	m.initLocked()
	m.mu.Unlock()

	// Wake the waiter on cancellation. Registered before the lock is taken so
	// the deferred stop runs after the deferred unlock: stop may have to wait
	// for a running callback, and that callback wants m.mu.
	stop := context.AfterFunc(ctx, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.cond.Broadcast()
	})
	defer stop()

	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		if !m.anyInUseLocked(ids) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		m.cond.Wait()
	}
}

// anyInUseLocked reports whether any of ids is still pinned. The caller holds
// m.mu.
func (m *LeaseManager) anyInUseLocked(ids []int64) bool {
	for _, id := range ids {
		if m.counts[id] > 0 {
			return true
		}
	}
	return false
}

// release drops one pin per id and wakes the waiters only when something
// actually reached zero — that is the only transition WaitDrain cares about.
func (m *LeaseManager) release(ids []int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	drained := false
	released := 0
	for _, id := range ids {
		switch n := m.counts[id]; {
		case n <= 0:
			// Not pinned: a lease can only be released once, so this means
			// the id was never acquired. Nothing to drop.
		case n == 1:
			delete(m.counts, id)
			drained = true
			released++
		default:
			m.counts[id] = n - 1
		}
	}
	viewmetrics.AddGauge(viewmetrics.LeasesHeld, -int64(released))
	if drained {
		m.cond.Broadcast()
	}
}

// Release drops the lease. It is idempotent: later calls, and calls on a nil
// lease, do nothing.
func (l *Lease) Release() {
	if l == nil || l.mgr == nil {
		return
	}
	l.once.Do(func() { l.mgr.release(l.ids) })
}

// IDs returns a copy of the generations this lease pins.
func (l *Lease) IDs() []int64 {
	if l == nil {
		return nil
	}
	return slices.Clone(l.ids)
}
