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
	// highRevision is the largest revision this authority ever published. It
	// only ever rises, including across the one operation that moves revision
	// DOWN (CompleteUnchanged, which restores the observation it displaced),
	// so a revision number is never handed to two different source states.
	// Without it a restore would let the next mutation reuse a revision a live
	// BasePin had already observed, and that pin would compare equal to a
	// state it never saw.
	highRevision uint64
}

// nextRevisionLocked chooses the revision a new mutation publishes: one past
// whichever is larger, the current revision or the high-water mark. Call with
// d.mu held. It reports false when the counter would overflow.
func (d *rawRepositoryDataState) nextRevisionLocked() (uint64, bool) {
	next := d.revision
	if d.highRevision > next {
		next = d.highRevision
	}
	if next >= math.MaxInt64 {
		return 0, false
	}
	next++
	return next, true
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
	data.highRevision = 1
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
	// prior is the observation this mutation displaced, captured under the
	// exclusive gate before the revision moved. It is what CompleteUnchanged
	// restores when the mutation turns out to have moved no content.
	priorRevision    uint64
	priorFingerprint string
	priorAvailable   bool
	mu               sync.Mutex
	released         bool
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
	return openRawRepositoryMutation(ctx, data, owner, reg, durableRevision)
}

// openRawRepositoryMutation is the half of a raw source mutation that runs
// after the owner state is resolved: take the exclusive data gate, move the
// revision forward and mark the source unavailable for the length of the
// write. It is shared by the registration-keyed door above and the
// prefix-keyed base-corpus door below so both choose revisions the same way.
func openRawRepositoryMutation(
	ctx context.Context,
	data *rawRepositoryDataState,
	owner *RepositoryReadLease,
	reg *RawRepositoryRegistration,
	durableRevision uint64,
) (*RawRepositoryMutationLease, error) {
	if err := data.gate.Acquire(ctx, rawRepositoryWriteWeight); err != nil {
		owner.Release()
		return nil, err
	}
	data.mu.Lock()
	if durableRevision > data.revision {
		data.revision = durableRevision
	}
	next, ok := data.nextRevisionLocked()
	if !ok {
		data.mu.Unlock()
		data.gate.Release(rawRepositoryWriteWeight)
		owner.Release()
		return nil, ErrRawRepositorySourceChanged
	}
	prior := rawSourceObservation{
		present:     true,
		revision:    data.revision,
		fingerprint: data.fingerprint,
		available:   data.available,
	}
	data.revision = next
	data.highRevision = next
	data.available = false
	data.fingerprint = ""
	revision := data.revision
	data.mu.Unlock()
	return &RawRepositoryMutationLease{
		owner: owner, data: data, registration: reg, revision: revision,
		priorRevision: prior.revision, priorFingerprint: prior.fingerprint, priorAvailable: prior.available,
	}, nil
}

// baseCorpusOwnerLocked resolves the owner registered for prefix and reports
// whether it can still admit a source mutation. Call with r.mu held.
//
// It deliberately separates two refusals that look alike and are not:
//
//   - state == nil, err != nil — this mutation may not write through the
//     owner's gate: no owner is registered for the prefix, or a raw
//     registration is bound to another root, or it is still provisional.
//     Whether a WITNESS should still move is a separate question, answered by
//     InvalidateBaseCorpusSource.
//   - state != nil, err != nil — the owner is there, with its source data and
//     whatever witnesses readers took from it, but its admission is closing or
//     the manager stopped, so it can no longer admit a lease. Existing pins
//     stay valid and keep answering requests while they drain behind the close
//     (CloseRepositoryAdmission: "existing leases remain valid"), so a write
//     admitted now still has to move their witness — see
//     InvalidateBaseCorpusSource.
//
// A finalized state is reported as nobody: it is removed from byPrefix at
// finalization, and finalization requires zero readers, so no live pin can
// hold a witness on it.
func (r *repositoryLeaseState) baseCorpusOwnerLocked(prefix, canonicalRoot string) (*repositoryOwnerState, error) {
	state := r.byPrefix[prefix]
	if state == nil || state.finalized {
		return nil, ErrRepositoryOwnerUnknown
	}
	if state.rawOwner != nil {
		if canonicalRoot == "" || state.rawOwner.RootIdentity != canonicalRoot || r.byRawRoot[canonicalRoot] != state {
			return nil, ErrRepositoryOwnerUnknown
		}
	}
	if state.rawProvisional {
		return nil, ErrRawRepositoryNotReady
	}
	if r.stopped {
		return state, ErrRepositoryAdmissionsStopped
	}
	if state.closing {
		return state, ErrRepositoryAdmissionClosed
	}
	return state, nil
}

