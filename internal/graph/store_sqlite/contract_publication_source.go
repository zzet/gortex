package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

// A leased view keeps its immutable stack across a content-equal fold rebase.
// Keep the ancestry that actually selected this preparation too: recomputing
// the catalog chain after that metadata-only move would reject the old stack.
// No caller can construct this authority; it is minted from current SQL below.
// Physical states, exact headers/absences and debt are still checked at commit.
type contractPublicationSource struct {
	expected  graph.ContractInputState
	witnesses []graph.ContractInputWitness
	chain     []int64
}

// CaptureContractPublicationSource returns a non-owning, per-preparation
// receiver. The caller must retain the selected source leases through publish.
// It validates actual ancestry and every physical witness in one read snapshot;
// publication may subsequently use that captured ancestry, never a sibling's.
// Ordinary AtGeneration derivations deliberately lose this authority.
func (s *Store) CaptureContractPublicationSource(ctx context.Context, expected graph.ContractInputState, witnesses []graph.ContractInputWitness) (*Store, error) {
	if ctx == nil || s.coreless() || s.db == nil || len(witnesses) == 0 || len(witnesses) > 1024 {
		return nil, fmt.Errorf("%w: missing contract publication source", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	logical, err := graph.ComposeContractInputState(expected.RepoPrefix, expected.CheckoutID, witnesses)
	if err != nil {
		return nil, err
	}
	if !expected.Accepted || logical != expected {
		return nil, ErrCatalogStaleGuard
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	chain, err := contractGenerationAncestryTx(ctx, tx, s.viewGen)
	if err != nil {
		return nil, err
	}
	if err = validateSelectedContractWitnessAncestry(chain, expected.RepoPrefix, witnesses); err != nil {
		return nil, err
	}
	if err = validateContractInputWitnessesTx(ctx, tx, witnesses); err != nil {
		return nil, err
	}
	if err = tx.Rollback(); err != nil {
		return nil, err
	}
	result := s.AtGeneration(s.viewGen)
	result.contractPublicationSource = &contractPublicationSource{expected: expected, witnesses: slices.Clone(witnesses), chain: slices.Clone(chain)}
	return result, nil
}
