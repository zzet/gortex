package platform

import (
	"os"
	"time"
)

// ClaimMarkerSuffix is appended to a path to name the exclusive marker
// ClaimFile and ConsumeFile create while they claim it. A directory sweeper
// recognises a marker abandoned by a process that died mid-claim by this
// suffix; a live marker exists only for the microseconds of the claim.
const ClaimMarkerSuffix = ".claim"

const (
	// A rename, read or delete can lose a race with a concurrent holder on
	// Windows. Give editors, language servers, antivirus and indexers a few
	// seconds to release the handle while keeping an interactive failure bounded.
	transientRetryBudget   = 3 * time.Second
	transientRetryDelay    = 5 * time.Millisecond
	transientRetryMaxDelay = 250 * time.Millisecond
)

const (
	// ClaimFile and ConsumeFile keep a short first probe: their callers use
	// those primitives to arbitrate concurrent ownership, so an external reader
	// must not make arbitration wait for the full user-facing write budget.
	claimProbeBudget = 60 * time.Millisecond
	claimProbeDelay  = 3 * time.Millisecond

	// Once a claim is won, retiring the payload runs on a longer budget because
	// only the file name is outstanding and readers may hold it for the duration
	// of their own read.
	claimRemoveBudget   = 2 * time.Second
	claimRemoveDelay    = time.Millisecond
	claimRemoveMaxDelay = 64 * time.Millisecond
)

// ReplaceFile moves oldpath onto newpath, replacing an existing newpath.
//
// POSIX rename(2) is atomic: it never fails because somebody holds either end
// open, and concurrent renames onto one destination all report success.
// Windows MoveFileEx(REPLACE_EXISTING) instead fails with a sharing violation
// or "Access is denied" while any handle to either end is open — including one
// held for microseconds by another goroutine that is reading the destination
// or replacing it at the same instant — so on Windows those transient failures
// retry with bounded exponential backoff. The wait is deliberately long enough
// for antivirus, language servers and indexers, while errors outside the
// platform's sharing classification still fail immediately. A vanished source
// is never retried: it reports
// ERROR_FILE_NOT_FOUND, which means another caller already moved it and no
// amount of waiting brings it back.
func ReplaceFile(oldpath, newpath string) error {
	return retryTransientWithin(func() error { return os.Rename(oldpath, newpath) }, transientRetryBudget, transientRetryDelay, transientRetryMaxDelay)
}

// RemoveFile deletes path, absorbing transient Windows sharing refusals caused
// by readers that have not yet released their handle. On POSIX no error is
// transient, so the remove runs exactly once.
func RemoveFile(path string) error {
	return retryTransientWithin(func() error { return os.Remove(path) }, transientRetryBudget, transientRetryDelay, transientRetryMaxDelay)
}

// ClaimFile grants exactly one caller the right to consume path and retires it
// as the claim. It reports true for that single winner, and false for every
// loser of a concurrent race, for a repeat claim, and for a path that is not
// there.
//
// Neither a rename nor an unlink is a portable single-winner claim. On Windows
// two racers moving one source can both fail with a sharing violation, so the
// resource is claimed by nobody; on macOS concurrent unlink(2) of one path
// reports success to several callers, so it is claimed by everybody. Exclusive
// creation is atomic everywhere, so the marker — not the delete — decides the
// winner, and the delete it guards then runs against no other claimer.
//
// Retiring the payload is where the two platforms part company. POSIX
// unlink(2) always succeeds for the winner: an open reader keeps its handle,
// the name goes immediately. Windows refuses the delete outright while a
// reader holds the payload open, because Go opens files with FILE_SHARE_READ
// and FILE_SHARE_WRITE but never FILE_SHARE_DELETE, and the delete has to open
// the file for DELETE access first. A winner that reported failure just
// because a reader was mid-read would leave a contended payload claimed by
// nobody, so instead it empties the payload — a write the reader's own share
// mode does permit — and then retries the delete on the longer budget above.
// A delete that still does not land leaves an empty file behind and the claim
// stands. An emptied payload is a consumed payload: a later ClaimFile or
// ConsumeFile refuses it and collects the leftover once the readers are gone,
// so the emptying can never hand a second caller the claim.
//
// The cost of that is visible only to a racing reader on Windows, which can
// now read an empty or truncated payload where it used to read either the
// whole one or nothing. Every consumer of a claimed payload already has to
// validate what it read — on POSIX the delete can land mid-read too — so a
// short read is rejected rather than misread.
func ClaimFile(path string) bool {
	release, ok := claimMarker(path)
	if !ok {
		return false
	}
	defer release()
	if payloadConsumed(path) {
		collectConsumedPayload(path)
		return false
	}
	return retirePayload(path)
}

