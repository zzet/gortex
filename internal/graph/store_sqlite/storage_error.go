package store_sqlite

import (
	"context"
	"errors"
	"fmt"

	sqlite "modernc.org/sqlite"
)

// StorageError identifies a failure reported by the persistent graph store.
// It is both returned by checked write capabilities and used as the payload of
// legacy Store-method panics. That distinction lets index/build boundaries
// recover storage failures without swallowing programmer or extractor panics.
type StorageError struct {
	err error
}

func (e *StorageError) Error() string {
	if e == nil || e.err == nil {
		return "store_sqlite: storage operation failed"
	}
	return "store_sqlite: " + e.err.Error()
}

func (e *StorageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// isSQLiteFullFailure recognizes the actual SQLite driver cause, not errors
// that merely resemble a full-volume failure or implement a Code method.
func isSQLiteFullFailure(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr != nil && sqliteErr.Code()&0xff == 13
}

func wrapStorageError(err error) error {
	if err == nil {
		return nil
	}
	var wrapped *StorageError
	if errors.As(err, &wrapped) {
		return err
	}
	return &StorageError{err: err}
}

// StorageErrorFromPanic recognizes only panics emitted by the SQLite store.
// Arbitrary error-valued, runtime, parser, and programmer panics deliberately
// return ok=false and must continue propagating.
func StorageErrorFromPanic(recovered any) (err error, ok bool) {
	storageErr, ok := recovered.(*StorageError)
	if !ok || storageErr == nil {
		return nil, false
	}
	return storageErr, true
}

// SafeStorageFailureReason returns a bounded, path-free message suitable for
// durable readiness state. The returned StorageError itself retains the full
// cause for errors.Is/errors.As and daemon diagnostics.
func SafeStorageFailureReason(err error) string {
	var storageErr *StorageError
	if !errors.As(err, &storageErr) {
		return "graph generation build failed; see daemon log"
	}
	var sqliteErr *sqlite.Error
	if !errors.As(storageErr, &sqliteErr) {
		return "graph storage write failed; see daemon log"
	}
	code := sqliteErr.Code() & 0xff
	switch code {
	case 7: // SQLITE_NOMEM
		return "graph storage ran out of memory (SQLite code 7); retry after reducing system pressure"
	case 10: // SQLITE_IOERR
		return "graph storage I/O failed (SQLite code 10); check the store volume and daemon log"
	case 13: // SQLITE_FULL
		return "graph storage volume is full (SQLite code 13); free disk space and retry"
	default:
		return fmt.Sprintf("graph storage failed (SQLite code %d); see daemon log", code)
	}
}

// storageFailure classifies an error raised by the store's own maintenance —
// the retirement sweep, and anything else that deletes or rewrites payload on
// the daemon's behalf rather than on a caller's.
//
// A maintenance path has no user to hand an error to, so the question it has
// to answer is narrower than "did this fail": it is "is this the storage layer
// telling us the volume or the database is unusable", because that is the one
// class a person has to act on and the one class that will still be true on
// the next pass. Three answers:
//
//   - a cancelled or expired context is the caller leaving, not the store
//     failing, and is never a storage failure however deep it is wrapped. It
//     is checked first, and that ordering is the decision: an interrupted
//     statement reports whatever the driver was doing when the interrupt
//     arrived, so an error that carries both a cancellation and a driver code
//     is read as the cancellation. A durable "check the store volume" earned
//     by a pass somebody cancelled would be wrong the moment it was written,
//     and a volume that really is full says so again — with no cancellation
//     over it — on the very next pass;
//   - anything that already carries a *StorageError has been classified by
//     the write gate (panicOnFatal wraps SQLITE_FULL and sealed-generation
//     refusals there) and is passed through unchanged;
//   - anything else that unwraps to a driver error is the storage layer
//     speaking — SQLITE_FULL on a volume with no space, SQLITE_IOERR on a
//     failing disk, SQLITE_CORRUPT on a damaged database — and is wrapped so
//     it reads the same way as the write gate's, including under
//     SafeStorageFailureReason.
//
// Everything else — a refusal this package raised itself, a lost fence, a
// programmer error — returns ok=false and keeps its own identity, because
// calling it a storage failure would put "check the store volume" in front of
// a person whose volume is fine.
func storageFailure(err error) (error, bool) {
	if err == nil {
		return nil, false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err, false
	}
	var storageErr *StorageError
	if errors.As(err, &storageErr) {
		return err, true
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return wrapStorageError(err), true
	}
	return err, false
}

// storageFailureReason is storageFailure rendered for durable readiness state.
// It is the one place SafeStorageFailureReason is consumed, so the string a
// person reads on a status payload and the string in the daemon log come from
// the same classification of the same error.
func storageFailureReason(err error) (string, bool) {
	classified, ok := storageFailure(err)
	if !ok {
		return "", false
	}
	return SafeStorageFailureReason(classified), true
}
