package indexer

import "errors"

// RepositoryCollisionSkip is an unchanged repository load, not a successful
// registration or an uncertain failed mutation. Warmup must record the failed
// job without invalidating the delta frontier or publishing the owner's base.
// Unwrap retains the structured collision for other callers.
type RepositoryCollisionSkip struct {
	Collision *PrefixCollisionError
}

func (s *RepositoryCollisionSkip) Error() string {
	if s.Collision == nil {
		return "repository load skipped because prefix ownership is unknown"
	}
	return s.Collision.Error()
}

func (s *RepositoryCollisionSkip) Unwrap() error {
	if s.Collision == nil {
		return nil
	}
	return s.Collision
}

func repositoryCollisionSkip(err error) error {
	var collision *PrefixCollisionError
	if errors.As(err, &collision) {
		return &RepositoryCollisionSkip{Collision: collision}
	}
	return err
}
