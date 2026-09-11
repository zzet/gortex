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

	repositories repositoryLeaseState
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

// Lease is a pin on a set of generations, held until every holder releases
// it.
//
// A freshly acquired lease has exactly one holder: the caller of Acquire.
// Handoff adds a joined consumer — work that outlives the request that
// acquired the lease — and the pins survive until the acquirer *and* every
// joined consumer has released. That is the same shape
// RawRepositorySnapshotLease states for mutable source data ("only until its
// joined consumer closes it"), applied to payload generations: a detached
// worker or a cancellation tail keeps reading a live pin until it actually
// finishes, instead of running over a generation the retirement sweep was
// free to collect the moment the handler returned.
type Lease struct {
	mgr  *LeaseManager
	ids  []int64
	once sync.Once

	mu sync.Mutex
	// holders counts the live holders: the acquirer, plus one per joined
	// consumer that has not released yet. The manager's per-id refcount is
	// dropped exactly once, when this reaches zero.
	holders int
}

// Acquire pins every id and returns the lease that holds them. The returned
// lease is never nil; acquiring no ids yields a lease whose Release is a no-op.
// Repeating an id in one call pins it that many times, and Release drops each
// of those pins — the refcount stays balanced either way.
func (m *LeaseManager) Acquire(ids ...int64) *Lease {
	l := &Lease{mgr: m, ids: slices.Clone(ids), holders: 1}
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

// Release drops the acquirer's hold on the lease. It is idempotent: later
// calls, and calls on a nil lease, do nothing.
//
// It releases the pinned generations only when no joined consumer is live.
// A handler that hands its view to a detached worker and then returns keeps
// the payload pinned until that worker closes its own handle.
func (l *Lease) Release() {
	if l == nil || l.mgr == nil {
		return
	}
	l.once.Do(l.drop)
}

// drop retires one holder and, when it was the last one, drops the manager's
// pins. Each holder calls it at most once — the acquirer through Release's
// sync.Once, a joined consumer through its own.
func (l *Lease) drop() {
	l.mu.Lock()
	if l.holders == 0 {
		l.mu.Unlock()
		return
	}
	l.holders--
	last := l.holders == 0
	l.mu.Unlock()
	if last {
		l.mgr.release(l.ids)
	}
}

// Holders reports how many live holders the lease has: the acquirer while it
// has not released, plus every joined consumer that has not closed. Zero means
// the pins are gone.
func (l *Lease) Holders() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holders
}

// LeaseHandoff is a joined consumer of a Lease: a second holder of the same
// pinned generations, handed to work that outlives the request that acquired
// them — a detached worker, a cancellation tail, a background build.
//
// Close is mandatory and idempotent. Until every joined consumer closes, the
// generations stay pinned, InUse keeps reporting them, retirement keeps
// refusing them and WaitDrain keeps blocking, even after the acquirer has
// released.
type LeaseHandoff struct {
	lease *Lease
	once  sync.Once
}

// Handoff joins a consumer to the lease and returns its handle.
//
// It returns nil when there is nothing left to join — the lease is nil, or
// every holder has already released, so the pins are gone and the payload
// underneath may already have been collected. A caller that wanted to detach
// work must treat nil as a refusal and not run that work against this view;
// it must never treat it as a successful handoff.
func (l *Lease) Handoff() *LeaseHandoff {
	if l == nil || l.mgr == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holders == 0 {
		return nil
	}
	l.holders++
	return &LeaseHandoff{lease: l}
}

// Release closes the joined consumer's hold. It is idempotent, and safe on a
// nil handle. The pinned generations are released once this was the last live
// holder.
func (h *LeaseHandoff) Release() {
	if h == nil || h.lease == nil {
		return
	}
	h.once.Do(h.lease.drop)
}

// IDs returns a copy of the generations this handle keeps pinned.
func (h *LeaseHandoff) IDs() []int64 {
	if h == nil || h.lease == nil {
		return nil
	}
	return h.lease.IDs()
}

// IDs returns a copy of the generations this lease pins.
func (l *Lease) IDs() []int64 {
	if l == nil {
		return nil
	}
	return slices.Clone(l.ids)
}
