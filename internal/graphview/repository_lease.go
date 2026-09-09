package graphview

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

var (
	ErrRepositoryOwnerInvalid      = errors.New("graphview: invalid repository owner")
	ErrRepositoryOwnerUnknown      = errors.New("graphview: repository owner is not registered")
	ErrRepositoryOwnerConflict     = errors.New("graphview: repository owner conflicts with a registered owner")
	ErrRepositoryAdmissionClosed   = errors.New("graphview: repository admission is closed")
	ErrRepositoryAdmissionsStopped = errors.New("graphview: repository admissions are stopped")
	ErrRepositoryLeaseInUse        = errors.New("graphview: repository still has readers")
	ErrRepositoryDrainInvalid      = errors.New("graphview: invalid repository cleanup handle")
)

// RepositoryOwner identifies the dedicated owner of a repository's payload,
// including generation zero. CheckoutID is the dedicated owner's checkout, not
// an automatic checkout whose reader happens to use that repository as a base.
// The lifecycle must supply canonical identities from its authorized catalog;
// this process-local admission domain does not establish catalog authority.
type RepositoryOwner struct {
	GraphID     string
	CheckoutID  string
	Incarnation string
	RepoPrefix  string
}

func (o RepositoryOwner) valid() bool {
	return o.GraphID != "" && o.CheckoutID != "" && o.Incarnation != "" && o.RepoPrefix != ""
}

// repositoryLeaseState is a separate namespace in the shared LeaseManager.
// Its mutex never nests with the generation lease mutex. In particular, closing
// repository admission does not change the existing Acquire(ids...) contract.
type repositoryLeaseState struct {
	mu             sync.Mutex
	byPrefix       map[string]*repositoryOwnerState
	byGraph        map[string]*repositoryOwnerState
	closing        map[*repositoryOwnerState]struct{}
	readers        int
	broadReaders   int
	stopped        bool
	stoppedDone    chan struct{}
	stoppedDrained bool
}

type repositoryOwnerState struct {
	owner            RepositoryOwner
	readers          int
	closing          bool
	finalized        bool
	drain            *RepositoryDrain
	drained          bool
	nextNotification uint64
	notifications    map[uint64]func()
}

// RepositoryReadLease protects generation-zero-only readers without naming a
// payload generation. An explicit lease pins an immutable owner scope. A broad
// lease additionally protects owners registered later; its Owners snapshot is
// diagnostic, not the full extent of that conservative lifetime protection.
// Release is safe to call concurrently and repeatedly, including on nil.
type RepositoryReadLease struct {
	mgr    *LeaseManager
	states []*repositoryOwnerState
	broad  bool
	once   sync.Once
}

// RepositoryDrain is the cleanup capability for one closed registration. Its
// identity is the registration object, not merely a reusable repository prefix.
type RepositoryDrain struct {
	mgr   *LeaseManager
	state *repositoryOwnerState
	done  chan struct{}
}

// RegisterRepositoryOwner explicitly opens one owner. Re-registering the exact
// currently open owner is idempotent. Neither a closing owner nor another
// incarnation may reopen its prefix or graph ID before explicit finalization.
// After finalization, registering an owner is a new privileged lifecycle action;
// callers must revalidate its catalog authority, never replay an old snapshot.
func (m *LeaseManager) RegisterRepositoryOwner(owner RepositoryOwner) error {
	if m == nil || !owner.valid() {
		return ErrRepositoryOwnerInvalid
	}
	r := &m.repositories
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return ErrRepositoryAdmissionsStopped
	}
	if current := r.byPrefix[owner.RepoPrefix]; current != nil {
		if current.owner != owner {
			return fmt.Errorf("%w: prefix %q", ErrRepositoryOwnerConflict, owner.RepoPrefix)
		}
		if current.closing {
			return fmt.Errorf("%w: prefix %q", ErrRepositoryAdmissionClosed, owner.RepoPrefix)
		}
		return nil
	}
	if r.byGraph[owner.GraphID] != nil {
		return fmt.Errorf("%w: graph %q", ErrRepositoryOwnerConflict, owner.GraphID)
	}
	if r.byPrefix == nil {
		r.byPrefix = make(map[string]*repositoryOwnerState)
		r.byGraph = make(map[string]*repositoryOwnerState)
	}
	state := &repositoryOwnerState{owner: owner}
	r.byPrefix[owner.RepoPrefix] = state
	r.byGraph[owner.GraphID] = state
	return nil
}

