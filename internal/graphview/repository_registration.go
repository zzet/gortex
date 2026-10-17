package graphview

import "fmt"

// RegisterRepositoryOwnerPrepared validates admission before invoking prepare,
// then publishes the owner with no remaining fallible step. The callback is
// serialized with Acquire/Close/Shutdown under the repository lease mutex; it
// must be bounded, do no SQL or waiting, never reenter this lease domain, and
// leave its external state unchanged when returning an error. This is a
// privileged cross-registry binding boundary, not an ordinary query hook.
func (m *LeaseManager) RegisterRepositoryOwnerPrepared(owner RepositoryOwner, prepare func() error) error {
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
		if prepare != nil {
			return prepare()
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
	if prepare != nil {
		if err := prepare(); err != nil {
			return err
		}
	}
	r.byPrefix[owner.RepoPrefix] = state
	r.byGraph[owner.GraphID] = state
	return nil
}
