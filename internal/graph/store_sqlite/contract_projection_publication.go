package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// validateContractSelectionLocked requires the exact captured selected vector,
// including absent-header generations and explicit generation zero. A witness
// for a convenient subset or sibling can never stand in for selected proof.
// The caller owns the existing write gate; no public locking method is called.
func (w *PayloadInputWitness) validateContractSelectionLocked(s *Store, ctx context.Context, inputs []graph.ContractInputWitness) error {
	if w == nil || len(inputs) == 0 {
		return ErrPayloadInputChanged
	}
	selected := make(map[int64]bool, len(inputs))
	for _, input := range inputs {
		if input.GenerationID < 0 {
			return ErrPayloadInputChanged
		}
		selected[input.GenerationID] = true
	}
	if len(selected) != len(w.revisions) {
		return ErrPayloadInputChanged
	}
	for _, revision := range w.revisions {
		if !selected[revision.generation] {
			return ErrPayloadInputChanged
		}
	}
	return w.validateLocked(s, ctx)
}