// AcquireRepositoryRead atomically pins an explicit owner scope or pins none
// and returns an error. Unknown and closing owners fail closed. Duplicate owners
// are pinned once. An empty explicit scope is empty, NOT the entire corpus;
// only a caller that already proved a catalog-only/empty scope should use it.
func (m *LeaseManager) AcquireRepositoryRead(owners ...RepositoryOwner) (*RepositoryReadLease, error) {
	if m == nil {
		return nil, ErrRepositoryOwnerInvalid
	}
	r := &m.repositories
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil, ErrRepositoryAdmissionsStopped
	}
	states := make([]*repositoryOwnerState, 0, len(owners))
	for _, owner := range owners {
		if !owner.valid() {
			return nil, ErrRepositoryOwnerInvalid
		}
		state := r.byPrefix[owner.RepoPrefix]
		if state == nil || state.owner != owner {
			return nil, fmt.Errorf("%w: prefix %q", ErrRepositoryOwnerUnknown, owner.RepoPrefix)
		}
		if state.closing {
			return nil, fmt.Errorf("%w: prefix %q", ErrRepositoryAdmissionClosed, owner.RepoPrefix)
		}
		if !slices.Contains(states, state) {
			states = append(states, state)
		}
	}
	return r.acquireLocked(m, states, false), nil
}

// AcquireAllRepositoryReads atomically admits a broad reader. Its lifetime
// protects every registered owner AND owners registered later, since a live
// legacy corpus reader can observe those later publications. Even an initially
// empty broad scope is counted. Owners reports only the acquisition snapshot.
// Closing tombstones refuse the entire broad read instead of silently omitting
// still-present payload. Neither acquisition path accesses a catalog or starts
// a goroutine. Registration/publication still requires lifecycle authority.
func (m *LeaseManager) AcquireAllRepositoryReads() (*RepositoryReadLease, error) {
	if m == nil {
		return nil, ErrRepositoryOwnerInvalid
	}
	r := &m.repositories
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil, ErrRepositoryAdmissionsStopped
	}
	states := make([]*repositoryOwnerState, 0, len(r.byPrefix))
	for _, state := range r.byPrefix {
		if state.closing {
			return nil, fmt.Errorf("%w: prefix %q", ErrRepositoryAdmissionClosed, state.owner.RepoPrefix)
		}
		states = append(states, state)
	}
	// Stable scope reporting is useful to downstream binding and tests. Explicit
	// acquisition preserves the caller's order after duplicate removal.
	slices.SortFunc(states, func(a, b *repositoryOwnerState) int {
		if a.owner.RepoPrefix < b.owner.RepoPrefix {
			return -1
		}
		if a.owner.RepoPrefix > b.owner.RepoPrefix {
			return 1
		}
		return 0
	})
	return r.acquireLocked(m, states, true), nil
}

func (r *repositoryLeaseState) acquireLocked(m *LeaseManager, states []*repositoryOwnerState, broad bool) *RepositoryReadLease {
	if broad {
		r.broadReaders++
	} else {
		for _, state := range states {
			state.readers++
		}
		r.readers += len(states)
	}
	return &RepositoryReadLease{mgr: m, states: states, broad: broad}
}

// Owners returns an owned copy of the immutable acquisition snapshot. For a
// broad lease this is diagnostic only: later registrations are also protected.
func (l *RepositoryReadLease) Owners() []RepositoryOwner {
	if l == nil {
		return nil
	}
	owners := make([]RepositoryOwner, len(l.states))
	for i, state := range l.states {
		owners[i] = state.owner
	}
	return owners
}

func (l *RepositoryReadLease) Release() {
	if l == nil || l.mgr == nil {
		return
	}
	var notifications []func()
	l.once.Do(func() {
		r := &l.mgr.repositories
		r.mu.Lock()
		if l.broad {
			r.broadReaders--
			if r.broadReaders == 0 {
				// Only the last broad release can unblock cleanup, and only
				// closed owners need to be examined. Explicit releases never
				// scan the registry or unrelated pending owners.
				for state := range r.closing {
					notifications = r.drainLocked(state, notifications)
				}
			}
		} else {
			for _, state := range l.states {
				state.readers--
				r.readers--
				notifications = r.drainLocked(state, notifications)
			}
		}
		r.drainStoppedLocked()
		r.mu.Unlock()
	})
	// Outside sync.Once as well as the mutex: a callback may idempotently
	// release this lease again without waiting on its own Once invocation.
	runRepositoryNotifications(notifications)
}

// CloseRepositoryAdmission closes one registered owner, once. Existing leases
// remain valid; no new reader can cross this boundary. A close is already
// drained only with zero explicit pins AND zero broad readers. The tombstone
// survives draining until explicit finalization.
func (m *LeaseManager) CloseRepositoryAdmission(owner RepositoryOwner) (*RepositoryDrain, error) {
	if m == nil || !owner.valid() {
		return nil, ErrRepositoryOwnerInvalid
	}
	r := &m.repositories
	r.mu.Lock()
	state := r.byPrefix[owner.RepoPrefix]
	if state == nil || state.owner != owner {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: prefix %q", ErrRepositoryOwnerUnknown, owner.RepoPrefix)
	}
	r.closeLocked(m, state)
	notifications := r.drainLocked(state, nil)
	drain := state.drain
	r.mu.Unlock()
	runRepositoryNotifications(notifications)
	return drain, nil
}

