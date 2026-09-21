package graphview

import (
	"errors"
	"fmt"
	"slices"
	"strings"
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
	// registerMu orders a registration that must also name the registration
	// object it opened against every finalization. It is taken BEFORE mu and
	// by nothing else, so it adds no lock cycle; see
	// RegisterRepositoryOwnerHandle for what it buys.
	registerMu     sync.Mutex
	mu             sync.Mutex
	byPrefix       map[string]*repositoryOwnerState
	byGraph        map[string]*repositoryOwnerState
	byRawRoot      map[string]*repositoryOwnerState
	closing        map[*repositoryOwnerState]struct{}
	readers        int
	broadReaders   int
	stopped        bool
	stoppedDone    chan struct{}
	stoppedDrained bool
}

type repositoryOwnerState struct {
	owner            RepositoryOwner
	rawOwner         *RawRepositoryOwner     // nil for dedicated owners; never fake dedicated IDs
	rawProvisional   bool                    // reserves lifetime before constructor writes; not visible to new readers
	rawData          *rawRepositoryDataState // initialized under the shared owner mutex
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
// A freshly acquired lease has exactly one holder: the caller that acquired
// it. Handoff adds a joined consumer — work that outlives the request that
// acquired the lease — and the owner pins survive until the acquirer AND
// every joined consumer has released, exactly as Lease does for payload
// generations. That is what lets a detached worker keep reading a repository
// whose owner cannot finalize and whose payload cannot be purged underneath
// it, instead of running over a registration the cleanup saga was free to
// finalize the moment the handler returned.
type RepositoryReadLease struct {
	mgr    *LeaseManager
	states []*repositoryOwnerState
	broad  bool
	once   sync.Once

	mu sync.Mutex
	// holders counts the live holders: the acquirer while it has not
	// released, plus one per joined consumer that has not released yet. The
	// manager's per-owner refcount is dropped exactly once, when this
	// reaches zero.
	holders int
}

// RepositoryReadHandoff is a joined consumer of a RepositoryReadLease: a
// second holder of the same admitted owner scope, handed to work that
// outlives the request that acquired it — a detached worker, a cancellation
// tail, a background build.
//
// Release is mandatory and idempotent. Until every joined consumer releases,
// the owners stay pinned and their drains keep waiting, even after the
// acquirer has released.
type RepositoryReadHandoff struct {
	lease *RepositoryReadLease
	once  sync.Once
}

// RepositoryDrain is the cleanup capability for one closed registration. Its
// identity is the registration object, not merely a reusable repository prefix.
type RepositoryDrain struct {
	mgr   *LeaseManager
	state *repositoryOwnerState
	done  chan struct{}
}

// RepositoryRegistration is the identity of ONE dedicated registration object,
// the dedicated counterpart of RawRepositoryRegistration. It is minted by the
// registration that created the live state and never resolves to a later
// registration that reuses the same prefix, graph ID or complete owner tuple.
// Closing through it is therefore reuse-safe: a stale publisher/drain holder or
// a delayed cleanup callback can only ever close what it actually captured.
type RepositoryRegistration struct {
	mgr   *LeaseManager
	state *repositoryOwnerState
}

// Owner reports the registered owner tuple, the zero value for a nil handle or
// one that does not name a dedicated registration.
func (r *RepositoryRegistration) Owner() RepositoryOwner {
	if r == nil || r.state == nil || r.state.rawOwner != nil {
		return RepositoryOwner{}
	}
	return r.state.owner
}

// dedicatedRegistrationLocked mirrors rawRegistrationLocked: a handle is live
// only while this manager still serves that exact object under BOTH identity
// indexes and it has not been finalized. Closing tombstones stay addressable so
// that re-closing the captured registration remains idempotent.
func (m *LeaseManager) dedicatedRegistrationLocked(registration *RepositoryRegistration) (*repositoryOwnerState, error) {
	if registration == nil || registration.mgr != m || registration.state == nil || registration.state.rawOwner != nil {
		return nil, ErrRepositoryOwnerInvalid
	}
	state := registration.state
	owner := state.owner
	if !owner.valid() {
		return nil, ErrRepositoryOwnerInvalid
	}
	if m.repositories.byPrefix[owner.RepoPrefix] != state || m.repositories.byGraph[owner.GraphID] != state || state.finalized {
		return nil, fmt.Errorf("%w: prefix %q", ErrRepositoryOwnerUnknown, owner.RepoPrefix)
	}
	return state, nil
}

// RegisterRepositoryOwnerHandle registers owner exactly as
// RegisterRepositoryOwnerPrepared does and additionally returns the identity
// handle of the registration that is live for it, so the caller can later close
// THAT registration instead of whatever object happens to hold the prefix. Like
// the raw path it is idempotent for the currently open identical owner.
//
// The handle is resolved in a second critical section of the lease mutex, so
// the two are ordered against each other by registerMu instead: this call
// holds it across BOTH sections, and FinalizeRepositoryCleanup — the only
// place a live registration is removed from byPrefix/byGraph, and therefore
// the only step that can free the prefix for a replacement — takes it too.
//
// That is what makes the returned handle the registration THIS call opened.
// Without it, a finalization plus a replacement registration slipping between
// the two sections would hand the caller a handle naming the replacement, and
// closing "its own" registration through it would close somebody else's — the
// exact reuse hazard the handle exists to prevent. A bare
// RegisterRepositoryOwnerPrepared racing in the same window cannot produce
// that state: with the prefix still held it is either idempotent for the
// identical open owner (the same object) or refused, and freeing the prefix
// requires the finalization registerMu now serializes.
func (m *LeaseManager) RegisterRepositoryOwnerHandle(owner RepositoryOwner, prepare func() error) (*RepositoryRegistration, error) {
	if m == nil {
		return nil, ErrRepositoryOwnerInvalid
	}
	r := &m.repositories
	r.registerMu.Lock()
	defer r.registerMu.Unlock()
	if err := m.RegisterRepositoryOwnerPrepared(owner, prepare); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.byPrefix[owner.RepoPrefix]
	if state == nil || state.rawOwner != nil || state.owner != owner || state.finalized {
		return nil, fmt.Errorf("%w: prefix %q", ErrRepositoryOwnerUnknown, owner.RepoPrefix)
	}
	return &RepositoryRegistration{mgr: m, state: state}, nil
}

// CloseRepositoryRegistration closes the captured registration, once, by object
// identity, and is the close every production caller uses. A handle whose
// registration was already finalized — including one whose prefix, graph ID or
// entire owner tuple has since been reused by a replacement registration — is
// refused instead of closing the replacement. Existing leases stay valid, the
// tombstone survives draining until explicit finalization, and re-closing the
// same registration returns the same drain.
func (m *LeaseManager) CloseRepositoryRegistration(registration *RepositoryRegistration) (*RepositoryDrain, error) {
	if m == nil {
		return nil, ErrRepositoryOwnerInvalid
	}
	r := &m.repositories
	r.mu.Lock()
	state, err := m.dedicatedRegistrationLocked(registration)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.closeLocked(m, state)
	notifications := r.drainLocked(state, nil)
	drain := state.drain
	r.mu.Unlock()
	runRepositoryNotifications(notifications)
	return drain, nil
}

// Registration re-addresses the exact registration this cleanup capability was
// created for. A holder of the drain can re-close or validate that object
// without going back through a reusable owner tuple. It is nil for a raw
// registration's drain, which is addressed by RawRepositoryRegistration.
func (d *RepositoryDrain) Registration() *RepositoryRegistration {
	if d == nil || d.mgr == nil || d.state == nil || d.state.rawOwner != nil {
		return nil
	}
	return &RepositoryRegistration{mgr: d.mgr, state: d.state}
}

// RegisterRepositoryOwner explicitly opens one owner. Re-registering the exact
// currently open owner is idempotent. Neither a closing owner nor another
// incarnation may reopen its prefix or graph ID before explicit finalization.
// After finalization, registering an owner is a new privileged lifecycle action;
// callers must revalidate its catalog authority, never replay an old snapshot.
func (m *LeaseManager) RegisterRepositoryOwner(owner RepositoryOwner) error {
	return m.RegisterRepositoryOwnerPrepared(owner, nil)
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
		if state.rawProvisional {
			return nil, ErrRawRepositoryNotReady
		}
		if state.closing {
			return nil, fmt.Errorf("%w: prefix %q", ErrRepositoryAdmissionClosed, state.prefix())
		}
		states = append(states, state)
	}
	// Stable scope reporting is useful to downstream binding and tests. Explicit
	// acquisition preserves the caller's order after duplicate removal.
	slices.SortFunc(states, func(a, b *repositoryOwnerState) int {
		if a.prefix() < b.prefix() {
			return -1
		}
		if a.prefix() > b.prefix() {
			return 1
		}
		return 0
	})
	return r.acquireLocked(m, states, true), nil
}

