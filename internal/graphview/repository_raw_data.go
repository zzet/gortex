package graphview

import (
	"context"
	"errors"
	"math"
	"sync"

	"golang.org/x/sync/semaphore"
)

var ErrRawRepositorySourceChanged = errors.New("graphview: raw repository source revision changed or unavailable")

const rawRepositoryWriteWeight int64 = math.MaxInt32

// rawRepositoryDataState protects mutable source-owned gen0 data separately
// from owner lifetime. Every relevant MI mutation MUST hold its write weight;
// an owner pin alone is never a snapshot. A staged derived generation retains
// a revision/fingerprint witness, not a promise that gen0 stays immutable.
type rawRepositoryDataState struct {
	gate        *semaphore.Weighted
	mu          sync.Mutex
	revision    uint64
	fingerprint string
	available   bool
}

type RawRepositorySourceWitness struct {
	Owner       RawRepositoryOwner
	Revision    uint64
	Fingerprint string
}

// RawRepositorySnapshotLease protects a captured mutable lower only until its
// joined consumer closes it. Future acquisitions must validate the captured
// revision again. Previously admitted readers remain coherent while a writer
// waits; a new current selection must not re-label stale derived output fresh.
type RawRepositorySnapshotLease struct {
	owner   *RepositoryReadLease
	data    *rawRepositoryDataState
	witness RawRepositorySourceWitness
	once    sync.Once
}

func (l *RawRepositorySnapshotLease) Witness() RawRepositorySourceWitness {
	if l == nil {
		return RawRepositorySourceWitness{}
	}
	return l.witness
}

// ValidateCurrent reports whether the captured witness still describes the
// owner's current source state.
//
// The gate this lease holds excludes a writer only while it is held; a lease
// that was taken, validated and handed on protects LIFETIME, not bytes. A
// holder that needs to state on the wire whether its answer was stitched from
// one state of the source therefore asks here rather than assuming, and gets
// ErrRawRepositorySourceChanged when the revision, the fingerprint or the
// availability moved under it.
func (l *RawRepositorySnapshotLease) ValidateCurrent() error {
	if l == nil || l.data == nil {
		return ErrRawRepositorySourceChanged
	}
	current := l.data.observe()
	if !current.present || !current.available ||
		current.revision != l.witness.Revision || current.fingerprint != l.witness.Fingerprint {
		return ErrRawRepositorySourceChanged
	}
	return nil
}

func (l *RawRepositorySnapshotLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() { l.data.gate.Release(1) })
	// Owner drain notifications can reenter this method. Keep that delivery
	// outside our Once, just like RepositoryReadLease itself.
	l.owner.Release()
}

// rawSourceObservation is one owner's mutable source state at one instant:
// whether a data authority exists for it at all, and what that authority says.
//
// It is a comparison value, not a pin. Two observations that differ mean the
// source moved between them — including the case where no authority existed at
// the first observation and one exists at the second, which is the first
// mutation of a never-mutated owner.
type rawSourceObservation struct {
	present     bool
	revision    uint64
	fingerprint string
	available   bool
}

