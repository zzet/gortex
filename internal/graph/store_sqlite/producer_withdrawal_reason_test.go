package store_sqlite

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProducerWithdrawalReasonNormalizesInvalidUTF8BeforeTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reason.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	generationID, generation := newProducerWithdrawalTestGeneration(t, store,
		"source.invalid.before", "source.invalid.boundary")

	invalidBefore := "bad\xffreason"
	if !generation.ScheduleProducerWithdrawal(generationID, "source.invalid.before", invalidBefore) {
		t.Fatal("invalid-before schedule rejected")
	}
	invalidBoundary := strings.Repeat("a", maxProducerWithdrawalReasonBytes-1) + "\xfftail"
	if !generation.ScheduleProducerWithdrawal(generationID, "source.invalid.boundary", invalidBoundary) {
		t.Fatal("invalid-boundary schedule rejected")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close and drain store: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()

	assertPersistedReason := func(producer, want string) {
		t.Helper()
		var got string
		if err := reopened.db.QueryRow(`
SELECT reason FROM generation_producer_completeness
 WHERE view_gen = ? AND producer = ?`, generationID, producer).Scan(&got); err != nil {
			t.Fatalf("read persisted reason for %s: %v", producer, err)
		}
		if got != want {
			t.Fatalf("persisted reason for %s = %q (%d bytes), want %q (%d bytes)", producer, got, len(got), want, len(want))
		}
		if len(got) > maxProducerWithdrawalReasonBytes {
			t.Fatalf("persisted reason for %s has %d bytes, limit %d", producer, len(got), maxProducerWithdrawalReasonBytes)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("persisted reason for %s is invalid UTF-8", producer)
		}
	}

	assertPersistedReason("source.invalid.before", strings.ToValidUTF8(invalidBefore, "\uFFFD"))
	// ToValidUTF8 expands the invalid byte to a three-byte replacement rune.
	// That rune crosses the 512-byte boundary, so truncation must back up to
	// its start and persist only the valid 511-byte prefix.
	assertPersistedReason("source.invalid.boundary", strings.Repeat("a", maxProducerWithdrawalReasonBytes-1))
}
