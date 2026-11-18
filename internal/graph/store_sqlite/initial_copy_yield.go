package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrGenerationCopyYielded means the entire copy transaction has rolled back
// and released its writer. It is not cancellation of the caller's build.
var ErrGenerationCopyYielded = errors.New("store_sqlite: generation copy yielded to queued writer")

func (s *Store) withInitialCopyTx(ctx context.Context, destination *Store, fn func(context.Context, *sql.Tx) error) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	step, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(chainFoldYieldPoll)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-step.Done():
				return
			case <-ticker.C:
				// Checkout demand may be waiting for this very base. Only a real writer
				// queued behind the held gate proves competing work can use this turn.
				if s.writeMu.waiting() > 0 {
					cancel(ErrGenerationCopyYielded)
					return
				}
			}
		}
	}()
	defer func() { close(stop); <-joined }()
	cause := func(err error) error {
		if errors.Is(context.Cause(step), ErrGenerationCopyYielded) {
			return ErrGenerationCopyYielded
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	conn, release, err := s.activeWriteConnLocked(step)
	if err != nil {
		return cause(err)
	}
	defer release()
	// SQL cancellation must not let database/sql asynchronously roll back a
	// pinned bulk writer after this method has handed the gate to the waiter.
	tx, err := destination.beginWriteOnWithLifetimeContext(step, context.WithoutCancel(step), conn)
	if err != nil {
		return cause(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(step, tx); err != nil {
		return cause(err)
	}
	if err := step.Err(); err != nil {
		return cause(err)
	}
	if err := tx.Commit(); err != nil {
		return cause(err)
	}
	destination.constantInputCounter(destination.viewGen).Add(1)
	return nil
}