// observe reads the data state's current witness fields. A nil state is the
// zero observation: no authority, nothing to compare.
func (d *rawRepositoryDataState) observe() rawSourceObservation {
	if d == nil {
		return rawSourceObservation{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return rawSourceObservation{
		present:     true,
		revision:    d.revision,
		fingerprint: d.fingerprint,
		available:   d.available,
	}
}

// observeRawSource reads the current source observation of one registered
// owner. The registry mutex guards the rawData pointer; the data mutex guards
// its fields, and neither is held when this returns.
func (m *LeaseManager) observeRawSource(state *repositoryOwnerState) rawSourceObservation {
	if m == nil || state == nil {
		return rawSourceObservation{}
	}
	r := &m.repositories
	r.mu.Lock()
	data := state.rawData
	r.mu.Unlock()
	return data.observe()
}

// CaptureInitialRawRepositorySource opens the data authority of a registered
// owner that has never been mutated, so a reader can pin and validate it.
//
// Without this an owner that was registered and then only ever read has no
// rawRepositoryDataState at all — it is created inside
// AcquireRawRepositoryMutationAfter — so AcquireRawRepositorySnapshot refuses
// it as changed and the owner is unpinnable until somebody writes to it. That
// is the wrong shape: a source nobody has mutated is the easiest state to
// certify, not the hardest.
//
// It fails closed. A second capture, a capture over an authority some mutation
// already opened, and a capture with no fingerprint are all refused rather
// than allowed to overwrite a witness a reader may already hold. The exclusive
// write weight is taken for the capture itself, so it cannot interleave with a
// mutation that is choosing the next revision.
func (m *LeaseManager) CaptureInitialRawRepositorySource(
	ctx context.Context, reg *RawRepositoryRegistration, fingerprint string,
) (uint64, error) {
	if m == nil || ctx == nil {
		return 0, ErrRepositoryOwnerInvalid
	}
	if fingerprint == "" {
		return 0, ErrRawRepositorySourceChanged
	}
	r := &m.repositories
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return 0, ErrRepositoryAdmissionsStopped
	}
	state, err := m.rawRegistrationLocked(reg)
	if err != nil {
		r.mu.Unlock()
		return 0, err
	}
	if state.closing {
		r.mu.Unlock()
		return 0, ErrRepositoryAdmissionClosed
	}
	if state.rawProvisional {
		r.mu.Unlock()
		return 0, ErrRawRepositoryNotReady
	}
	if state.rawData == nil {
		state.rawData = &rawRepositoryDataState{gate: semaphore.NewWeighted(rawRepositoryWriteWeight)}
	}
	data := state.rawData
	owner := r.acquireLocked(m, []*repositoryOwnerState{state}, false)
	r.mu.Unlock()
	if err := data.gate.Acquire(ctx, rawRepositoryWriteWeight); err != nil {
		owner.Release()
		return 0, err
	}
	defer func() {
		data.gate.Release(rawRepositoryWriteWeight)
		owner.Release()
	}()
	data.mu.Lock()
	defer data.mu.Unlock()
	if data.revision != 0 || data.available || data.fingerprint != "" {
		return 0, ErrRawRepositorySourceChanged
	}
	data.revision = 1
	data.fingerprint = fingerprint
	data.available = true
	return data.revision, nil
}

// AcquireRawRepositorySnapshot pins lifetime before entering the cancellable
// data-read gate. expectedRevision=0 requests the current stable source state;
// a positive value is mandatory when materializing an existing derived upper.
func (m *LeaseManager) AcquireRawRepositorySnapshot(ctx context.Context, reg *RawRepositoryRegistration, expectedRevision uint64) (*RawRepositorySnapshotLease, error) {
	if ctx == nil {
		return nil, ErrRepositoryOwnerInvalid
	}
	owner, err := m.AcquireMixedRepositoryRead(nil, []*RawRepositoryRegistration{reg})
	if err != nil {
		return nil, err
	}
	r := &m.repositories
	r.mu.Lock()
	data := reg.state.rawData
	r.mu.Unlock()
	if data == nil {
		owner.Release()
		return nil, ErrRawRepositorySourceChanged
	}
	if err := data.gate.Acquire(ctx, 1); err != nil {
		owner.Release()
		return nil, err
	}
	data.mu.Lock()
	witness := RawRepositorySourceWitness{Owner: reg.Owner(), Revision: data.revision, Fingerprint: data.fingerprint}
	valid := data.available && data.revision > 0 && data.fingerprint != "" && (expectedRevision == 0 || expectedRevision == data.revision)
	data.mu.Unlock()
	if !valid {
		data.gate.Release(1)
		owner.Release()
		return nil, ErrRawRepositorySourceChanged
	}
	return &RawRepositorySnapshotLease{owner: owner, data: data, witness: witness}, nil
}

type RawRepositoryMutationLease struct {
	owner        *RepositoryReadLease
	data         *rawRepositoryDataState
	registration *RawRepositoryRegistration
	revision     uint64
	mu           sync.Mutex
	released     bool
}

// AcquireRawRepositoryMutation admits both committed and provisional owner
// actors, then takes the exclusive data gate. Call only after the existing
// batch admission and stable repository lane(s), in deterministic owner order.
// No lifetime/data wait is permitted while a lifecycle registry mutex is held.
// The returned unavailable revision MUST be persisted in typed catalog owner
// authority before payload writes; failure to persist means do not execute them.
func (m *LeaseManager) AcquireRawRepositoryMutation(ctx context.Context, reg *RawRepositoryRegistration) (*RawRepositoryMutationLease, error) {
	return m.AcquireRawRepositoryMutationAfter(ctx, reg, 0)
}

// AcquireRawRepositoryMutationAfter carries the durably observed source floor
// across process restart/authority replacement. It never moves local state
// backwards, and takes the exclusive gate before choosing the next revision.
// The caller must still persist BeginRawRepositorySourceMutation with the
// observed durable revision; a concurrent durable change makes that CAS fail
// before any payload writer runs.
func (m *LeaseManager) AcquireRawRepositoryMutationAfter(ctx context.Context, reg *RawRepositoryRegistration, durableRevision uint64) (*RawRepositoryMutationLease, error) {
	if m == nil || ctx == nil {
		return nil, ErrRepositoryOwnerInvalid
	}
	if durableRevision >= math.MaxInt64 {
		return nil, ErrRawRepositorySourceChanged
	}
	r := &m.repositories
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil, ErrRepositoryAdmissionsStopped
	}
	state, err := m.rawRegistrationLocked(reg)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if state.closing {
		r.mu.Unlock()
		return nil, ErrRepositoryAdmissionClosed
	}
	if state.rawData == nil {
		state.rawData = &rawRepositoryDataState{gate: semaphore.NewWeighted(rawRepositoryWriteWeight)}
	}
	data := state.rawData
	owner := r.acquireLocked(m, []*repositoryOwnerState{state}, false)
	r.mu.Unlock()
	if err := data.gate.Acquire(ctx, rawRepositoryWriteWeight); err != nil {
		owner.Release()
		return nil, err
	}
	data.mu.Lock()
	if durableRevision > data.revision {
		data.revision = durableRevision
	}
	if data.revision >= math.MaxInt64 {
		data.mu.Unlock()
		data.gate.Release(rawRepositoryWriteWeight)
		owner.Release()
		return nil, ErrRawRepositorySourceChanged
	}
	data.revision++
	data.available = false
	data.fingerprint = ""
	revision := data.revision
	data.mu.Unlock()
	return &RawRepositoryMutationLease{owner: owner, data: data, registration: reg, revision: revision}, nil
}

func (l *RawRepositoryMutationLease) Revision() uint64 {
	if l == nil {
		return 0
	}
	return l.revision
}

// Complete only after ALL raw payload/source workers have joined and the
// catalog source witness was committed under this same exclusive admission.
// Release without Complete deliberately leaves partial/failed data unavailable.
func (l *RawRepositoryMutationLease) Complete(fingerprint string) error {
	if l == nil || fingerprint == "" {
		return ErrRawRepositorySourceChanged
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return ErrRawRepositorySourceChanged
	}
	l.data.mu.Lock()
	defer l.data.mu.Unlock()
	if l.data.revision != l.revision {
		return ErrRawRepositorySourceChanged
	}
	l.data.fingerprint = fingerprint
	l.data.available = true
	return nil
}

func (l *RawRepositoryMutationLease) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return
	}
	l.released = true
	l.mu.Unlock()
	l.data.gate.Release(rawRepositoryWriteWeight)
	l.owner.Release()
}