// AcquireServingRepositoryRead admits ONE serving request to every repository
// owner that is currently registered and open, for that request's lifetime.
//
// It is the acquisition a request surface makes when it does not yet know
// which repositories its handler will read — the shared corpus spans all of
// them — and it deliberately differs from AcquireAllRepositoryReads on all
// three points that matter to a serving path:
//
//   - It is an EXPLICIT scope, not a broad one. A broad reader blocks every
//     owner's drain globally for as long as it lives (drainLocked's
//     broadReaders check), so admitting one per request would let a busy
//     server starve repository cleanup indefinitely. An explicit scope only
//     holds the owners it actually pinned.
//   - It OMITS closing owners instead of refusing the whole acquisition. A
//     closing owner is one no new reader may be admitted to; omitting it both
//     honours that boundary and is what keeps its drain reachable — every pin
//     on it then comes from a request that started before the close, so the
//     count falls to zero within one request lifetime instead of being
//     renewed forever.
//   - It never fails. Refusing to serve every request because an unrelated
//     repository is being untracked would trade a lifetime problem for an
//     availability one; a request that cannot be admitted anywhere simply
//     holds no repository lifetime, exactly as it did before owners existed.
//
// It returns nil when there is nothing to admit to — no registered owner, or
// admissions are stopped — so a caller never has to branch on the result:
// Release, Handoff and Owners are all nil-safe. It takes no context, touches
// no catalog, starts no goroutine and blocks on nothing.
func (m *LeaseManager) AcquireServingRepositoryRead() *RepositoryReadLease {
	if m == nil {
		return nil
	}
	r := &m.repositories
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil
	}
	states := make([]*repositoryOwnerState, 0, len(r.byPrefix))
	for _, state := range r.byPrefix {
		if state.closing || state.finalized || state.rawProvisional {
			continue
		}
		states = append(states, state)
	}
	if len(states) == 0 {
		return nil
	}
	// Stable scope reporting, as the broad path does: the map iteration order
	// must not leak into Owners().
	slices.SortFunc(states, func(a, b *repositoryOwnerState) int {
		return strings.Compare(a.prefix(), b.prefix())
	})
	return r.acquireLocked(m, states, false)
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
	return &RepositoryReadLease{mgr: m, states: states, broad: broad, holders: 1}
}