// ConsumeFile reads path and claims it, handing the content to exactly one
// caller. ok is false for every loser of a concurrent race, for an
// already-consumed path, and for a file that cannot be read.
//
// An empty payload is an already-consumed one, as it is for ClaimFile: the
// content is read before the claim retires the file, so a caller that finds
// nothing to hand over is looking at a payload some earlier winner emptied.
func ConsumeFile(path string) ([]byte, bool) {
	release, ok := claimMarker(path)
	if !ok {
		return nil, false
	}
	defer release()
	var data []byte
	err := retryTransientWithin(func() error {
		var readErr error
		data, readErr = os.ReadFile(path) //nolint:gosec // the caller owns the path
		return readErr
	}, claimProbeBudget, claimProbeDelay, claimProbeDelay)
	if err != nil {
		return nil, false
	}
	if len(data) == 0 {
		collectConsumedPayload(path)
		return nil, false
	}
	if !retirePayload(path) {
		return nil, false
	}
	return data, true
}

// retirePayload deletes the payload on behalf of the caller that holds its
// claim marker, and reports whether that caller comes away owning it. A plain
// delete settles it everywhere but Windows, where a reader can refuse it; the
// winner then empties the payload, which no reader can refuse, and keeps
// retrying the delete on the claim budget. Only a payload that was not there
// to take, or that could not even be emptied, leaves the caller empty-handed.
func retirePayload(path string) bool {
	err := removeFile(path)
	if err == nil {
		return true
	}
	if !transientSharingError(err) {
		return false
	}
	if emptyPayload(path) != nil {
		return false
	}
	_ = removeWithin(path, claimRemoveBudget)
	return true
}

// payloadConsumed reports whether path is the empty file an earlier claim left
// behind when a reader outlasted its delete. A path that is not there is not a
// consumed payload — it is nothing at all, which retirePayload reports on its
// own.
func payloadConsumed(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() == 0
}

// collectConsumedPayload clears an emptied payload once nothing holds it open,
// so the leftover of a delete that lost its race does not outlive the readers
// that caused it. Best effort: a still-held payload stays for the next caller.
func collectConsumedPayload(path string) {
	_ = removeFile(path)
}

// emptyPayload truncates the payload the caller has claimed but cannot yet
// delete. Opening for writing is allowed while a reader holds the file open —
// Go opens files with FILE_SHARE_WRITE — so the content goes away even when
// the name cannot yet.
func emptyPayload(path string) error {
	return retryTransientWithin(func() error {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // the caller owns the path
		if err != nil {
			return err
		}
		return f.Close()
	}, claimProbeBudget, claimProbeDelay, claimProbeDelay)
}

// removeWithin deletes path, retrying a transient sharing refusal with an
// exponential backoff until the budget is spent. On POSIX no error is
// transient, so the delete runs exactly once.
func removeWithin(path string, budget time.Duration) error {
	return retryTransientWithin(func() error { return os.Remove(path) }, budget, claimRemoveDelay, claimRemoveMaxDelay)
}

// claimMarker exclusively creates path's claim marker, returning the release
// that removes it again. A caller that dies before releasing leaves the marker
// behind and the claimed file unclaimable, which keeps the at-most-once
// guarantee the claim exists for.
func claimMarker(path string) (func(), bool) {
	if path == "" {
		return nil, false
	}
	markerPath := path + ClaimMarkerSuffix
	marker, err := os.OpenFile(markerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // the caller owns the path
	if err != nil {
		return nil, false
	}
	_ = marker.Close()
	return func() { _ = removeFile(markerPath) }, true
}

// removeFile deletes path, absorbing the transient Windows refusals. Go opens
// files without FILE_SHARE_DELETE, so a reader that has the file open for the
// microseconds of its read makes DeleteFile fail with "Access is denied" —
// which on POSIX cannot happen at all, since unlink(2) only detaches the name.
func removeFile(path string) error {
	return retryTransientWithin(func() error { return os.Remove(path) }, claimProbeBudget, claimProbeDelay, claimProbeDelay)
}

// retryTransientWithin runs op, repeating it with exponential backoff while it
// fails with a sharing failure another holder will release. The final sleep is
// capped by the remaining budget. On POSIX no error is transient, so op runs
// exactly once.
func retryTransientWithin(op func() error, budget, delay, maxDelay time.Duration) error {
	deadline := time.Now().Add(budget)
	err := op()
	for err != nil && transientSharingError(err) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		if delay > remaining {
			delay = remaining
		}
		time.Sleep(delay)
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
		err = op()
	}
	return err
}
