package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	sqlite "modernc.org/sqlite"
)

var (
	errSQLiteBusyRetryExhausted   = errors.New("sqlite contention retry window exhausted")
	errSQLiteCheckpointIncomplete = errors.New("sqlite checkpoint incomplete")
)

const (
	// busy_timeout handles ordinary lock acquisition inside SQLite. The
	// application retry below is still required for BUSY_SNAPSHOT and lock
	// promotion failures for which SQLite deliberately does not invoke the busy
	// handler.
	sqliteBusyTimeoutMillis = 5000

	defaultSQLiteBusyRetryTimeout = 15 * time.Second
	sqliteBusyRetryBaseDelay      = 5 * time.Millisecond
	sqliteBusyRetryMaxDelay       = 250 * time.Millisecond
)

// isSQLiteBusyErr matches both primary and extended BUSY/LOCKED result codes.
// Extended codes keep the primary result in the low byte.
func isSQLiteBusyErr(err error) bool {
	if errors.Is(err, errSQLiteCheckpointIncomplete) {
		return true
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
		return true
	default:
		return false
	}
}

// SetBusyRetryTimeoutForTest overrides the store's contention retry window for
// deterministic cross-package tests and returns a restore callback.
func (s *Store) SetBusyRetryTimeoutForTest(d time.Duration) (restore func()) {
	prev := s.busyRetryTimeoutOverride.Swap(int64(d))
	return func() { s.busyRetryTimeoutOverride.Store(prev) }
}

// withSQLiteBusyRetry retries fn only when an entire idempotent transaction
// has rolled back with SQLITE_BUSY/SQLITE_LOCKED. Callers must never use it
// around a statement in an already-open transaction: replaying from the
// transaction boundary is what preserves atomicity.
func (s *Store) sqliteBusyRetryWindow() time.Duration {
	if override := time.Duration(s.busyRetryTimeoutOverride.Load()); override > 0 {
		return override
	}
	if s.busyRetryTimeout > 0 {
		return s.busyRetryTimeout
	}
	return defaultSQLiteBusyRetryTimeout
}

func (s *Store) withSQLiteBusyRetry(
	parent context.Context,
	operation string,
	fn func(context.Context) error,
) error {
	return s.withSQLiteBusyRetryWithWindow(parent, operation, 0, false, fn)
}

func (s *Store) withSQLiteBusyRetryWithWindow(
	parent context.Context,
	operation string,
	window time.Duration,
	suppressLog bool,
	fn func(context.Context) error,
) error {
	// Keep the callback on the caller's context. In particular, a successful
	// sql.BeginTx owns that context until Commit/Rollback; canceling an internal
	// retry context on return would make the transaction roll itself back. The
	// local deadline bounds only repeated BUSY/LOCKED recovery, while ordinary
	// successful work may take longer than the contention window.
	if window <= 0 {
		window = s.sqliteBusyRetryWindow()
	}
	started := time.Now()
	retryDeadline := started.Add(window)
	delay := sqliteBusyRetryBaseDelay
	retries := 0
	var lastBusy error
	for {
		if err := parent.Err(); err != nil {
			return err
		}
		err := fn(parent)
		if err == nil {
			if retries > 0 && !suppressLog {
				log.Printf("store_sqlite: sqlite busy recovered operation=%s retries=%d elapsed=%s", operation, retries, time.Since(started))
			}
			return nil
		}
		if errors.Is(err, errSQLiteBusyRetryExhausted) {
			return err
		}
		if !isSQLiteBusyErr(err) {
			return err
		}
		lastBusy = err

		remaining := time.Until(retryDeadline)
		if remaining <= 0 {
			if !suppressLog {
				log.Printf("store_sqlite: sqlite busy exhausted operation=%s retries=%d elapsed=%s error=%q", operation, retries, time.Since(started), lastBusy)
			}
			return fmt.Errorf("%s: %w", operation, errors.Join(errSQLiteBusyRetryExhausted, lastBusy, context.DeadlineExceeded))
		}

		retries++
		wait := minDuration(delay, remaining)
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-parent.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if !suppressLog {
				log.Printf("store_sqlite: sqlite busy exhausted operation=%s retries=%d elapsed=%s error=%q", operation, retries, time.Since(started), lastBusy)
			}
			return fmt.Errorf("%s: %w", operation, errors.Join(errSQLiteBusyRetryExhausted, lastBusy, parent.Err()))
		}
		if delay *= 2; delay > sqliteBusyRetryMaxDelay {
			delay = sqliteBusyRetryMaxDelay
		}
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