// Owners returns an owned copy of the immutable acquisition snapshot. For a
// broad lease this is diagnostic only: later registrations are also protected.
func (l *RepositoryReadLease) Owners() []RepositoryOwner {
	if l == nil {
		return nil
	}
	owners := make([]RepositoryOwner, 0, len(l.states))
	for _, state := range l.states {
		if state.rawOwner == nil {
			owners = append(owners, state.owner)
		}
	}
	return owners
}

// Release drops the acquirer's hold on the admitted owner scope. It is
// idempotent and safe on a nil lease.
//
// It drops the owner pins only when no joined consumer is still live: a
// request that handed its scope to a detached worker keeps that worker's
// repositories un-finalizable until the worker releases its own handle.
func (l *RepositoryReadLease) Release() {
	if l == nil || l.mgr == nil {
		return
	}
	var notifications []func()
	l.once.Do(func() { notifications = l.drop() })
	// Outside sync.Once as well as the mutex: a callback may idempotently
	// release this lease again without waiting on its own Once invocation.
	runRepositoryNotifications(notifications)
}

// drop retires one holder and, when it was the last one, drops the manager's
// owner pins. Each holder calls it at most once — the acquirer through
// Release's sync.Once, a joined consumer through its own — and the drain
// notifications it collects are run by that caller OUTSIDE its Once, because
// a notification is allowed to release this lease again.
func (l *RepositoryReadLease) drop() []func() {
	l.mu.Lock()
	if l.holders == 0 {
		l.mu.Unlock()
		return nil
	}
	l.holders--
	last := l.holders == 0
	l.mu.Unlock()
	if !last {
		return nil
	}
	var notifications []func()
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
	return notifications
}

// Handoff joins a consumer to this admitted owner scope and returns its
// handle.
//
// It returns nil when there is nothing left to join — a nil lease, or a lease
// whose every holder has already released, so the owners are no longer pinned
// and their registrations may already have finalized. A caller that wanted to
// detach work must treat nil as a refusal, never as a successful handoff.
func (l *RepositoryReadLease) Handoff() *RepositoryReadHandoff {
	if l == nil || l.mgr == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holders == 0 {
		return nil
	}
	l.holders++
	return &RepositoryReadHandoff{lease: l}
}

