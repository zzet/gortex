package graphview

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

var ErrRawRepositoryNotReady = errors.New("graphview: raw repository registration is not committed")

// RawRepositoryOwner is a process registration, NOT a dedicated checkout.
// The privileged MI registration boundary supplies a canonical root identity
// and a newly minted incarnation; no fake graph or checkout IDs are accepted.
type RawRepositoryOwner struct {
	RepoPrefix   string
	RootIdentity string
	Incarnation  string
}

func (o RawRepositoryOwner) valid() bool {
	return o.RepoPrefix != "" && o.RootIdentity != "" && o.Incarnation != ""
}

type RawRepositoryRegistration struct {
	mgr   *LeaseManager
	state *repositoryOwnerState
}

func (r *RawRepositoryRegistration) Owner() RawRepositoryOwner {
	if r == nil || r.state == nil || r.state.rawOwner == nil {
		return RawRepositoryOwner{}
	}
	return *r.state.rawOwner
}

func (s *repositoryOwnerState) prefix() string {
	if s.rawOwner != nil {
		return s.rawOwner.RepoPrefix
	}
	return s.owner.RepoPrefix
}

// RegisterRawRepositoryOwnerPrepared shares the EXACT dedicated owner mutex,
// namespace, broad-reader counter and shutdown domain. prepare has the same
// bounded/no-SQL/no-wait/no-reentry contract as RegisterRepositoryOwnerPrepared.
// Registration alone does not freeze mutable source or graph payload; that
// requires the real MI mutation/topology owner and source capture below it.
func (m *LeaseManager) RegisterRawRepositoryOwnerPrepared(owner RawRepositoryOwner, prepare func() error) (*RawRepositoryRegistration, error) {
	return m.registerRawRepositoryOwner(owner, prepare, false)
}

func (m *LeaseManager) registerRawRepositoryOwner(owner RawRepositoryOwner, prepare func() error, provisional bool) (*RawRepositoryRegistration, error) {
	if m == nil || !owner.valid() {
		return nil, ErrRepositoryOwnerInvalid
	}
	r := &m.repositories
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil, ErrRepositoryAdmissionsStopped
	}
	if state := r.byPrefix[owner.RepoPrefix]; state != nil {
		if state.rawOwner == nil || *state.rawOwner != owner {
			return nil, fmt.Errorf("%w: prefix %q", ErrRepositoryOwnerConflict, owner.RepoPrefix)
		}
		if state.closing {
			return nil, ErrRepositoryAdmissionClosed
		}
		// A second installer never borrows or commits another actor's provisional state.
		if provisional || state.rawProvisional {
			return nil, ErrRepositoryOwnerConflict
		}
		if prepare != nil {
			if err := prepare(); err != nil {
				return nil, err
			}
		}
		return &RawRepositoryRegistration{mgr: m, state: state}, nil
	}
	if r.byRawRoot[owner.RootIdentity] != nil {
		return nil, fmt.Errorf("%w: raw root %q", ErrRepositoryOwnerConflict, owner.RootIdentity)
	}
	if prepare != nil {
		if err := prepare(); err != nil {
			return nil, err
		}
	}
	if r.byPrefix == nil {
		r.byPrefix = make(map[string]*repositoryOwnerState)
		r.byGraph = make(map[string]*repositoryOwnerState)
	}
	if r.byRawRoot == nil {
		r.byRawRoot = make(map[string]*repositoryOwnerState)
	}
	copyOwner := owner
	state := &repositoryOwnerState{rawOwner: &copyOwner, rawProvisional: provisional}
	r.byPrefix[owner.RepoPrefix], r.byRawRoot[owner.RootIdentity] = state, state
	return &RawRepositoryRegistration{mgr: m, state: state}, nil
}

func (m *LeaseManager) rawRegistrationLocked(registration *RawRepositoryRegistration) (*repositoryOwnerState, error) {
	if registration == nil || registration.mgr != m || registration.state == nil || registration.state.rawOwner == nil {
		return nil, ErrRepositoryOwnerInvalid
	}
	state := registration.state
	owner := state.rawOwner
	if m.repositories.byPrefix[owner.RepoPrefix] != state || m.repositories.byRawRoot[owner.RootIdentity] != state || state.finalized {
		return nil, ErrRepositoryOwnerUnknown
	}
	return state, nil
}

