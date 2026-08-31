package store_sqlite

import (
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