// InvalidateBaseCorpusSource moves the source observation of the owner
// registered for prefix forward WITHOUT taking a lease, so every BasePin that
// witnessed this owner earlier reports ErrBaseCorpusChanged instead of nil.
//
// It exists for the case AcquireBaseCorpusMutation cannot serve honestly: a
// generation-zero write admitted while that door refuses a lease, most of all
// while the owner's admission is closing (or the manager stopped). The owner
// state, its source data and the witnesses readers took from it all still
// exist — closing refuses NEW leases, it does not invalidate the ones already
// out, and finalization cannot run until they drain — so a mutation admitted at
// that moment really does change the corpus a live request is reading.
// Reporting it unwitnessed would let that request claim an exactness nobody
// observed.
//
// It applies NO canonical-root check, deliberately. The root check on the lease
// door answers "may this caller write through this owner's gate"; it is not a
// reason to leave that owner's readers holding a witness the write invalidated.
// Only "no owner is registered for this prefix" (and a provisional one, which
// AcquireBaseCorpus refuses to witness in the first place) moves nothing.
//
// It takes no owner read lease, on purpose: a closed owner may already have
// drained (drainLocked fires on close when there are no readers), and taking a
// reader after that would put a live reader behind a drain that already closed
// its channel. It takes no context and blocks on nothing.
//
// The source is left UNAVAILABLE at the new revision, because nothing will
// certify what the write leaves behind: the caller could not take the gate, so
// it has no lease to Complete. Availability returns with the next mutation that
// can take one.
//
// It reports whether an observation was moved. An owner with no source data at
// all moves nothing and reports false: no pin can hold a witness on it, which
// BasePin already answers as ErrBaseCorpusUnwitnessed.
func (m *LeaseManager) InvalidateBaseCorpusSource(prefix string) (bool, error) {
	if m == nil || prefix == "" {
		return false, ErrRepositoryOwnerInvalid
	}
	r := &m.repositories
	r.mu.Lock()
	state := r.byPrefix[prefix]
	if state == nil || state.finalized {
		r.mu.Unlock()
		return false, ErrRepositoryOwnerUnknown
	}
	if state.rawProvisional {
		r.mu.Unlock()
		return false, ErrRawRepositoryNotReady
	}
	data := state.rawData
	r.mu.Unlock()
	if data == nil {
		return false, nil
	}
	data.mu.Lock()
	defer data.mu.Unlock()
	next, ok := data.nextRevisionLocked()
	if !ok {
		return false, ErrRawRepositorySourceChanged
	}
	data.revision = next
	data.highRevision = next
	data.fingerprint = ""
	data.available = false
	return true, nil
}

// AcquireBaseCorpusMutation takes the exclusive source-data gate of whichever
// owner is registered for prefix — a raw registration or a dedicated one.
//
// Generation zero's bytes are the same bytes whichever registration speaks for
// the prefix, and AcquireBaseCorpus pins a request against that one owner state
// regardless of its kind (it observes state.rawData, not state.rawOwner). The
// registration-keyed door above cannot serve a dedicated owner, and the
// lifecycle registers exactly one dedicated owner per tracked repository
// prefix (bindDedicatedGraph -> RegisterRepositoryOwner), so without this door
// the source witness a legacy mutation is supposed to move is unreachable for
// every repository a daemon actually tracks.
//
// It is NOT dedicated-to-raw inference: it hands back no RawRepositoryOwner and
// authorizes no read. It is the source-mutation gate of one registered owner,
// addressed the way a mutation knows it — by the prefix and root it writes.
// A RAW registration keeps its canonical-root check exactly as
// LookupRawRepositoryRegistration applies it, so a prefix rebound to a
// different root is still refused.
//
// Call it only after the existing batch admission and the stable repository
// lane, which is the order the registration-keyed door documents.
func (m *LeaseManager) AcquireBaseCorpusMutation(ctx context.Context, prefix, canonicalRoot string) (*RawRepositoryMutationLease, error) {
	if m == nil || ctx == nil || prefix == "" {
		return nil, ErrRepositoryOwnerInvalid
	}
	r := &m.repositories
	r.mu.Lock()
	state, err := r.baseCorpusOwnerLocked(prefix, canonicalRoot)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if state.rawData == nil {
		state.rawData = &rawRepositoryDataState{gate: semaphore.NewWeighted(rawRepositoryWriteWeight)}
	}
	data := state.rawData
	owner := r.acquireLocked(m, []*repositoryOwnerState{state}, false)
	r.mu.Unlock()
	return openRawRepositoryMutation(ctx, data, owner, nil, 0)
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

// PriorFingerprint reports the source fingerprint this mutation displaced —
// empty when the owner had no available source before it (the first mutation of
// a never-captured owner, or one whose previous mutation was abandoned).
//
// A caller that derives its fingerprint from CONTENT uses it to answer "did I
// actually move anything": a fingerprint equal to this one means the source it
// wrote is the source that was already there.
func (l *RawRepositoryMutationLease) PriorFingerprint() string {
	if l == nil || !l.priorAvailable {
		return ""
	}
	return l.priorFingerprint
}

// CompleteUnchanged completes a mutation whose CONTENT fingerprint turns out to
// match the one it displaced, by restoring the exact observation readers were
// already holding instead of publishing a new revision.
//
// A revision is allocated at acquisition, before anybody can know whether the
// payload will move a byte, so every admitted mutation — a watcher tick over a
// file nothing changed included — would otherwise make every live BasePin
// report ErrBaseCorpusChanged. Restoring is sound precisely because the
// fingerprints match: the source a reader pinned is the source that is there
// now, and no snapshot could have been taken of the intermediate revision
// because it was marked unavailable for its whole life.
//
// It reports whether the prior observation was restored. A differing (or a
// first-ever) fingerprint takes the ordinary Complete path and publishes the
// new revision.
func (l *RawRepositoryMutationLease) CompleteUnchanged(fingerprint string) (bool, error) {
	if l == nil || fingerprint == "" {
		return false, ErrRawRepositorySourceChanged
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return false, ErrRawRepositorySourceChanged
	}
	l.data.mu.Lock()
	defer l.data.mu.Unlock()
	if l.data.revision != l.revision {
		return false, ErrRawRepositorySourceChanged
	}
	if l.priorAvailable && l.priorFingerprint != "" && l.priorFingerprint == fingerprint {
		l.data.revision = l.priorRevision
		l.data.fingerprint = l.priorFingerprint
		l.data.available = true
		return true, nil
	}
	l.data.fingerprint = fingerprint
	l.data.available = true
	return false, nil
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
