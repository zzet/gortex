package graph

import (
	"fmt"
	"time"
)

// Epoch-marker plumbing for the memory backend (issue davalillo/gortex#23):
// a monotonic snapshot generation, carried by journal_open header lines and
// paired with the on-disk epoch sidecar the backend maintains beside the
// snapshot. See store_memory.go's epoch field comment for the replay rules.

// WithJournalEpoch seeds the store's generation from the snapshot sidecar.
// The backend calls it via NewMemoryStore before ReplayJournal so the header
// pairing has the loaded snapshot's generation.
func WithJournalEpoch(epoch uint64) MemoryOption {
	return func(m *MemoryStore) {
		m.epoch = epoch
	}
}

// journalEpochHeader is the dedicated marshal shape for the generation
// header: journalOp carries Epoch with omitempty (regular ops must not emit
// it), but the header must state epoch 0 explicitly to be pairable.
type journalEpochHeader struct {
	Op    string `json:"op"`
	TS    int64  `json:"ts"`
	Epoch uint64 `json:"epoch"`
}

// writeJournalHeaderLocked records the generation header as the next journal
// line. Caller holds m.mu.
func (m *MemoryStore) writeJournalHeaderLocked() {
	if m.journalEnc == nil || m.journalErr != nil {
		return
	}
	if err := m.journalEnc.Encode(&journalEpochHeader{Op: "journal_open", TS: time.Now().UnixMicro(), Epoch: m.epoch}); err != nil {
		m.journalErr = fmt.Errorf("memory journal header: %w", err)
		return
	}
	if err := m.journalW.Flush(); err != nil {
		m.journalErr = fmt.Errorf("memory journal header flush: %w", err)
	}
}

// SnapshotBarrier runs apply — the snapshot dump + journal reset sequence —
// while blocking all mutations: journaled writes queue on applyMu, so the
// dump's read of the graph and the follow-up journal reset are atomic with
// respect to mutation traffic. That is what makes "the journal's ops are a
// subset of the snapshot" provable, which the epoch replay check relies on
// when it skips a stale journal. The barrier is reentrant-safe for the
// dump's internals: nothing inside the sequence calls back into the
// journaled write wrappers or ReplayJournal.
func (m *MemoryStore) SnapshotBarrier(apply func() error) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	return apply()
}
