package graphview

// PrepareRawRepositoryOwner reserves an exact lifetime state before the first
// constructor/payload mutation. It never exposes that state to a new reader.
// The caller owns classification, incarnation minting and the MI topology
// writer. A losing actor never receives another actor's reservation.
func (m *LeaseManager) PrepareRawRepositoryOwner(owner RawRepositoryOwner) (*RawRepositoryRegistration, error) {
	return m.registerRawRepositoryOwner(owner, nil, true)
}

// CommitRawRepositoryOwner opens a previously reserved state only after its
// matching MI metadata/indexer installation succeeds. Installation and commit
// occur under the same topology owner; failures must hide/revert only that
// actor's installation and close its exact registration capability. This method
// does not construct an Indexer, read SQL, wait or invoke external callbacks.
func (m *LeaseManager) CommitRawRepositoryOwner(registration *RawRepositoryRegistration) error {
	if m == nil {
		return ErrRepositoryOwnerInvalid
	}
	r := &m.repositories
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return ErrRepositoryAdmissionsStopped
	}
	state, err := m.rawRegistrationLocked(registration)
	if err != nil {
		return err
	}
	if state.closing {
		return ErrRepositoryAdmissionClosed
	}
	state.rawProvisional = false
	return nil
}

// LookupRawRepositoryRegistration resolves BOTH canonical selected root and
// prefix to a current committed capability. It does not authorize a read: the
// caller must subsequently AcquireMixedRepositoryRead with this exact handle,
// which atomically rejects an intervening close/finalize/retrack. No filesystem
// canonicalization or dedicated-to-raw inference occurs at this boundary.
func (m *LeaseManager) LookupRawRepositoryRegistration(prefix, canonicalRoot string) (*RawRepositoryRegistration, error) {
	if m == nil || prefix == "" || canonicalRoot == "" {
		return nil, ErrRepositoryOwnerInvalid
	}
	r := &m.repositories
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil, ErrRepositoryAdmissionsStopped
	}
	state := r.byPrefix[prefix]
	if state == nil || state.rawOwner == nil || state.rawOwner.RootIdentity != canonicalRoot || r.byRawRoot[canonicalRoot] != state || state.finalized {
		return nil, ErrRepositoryOwnerUnknown
	}
	if state.closing {
		return nil, ErrRepositoryAdmissionClosed
	}
	if state.rawProvisional {
		return nil, ErrRawRepositoryNotReady
	}
	return &RawRepositoryRegistration{mgr: m, state: state}, nil
}