// HandoffFor joins a consumer to the NAMED SUBSET of this admitted owner
// scope: the repositories whose payload the detached work will actually keep
// reading, rather than every repository the acquirer happened to be admitted
// to.
//
// It exists because a serving request cannot know which repositories its
// handler will read — the shared corpus spans all of them — so it is admitted
// to all of them, and that whole-registry scope is only safe for as long as
// the request itself lives. Handing THAT scope to work of unbounded duration
// (a cold first index, say) would make one repository's background work block
// every other repository's drain, and therefore the physical payload purge
// that follows an untrack, for as long as it runs. A worker that names what
// it reads blocks only the drains of what it reads.
//
// The named owners are pinned afresh rather than by refcounting this lease, so
// the handle's lifetime is independent of the acquirer's. Prefixes this lease
// was never admitted to are ignored: a caller cannot widen its own scope by
// naming a repository the acquisition skipped (a closing one, say).
//
// It returns nil when there is nothing to join — a nil lease, no prefixes, a
// lease whose every holder has already released, or a name set that intersects
// the admitted scope in nothing. A caller must read nil as "this worker
// carries no repository lifetime", which for a worker that names repositories
// it reads is a refusal.
func (l *RepositoryReadLease) HandoffFor(prefixes ...string) *RepositoryReadHandoff {
	if l == nil || l.mgr == nil || len(prefixes) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(prefixes))
	for _, prefix := range prefixes {
		if prefix != "" {
			wanted[prefix] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	// The holder check and the fresh pins are taken together under this
	// lease's own mutex, which drop() takes before it touches the manager:
	// while it is held the acquirer cannot fall to zero holders and start
	// draining the very states being re-pinned below. Nothing anywhere takes
	// the manager's repository mutex before a lease mutex, so this nesting
	// introduces no cycle.
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holders == 0 {
		return nil
	}
	r := &l.mgr.repositories
	r.mu.Lock()
	defer r.mu.Unlock()
	states := make([]*repositoryOwnerState, 0, len(wanted))
	for _, state := range l.states {
		if _, ok := wanted[state.prefix()]; !ok {
			continue
		}
		// A closing owner is deliberately still joinable here: the acquirer
		// already pins it, so its drain is already waiting, and the worker
		// really is about to keep reading it. Skipping it would hand the
		// worker an unpinned payload, which is the failure the pin exists to
		// prevent.
		states = append(states, state)
	}
	if len(states) == 0 {
		return nil
	}
	slices.SortFunc(states, func(a, b *repositoryOwnerState) int {
		return strings.Compare(a.prefix(), b.prefix())
	})
	return &RepositoryReadHandoff{lease: r.acquireLocked(l.mgr, states, false)}
}

// Holders reports how many live holders this lease has: the acquirer while it
// has not released, plus every joined consumer that has not released. Zero
// means the owner pins are gone.
func (l *RepositoryReadLease) Holders() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holders
}

// Release closes the joined consumer's hold. It is idempotent, and safe on a
// nil handle. The owner pins are dropped once this was the last live holder.
func (h *RepositoryReadHandoff) Release() {
	if h == nil || h.lease == nil {
		return
	}
	var notifications []func()
	h.once.Do(func() { notifications = h.lease.drop() })
	runRepositoryNotifications(notifications)
}

// Owners returns the acquisition snapshot this handle keeps pinned, exactly
// as the originating lease reported it.
func (h *RepositoryReadHandoff) Owners() []RepositoryOwner {
	if h == nil {
		return nil
	}
	return h.lease.Owners()
}