func (r *repositoryLeaseState) closeLocked(m *LeaseManager, state *repositoryOwnerState) {
	if state.closing {
		return
	}
	state.closing = true
	state.drain = &RepositoryDrain{mgr: m, state: state, done: make(chan struct{})}
	if r.closing == nil {
		r.closing = make(map[*repositoryOwnerState]struct{})
	}
	r.closing[state] = struct{}{}
}

func (r *repositoryLeaseState) drainLocked(state *repositoryOwnerState, notifications []func()) []func() {
	if !state.closing || state.drained || state.readers != 0 || r.broadReaders != 0 {
		return notifications
	}
	state.drained = true
	close(state.drain.done)
	for _, notify := range state.notifications {
		notifications = append(notifications, notify)
	}
	state.notifications = nil
	return notifications
}

// Done closes exactly once when this closed registration has no readers.
// A nil/invalid handle returns nil, never a falsely drained channel.
func (d *RepositoryDrain) Done() <-chan struct{} {
	if d == nil {
		return nil
	}
	return d.done
}

// Notify registers one targeted drain notification and returns its cancellation
// function. If already drained, notify runs immediately before Notify returns.
// Otherwise it runs once on drain, outside the lease mutex. Cancellation removes
// a queued callback but does not join one already selected for delivery. The
// callback must be quick (normally a nonblocking send to a coalescing wake
// channel); it must not close that wake channel while delivery can still occur.
// No goroutine is created. Callbacks are discarded after drain or cancellation.
func (d *RepositoryDrain) Notify(notify func()) func() {
	if d == nil || d.mgr == nil || d.state == nil || notify == nil {
		return func() {}
	}
	r := &d.mgr.repositories
	r.mu.Lock()
	if d.state.drained {
		r.mu.Unlock()
		notify()
		return func() {}
	}
	d.state.nextNotification++
	id := d.state.nextNotification
	if d.state.notifications == nil {
		d.state.notifications = make(map[uint64]func())
	}
	d.state.notifications[id] = notify
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(d.state.notifications, id)
		r.mu.Unlock()
	}
}

func runRepositoryNotifications(notifications []func()) {
	for _, notify := range notifications {
		notify()
	}
}

// FinalizeRepositoryCleanup releases a drained tombstone only when the caller's
// exact cleanup handle owns it. Call only after durable physical/config/catalog
// cleanup succeeds. It is idempotent for that handle, including after prefix
// reuse, and cannot remove a replacement registration. Manager-owned state is
// removed here; retained old leases/handles refer only to the old object.
func (m *LeaseManager) FinalizeRepositoryCleanup(drain *RepositoryDrain) error {
	if m == nil || drain == nil || drain.mgr != m || drain.state == nil {
		return ErrRepositoryDrainInvalid
	}
	r := &m.repositories
	r.mu.Lock()
	defer r.mu.Unlock()
	state := drain.state
	if state.drain != drain || !state.closing {
		return ErrRepositoryDrainInvalid
	}
	if state.finalized {
		return nil
	}
	if r.byPrefix[state.owner.RepoPrefix] != state || r.byGraph[state.owner.GraphID] != state {
		return ErrRepositoryDrainInvalid
	}
	if !state.drained {
		return ErrRepositoryLeaseInUse
	}
	delete(r.byPrefix, state.owner.RepoPrefix)
	delete(r.byGraph, state.owner.GraphID)
	delete(r.closing, state)
	if len(r.closing) == 0 {
		r.closing = nil
	}
	if len(r.byPrefix) == 0 {
		r.byPrefix = nil
		r.byGraph = nil
	}
	state.finalized = true
	return nil
}

// ShutdownRepositoryAdmissions permanently denies registrations and all new
// acquisitions in this owner domain. It closes each owner and returns the same
// channel on every call; that channel closes when all owner leases drain. It
// neither waits nor creates a goroutine, and does not affect generation leases.
// Finalization remains available, but can never reopen a stopped manager.
func (m *LeaseManager) ShutdownRepositoryAdmissions() <-chan struct{} {
	if m == nil {
		return nil
	}
	r := &m.repositories
	r.mu.Lock()
	if r.stopped {
		done := r.stoppedDone
		r.mu.Unlock()
		return done
	}
	r.stopped = true
	r.stoppedDone = make(chan struct{})
	var notifications []func()
	for _, state := range r.byPrefix {
		r.closeLocked(m, state)
		notifications = r.drainLocked(state, notifications)
	}
	r.drainStoppedLocked()
	done := r.stoppedDone
	r.mu.Unlock()
	runRepositoryNotifications(notifications)
	return done
}

func (r *repositoryLeaseState) drainStoppedLocked() {
	if r.stopped && !r.stoppedDrained && r.readers == 0 && r.broadReaders == 0 {
		r.stoppedDrained = true
		close(r.stoppedDone)
	}
}