// AcquireMixedRepositoryRead pins the requested dedicated/raw scope atomically.
// A retained old raw handle cannot acquire a replacement even if a privileged
// caller mistakenly reused all identity strings on registration.
func (m *LeaseManager) AcquireMixedRepositoryRead(dedicated []RepositoryOwner, raw []*RawRepositoryRegistration) (*RepositoryReadLease, error) {
	if m == nil {
		return nil, ErrRepositoryOwnerInvalid
	}
	r := &m.repositories
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil, ErrRepositoryAdmissionsStopped
	}
	states := make([]*repositoryOwnerState, 0, len(dedicated)+len(raw))
	add := func(state *repositoryOwnerState) error {
		if state.closing {
			return ErrRepositoryAdmissionClosed
		}
		if state.rawProvisional {
			return ErrRawRepositoryNotReady
		}
		if !slices.Contains(states, state) {
			states = append(states, state)
		}
		return nil
	}
	for _, owner := range dedicated {
		if !owner.valid() {
			return nil, ErrRepositoryOwnerInvalid
		}
		state := r.byPrefix[owner.RepoPrefix]
		if state == nil || state.rawOwner != nil || state.owner != owner {
			return nil, ErrRepositoryOwnerUnknown
		}
		if err := add(state); err != nil {
			return nil, err
		}
	}
	for _, registration := range raw {
		state, err := m.rawRegistrationLocked(registration)
		if err != nil {
			return nil, err
		}
		if err := add(state); err != nil {
			return nil, err
		}
	}
	return r.acquireLocked(m, states, false), nil
}

func (m *LeaseManager) CloseRawRepositoryAdmission(registration *RawRepositoryRegistration) (*RepositoryDrain, error) {
	if m == nil {
		return nil, ErrRepositoryOwnerInvalid
	}
	r := &m.repositories
	r.mu.Lock()
	state, err := m.rawRegistrationLocked(registration)
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

func (l *RepositoryReadLease) RawOwners() []RawRepositoryOwner {
	if l == nil {
		return nil
	}
	var owners []RawRepositoryOwner
	for _, state := range l.states {
		if state.rawOwner != nil {
			owners = append(owners, *state.rawOwner)
		}
	}
	return owners
}

// RepositoryRosterLease owns a fixed COMPLETE registered local roster. Unlike
// broad legacy readers it never reads later registrations. ValidateCurrent is
// a comparison, not an atomic publication primitive: the MI/lifecycle owner
// MUST serialize register/close with its final validation+catalog transaction.
// Dedicated catalog membership is separately captured/validated by Catalog.
type RepositoryRosterLease struct {
	mu       sync.Mutex
	lease    *RepositoryReadLease
	released bool
}

func (m *LeaseManager) AcquireRepositoryRoster() (*RepositoryRosterLease, error) {
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
			return nil, ErrRepositoryAdmissionClosed
		}
		if state.rawProvisional {
			return nil, ErrRawRepositoryNotReady
		}
		states = append(states, state)
	}
	slices.SortFunc(states, func(a, b *repositoryOwnerState) int {
		if a.prefix() < b.prefix() {
			return -1
		}
		if a.prefix() > b.prefix() {
			return 1
		}
		return 0
	})
	return &RepositoryRosterLease{lease: r.acquireLocked(m, states, false)}, nil
}

func (l *RepositoryRosterLease) DedicatedOwners() []RepositoryOwner {
	if l == nil || l.lease == nil {
		return nil
	}
	return l.lease.Owners()
}

func (l *RepositoryRosterLease) RawRegistrations() []*RawRepositoryRegistration {
	if l == nil || l.lease == nil {
		return nil
	}
	var out []*RawRepositoryRegistration
	for _, state := range l.lease.states {
		if state.rawOwner != nil {
			out = append(out, &RawRepositoryRegistration{mgr: l.lease.mgr, state: state})
		}
	}
	return out
}

func (l *RepositoryRosterLease) ValidateCurrent() error {
	if l == nil || l.lease == nil || l.lease.mgr == nil {
		return ErrRepositoryOwnerUnknown
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return ErrRepositoryAdmissionClosed
	}
	r := &l.lease.mgr.repositories
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return ErrRepositoryAdmissionsStopped
	}
	if len(r.byPrefix) != len(l.lease.states) {
		return ErrRepositoryOwnerConflict
	}
	for _, state := range l.lease.states {
		if r.byPrefix[state.prefix()] != state || state.finalized {
			return ErrRepositoryOwnerConflict
		}
		if state.closing {
			return ErrRepositoryAdmissionClosed
		}
		if state.rawProvisional {
			return ErrRawRepositoryNotReady
		}
	}
	return nil
}

func (l *RepositoryRosterLease) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return
	}
	l.released = true
	lease := l.lease
	l.mu.Unlock()
	if lease != nil {
		lease.Release()
	}
}