// CloseRepositoryAdmission closes one registered owner, once, by owner TUPLE:
// it resolves whichever registration currently serves that prefix, so a caller
// holding a stale tuple closes a replacement registration that reused the same
// identity strings. It is retained only for callers that provably hold the
// current identity; every production caller captures a RepositoryRegistration
// at registration time and closes through CloseRepositoryRegistration instead.
//
// Otherwise identical: existing leases remain valid, no new reader can cross
// this boundary, a close is already drained only with zero explicit pins AND
// zero broad readers, and the tombstone survives draining until explicit
// finalization.
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
	// Freeing a prefix is the one step that lets a replacement registration
	// take it, so it is ordered against RegisterRepositoryOwnerHandle's two
	// sections — see that function. registerMu is taken before mu here and
	// nowhere else, so the order is total.
	r.registerMu.Lock()
	defer r.registerMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	state := drain.state
	if state.drain != drain || !state.closing {
		return ErrRepositoryDrainInvalid
	}
	if state.finalized {
		return nil
	}
	if r.byPrefix[state.prefix()] != state {
		return ErrRepositoryDrainInvalid
	}
	if state.rawOwner == nil {
		if r.byGraph[state.owner.GraphID] != state {
			return ErrRepositoryDrainInvalid
		}
	} else if r.byRawRoot[state.rawOwner.RootIdentity] != state {
		return ErrRepositoryDrainInvalid
	}
	if !state.drained {
		return ErrRepositoryLeaseInUse
	}
	delete(r.byPrefix, state.prefix())
	if state.rawOwner == nil {
		delete(r.byGraph, state.owner.GraphID)
	} else {
		delete(r.byRawRoot, state.rawOwner.RootIdentity)
	}
	delete(r.closing, state)
	if len(r.closing) == 0 {
		r.closing = nil
	}
	if len(r.byPrefix) == 0 {
		r.byPrefix = nil
		r.byGraph = nil
		r.byRawRoot = nil
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

// BasePinHandoff is a joined consumer of a BasePin: the same hold on
// generation zero AND on the registered owner that speaks for it, handed to
// work that outlives the request that acquired the pin.
//
// It exists because the two halves of a base pin have to travel together. The
// generation half alone keeps the rows readable; the owner half alone keeps
// the registration from finalizing and the payload from being purged out from
// under the reader. A detached worker handed only one of them is protected
// against only one of the two ways its corpus can disappear.
//
// The source witness is deliberately NOT carried. "Did the corpus move under
// me?" is a question about one request's answer, and a worker that outlives
// that request has no answer left to label; see BasePin's own contract.
//
// Release is mandatory and idempotent.
type BasePinHandoff struct {
	lease *LeaseHandoff
	owner *RepositoryReadHandoff
	once  sync.Once
}

// Handoff joins a consumer to this pin's holds and returns a handle that keeps
// generation zero pinned and its owner un-finalizable on its own.
//
// It returns nil when there is nothing left to join: a nil pin, or one whose
// every holder has already released. A caller that wanted to detach work must
// treat nil as a refusal — the corpus underneath may already be gone — and
// never as a successful handoff.
//
// A non-nil handle is never PARTIAL. Both halves are joined under the pin's
// own mutex, which Release takes and sets `released` under before it drops
// either half, so a handoff that races a release either sees the pin live and
// joins both halves or sees it released and refuses. Without that, joining the
// generation half first could succeed, Release could then drop both halves,
// and the owner half would come back nil — a handle presented as a successful
// handoff that silently lost the lifetime keeping its payload from being
// purged. That race is reachable in production: the deadline firewall's
// retain() runs on the firewall goroutine while the handler goroutine may
// already be inside its own `defer view.close()`.
//
// It lives beside the repository-lease primitives rather than beside BasePin
// because the owner half is the half a request used to drop on return, and
// RepositoryReadHandoff is what makes carrying it possible.
func (p *BasePin) Handoff() *BasePinHandoff {
	if p == nil || p.mgr == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return nil
	}
	joined := p.lease.Handoff()
	if joined == nil {
		return nil
	}
	// A pin taken for an unregistered, closing or absent owner holds no owner
	// half at all; joining it is then correctly a no-op rather than a refusal,
	// because there was never an owner lifetime to extend.
	return &BasePinHandoff{lease: joined, owner: p.owner.Handoff()}
}

// Generations lists what this handle keeps pinned in the generation manager:
// the base corpus generation the originating pin held.
func (h *BasePinHandoff) Generations() []int64 {
	if h == nil {
		return nil
	}
	return h.lease.IDs()
}

// OwnerPinned reports whether this handle also carries the registered owner
// half, so that closing that owner's admission drains behind the detached
// worker rather than through it.
func (h *BasePinHandoff) OwnerPinned() bool {
	return h != nil && h.owner != nil
}

// Release drops the joined consumer's generation hold and owner hold. It is
// idempotent and safe on a nil handle.
func (h *BasePinHandoff) Release() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		h.lease.Release()
		h.owner.Release()
	})
}

func (r *repositoryLeaseState) drainStoppedLocked() {
	if r.stopped && !r.stoppedDrained && r.readers == 0 && r.broadReaders == 0 {
		r.stoppedDrained = true
		close(r.stoppedDone)
	}
}
