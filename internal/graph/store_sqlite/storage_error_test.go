package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// SafeStorageFailureReason's classification, from the side that consumes it.
//
// The reason string is what a person reads on a health census when a daemon
// has stopped collecting payload, so two things have to be true of it at once:
// it must be produced for the failures a person can act on — a full volume, a
// failing disk, a damaged database — and it must NOT be produced for the
// ordinary outcomes of a background pass, because "check the store volume" in
// front of someone whose volume is fine makes the surface worthless the first
// time it matters.

// realSQLiteFullError provokes an actual SQLITE_FULL from the store's own
// writer connection by pinning the database's page ceiling at its current size
// and then asking for more pages. It changes only this test database's SQLite
// page limit, never filesystem space.
func realSQLiteFullError(t *testing.T, store *Store) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if store.bulkConn != nil {
		t.Fatal("this page-limit fixture requires the actual non-bulk writer pool")
	}
	if _, err := store.writerDB.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS gxh_full_probe(id INTEGER PRIMARY KEY, payload BLOB)`); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	var pages int64
	if err := store.writerDB.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatalf("read page count: %v", err)
	}
	previous, err := privateFullPageLimit(ctx, store, 0)
	if err != nil {
		t.Fatalf("read page ceiling: %v", err)
	}
	t.Cleanup(func() {
		restoreCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := privateFullPageLimit(restoreCtx, store, previous); err != nil {
			t.Errorf("restore page ceiling: %v", err)
		}
	})
	if capped, err := privateFullPageLimit(ctx, store, pages); err != nil || capped != pages {
		t.Fatalf("page ceiling not installed: got=%d want=%d err=%v", capped, pages, err)
	}
	for attempt := 0; attempt < 32; attempt++ {
		_, err := store.writerDB.ExecContext(ctx,
			`INSERT INTO gxh_full_probe(payload) VALUES (zeroblob(8388608))`)
		if err != nil {
			return err
		}
	}
	t.Fatal("the page ceiling never refused a write, so this fixture proves nothing")
	return nil
}

// TestAFullVolumeIsClassifiedAndStatedInBoundedWords is the wiring pin for
// SafeStorageFailureReason, which had no caller at all before this item: a
// real driver refusal has to reach the readiness vocabulary through
// storageFailure, and it has to arrive wrapped so every existing storage-error
// recovery recognizes it.
//
// Revert-red: drop the *sqlite.Error arm from storageFailure and the ok
// assertion fails; drop the wrapStorageError call and the *StorageError
// assertion fails.
func TestAFullVolumeIsClassifiedAndStatedInBoundedWords(t *testing.T) {
	store := openPayloadStore(t)
	full := realSQLiteFullError(t, store)

	classified, ok := storageFailure(fmt.Errorf("payload generation gc: generation 7: %w", full))
	if !ok {
		t.Fatalf("an actual SQLITE_FULL is not classified as a storage failure: %T %v", full, full)
	}
	var storageErr *StorageError
	if !errors.As(classified, &storageErr) {
		t.Fatalf("the classified error is not a *StorageError: %T", classified)
	}
	if !errors.Is(classified, full) {
		t.Fatal("classification lost the cause, so errors.Is on the driver error stops working")
	}
	reason, ok := storageFailureReason(classified)
	if !ok {
		t.Fatal("an already-classified storage failure was refused a reason")
	}
	if !strings.Contains(reason, "SQLite code 13") || !strings.Contains(reason, "free disk space") {
		t.Fatalf("reason %q does not name a full volume", reason)
	}
	if strings.Contains(reason, "gxh_full_probe") || strings.Contains(reason, t.TempDir()) {
		t.Fatalf("reason %q leaks a path or a table name into durable readiness state", reason)
	}
}

// TestOrdinaryFailuresAreNotStorageFailures is the other half, and it is the
// half that keeps the surface usable: a background pass whose context expired,
// a retirement that lost its fence, and a programmer error are all normal and
// none of them is a disk problem.
func TestOrdinaryFailuresAreNotStorageFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "nil", err: nil},
		{name: "cancelled", err: fmt.Errorf("payload generation gc: %w", context.Canceled)},
		{name: "deadline", err: fmt.Errorf("payload generation gc: %w", context.DeadlineExceeded)},
		{name: "lost fence", err: errors.New("payload generation gc: generation 7 left the retiring state")},
		{name: "lifecycle refusal", err: fmt.Errorf("%w: generation 7", ErrPayloadGenerationInUse)},
		{name: "budget yield", err: fmt.Errorf("%w: generation 7", ErrPayloadSweepBudgetExhausted)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := storageFailure(tc.err); ok {
				t.Fatalf("%v was classified as a storage failure", tc.err)
			}
			if reason, ok := storageFailureReason(tc.err); ok {
				t.Fatalf("%v was given the readiness reason %q", tc.err, reason)
			}
		})
	}
}

// TestACancelledPassIsNotBlamedOnTheVolume pins the one arm of storageFailure
// whose behaviour is a decision rather than a definition.
//
// An interrupted statement reports whatever the driver was doing when the
// interrupt arrived, so "cancelled" and "the driver said something" arrive
// together routinely, and the classifier has to pick one. It picks
// cancellation, deliberately: the register is durable state a person reads as
// the daemon's present, and a "free disk space and retry" earned by a pass
// somebody cancelled is wrong the moment it is written. Nothing is lost — a
// volume that really is full says so again, with no cancellation over it, on
// the very next pass, which is what the second half asserts.
//
// Revert-red: delete the context arm from storageFailure and the first case is
// classified as a storage failure and recorded on the census.
func TestACancelledPassIsNotBlamedOnTheVolume(t *testing.T) {
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)
	full := realSQLiteFullError(t, store)

	// The shape an interrupted sweep produces: the caller's cancellation and
	// the driver's own complaint in one error.
	interrupted := fmt.Errorf("payload generation gc: generation %d: %w: %w", generationID, context.Canceled, full)
	if !errors.Is(interrupted, context.Canceled) || !isSQLiteFullFailure(interrupted) {
		t.Fatalf("fixture error carries only one of the two causes: %v", interrupted)
	}
	if classified, ok := storageFailure(interrupted); ok {
		t.Fatalf("a cancelled pass was classified as a storage failure: %v", classified)
	}
	if store.RecordStorageFailure(generationID, interrupted) {
		t.Fatal("a cancelled pass was recorded against the generation")
	}
	if failures := store.StorageFailures(); len(failures) != 0 {
		t.Fatalf("a cancelled pass reached the census: %+v", failures)
	}

	// The same volume, reported by a pass nobody cancelled.
	if !store.RecordStorageFailure(generationID, fmt.Errorf("payload generation gc: %w", full)) {
		t.Fatal("an uncancelled full volume was not recorded")
	}
	failures := store.StorageFailures()
	if len(failures) != 1 || !strings.Contains(failures[0].Reason, "SQLite code 13") {
		t.Fatalf("storage failures after an uncancelled full volume = %+v", failures)
	}
}

// TestAStorageErrorWithoutADriverCauseStillStatesSomething covers the write
// gate's own wrapping: panicOnFatal produces a *StorageError whose cause may
// be a sealed-generation refusal rather than a driver error, and the reason
// for that must still be a sentence rather than an empty string.
func TestAStorageErrorWithoutADriverCauseStillStatesSomething(t *testing.T) {
	reason, ok := storageFailureReason(&StorageError{})
	if !ok {
		t.Fatal("a *StorageError was refused a reason")
	}
	if reason == "" || !strings.Contains(reason, "graph storage") {
		t.Fatalf("reason %q does not state a storage problem", reason)
	}
}
